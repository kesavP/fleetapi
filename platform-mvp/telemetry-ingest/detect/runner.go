package detect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/incident"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/inventory"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/store"
)

// Actor is the name the detector's incidents and evidence are recorded under.
const Actor = "detector"

// Source is what the Runner reads: the same two calls the read API is made of.
type Source interface {
	ListMetrics(ctx context.Context, project string) ([]string, error)
	QueryMetric(ctx context.Context, q store.MetricQuery) (store.MetricResult, error)
	// What the cluster collector says is in the cluster, which silence detection needs: without it, "stopped sending" cannot be told from "scaled down".
	InventorySummary(ctx context.Context, project string) ([]store.InventoryCluster, error)
	ListInventory(ctx context.Context, q store.InventoryQuery) ([]store.InventoryObject, error)
}

// Incidents is what the Runner opens incidents with: the incident manager.
type Incidents interface {
	Create(ctx context.Context, project, actor string, in incident.CreateInput) (incident.Incident, bool, error)
	List(ctx context.Context, project string, f incident.Filter) ([]incident.Incident, int, error)
	AddEvidence(ctx context.Context, project, id, actor string, expected *uint32, ref incident.EvidenceRef) (incident.Incident, incident.Evidence, bool, error)
}

// Runner looks at every project on a timer and opens an incident for each application that is burning its error budget and has none open.
// It never changes or closes an incident: whoever is on call does, and an incident that is still burning when it is resolved is not
// opened again for the same stretch of trouble (see Fresh).
type Runner struct {
	Projects  func() []string
	Source    Source
	Incidents Incidents
	Config    Config
	Log       *slog.Logger

	Interval time.Duration // how often to look
	Lookback time.Duration // how far back each look reaches; at least the long window plus the short one
	// MaxAge: a breach that has already ended is only opened when it ended this recently. Without it, switching the detector on would open
	// an incident for everything that went wrong in the lookback.
	MaxAge    time.Duration
	Step      int // seconds per step of the timeline
	MaxSeries int
	MaxPoints int
	Now       func() time.Time

	// SilenceAfter: an application that has stopped sending (see silence.go) is opened as an incident once it has been silent this long by this
	// runner's own count. Zero switches silence detection off. SilenceExclude, when set, names the namespaces to leave out.
	SilenceAfter   time.Duration
	SilenceExclude *regexp.Regexp

	silentSince map[string]time.Time // project|application -> when this runner first saw it silent
}

// Validate says what is wrong with the settings, or nil.
func (r *Runner) Validate() error {
	if err := r.Config.Validate(); err != nil {
		return err
	}
	switch {
	case r.Interval <= 0:
		return fmt.Errorf("the detection interval must be positive")
	case r.Step <= 0:
		return fmt.Errorf("the step must be positive")
	case r.Lookback < time.Duration(r.Config.LongSeconds+r.Config.ShortSeconds)*time.Second:
		return fmt.Errorf("the lookback must cover the long and the short window (%ds), or the long window never fills", r.Config.LongSeconds+r.Config.ShortSeconds)
	case r.SilenceAfter < 0 || (r.SilenceAfter > 0 && r.SilenceAfter < time.Minute):
		return fmt.Errorf("the silence delay must be zero (off) or at least a minute")
	case r.MaxSeries <= 0 || r.MaxPoints <= 0:
		return fmt.Errorf("the series and point limits must be positive")
	}
	return nil
}

