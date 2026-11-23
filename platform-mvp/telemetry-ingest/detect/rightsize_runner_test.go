package detect

import (
	"context"
	"errors"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/incident"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/rules"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/store"
)

// fakeRules stands for the rules the hub holds: what is in force for the project, or a failure to read it.
type fakeRules struct {
	eff   map[string]rules.Settings
	err   error
	calls int
}

func (f *fakeRules) Effective(_ context.Context, _ string) (map[string]rules.Settings, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := rules.Defaults()
	for k, v := range f.eff {
		out[k] = v
	}
	return out, nil
}

type rsRig struct {
	*rig
	rules *fakeRules
}

// newRSRig: shop/web gives 1 GiB of memory, 1 core and a 100 GiB volume to each of its containers. The test says what they use.
func newRSRig(t *testing.T) *rsRig {
	t.Helper()
	g := &rsRig{rig: newRig(t), rules: &fakeRules{}}
	// time runs in the past, so that the incident manager (which refuses a start in the future by its own clock) is never asked for one
	g.now = g.now.Add(-6 * time.Hour)
	g.r.Rightsize = true
	g.r.RightsizeEvery = time.Hour
	g.r.Rules = g.rules
	return g
}

func (g *rsRig) metrics(names ...string) { g.src.metrics["gke"] = names }

func (g *rsRig) memory(use float64) {
	g.metrics(MemoryUsageMetric, MemoryLimitMetric)
	g.src.series["gke"][MemoryUsageMetric] = []gen{{lab("container_id", webPod), func(int64) float64 { return use * gib }}}
	g.src.series["gke"][MemoryLimitMetric] = []gen{{lab("container_id", webPod), func(int64) float64 { return gib }}}
}

func (g *rsRig) cpu(use float64) {
	g.metrics(CPUUsageMetric, CPULimitMetric)
	g.src.series["gke"][CPUUsageMetric] = []gen{{lab("container_id", webPod), func(int64) float64 { return use }}}
	g.src.series["gke"][CPULimitMetric] = []gen{{lab("container_id", webPod), func(int64) float64 { return 1 }}}
}

func (g *rsRig) storage(use float64) {
	g.metrics(DiskUsedMetric, DiskSizeMetric)
	l := lab("container_id", mongoPod, "volume", "data", "mount_point", "/data", "device", "sda")
	g.src.series["gke"][DiskUsedMetric] = []gen{{l, func(int64) float64 { return use * 100 * gib }}}
	g.src.series["gke"][DiskSizeMetric] = []gen{{l, func(int64) float64 { return 100 * gib }}}
}

