// Package server is the HTTP and gRPC shell. Ingest endpoints keep the paths
// and the X-API-Key header the Coroot agents already use, so the spoke chart
// does not change; the query API is new and is scoped to one project per key.
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/model"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/pipeline"
)

type Resolver interface {
	IngestProject(key string) (string, bool)
	ReadProject(key string) (string, bool)
}

type Sink interface {
	AddMetrics([]model.Sample, []model.Series, []model.MetricMeta) error
	AddSpans([]model.Span) error
	AddLogs([]model.LogRecord) error
	AddProfile([]model.ProfileSample, []model.Stack) error
}

// apiKey reads the key the way agents send it: X-API-Key, or a bearer token
// for OTLP exporters that can only set Authorization.
func apiKey(r *http.Request) string {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k
	}
	if a := r.Header.Get("Authorization"); len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
		return strings.TrimSpace(a[7:])
	}
	return ""
}

// Counters is a tiny Prometheus counter set, enough for this service's
// /metrics without a client library.
type Counters struct {
	mu sync.Mutex
	m  map[string]uint64
}

func NewCounters() *Counters { return &Counters{m: map[string]uint64{}} }

// Inc counts one event. Label values are fixed strings chosen by the caller
// (signal names, status codes), never request data.
func (c *Counters) Inc(name string, labels ...string) {
	var b strings.Builder
	b.WriteString(name)
	for i := 0; i+1 < len(labels); i += 2 {
		if i == 0 {
			b.WriteByte('{')
		} else {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", labels[i], labels[i+1])
	}
	if len(labels) > 0 {
		b.WriteByte('}')
	}
	c.mu.Lock()
	c.m[b.String()]++
	c.mu.Unlock()
}

func MetricsHandler(c *Counters, stats func() map[string]pipeline.Stats) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		c.mu.Lock()
		lines := make([]string, 0, len(c.m))
		for k, v := range c.m {
			lines = append(lines, fmt.Sprintf("%s %d", k, v))
		}
		c.mu.Unlock()
		sort.Strings(lines)
		for _, l := range lines {
			_, _ = io.WriteString(w, l+"\n")
		}
		if stats == nil {
			return
		}
		st := stats()
		names := make([]string, 0, len(st))
		for n := range st {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			s := st[n]
			fmt.Fprintf(w, "telemetry_rows_written_total{signal=%q} %d\n", n, s.Written)
			fmt.Fprintf(w, "telemetry_rows_dropped_total{signal=%q} %d\n", n, s.Dropped)
			fmt.Fprintf(w, "telemetry_insert_errors_total{signal=%q} %d\n", n, s.FlushErrors)
			fmt.Fprintf(w, "telemetry_rows_buffered{signal=%q} %d\n", n, s.Buffered)
		}
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