// Run looks every Interval until ctx ends, and once at the start.
func (r *Runner) Run(ctx context.Context) {
	r.Log.Info("incident detection on", "interval", r.Interval, "lookback", r.Lookback, "burn", r.Config.BurnThreshold)
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		r.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick looks at every project once. A project that fails is logged and does not stop the others.
func (r *Runner) Tick(ctx context.Context) {
	for _, p := range r.Projects() {
		if ctx.Err() != nil {
			return
		}
		pctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		n, err := r.Project(pctx, p)
		cancel()
		if err != nil {
			r.Log.Error("incident detection failed", "project", p, "error", err)
		} else if n > 0 {
			r.Log.Info("incidents opened", "project", p, "count", n)
		}
	}
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Project looks at one project and returns how many incidents it opened: for applications burning their error budget, and for those that
// have stopped sending. One failing does not stop the other.
func (r *Runner) Project(ctx context.Context, project string) (int, error) {
	names, err := r.Source.ListMetrics(ctx, project)
	if err != nil {
		return 0, fmt.Errorf("listing metrics: %w", err)
	}
	has := map[string]bool{}
	for _, n := range names {
		has[n] = true
	}
	opened, berr := r.burning(ctx, project, has)
	n, serr := r.silent(ctx, project, has)
	opened += n
	if berr != nil {
		return opened, berr
	}
	return opened, serr
}

func (r *Runner) burning(ctx context.Context, project string, has map[string]bool) (int, error) {
	if !has[RequestsMetric] { // no inbound HTTP counted: nothing to judge (the UI says so as "no agent")
		return 0, nil
	}
	step := int64(r.Step)
	end := r.now().Unix() / step * step
	start := end - int64(r.Lookback/time.Second)
	g := Grid{Start: start, Step: r.Step, N: int((end - start) / step)}
	query := func(name string) (store.MetricResult, error) {
		return r.Source.QueryMetric(ctx, store.MetricQuery{
			Project: project, Name: name, From: time.Unix(start, 0), To: time.Unix(end, 0),
			StepSec: r.Step, Rate: true, MaxSeries: r.MaxSeries, MaxPoints: r.MaxPoints,
		})
	}
	reqs, err := query(RequestsMetric)
	if err != nil {
		return 0, fmt.Errorf("reading requests: %w", err)
	}
	var buckets store.MetricResult
	if has[LatencyMetric] {
		if buckets, err = query(LatencyMetric); err != nil {
			return 0, fmt.Errorf("reading latency: %w", err)
		}
	}
	if reqs.Truncated || buckets.Truncated {
		r.Log.Warn("request metrics hit the series or point limit, so some applications are not judged", "project", project)
	}

	tl := BuildTimeline(reqs.Series, buckets.Series, g, r.Config.LatencySeconds)
	var found []Found
	for _, a := range tl.Apps {
		for _, b := range FindBreaches(a, g, r.Config) {
			found = append(found, Found{App: a.ID, Label: a.Label, Breach: b})
		}
	}
	if len(found) == 0 {
		return 0, nil
	}
	existing, _, err := r.Incidents.List(ctx, project, incident.Filter{})
	if err != nil {
		return 0, fmt.Errorf("listing incidents: %w", err)
	}
	opened := 0
	for _, f := range Fresh(found, existing, r.now(), r.Config, r.MaxAge) {
		in := Describe(f, r.Config, tl.Bound)
		inc, created, err := r.Incidents.Create(ctx, project, Actor, in)
		if err != nil {
			r.Log.Error("opening an incident failed", "project", project, "app", f.App, "error", err)
			continue
		}
		if !created {
			continue
		}
		opened++
		r.attach(ctx, project, inc.ID, f)
	}
	return opened, nil
}

// forget drops what is remembered of when applications of a project were first seen silent, but for those in keep.
func (r *Runner) forget(project string, keep map[string]bool) {
	for k := range r.silentSince {
		if strings.HasPrefix(k, project+"|") && !keep[k] {
			delete(r.silentSince, k)
		}
	}
}

// silent opens incidents for the applications that have stopped sending: not for the first look that sees one (a rollout, a restart, a blip),
// but when it has been so for SilenceAfter, counted from this runner's first sight of it (so a restart of the service starts the count again).
func (r *Runner) silent(ctx context.Context, project string, has map[string]bool) (int, error) {
	if r.SilenceAfter <= 0 || !has[RSSMetric] {
		return 0, nil
	}
	if r.silentSince == nil {
		r.silentSince = map[string]time.Time{}
	}
	now := r.now().UTC()

	// Is there an inventory to judge by, and is it current?
	clusters, err := r.Source.InventorySummary(ctx, project)
	if err != nil {
		return 0, fmt.Errorf("reading the inventory summary: %w", err)
	}
	fresh := map[string]bool{}
	for _, c := range clusters {
		if now.Sub(c.LastReceived) <= staleInventory {
			fresh[c.Cluster] = true
		}
	}
	if len(fresh) == 0 {
		r.forget(project, nil)
		return 0, nil
	}
	rows, err := r.Source.ListInventory(ctx, store.InventoryQuery{Project: project, Kinds: inventory.Workloads, Limit: 1000})
	if err != nil {
		return 0, fmt.Errorf("reading the inventory: %w", err)
	}
	var current []store.InventoryObject
	for _, o := range rows {
		if fresh[o.Cluster] {
			current = append(current, o)
		}
	}

	step := int64(r.Step)
	end := time.Unix(now.Unix()/step*step, 0).UTC()
	res, err := r.Source.QueryMetric(ctx, store.MetricQuery{
		Project: project, Name: RSSMetric, From: end.Add(-silenceWindow), To: end, StepSec: r.Step, MaxSeries: r.MaxSeries, MaxPoints: r.MaxPoints,
	})
	if err != nil {
		return 0, fmt.Errorf("reading the memory gauge: %w", err)
	}
	if res.Truncated {
		r.Log.Warn("the memory gauge hit the series or point limit, so some applications are not judged for silence", "project", project)
	}
	tel := TelemetryOf(res.Series, end)
	if !AgentAlive(tel) { // nothing is sending: the agent, not the applications
		r.forget(project, nil)
		return 0, nil
	}

	silent := JudgeSilence(WantedFrom(current), tel, r.SilenceExclude)
	keep := map[string]bool{}
	var due []Silent
	since := map[string]time.Time{}
	for _, s := range silent {
		k := project + "|" + s.App
		keep[k] = true
		first, ok := r.silentSince[k]
		if !ok {
			first = now
			r.silentSince[k] = first
		}
		if now.Sub(first) < r.SilenceAfter {
			continue
		}
		due = append(due, s)
		// it began when it last sent, when that is known and is not later than this runner's first sight of it
		since[s.App] = first
		if !s.LastData.IsZero() && s.LastData.Before(first) {
			since[s.App] = s.LastData
		}
	}
	r.forget(project, keep)
	if len(due) == 0 {
		return 0, nil
	}

	// why the pods that are not ready are not: read only when something is
	for _, s := range due {
		if s.Cause != CauseNotReady {
			continue
		}
		pods, err := r.Source.ListInventory(ctx, store.InventoryQuery{Project: project, Kinds: []string{inventory.Pod}, Limit: 1000})
		if err != nil {
			r.Log.Warn("reading the pods of the inventory failed, so the incidents will not say why they are not ready", "project", project, "error", err)
			break
		}
		var mine []store.InventoryObject
		for _, o := range pods {
			if fresh[o.Cluster] {
				mine = append(mine, o)
			}
		}
		reasons := PodReasons(mine)
		for i := range due {
			due[i].Reasons = reasons[due[i].App]
		}
		break
	}

	var found []Found
	byApp := map[string]Silent{}
	for _, s := range due {
		byApp[s.App] = s
		found = append(found, Found{App: s.App, Label: s.Label, Family: familySilence, Breach: Breach{Start: since[s.App], End: now, Open: true}})
	}
	existing, _, err := r.Incidents.List(ctx, project, incident.Filter{})
	if err != nil {
		return 0, fmt.Errorf("listing incidents: %w", err)
	}
	opened := 0
	for _, f := range Fresh(found, existing, now, r.Config, r.MaxAge) {
		s := byApp[f.App]
		inc, created, err := r.Incidents.Create(ctx, project, Actor, DescribeSilence(s, f.Start, now))
		if err != nil {
			r.Log.Error("opening an incident failed", "project", project, "app", f.App, "error", err)
			continue
		}
		if !created {
			continue
		}
		opened++
		r.attach(ctx, project, inc.ID, f)
	}
	return opened, nil
}

// attach points a new incident at where to look. A failure is logged only: the incident is already open without it.
func (r *Runner) attach(ctx context.Context, project, id string, f Found) {
	from := f.Start.Add(-5 * time.Minute)
	to := f.End.Add(5 * time.Minute)
	if f.Open {
		to = r.now().UTC()
	}
	for _, ref := range []incident.EvidenceRef{
		{Kind: incident.KindApplication, Service: f.App},
		{Kind: incident.KindLogs, Service: f.App, Severity: "error", From: &from, To: &to},
	} {
		if _, _, _, err := r.Incidents.AddEvidence(ctx, project, id, Actor, nil, ref); err != nil {
			r.Log.Warn("attaching evidence failed", "project", project, "incident", id, "kind", ref.Kind, "error", err)
		}
	}
}

// The kinds of incident the detector opens. They are kept apart when deciding whether an application already has one: a silent application
// with an incident open must still get one when it starts burning its error budget, and the other way round.
const (
	familyBurn    = ""
	familySilence = "silence"
)

// familyOf is which kind of incident the detector opened an incident as, from its dedupe key.
func familyOf(i incident.Incident) string {
	if strings.HasPrefix(i.DedupeKey, "silence:") {
		return familySilence
	}
	return familyBurn
}

// Found is a breach, or a silence, of one application.
type Found struct {
	App, Label string
	Family     string // familyBurn or familySilence
	Breach
}

func active(s incident.State) bool { return s != incident.Resolved && s != incident.Closed }

// endedAt is when an incident that is no longer active stopped being one.
func endedAt(i incident.Incident) time.Time {
	t := i.UpdatedAt
	for _, p := range []*time.Time{i.ResolvedAt, i.ClosedAt} {
		if p != nil && p.After(t) {
			t = *p
		}
	}
	return t
}

// Fresh keeps the breaches that need a new incident. A breach is left out when
//   - it ended more than maxAge ago: it is history, not something to wake anyone for; or
//   - the detector already opened an incident for the same application that covers it: one that is still active (so a trouble that
//     outlasts the lookback, whose start then moves forward on every look, stays one incident), or one that ended at or after the breach
//     began (so resolving an incident while its trouble is still going does not open another at once).
//
// Incidents a person opened are not considered: they may be stale, and a duplicate is cheaper than a missed incident.
func Fresh(found []Found, existing []incident.Incident, now time.Time, c Config, maxAge time.Duration) []Found {
	join := time.Duration(c.JoinSeconds) * time.Second
	var out []Found
	for _, f := range found {
		if !f.Open && now.Sub(f.End) > maxAge {
			continue
		}
		covered := false
		for _, e := range existing {
			if e.Source != incident.Detected || e.App != f.App || familyOf(e) != f.Family {
				continue
			}
			if e.StartedAt.After(f.End.Add(join)) {
				continue // began after this breach ended: a later incident
			}
			if active(e.State) || !endedAt(e).Before(f.Start.Add(-join)) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, f)
		}
	}
	return out
}

// DedupeKey names a breach so that finding it twice opens it once: the application and when it began. The application id is hashed, as it
// may hold characters a key may not.
func DedupeKey(app string, start time.Time) string {
	h := sha256.Sum256([]byte(app))
	return "detect:" + hex.EncodeToString(h[:6]) + ":" + strconv.FormatInt(start.Unix(), 10)
}

// Describe is the incident a breach becomes: a title, a severity, and a summary that says what was measured in plain words.
func Describe(f Found, c Config, bound float64) incident.CreateInput {
	var what []string
	var lines []string
	var worst float64
	for _, k := range f.Kinds {
		peak := f.PeakBurn[k]
		worst = max(worst, peak)
		switch k {
		case Availability:
			what = append(what, "availability")
			lines = append(lines, share("Availability", f.Compliance[k], f.Requests, "of requests failed", c.Availability, peak))
		case LatencyKind:
			what = append(what, "latency")
			ms := c.LatencySeconds * 1000
			if bound > 0 {
				ms = bound * 1000
			}
			lines = append(lines, share("Latency", f.Compliance[k], f.Requests, fmt.Sprintf("of requests took longer than %s ms", trim(ms)), c.Latency, peak))
		}
	}
	sev := incident.Warning
	if worst >= 2*c.BurnThreshold {
		sev = incident.Critical
	}
	span := f.End.Sub(f.Start).Round(time.Minute)
	state := "ended " + f.End.Format("15:04") + " UTC"
	if f.Open {
		state = "still going"
	}
	head := fmt.Sprintf("Opened automatically. %s was burning its %s objective too fast from %s UTC (%s, %s).",
		f.Label, strings.Join(what, " and "), f.Start.Format("15:04"), span, state)
	return incident.CreateInput{
		Title:     fmt.Sprintf("%s: %s objective burning", f.Label, strings.Join(what, " and ")),
		App:       f.App,
		Severity:  sev,
		Source:    incident.Detected,
		DedupeKey: DedupeKey(f.App, f.Start),
		Summary:   head + "\n" + strings.Join(lines, "\n"),
		StartedAt: f.Start,
	}
}

func share(name string, compliance, requests float64, what string, objective, peak float64) string {
	if requests < 1 {
		return name + ": too few requests to give a share."
	}
	return fmt.Sprintf("%s: %s%% %s (%s requests; objective %s%%). The budget burned up to %sx faster than allowed.",
		name, trim(100*(1-compliance)), what, trim(requests), trim(100*objective), trim(peak))
}

// trim prints a number with at most two decimals and no trailing zeros.
func trim(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" || s == "-0" {
		return "0"
	}
	return s
}