func (g *rsRig) look(t *testing.T) int {
	t.Helper()
	n, err := g.r.Project(bg, "gke")
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAnOverProvisionedContainerOpensOneInformationIncident(t *testing.T) {
	g := newRSRig(t)
	g.memory(0.2)
	if n := g.look(t); n != 1 {
		t.Fatalf("opened %d", n)
	}
	got := g.list(t)
	if len(got) != 1 {
		t.Fatalf("incidents = %d", len(got))
	}
	in := got[0]
	if in.Title != "shop/web: memory over-provisioned" || in.App != webApp || in.Source != incident.Detected || in.State != incident.Open || in.Severity != incident.Info {
		t.Fatalf("incident = %+v", in)
	}
	for _, want := range []string{"Opened automatically", "last 7 days", "container app: limit 1 GiB", "20%", "about 272 MiB"} {
		if !strings.Contains(in.Summary, want) {
			t.Errorf("summary lacks %q:\n%s", want, in.Summary)
		}
	}
	if len(in.Evidence) != 2 || in.Evidence[0].Ref.Kind != incident.KindApplication || in.Evidence[0].Ref.Service != webApp || in.Evidence[1].Ref.Kind != incident.KindMetric || in.Evidence[1].Ref.Metric != MemoryUsageMetric {
		t.Fatalf("evidence = %+v", in.Evidence)
	}
	if e := in.Evidence[1].Ref; e.From == nil || e.To == nil || e.To.Sub(*e.From) != 7*24*time.Hour {
		t.Fatalf("the metric evidence covers the window the rule judged: %+v", e)
	}
}

func TestItLooksAtTheRightPlaceOnlyWhenItIsTimeAndDoesNotOpenTheSameOneAgain(t *testing.T) {
	g := newRSRig(t)
	g.memory(0.2)
	g.look(t)
	asked := g.src.queries
	if asked == 0 {
		t.Fatal("it asked nothing")
	}
	if n := g.look(t); n != 0 || g.src.queries != asked {
		t.Fatalf("a look a moment later opened %d after %d more queries: the check is slow, and runs about as often as the rule's cadence", n, g.src.queries-asked)
	}
	g.now = g.now.Add(61 * time.Minute)
	if n := g.look(t); n != 0 || len(g.list(t)) != 1 {
		t.Fatalf("a look an hour later opened %d: the incident is still open", n)
	}
	if g.src.queries == asked {
		t.Fatal("an hour later it did not look")
	}
}

func TestABusyContainerOpensNothing(t *testing.T) {
	g := newRSRig(t)
	g.memory(0.8)
	if n := g.look(t); n != 0 {
		t.Fatalf("opened %d", n)
	}
}

func TestTheQueriesAskForPeaksOfAGaugeAndRatesOfACounterOverTheRulesWindow(t *testing.T) {
	g := newRSRig(t)
	g.memory(0.2)
	g.look(t)
	var usage, limit *store.MetricQuery
	for i := range g.src.asked {
		switch g.src.asked[i].Name {
		case MemoryUsageMetric:
			usage = &g.src.asked[i]
		case MemoryLimitMetric:
			limit = &g.src.asked[i]
		}
	}
	if usage == nil || limit == nil {
		t.Fatalf("asked %+v", g.src.asked)
	}
	if !usage.Max || usage.Rate || usage.StepSec != 2100 || usage.To.Sub(usage.From) != 7*24*time.Hour {
		t.Fatalf("usage query = %+v: the peak of each step, over seven days", *usage)
	}
	if limit.Max || limit.Rate {
		t.Fatalf("limit query = %+v", *limit)
	}
	c := newRSRig(t)
	c.cpu(0.1)
	c.look(t)
	for _, q := range c.src.asked {
		if q.Name == CPUUsageMetric && (!q.Rate || q.Max) {
			t.Fatalf("a counter is read as a rate: %+v", q)
		}
	}
	// the window is the rule's
	w := newRSRig(t)
	w.rules.eff = map[string]rules.Settings{RightsizeMemoryName: {Enabled: true, Percent: 50, Days: 3, Percentile: 95}}
	w.memory(0.2)
	w.look(t)
	if q := w.src.asked[0]; q.To.Sub(q.From) != 72*time.Hour || q.StepSec != 900 {
		t.Fatalf("a three-day rule asked %+v", q)
	}
}

const RightsizeMemoryName = rules.RightsizeMemory

func TestTheProjectsRuleDecidesWhatIsOverProvisioned(t *testing.T) {
	g := newRSRig(t)
	g.memory(0.2)
	g.rules.eff = map[string]rules.Settings{rules.RightsizeMemory: {Enabled: true, Percent: 15, Days: 7, Percentile: 95}}
	if n := g.look(t); n != 0 {
		t.Fatalf("20%% is not under a rule of 15%%: opened %d", n)
	}
	g.rules.eff[rules.RightsizeMemory] = rules.Settings{Enabled: true, Percent: 25, Days: 7, Percentile: 95}
	g.now = g.now.Add(2 * time.Hour)
	if n := g.look(t); n != 1 {
		t.Fatalf("20%% is under a rule of 25%%: opened %d", n)
	}
}

func TestARuleThatIsOffIsNotEvenLookedAt(t *testing.T) {
	g := newRSRig(t)
	g.memory(0.2)
	g.rules.eff = map[string]rules.Settings{rules.RightsizeMemory: {Enabled: false, Percent: 50, Days: 7, Percentile: 95}}
	if n := g.look(t); n != 0 || g.src.queries != 0 {
		t.Fatalf("opened %d after %d queries", n, g.src.queries)
	}
	// storage is off by default
	s := newRSRig(t)
	s.storage(0.1)
	if n := s.look(t); n != 0 || s.src.queries != 0 {
		t.Fatalf("storage opened %d after %d queries without being switched on", n, s.src.queries)
	}
}

func TestSwitchedOnStorageIsJudgedPerVolume(t *testing.T) {
	g := newRSRig(t)
	g.storage(0.1)
	g.rules.eff = map[string]rules.Settings{rules.RightsizeStorage: {Enabled: true, Percent: 50, Days: 7, Percentile: 100}}
	if n := g.look(t); n != 1 {
		t.Fatalf("opened %d", n)
	}
	in := g.list(t)[0]
	if in.Title != "db/mongo: storage over-provisioned" || in.Severity != incident.Info || !strings.Contains(in.Summary, "volume data: size 100 GiB") {
		t.Fatalf("incident = %+v", in)
	}
}

func TestCPUAndMemoryAreSeparateIncidents(t *testing.T) {
	g := newRSRig(t)
	g.metrics(MemoryUsageMetric, MemoryLimitMetric, CPUUsageMetric, CPULimitMetric)
	g.memory(0.2)
	g.metrics(MemoryUsageMetric, MemoryLimitMetric, CPUUsageMetric, CPULimitMetric)
	g.src.series["gke"][CPUUsageMetric] = []gen{{lab("container_id", webPod), func(int64) float64 { return 0.1 }}}
	g.src.series["gke"][CPULimitMetric] = []gen{{lab("container_id", webPod), func(int64) float64 { return 1 }}}
	if n := g.look(t); n != 2 {
		t.Fatalf("opened %d", n)
	}
	titles := map[string]bool{}
	for _, in := range g.list(t) {
		titles[in.Title] = true
	}
	if !titles["shop/web: memory over-provisioned"] || !titles["shop/web: CPU over-provisioned"] {
		t.Fatalf("titles = %v", titles)
	}
}

func TestAContainerWatchedForPartOfTheWindowOnlyIsNotJudged(t *testing.T) {
	g := newRSRig(t)
	g.memory(0.2)
	young := g.now.Add(-24 * time.Hour).Unix() // began a day ago, of seven
	g.src.series["gke"][MemoryUsageMetric] = []gen{{lab("container_id", webPod), func(t int64) float64 {
		if t < young {
			return math.NaN()
		}
		return 0.2 * gib
	}}}
	if n := g.look(t); n != 0 {
		t.Fatalf("opened %d for a container that was only there for a seventh of the window", n)
	}
}

func TestNamespacesLeftOutAreNotJudgedForSize(t *testing.T) {
	g := newRSRig(t)
	g.memory(0.2)
	g.r.RightsizeExclude = regexp.MustCompile(`^shop$`)
	if n := g.look(t); n != 0 {
		t.Fatalf("opened %d", n)
	}
}

func TestResolvingItDoesNotOpenAnotherWhileItIsStillOverProvisioned(t *testing.T) {
	g := newRSRig(t)
	g.memory(0.2)
	g.look(t)
	id := g.list(t)[0].ID
	if _, err := g.m.Transition(bg, "gke", id, "ana", nil, incident.TransitionInput{To: incident.Resolved, Resolution: incident.Workaround, Note: "left as it is"}); err != nil {
		t.Fatal(err)
	}
	g.now = g.now.Add(2 * time.Hour)
	if n := g.look(t); n != 0 {
		t.Fatalf("opened %d: someone looked at it and resolved it, and it should not be back in a day", n)
	}
}

func TestAFailureToReadTheRulesIsAnErrorAndNeverTheDefaults(t *testing.T) {
	g := newRSRig(t)
	g.memory(0.2)
	g.rules.err = errors.New("clickhouse is busy")
	n, err := g.r.Project(bg, "gke")
	if n != 0 || err == nil || !strings.Contains(err.Error(), "clickhouse is busy") {
		t.Fatalf("opened %d, err %v", n, err)
	}
	if g.src.queries != 0 {
		t.Fatal("it judged by defaults when the rules could not be read")
	}
	// and it is tried again at the next look, not an hour later
	g.rules.err = nil
	if n := g.look(t); n != 1 {
		t.Fatalf("the next look opened %d", n)
	}
}

func TestAFailureToReadOneResourceDoesNotStopTheOthers(t *testing.T) {
	g := newRSRig(t)
	g.memory(0.2)
	g.metrics(MemoryUsageMetric, MemoryLimitMetric, CPUUsageMetric, CPULimitMetric)
	g.src.series["gke"][CPUUsageMetric] = []gen{{lab("container_id", webPod), func(int64) float64 { return 0.1 }}}
	g.src.series["gke"][CPULimitMetric] = []gen{{lab("container_id", webPod), func(int64) float64 { return 1 }}}
	g.src.queryErr = map[string]error{CPUUsageMetric: errors.New("boom")}
	n, err := g.r.Project(bg, "gke")
	if n != 1 || err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("opened %d, err %v: memory should still be judged, and the failure said", n, err)
	}
	// a look that failed is tried again at the next tick, not an hour later
	asked := g.src.queries
	g.src.queryErr = nil
	if n, err := g.r.Project(bg, "gke"); err != nil || n != 1 || g.src.queries == asked {
		t.Fatalf("the next look: opened %d, err %v, queries %d -> %d: it should have looked again, and found CPU", n, err, asked, g.src.queries)
	}
	if len(g.list(t)) != 2 {
		t.Fatalf("the second look should have found CPU too: %d incidents", len(g.list(t)))
	}
}

