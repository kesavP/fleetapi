package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Sink carries a batch away. Send may block (the network); the scheduler never calls a slow sink directly: it hands batches to a Queue.
type Sink interface {
	Send(ctx context.Context, b Batch) error
}

// permanent wraps an error that trying again will not fix (the server refused the batch).
type permanent struct{ error }

func (p permanent) Unwrap() error { return p.error }

// IsPermanent reports whether an error from a Sink is one that a retry cannot fix.
func IsPermanent(err error) bool { var p permanent; return errors.As(err, &p) }

// HTTPSink posts batches as JSON to telemetry-ingest, with the project's ingest key. The key is held here and put only in the request header:
// it is never part of an error, a log line or a metric.
type HTTPSink struct {
	URL    string
	Key    string
	Client *http.Client
}

func (s *HTTPSink) Send(ctx context.Context, b Batch) error {
	body, err := json.Marshal(b)
	if err != nil {
		return permanent{err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return permanent{fmt.Errorf("the ingest address is not usable: %w", errors.Unwrap(err))}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", s.Key)
	c := s.Client
	if c == nil {
		c = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return errors.New("could not reach the ingest service: " + stripURL(err))
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	switch {
	case resp.StatusCode/100 == 2:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500:
		return fmt.Errorf("the ingest service answered %d", resp.StatusCode)
	default:
		return permanent{fmt.Errorf("the ingest service refused the batch (%d): %s", resp.StatusCode, bytes.TrimSpace(snippet))}
	}
}

// stripURL is the transport error without the URL the net/http package puts in it (which may carry a query a person typed).
func stripURL(err error) string {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) && ue.Unwrap() != nil {
		return ue.Unwrap().Error()
	}
	return err.Error()
}

// Queue delivers batches in the background, in order, with retries, and holds a bounded number: when the sink is down for longer than the
// queue can hold, the OLDEST batches are dropped (and counted), because the newest are what someone looking now needs. A collector that
// blocks on a dead ingest service would stop collecting; one that grows without bound would be killed.
type Queue struct {
	sink    Sink
	log     *slog.Logger
	metrics *Metrics
	retries int
	backoff time.Duration

	mu     sync.Mutex
	items  []Batch
	wake   chan struct{}
	closed bool
	done   chan struct{}
	size   int
}

func NewQueue(sink Sink, size, retries int, backoff time.Duration, log *slog.Logger, m *Metrics) *Queue {
	if size < 1 {
		size = 256
	}
	if backoff <= 0 {
		backoff = time.Second
	}
	if m == nil {
		m = NewMetrics()
	}
	q := &Queue{sink: sink, log: log, metrics: m, retries: retries, backoff: backoff, wake: make(chan struct{}, 1), done: make(chan struct{}), size: size}
	return q
}

// Start runs the delivery until ctx ends; Close then drains what can be sent within the grace period.
func (q *Queue) Start(ctx context.Context) {
	go func() {
		defer close(q.done)
		for {
			b, ok := q.next()
			if !ok {
				select {
				case <-ctx.Done():
					q.drain()
					return
				case <-q.wake:
					continue
				}
			}
			q.deliver(ctx, b)
		}
	}()
}

func (q *Queue) next() (Batch, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return Batch{}, false
	}
	b := q.items[0]
	q.items = q.items[1:]
	q.metrics.Set("db_collector_queue_batches", float64(len(q.items)))
	return b, true
}

// Send queues a batch; it never blocks and never fails.
func (q *Queue) Send(_ context.Context, b Batch) error {
	q.mu.Lock()
	if len(q.items) >= q.size {
		q.items = q.items[1:]
		q.metrics.Inc("db_collector_batches_dropped_total", "reason", "queue_full")
	}
	q.items = append(q.items, b)
	q.metrics.Set("db_collector_queue_batches", float64(len(q.items)))
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return nil
}

func (q *Queue) deliver(ctx context.Context, b Batch) {
	wait := q.backoff
	for attempt := 0; ; attempt++ {
		sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := q.sink.Send(sctx, b)
		cancel()
		if err == nil {
			q.metrics.Inc("db_collector_batches_sent_total", "target", b.Target)
			return
		}
		if IsPermanent(err) {
			q.metrics.Inc("db_collector_batches_dropped_total", "reason", "refused")
			q.log.Error("a batch was refused and is dropped", "target", b.Target, "error", err)
			return
		}
		if ctx.Err() != nil {
			q.metrics.Inc("db_collector_batches_dropped_total", "reason", "shutdown")
			return
		}
		if attempt >= q.retries {
			q.metrics.Inc("db_collector_batches_dropped_total", "reason", "unreachable")
			q.log.Warn("a batch is dropped after retries", "target", b.Target, "attempts", attempt+1, "error", err)
			return
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			q.metrics.Inc("db_collector_batches_dropped_total", "reason", "shutdown")
			return
		}
		if wait < 30*time.Second {
			wait *= 2
		}
	}
}

// drain sends what is left, once each, within a few seconds: the last words of a collector that is stopping.
func (q *Queue) drain() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		b, ok := q.next()
		if !ok {
			return
		}
		if err := q.sink.Send(ctx, b); err != nil {
			q.metrics.Inc("db_collector_batches_dropped_total", "reason", "shutdown")
			return
		}
		q.metrics.Inc("db_collector_batches_sent_total", "target", b.Target)
	}
}

// Wait blocks until the delivery has stopped (after its context ended).
func (q *Queue) Wait() { <-q.done }

// Len is how many batches wait, for tests.
func (q *Queue) Len() int { q.mu.Lock(); defer q.mu.Unlock(); return len(q.items) }

// WriterSink writes each batch as one line of JSON: for looking at what would be sent (the --stdout mode), not for production.
type WriterSink struct {
	mu sync.Mutex
	W  io.Writer
}

func (s *WriterSink) Send(_ context.Context, b Batch) error {
	line, err := json.Marshal(b)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.W.Write(append(line, '\n'))
	return err
}
