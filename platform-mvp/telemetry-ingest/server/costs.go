package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/costs"
)

// Reporter makes the cost report of a project over a window: costs.Collector in service.
type Reporter interface {
	Report(ctx context.Context, project string, window int) (costs.Report, error)
}

// Costs serves the infrastructure cost report, and says which optional features this hub has switched on:
//
//	GET .../features        read or manage key   {"costs": true|false}, so the UI shows a page only where it works
//	GET .../costs?window=N  read or manage key   the cost report of the last N seconds (an hour by default); 404 while the feature is switched off
//
// The costs are a FEATURE: a hub that has not switched them on answers 404 and reads nothing. A report reads days of metrics at most, so one is kept for half a
// minute and a page that is refreshed does not read them again each time.
type Costs struct {
	Projects Resolver
	Reporter Reporter
	Enabled  bool
	Log      *slog.Logger

	now   func() time.Time
	mu    sync.Mutex
	cache map[string]cachedReport
}

type cachedReport struct {
	report costs.Report
	until  time.Time
}

const (
	costsTimeout  = 60 * time.Second
	costsKeepFor  = 30 * time.Second
	defaultWindow = 3600
)

func (h *Costs) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/projects/{project}/features", h.serve(h.features))
	mux.HandleFunc("GET /api/v1/projects/{project}/costs", h.serve(h.report))
}

func (h *Costs) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

func (h *Costs) serve(fn func(ctx context.Context, project string, r *http.Request) (int, any)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		key := apiKey(r)
		if key == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing API key"})
			return
		}
		project, ok := h.Projects.ReadProject(key)
		if !ok {
			project, ok = h.Projects.ManageProject(key)
		}
		if !ok || project != r.PathValue("project") {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "this key cannot read that project"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), costsTimeout)
		defer cancel()
		status, body := fn(ctx, project, r)
		writeJSON(w, status, body)
	}
}

func (h *Costs) features(_ context.Context, _ string, _ *http.Request) (int, any) {
	return http.StatusOK, map[string]any{"data": map[string]bool{"costs": h.Enabled}}
}

func (h *Costs) report(ctx context.Context, project string, r *http.Request) (int, any) {
	if !h.Enabled || h.Reporter == nil {
		return http.StatusNotFound, map[string]string{"error": "the costs are not switched on for this hub"}
	}
	// a query that does not parse (a ; in it, say) is refused: Query() would drop what it cannot read and answer for the default window
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return http.StatusBadRequest, map[string]string{"error": "the query is not valid"}
	}
	window := defaultWindow
	if v := q.Get("window"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 60 || n > 31*24*3600 {
			return http.StatusBadRequest, map[string]string{"error": "window must be a whole number of seconds, from 60 to 2678400"}
		}
		window = n
	}
	key := project + "|" + strconv.Itoa(window)
	h.mu.Lock()
	if c, ok := h.cache[key]; ok && h.clock().Before(c.until) {
		h.mu.Unlock()
		return http.StatusOK, map[string]any{"data": c.report}
	}
	h.mu.Unlock()

	rep, err := h.Reporter.Report(ctx, project, window)
	switch {
	case errors.Is(err, costs.ErrWindow):
		return http.StatusBadRequest, map[string]string{"error": "that window is not allowed"}
	case err != nil:
		h.Log.Error("cost report failed", "project", project, "window", window, "error", err)
		return http.StatusInternalServerError, map[string]string{"error": "the report could not be made"}
	}
	h.mu.Lock()
	if h.cache == nil {
		h.cache = map[string]cachedReport{}
	}
	now := h.clock()
	for k, c := range h.cache {
		if !now.Before(c.until) {
			delete(h.cache, k)
		}
	}
	h.cache[key] = cachedReport{report: rep, until: now.Add(costsKeepFor)}
	h.mu.Unlock()
	return http.StatusOK, map[string]any{"data": rep}
}