func TestResultsThatWereCutShortAreNotJudged(t *testing.T) {
	g := newRSRig(t)
	g.memory(0.2)
	g.src.truncated = true
	if n := g.look(t); n != 0 {
		t.Fatalf("opened %d from a partial read: a container that only some of its series were read for would look quiet", n)
	}
}

func TestItIsOffUnlessSwitchedOnAndWithoutTheMetricsItNeedsItAsksNothing(t *testing.T) {
	g := newRSRig(t)
	g.memory(0.2)
	g.r.Rightsize = false
	if n := g.look(t); n != 0 || g.src.queries != 0 {
		t.Fatalf("opened %d after %d queries while off", n, g.src.queries)
	}
	h := newRSRig(t)
	h.memory(0.2)
	h.metrics(MemoryUsageMetric) // no limit metric
	if n := h.look(t); n != 0 || h.src.queries != 0 {
		t.Fatalf("opened %d after %d queries with no limits", n, h.src.queries)
	}
}

func TestWithoutARulesSourceTheBuiltInDefaultsApply(t *testing.T) {
	g := newRSRig(t)
	g.r.Rules = nil
	g.memory(0.2)
	if n := g.look(t); n != 1 {
		t.Fatalf("opened %d", n)
	}
}

func TestTheSettingsAreCheckedWhenItIsOn(t *testing.T) {
	g := newRSRig(t)
	if err := g.r.Validate(); err != nil {
		t.Fatal(err)
	}
	g.r.RightsizeEvery = 5 * time.Minute
	if err := g.r.Validate(); err == nil {
		t.Fatal("a look every five minutes was accepted: it reads days of metrics")
	}
	g.r.Rightsize = false // not used, so not checked
	if err := g.r.Validate(); err != nil {
		t.Fatalf("settings that are not used were checked: %v", err)
	}
}

