package detect

import (
	"math"
	"sort"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/store"
)

// The metrics an application's own users are counted by: what the node-agent sees of INBOUND HTTP, the server's view of its callers.
const (
	RequestsMetric = "container_http_inbound_requests_total"
	LatencyMetric  = "container_http_inbound_requests_duration_seconds_total_bucket"
)

// Timeline is what BuildTimeline makes of the two metrics: every application's requests per step, and the histogram bound "slow" was
// judged at (0 when there is none).
type Timeline struct {
	Grid  Grid
	Apps  []App // by application id
	Bound float64
}

// BuildTimeline lays the series of the two metrics (counter rates per second, one point per step) onto the grid. A step with no point had
// no requests. `slow` is what took longer than thresholdSeconds, read from the smallest histogram bound at or above it, so a threshold
// between two bounds is judged by the larger one (Bound says which); it is nil when no bound is at or above the threshold, since then
// nothing can be said about it.
func BuildTimeline(reqs, buckets []store.MetricSeries, g Grid, thresholdSeconds float64) Timeline {
	zeros := func() []float64 { return make([]float64, g.N) }
	apps := map[string]*App{}
	fast := map[string][]float64{}
	get := func(w Workload) *App {
		a, ok := apps[w.ID]
		if !ok {
			a = &App{ID: w.ID, Label: w.Label, Total: zeros(), Errors: zeros()}
			apps[w.ID] = a
		}
		return a
	}
	add := func(dst []float64, s store.MetricSeries) {
		for _, p := range s.Points {
			i := int(math.Floor((float64(p.T)/1000 - float64(g.Start)) / float64(g.Step)))
			if i >= 0 && i < g.N && p.V > 0 {
				dst[i] += p.V
			}
		}
	}
	for _, s := range reqs {
		w, ok := WorkloadOf(s.Labels["container_id"])
		if !ok {
			continue
		}
		a := get(w)
		add(a.Total, s)
		if IsServerError(s.Labels["status"]) {
			add(a.Errors, s)
		}
	}
	bound := math.Inf(1)
	for _, s := range buckets {
		le := ParseLe(s.Labels["le"])
		if math.IsNaN(le) || math.IsInf(le, 1) || le < thresholdSeconds {
			continue
		}
		bound = math.Min(bound, le)
	}
	haveBound := !math.IsInf(bound, 1)
	if haveBound {
		for _, s := range buckets {
			w, ok := WorkloadOf(s.Labels["container_id"])
			if !ok || ParseLe(s.Labels["le"]) != bound {
				continue
			}
			get(w) // a histogram may name an application the counter did not
			f := fast[w.ID]
			if f == nil {
				f = zeros()
				fast[w.ID] = f
			}
			add(f, s)
		}
	}

	t := Timeline{Grid: g}
	if haveBound {
		t.Bound = bound
	}
	ids := make([]string, 0, len(apps))
	for id := range apps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a := apps[id]
		if f := fast[id]; haveBound && f != nil {
			a.Slow = make([]float64, g.N)
			for i := range a.Slow {
				a.Slow[i] = math.Max(0, a.Total[i]-f[i])
			}
		}
		t.Apps = append(t.Apps, *a)
	}
	return t
}
