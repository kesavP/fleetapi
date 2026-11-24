package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/cluster-collector/wire"
)

// Sink carries a batch away.
type Sink interface {
	Send(ctx context.Context, b wire.Batch) error
}

// Permanent wraps an error that trying again will not fix: the service refused the batch itself.
type Permanent struct{ error }

func (p Permanent) Unwrap() error { return p.error }

// IsPermanent reports whether err is one a retry cannot fix.
func IsPermanent(err error) bool { var p Permanent; return errors.As(err, &p) }

// HTTPSink posts batches as JSON to telemetry-ingest with the project's ingest key. The key is held here and put only in the request header:
// it is never part of an error or a log line.
type HTTPSink struct {
	URL    string // the full address, ending in /v1/inventory
	Key    string
	Client *http.Client
}

func (s *HTTPSink) Send(ctx context.Context, b wire.Batch) error {
	body, err := json.Marshal(b)
	if err != nil {
		return Permanent{err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return Permanent{fmt.Errorf("the ingest address is not usable: %w", errors.Unwrap(err))}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", s.Key)
	c := s.Client
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
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
		return Permanent{fmt.Errorf("the ingest service refused the batch (%d): %s", resp.StatusCode, bytes.TrimSpace(snippet))}
	}
}

// stripURL is the transport error without the URL net/http puts in it.
func stripURL(err error) string {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) && ue.Unwrap() != nil {
		return ue.Unwrap().Error()
	}
	return err.Error()
}

// WriterSink prints each batch as a line of JSON, for looking at what would be sent (--stdout).
type WriterSink struct{ W io.Writer }

func (s WriterSink) Send(_ context.Context, b wire.Batch) error {
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	_, err = s.W.Write(append(raw, '\n'))
	return err
}