// ---- not opening it twice ----

func TestAnIncidentIsCoveredWhileItIsActiveAndForTheWindowAfterItWasResolved(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	key := RightsizeKey(Memory, webApp, now.Add(-48*time.Hour))
	mk := func(state incident.State, updated time.Time, mod func(*incident.Incident)) incident.Incident {
		in := incident.Incident{App: webApp, Source: incident.Detected, DedupeKey: key, State: state, UpdatedAt: updated}
		if mod != nil {
			mod(&in)
		}
		return in
	}
	resolved := func(ago time.Duration) incident.Incident {
		at := now.Add(-ago)
		return mk(incident.Resolved, at, func(i *incident.Incident) { i.ResolvedAt = &at })
	}
	cases := map[string]struct {
		e    incident.Incident
		want bool
	}{
		"open":                     {mk(incident.Open, now.Add(-72*time.Hour), nil), true},
		"acknowledged":             {mk(incident.Acknowledged, now.Add(-720*time.Hour), nil), true},
		"resolved two days ago":    {resolved(48 * time.Hour), true},
		"resolved six days ago":    {resolved(6 * 24 * time.Hour), true},
		"resolved eight days ago":  {resolved(8 * 24 * time.Hour), false},
		"another resource":         {mk(incident.Open, now, func(i *incident.Incident) { i.DedupeKey = RightsizeKey(CPU, webApp, now) }), false},
		"another application":      {mk(incident.Open, now, func(i *incident.Incident) { i.App = apiApp }), false},
		"one a person opened":      {mk(incident.Open, now, func(i *incident.Incident) { i.Source = incident.Manual }), false},
		"another kind of incident": {mk(incident.Open, now, func(i *incident.Incident) { i.DedupeKey = DedupeKey(webApp, now) }), false},
	}
	for name, c := range cases {
		if got := rightsizeCovered([]incident.Incident{c.e}, Memory, webApp, now, 7); got != c.want {
			t.Errorf("%s: covered = %v, want %v", name, got, c.want)
		}
	}
	if rightsizeCovered(nil, Memory, webApp, now, 7) {
		t.Error("nothing covers nothing")
	}
}

