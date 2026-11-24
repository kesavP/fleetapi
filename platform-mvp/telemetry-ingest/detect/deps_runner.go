package detect

import (
	"context"
	"fmt"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/incident"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/inventory"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/store"
)

// refused opens an incident for each dependency that its callers could not connect to (see deps.go) and has none open for. It reads the
// connections of the whole look-back: the ones that failed, the ones that worked (they say what a Service's address leads to) and the listen
// addresses (they say which application a pod's address is).
func (r *Runner) refused(ctx context.Context, project string, has map[string]bool) (int, error) {
	if !r.Dependencies || !has[FailedConnectsMetric] || !has[ListenMetric] {
		return 0, nil
	}
	step := int64(r.Step)
	end := r.now().Unix() / step * step
	start := end - int64(r.Lookback/time.Second)
	g := Grid{Start: start, Step: r.Step, N: int((end - start) / step)}
	query := func(name string, rate bool) (store.MetricResult, error) {
		return r.Source.QueryMetric(ctx, store.MetricQuery{
			Project: project, Name: name, From: time.Unix(start, 0), To: time.Unix(end, 0),
			StepSec: r.Step, Rate: rate, MaxSeries: r.MaxSeries, MaxPoints: r.MaxPoints,
		})
	}
	failed, err := query(FailedConnectsMetric, true)
	if err != nil {
		return 0, fmt.Errorf("reading failed connections: %w", err)
	}
	if len(failed.Series) == 0 {
		return 0, nil // nothing failed in the look-back: no need to read the rest
	}
	var ok store.MetricResult
	if has[ConnectsMetric] {
		if ok, err = query(ConnectsMetric, true); err != nil {
			return 0, fmt.Errorf("reading connections: %w", err)
		}
	}
	listen, err := query(ListenMetric, false)
	if err != nil {
		return 0, fmt.Errorf("reading listen addresses: %w", err)
	}
	if failed.Truncated || ok.Truncated || listen.Truncated {
		r.Log.Warn("connection metrics hit the series or point limit, so some dependencies are not judged", "project", project)
	}

	deps := BuildDeps(failed.Series, ok.Series, listen.Series, g, r.DepsExclude)
	byKey := map[string]depBreach{}
	var found []Found
	for _, d := range deps {
		for _, b := range FindDepBreaches(d, g, r.Deps) {
			found = append(found, Found{App: d.ID, Label: d.Label, Family: familyDeps, Breach: b.Breach})
			byKey[d.ID+"|"+b.Start.String()] = depBreach{dep: d, breach: b}
		}
	}
	if len(found) == 0 {
		return 0, nil
	}
	existing, _, err := r.Incidents.List(ctx, project, incident.Filter{})
	if err != nil {
		return 0, fmt.Errorf("listing incidents: %w", err)
	}
	fresh := Fresh(found, existing, r.now(), r.Config, r.MaxAge)
	if len(fresh) == 0 {
		return 0, nil
	}
	wanted := r.wantedApps(ctx, project)
	opened := 0
	for _, f := range fresh {
		db := byKey[f.App+"|"+f.Start.String()]
		in := DescribeDep(db.dep, db.breach, r.Deps, g, wantedNote(wanted, f.App))
		inc, created, err := r.Incidents.Create(ctx, project, Actor, in)
		if err != nil {
			r.Log.Error("opening an incident failed", "project", project, "app", f.App, "error", err)
			continue
		}
		if !created {
			continue
		}
		opened++
		r.attachDep(ctx, project, inc.ID, db, f)
	}
	return opened, nil
}

type depBreach struct {
	dep    Dep
	breach DepBreach
}

// wantedApps is what the cluster collector says each workload is set to run, by application id; nil when there is no current inventory or it
// cannot be read (the incident is worth opening without the note).
func (r *Runner) wantedApps(ctx context.Context, project string) map[string]Wanted {
	clusters, err := r.Source.InventorySummary(ctx, project)
	if err != nil {
		r.Log.Warn("reading the inventory failed, so the incidents will not say whether a dependency was meant to be down", "project", project, "error", err)
		return nil
	}
	now := r.now().UTC()
	fresh := map[string]bool{}
	for _, c := range clusters {
		if now.Sub(c.LastReceived) <= staleInventory {
			fresh[c.Cluster] = true
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	rows, err := r.Source.ListInventory(ctx, store.InventoryQuery{Project: project, Kinds: inventory.Workloads, Limit: 1000})
	if err != nil {
		r.Log.Warn("reading the inventory failed, so the incidents will not say whether a dependency was meant to be down", "project", project, "error", err)
		return nil
	}
	var current []store.InventoryObject
	for _, o := range rows {
		if fresh[o.Cluster] {
			current = append(current, o)
		}
	}
	out := map[string]Wanted{}
	for _, w := range WantedFrom(current) {
		out[w.ID] = w
	}
	return out
}

// wantedNote is what the inventory says of the dependency, when it says anything: whether being down was meant.
func wantedNote(wanted map[string]Wanted, app string) string {
	w, ok := wanted[app]
	switch {
	case !ok:
		return ""
	case w.Desired <= 0:
		return "The cluster collector says it is set to run no replicas, so this may be a deliberate scale-down; its callers still depend on it."
	case w.Ready < w.Desired:
		return fmt.Sprintf("The cluster collector says it is set to run %d and %d are ready.", w.Desired, w.Ready)
	}
	return fmt.Sprintf("All %d of its pods are ready, and connections to it fail.", w.Ready)
}

// attachDep points a new incident at where to look: the dependency, its busiest callers, and the busiest caller's errors. A failure is logged
// only: the incident is already open without it.
func (r *Runner) attachDep(ctx context.Context, project, id string, db depBreach, f Found) {
	from := f.Start.Add(-5 * time.Minute)
	to := f.End.Add(5 * time.Minute)
	if f.Open {
		to = r.now().UTC()
	}
	refs := []incident.EvidenceRef{{Kind: incident.KindApplication, Service: db.dep.ID}}
	callers := db.dep.CallersIn(db.breach.First, db.breach.Last, r.Step)
	for i, c := range callers {
		if i == 3 {
			break
		}
		refs = append(refs, incident.EvidenceRef{Kind: incident.KindApplication, Service: c.ID})
	}
	if len(callers) > 0 {
		refs = append(refs, incident.EvidenceRef{Kind: incident.KindLogs, Service: callers[0].ID, Severity: "error", From: &from, To: &to})
	}
	for _, ref := range refs {
		if _, _, _, err := r.Incidents.AddEvidence(ctx, project, id, Actor, nil, ref); err != nil {
			r.Log.Warn("attaching evidence failed", "project", project, "incident", id, "kind", ref.Kind, "error", err)
		}
	}
}
