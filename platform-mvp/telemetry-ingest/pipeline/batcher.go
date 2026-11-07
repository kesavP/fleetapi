// Package pipeline buffers rows and inserts them in large batches. When
// ClickHouse is slow or down the buffer fills and Add returns ErrOverloaded;
// the ingest handlers turn that into 503 so the agent retries, instead of the
// service dropping data without telling anyone.
package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

var ErrOverloaded = errors.New("telemetry buffer full")

type Options struct {
	BatchRows     int
	MaxBuffered   int
	FlushInterval time.Duration
	Retries       int           // extra attempts after the first
	Backoff       time.Duration // doubled after each failed attempt
	FlushTimeout  time.Duration // per attempt
}

type Stats struct {
	Written, Dropped, FlushErrors, Buffered int64
}

type Batcher[T any] struct {
	name   string
	opt    Options
	flush  func(context.Context, []T) error
	onDrop func([]T)
	log    *slog.Logger

	mu   sync.Mutex
	buf  []T
	kick chan struct{}
	stop chan struct{}
	done chan struct{}

	written, dropped, flushErrors atomic.Int64
}

func NewBatcher[T any](name string, opt Options, log *slog.Logger, flush func(context.Context, []T) error) *Batcher[T] {
	if opt.FlushTimeout <= 0 {
		opt.FlushTimeout = 30 * time.Second
	}
	b := &Batcher[T]{
		name: name, opt: opt, flush: flush, log: log,
		kick: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
	}
	go b.loop()
	return b
}

// OnDrop registers a callback for rows given up on after all retries.
func (b *Batcher[T]) OnDrop(fn func([]T)) { b.onDrop = fn }

// Add queues rows, or returns ErrOverloaded when the buffer is full. One
// request bigger than the whole buffer is still accepted when the buffer is
// empty, so it cannot be refused forever.
func (b *Batcher[T]) Add(rows []T) error { return b.add(rows, false) }

// Force queues rows regardless of the limit. It is for small side tables
// (series, stacks, metadata) that must not fail a request whose main rows
// were already accepted.
func (b *Batcher[T]) Force(rows []T) { _ = b.add(rows, true) }

func (b *Batcher[T]) add(rows []T, force bool) error {
	if len(rows) == 0 {
		return nil
	}
	b.mu.Lock()
	if !force && len(b.buf) > 0 && len(b.buf)+len(rows) > b.opt.MaxBuffered {
		b.mu.Unlock()
		return ErrOverloaded
	}
	b.buf = append(b.buf, rows...)
	full := len(b.buf) >= b.opt.BatchRows
	b.mu.Unlock()
	if full {
		select {
		case b.kick <- struct{}{}:
		default:
		}
	}
	return nil
}

func (b *Batcher[T]) Stats() Stats {
	b.mu.Lock()
	n := len(b.buf)
	b.mu.Unlock()
	return Stats{Written: b.written.Load(), Dropped: b.dropped.Load(), FlushErrors: b.flushErrors.Load(), Buffered: int64(n)}
}

func (b *Batcher[T]) loop() {
	defer close(b.done)
	t := time.NewTicker(b.opt.FlushInterval)
	defer t.Stop()
	for {
		select {
		case <-b.stop:
			b.drain(true)
			return
		case <-t.C:
			b.drain(true)
		case <-b.kick:
			b.drain(false)
		}
	}
}

// drain sends full batches, and with all set the remainder too.
func (b *Batcher[T]) drain(all bool) {
	for {
		b.mu.Lock()
		n := len(b.buf)
		if n == 0 || (!all && n < b.opt.BatchRows) {
			b.mu.Unlock()
			return
		}
		take := n
		if take > b.opt.BatchRows {
			take = b.opt.BatchRows
		}
		rows := b.buf[:take:take]
		if take == n {
			b.buf = nil
		} else {
			b.buf = b.buf[take:]
		}
		b.mu.Unlock()
		b.send(rows)
	}
}

func (b *Batcher[T]) send(rows []T) {
	backoff := b.opt.Backoff
	var err error
	for attempt := 0; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), b.opt.FlushTimeout)
		err = b.flush(ctx, rows)
		cancel()
		if err == nil {
			b.written.Add(int64(len(rows)))
			return
		}
		b.flushErrors.Add(1)
		if attempt >= b.opt.Retries {
			break
		}
		b.log.Warn("insert failed, retrying", "signal", b.name, "rows", len(rows), "attempt", attempt+1, "error", err)
		select {
		case <-time.After(backoff):
		case <-b.stop:
			// shutting down: one more immediate attempt per loop turn is enough
		}
		backoff *= 2
	}
	b.dropped.Add(int64(len(rows)))
	b.log.Error("insert failed, rows dropped", "signal", b.name, "rows", len(rows), "error", err)
	if b.onDrop != nil {
		b.onDrop(rows)
	}
}

// Close flushes what is buffered and stops the loop, or gives up when ctx ends.
func (b *Batcher[T]) Close(ctx context.Context) error {
	select {
	case <-b.stop:
	default:
		close(b.stop)
	}
	select {
	case <-b.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
