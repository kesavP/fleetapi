package detect

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/incident"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/rules"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/store"
)

// rightsizeMaxPoints bounds what one resource's query may read: a window of days at a step of minutes, for every container of the project.
const rightsizeMaxPoints = 4_000_000

type sizeSpec struct {
	res          Resource
	usage, limit string
	rate         bool // the usage is a counter, read as a rate
}

var sizeSpecs = []sizeSpec{
	{Memory, MemoryUsageMetric, MemoryLimitMetric, false},
	{CPU, CPUUsageMetric, CPULimitMetric, true},
	{Storage, DiskUsedMetric, DiskSizeMetric, false},
}

// rightsizing opens an information incident for each application whose containers use far less of a resource than they were given, as the project's
// rules say (which resources, how low, how long a window, which percentile). It looks at most every RightsizeEvery, and a look that failed is tried again
// at the next tick. The rules come from the store and a failure to read them is an error, never the defaults: a rule someone tightened must not
// quietly loosen.
func (r *Runner) rightsizing(ctx context.Context, project string, has map[string]bool) (int, error) {
	if !r.Rightsize {
		return 0, nil
	}
	now := r.now().UTC()
	if last, ok := r.lastRightsize[project]; ok && now.Sub(last) < r.RightsizeEvery {
		return 0, nil
	}
	eff := rules.Defaults()
	if r.Rules != nil {
		var err error
		if eff, err = r.Rules.Effective(ctx, project); err != nil {
			return 0, fmt.Errorf("reading the rules: %w", err)
		}
	}
	var existing []incident.Incident
	loaded := false
	opened := 0
	var first error
	for _, sp := range sizeSpecs {
		s, ok := eff[sp.res.RuleName()]
		if !ok || !s.Enabled || !has[sp.usage] || !has[sp.limit] {
			continue
		}
		if !loaded {
			var err error
			if existing, _, err = r.Incidents.List(ctx, project, incident.Filter{}); err != nil {
				return opened, fmt.Errorf("listing incidents: %w", err)
			}
			loaded = true
		}
		n, err := r.rightsizeResource(ctx, project, sp, s, existing, now)
		opened += n
		if err != nil && first == nil {
			first = err
		}
	}
	if first == nil {
		if r.lastRightsize == nil {
			r.lastRightsize = map[string]time.Time{}
		}
		r.lastRightsize[project] = now
	}
	return opened, first
}

func (r *Runner) rightsizeResource(ctx context.Context, project string, sp sizeSpec, s rules.Settings, existing []incident.Incident, now time.Time) (int, error) {
	step := RightsizeStep(s.Days)
	n := s.Days * 86400 / step
	end := now.Unix() / int64(step) * int64(step)
	start := end - int64(n*step)
	g := Grid{Start: start, Step: step, N: n}
	query := func(name string, rate, peak bool) (store.MetricResult, error) {
		return r.Source.QueryMetric(ctx, store.MetricQuery{
			Project: project, Name: name, From: time.Unix(start, 0), To: time.Unix(end, 0),
			StepSec: step, Rate: rate, Max: peak, MaxSeries: r.MaxSeries, MaxPoints: rightsizeMaxPoints,
		})
	}
	usage, err := query(sp.usage, sp.rate, !sp.rate)
	if err != nil {
		return 0, fmt.Errorf("reading %s use: %w", sp.res.Label(), err)
	}
	limit, err := query(sp.limit, false, false)
	if err != nil {
		return 0, fmt.Errorf("reading %s limits: %w", sp.res.Label(), err)
	}
	if usage.Truncated || limit.Truncated {
		r.Log.Warn("right-sizing read hit the series or point limit, so it is not judged: a container with only some of its series read would look quiet", "project", project, "resource", sp.res)
		return 0, nil
	}

	by := map[string][]Use{}
	var order []string
	for _, u := range MeasureUse(sp.res, usage.Series, limit.Series, g, s, r.RightsizeExclude) {
		if !Judge(u, s) {
			continue
		}
		if _, seen := by[u.App]; !seen {
			order = append(order, u.App)
		}
		by[u.App] = append(by[u.App], u)
	}
	opened := 0
	for _, app := range order {
		if rightsizeCovered(existing, sp.res, app, now, s.Days) {
			continue
		}
		uses := by[app]
		in := DescribeRightsize(app, uses[0].Label, sp.res, uses, s, step, now)
		inc, created, err := r.Incidents.Create(ctx, project, Actor, in)
		if err != nil {
			r.Log.Error("opening an incident failed", "project", project, "app", app, "error", err)
			continue
		}
		if !created {
			continue
		}
		opened++
		from, to := time.Unix(start, 0).UTC(), time.Unix(end, 0).UTC()
		for _, ref := range []incident.EvidenceRef{
			{Kind: incident.KindApplication, Service: app},
			{Kind: incident.KindMetric, Metric: sp.usage, From: &from, To: &to},
		} {
			if _, _, _, err := r.Incidents.AddEvidence(ctx, project, inc.ID, Actor, nil, ref); err != nil {
				r.Log.Warn("attaching evidence failed", "project", project, "incident", inc.ID, "kind", ref.Kind, "error", err)
			}
		}
	}
	return opened, nil
}

// rightsizeCovered says whether the application already has an incident for the resource that makes a new one unwanted: one that is still active, or
// that was resolved or closed less than the rule's window ago (someone looked at it and decided, and the same measurement should not bring it back the
// next day). Incidents a person opened are not considered, nor are other kinds the detector opens.
func rightsizeCovered(existing []incident.Incident, res Resource, app string, now time.Time, days int) bool {
	prefix := "rightsize:" + string(res) + ":"
	for _, e := range existing {
		if e.Source != incident.Detected || e.App != app || !strings.HasPrefix(e.DedupeKey, prefix) {
			continue
		}
		if active(e.State) || now.Sub(endedAt(e)) < time.Duration(days)*24*time.Hour {
			return true
		}
	}
	return false
}
