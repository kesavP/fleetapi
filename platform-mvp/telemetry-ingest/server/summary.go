package server

import (
	"context"
	"net/http"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/incident"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/summary"
)

// SummarySources is what an incident's instant summary reads besides the incident: the store in service, a fake in a test.
type SummarySources interface {
	SummaryKubeEvents(ctx context.Context, project, namespace string, from, to time.Time) ([]summary.KubeEvent, error)
	SummaryRollouts(ctx context.Context, project, namespace, name string, from, to time.Time) ([]summary.Rollout, error)
	SummaryProcesses(ctx context.Context, project, service string, from, to time.Time) ([]summary.ProcessEvent, error)
}

// how long each source may take: a summary that waits for a slow one is not instant, and the rest is still worth showing
const summarySourceTimeout = 8 * time.Second

// stats answers GET .../incidents/stats?days=N (1 to 90, default 7): MTTD, MTTA, MTTM and MTTR over the last N days.
func (h *Incidents) stats(ctx context.Context, c call) (int, any, error) {
	days, err := intParam(c.r, "days", 7, 1, incident.MaxStatsDays)
	if err != nil {
		return 0, nil, err
	}
	to := h.clock()
	from := to.Add(-time.Duration(days) * 24 * time.Hour)
	st, skipped, err := h.Manager.Stats(ctx, c.project, from, to)
	if err != nil {
		return 0, nil, err
	}
	out := map[string]any{"data": st}
	if skipped > 0 {
		out["meta"] = map[string]int{"skipped": skipped}
	}
	return http.StatusOK, out, nil
}

// summary answers GET .../incidents/{id}/summary: the incident's instant summary (package summary). A source that cannot be read is named in the
// summary's gaps, and the rest is still answered; only the incident itself failing to load is an error.
func (h *Incidents) summary(ctx context.Context, c call) (int, any, error) {
	inc, err := h.Manager.Get(ctx, c.project, c.r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	now := h.clock()
	in := summary.Input{Incident: inc, Now: now}
	from, to := summary.Window(inc, now)
	if h.Summaries == nil {
		const off = "not available on this service"
		in.KubeSource, in.RolloutSource, in.ProcessSource = summary.Source{Note: off}, summary.Source{Note: off}, summary.Source{Note: off}
		return http.StatusOK, map[string]any{"data": summary.Build(in)}, nil
	}
	read := func(name string, fn func(ctx context.Context) error) summary.Source {
		sctx, cancel := context.WithTimeout(ctx, summarySourceTimeout)
		defer cancel()
		if err := fn(sctx); err != nil {
			h.Log.Warn("summary source failed", "project", c.project, "incident", inc.ID, "source", name, "error", err)
			return summary.Source{Note: "could not be read just now"}
		}
		return summary.Source{Read: true}
	}
	if ns, name, ok := summary.Workload(inc.App); ok {
		in.KubeSource = read("kubernetes", func(ctx context.Context) (err error) {
			in.Kube, err = h.Summaries.SummaryKubeEvents(ctx, c.project, ns, from, to)
			return err
		})
		in.RolloutSource = read("rollouts", func(ctx context.Context) (err error) {
			in.Rollouts, err = h.Summaries.SummaryRollouts(ctx, c.project, ns, name, from, to)
			return err
		})
	}
	if inc.App != "" {
		in.ProcessSource = read("processes", func(ctx context.Context) (err error) {
			in.Processes, err = h.Summaries.SummaryProcesses(ctx, c.project, inc.App, from, to)
			return err
		})
	} else {
		in.ProcessSource = summary.Source{Note: "the incident names no application"}
	}
	return http.StatusOK, map[string]any{"data": summary.Build(in)}, nil
}