func TestAnIncidentThatWasResolvedIsRememberedForTheRulesDaysAndNotLonger(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	at := now.Add(-10 * 24 * time.Hour)
	e := incident.Incident{App: webApp, Source: incident.Detected, DedupeKey: RightsizeKey(Memory, webApp, at), State: incident.Closed, UpdatedAt: at, ClosedAt: &at}
	if !rightsizeCovered([]incident.Incident{e}, Memory, webApp, now, 14) {
		t.Error("closed ten days ago, under a rule of fourteen days")
	}
	if rightsizeCovered([]incident.Incident{e}, Memory, webApp, now, 7) {
		t.Error("closed ten days ago, under a rule of seven days")
	}
}

func TestAnOpenRightsizeIncidentDoesNotHoldBackAnIncidentOfAnotherKind(t *testing.T) {
	g := newRSRig(t)
	if _, _, err := g.m.Create(bg, "gke", Actor, incident.CreateInput{
		Title: "shop/web: memory over-provisioned", App: webApp, Severity: incident.Info, Source: incident.Detected,
		DedupeKey: RightsizeKey(Memory, webApp, g.now), StartedAt: g.now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	g.metrics(RequestsMetric)
	g.failing(g.now.Add(-30*time.Minute), time.Time{}) // shop/web now burns its error budget
	if n := g.look(t); n != 1 {
		t.Fatalf("opened %d: an application that is over-provisioned still gets an incident when it starts failing", n)
	}
	titles := map[string]bool{}
	for _, in := range g.list(t) {
		titles[in.Title] = true
	}
	if !titles["shop/web: availability objective burning"] || !titles["shop/web: memory over-provisioned"] {
		t.Fatalf("titles = %v", titles)
	}
}
