// Package fakehub is a test hub: it answers the read API's routes with the fixtures in testdata/, which telemetry-ingest's tests write from
// its real handlers (server/apispec_test.go), and remembers what it was asked.
package fakehub

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Key is the read key the fake hub accepts, for the project "us".
const Key = "us-read-key-000000001"

type Hub struct {
	*httptest.Server
	mu   sync.Mutex
	Asks []*url.URL // every request, in order
	Keys []string   // the X-API-Key of every request
}

var routes = []struct{ suffix, fixture string }{
	{"/logs/services", "listLogServices"}, {"/logs", "searchLogs"},
	{"/traces/services", "listTraceServices"}, {"/traces", "searchSpans"},
	{"/metrics/series", "listSeries"}, {"/metrics/query_range", "queryRange"}, {"/metrics", "listMetrics"},
}

// New starts a fake hub serving testdata/ from dir (the module root).
func New(t *testing.T, dir string) *Hub {
	t.Helper()
	h := &Hub{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.Asks = append(h.Asks, r.URL)
		h.Keys = append(h.Keys, r.Header.Get("X-API-Key"))
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/projects/")
		if !ok {
			w.WriteHeader(404)
			w.Write([]byte(`{"error":"not found"}`))
			return
		}
		project, path, _ := strings.Cut(rest, "/")
		if r.Header.Get("X-API-Key") != Key || project != "us" {
			w.WriteHeader(403)
			w.Write([]byte(`{"error":"this key cannot read that project"}`))
			return
		}
		fixture := ""
		if strings.HasPrefix(path, "traces/") && path != "traces/services" {
			fixture = "getTrace"
		} else {
			for _, rt := range routes {
				if "/"+path == rt.suffix {
					fixture = rt.fixture
					break
				}
			}
		}
		if fixture == "" {
			w.WriteHeader(404)
			w.Write([]byte(`{"error":"not found"}`))
			return
		}
		if r.URL.Query().Get("limit") == "0" {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"limit must be an integer from 1 to 1000"}`))
			return
		}
		b, err := os.ReadFile(filepath.Join(dir, "testdata", fixture+".json"))
		if err != nil {
			t.Errorf("fixture %s: %v", fixture, err)
			w.WriteHeader(500)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(h.Close)
	return h
}

// Last is the newest request.
func (h *Hub) Last() *url.URL {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.Asks) == 0 {
		return nil
	}
	return h.Asks[len(h.Asks)-1]
}
