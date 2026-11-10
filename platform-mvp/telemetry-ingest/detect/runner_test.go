package detect

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/incident"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/store"
)

var bg = context.Background()

// gen is one series: its labels and its rate (per second) at the step that begins at a given time.
type gen struct {
	labels map[string]string
	rate   func(t int64) float64
}

// fakeSource serves generated series, per project, for exactly the window a query asks for.
type fakeSource struct {
	mu        sync.Mutex
	metrics   map[string][]string
	series    map[string]map[string][]gen // project -> metric -> series
	listErr   map[string]error
	truncated bool
	queries   int
}

func (f *fakeSource) ListMetrics(_ context.Context, project string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.listErr[project]; err != nil {
		return nil, err
	}
	return f.metrics[project], nil
}

func (f *fakeSource) QueryMetric(_ context.Context, q store.MetricQuery) (store.MetricResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries++
	res := store.MetricResult{Series: []store.MetricSeries{}, Truncated: f.truncated}
	for _, g := range f.series[q.Project][q.Name] {
		s := store.MetricSeries{Labels: g.labels}
		for t := q.From.Unix(); t+int64(q.StepSec) <= q.To.Unix(); t += int64(q.StepSec) {
			s.Points = append(s.Points, store.Point{T: t * 1000, V: g.rate(t)})
		}
		res.Series = append(res.Series, s)
	}
	return res, nil
}

type rig struct {
	r   *Runner
	src *fakeSource
	m   *incident.Manager
	now time.Time
}

// newRig: the project "gke" serves 10 requests a second from shop/web. The test says when it started failing.
func newRig(t *testing.T) *rig {
	t.Helper()
	rg := &rig{now: time.Now().UTC().Truncate(time.Minute)}
	rg.src = &fakeSource{
		metrics: map[string][]string{"gke": {RequestsMetric}},
		series:  map[string]map[string][]gen{"gke": {}},
		listErr: map[string]error{},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rg.m = incident.NewManager(incident.NewMemStore(), log)
	c := Defaults()
	rg.r = &Runner{
		Projects: func() []string { return []string{"gke"} }, Source: rg.src, Incidents: rg.m, Config: c, Log: log,
		Interval: time.Minute, Lookback: 3 * time.Hour, MaxAge: 30 * time.Minute, Step: 60, MaxSeries: 100, MaxPoints: 100000,
		Now: func() time.Time { return rg.now },
	}
	if err := rg.r.Validate(); err != nil {
		t.Fatal(err)
	}
	return rg
}

// failing makes shop/web fail 5 requests a second (against 10 that succeed) between from and until (zero until: for good).
func (rg *rig) failing(from, until time.Time) {
	web := "/k8s/shop/web-5d6f8c7b9-xk2lp/app"
	rg.src.series["gke"][RequestsMetric] = []gen{
		{lab("container_id", web, "status", "200"), func(int64) float64 { return 10 }},
		{lab("container_id", web, "status", "500"), func(t int64) float64 {
			if t >= from.Unix() && (until.IsZero() || t < until.Unix()) {
				return 5
			}
			return 0
		}},
	}
}

func (rg *rig) list(t *testing.T) []incident.Incident {
	t.Helper()
	got, _, err := rg.m.List(bg, "gke", incident.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestOpensOneIncidentForABurningApplication(t *testing.T) {
	rg := newRig(t)
	from := rg.now.Add(-30 * time.Minute)
	rg.failing(from, time.Time{})

	n, err := rg.r.Project(bg, "gke")
	if err != nil || n != 1 {
		t.Fatalf("opened %d, err %v", n, err)
	}
	got := rg.list(t)
	if len(got) != 1 {
		t.Fatalf("incidents = %d", len(got))
	}
	in := got[0]
	if in.Source != incident.Detected || in.App != "/k8s/shop/web" || in.State != incident.Open {
		t.Fatalf("incident = %+v", in)
	}
	if in.Severity != incident.Critical {
		t.Fatalf("severity = %v: a third of requests failing is far over twice the threshold", in.Severity)
	}
	if in.Title != "shop/web: availability objective burning" {
		t.Fatalf("title = %q", in.Title)
	}
	if !in.StartedAt.Equal(from) {
		t.Fatalf("started = %v, want %v", in.StartedAt, from)
	}
	for _, want := range []string{"Opened automatically", "still going", "Availability:", "33.33% of requests failed"} {
		if !strings.Contains(in.Summary, want) {
			t.Errorf("summary lacks %q:\n%s", want, in.Summary)
		}
	}
	if len(in.Evidence) != 2 || in.Evidence[0].Ref.Kind != incident.KindApplication || in.Evidence[1].Ref.Kind != incident.KindLogs {
		t.Fatalf("evidence = %+v", in.Evidence)
	}
	if in.Evidence[0].AddedBy != Actor || in.Evidence[1].Ref.Service != "/k8s/shop/web" || in.Evidence[1].Ref.Severity != "error" {
		t.Fatalf("evidence = %+v", in.Evidence)
	}
	if in.Evidence[1].Ref.To == nil || !in.Evidence[1].Ref.To.Equal(rg.now) {
		t.Fatalf("an open incident's logs run up to now: %+v", in.Evidence[1].Ref)
	}

	// looking again must not open it again
	n, err = rg.r.Project(bg, "gke")
	if err != nil || n != 0 || len(rg.list(t)) != 1 {
		t.Fatalf("second look: opened %d, err %v, incidents %d", n, err, len(rg.list(t)))
	}
}

func TestHealthyProjectOpensNothing(t *testing.T) {
	rg := newRig(t)
	rg.failing(rg.now.Add(time.Hour), time.Time{}) // starts failing only in the future
	if n, err := rg.r.Project(bg, "gke"); err != nil || n != 0 {
		t.Fatalf("opened %d, err %v", n, err)
	}
}

func TestATroubleLongerThanTheLookbackStaysOneIncident(t *testing.T) {
	rg := newRig(t)
	rg.failing(rg.now.Add(-30*time.Minute), time.Time{})
	if n, _ := rg.r.Project(bg, "gke"); n != 1 {
		t.Fatalf("first look opened %d", n)
	}
	// Five hours on it is still failing: the lookback no longer reaches where it began, so the start the detector sees has moved forward.
	rg.now = rg.now.Add(5 * time.Hour)
	if n, err := rg.r.Project(bg, "gke"); err != nil || n != 0 {
		t.Fatalf("opened %d more, err %v", n, err)
	}
	if got := rg.list(t); len(got) != 1 {
		t.Fatalf("incidents = %d", len(got))
	}
}

func TestResolvingWhileItIsStillBurningDoesNotOpenAnother(t *testing.T) {
	rg := newRig(t)
	rg.failing(rg.now.Add(-30*time.Minute), time.Time{})
	rg.r.Project(bg, "gke")
	id := rg.list(t)[0].ID
	if _, err := rg.m.Transition(bg, "gke", id, "ana", nil, incident.TransitionInput{To: incident.Resolved, Resolution: incident.Workaround, Note: "restarted"}); err != nil {
		t.Fatal(err)
	}
	rg.now = rg.now.Add(2 * time.Minute)
	if n, _ := rg.r.Project(bg, "gke"); n != 0 {
		t.Fatalf("opened %d: the trouble was resolved by a person a moment ago", n)
	}
}

// (Whether a trouble that follows a resolved incident is a new one is decided by Fresh, tested below: the manager judges a start time
// against its own clock, so a test here cannot move time forward.)
func TestHistoryIsNotOpenedButANewTroubleIs(t *testing.T) {
	rg := newRig(t)
	rg.failing(rg.now.Add(-150*time.Minute), rg.now.Add(-120*time.Minute)) // 30 minutes, ended two hours ago
	if n, _ := rg.r.Project(bg, "gke"); n != 0 {
		t.Fatalf("history opened %d", n)
	}
	rg.failing(rg.now.Add(-20*time.Minute), time.Time{}) // and now it is hit again
	if n, _ := rg.r.Project(bg, "gke"); n != 1 {
		t.Fatalf("a new trouble opened %d", n)
	}
}

func TestAnIncidentThatJustEndedIsStillOpened(t *testing.T) {
	rg := newRig(t)
	rg.failing(rg.now.Add(-40*time.Minute), rg.now.Add(-10*time.Minute)) // over ten minutes ago, within MaxAge
	n, err := rg.r.Project(bg, "gke")
	if err != nil || n != 1 {
		t.Fatalf("opened %d, err %v", n, err)
	}
	if s := rg.list(t)[0].Summary; !strings.Contains(s, "ended ") || strings.Contains(s, "still going") {
		t.Fatalf("summary = %q", s)
	}
}

func TestNoInboundMetricMeansNothingToJudge(t *testing.T) {
	rg := newRig(t)
	rg.src.metrics["gke"] = []string{"node_cpu_seconds_total"}
	if n, err := rg.r.Project(bg, "gke"); err != nil || n != 0 || rg.src.queries != 0 {
		t.Fatalf("opened %d, err %v, queries %d", n, err, rg.src.queries)
	}
}

func TestSourceErrorsAreReturned(t *testing.T) {
	rg := newRig(t)
	rg.src.listErr["gke"] = errors.New("clickhouse down")
	if _, err := rg.r.Project(bg, "gke"); err == nil || !strings.Contains(err.Error(), "clickhouse down") {
		t.Fatalf("err = %v", err)
	}
}

func TestOneFailingProjectDoesNotStopTheOthers(t *testing.T) {
	rg := newRig(t)
	rg.r.Projects = func() []string { return []string{"broken", "gke"} }
	rg.src.listErr["broken"] = errors.New("boom")
	rg.failing(rg.now.Add(-30*time.Minute), time.Time{})
	rg.r.Tick(bg)
	if got := rg.list(t); len(got) != 1 {
		t.Fatalf("incidents = %d", len(got))
	}
}

func TestTickStopsWhenCancelled(t *testing.T) {
	rg := newRig(t)
	rg.failing(rg.now.Add(-30*time.Minute), time.Time{})
	ctx, cancel := context.WithCancel(bg)
	cancel()
	rg.r.Tick(ctx)
	if got := rg.list(t); len(got) != 0 {
		t.Fatalf("a cancelled tick opened %d", len(got))
	}
}

func TestRunnerValidate(t *testing.T) {
	for name, mut := range map[string]func(*Runner){
		"no interval":      func(r *Runner) { r.Interval = 0 },
		"short lookback":   func(r *Runner) { r.Lookback = time.Hour },
		"no step":          func(r *Runner) { r.Step = 0 },
		"no series limit":  func(r *Runner) { r.MaxSeries = 0 },
		"bad objective":    func(r *Runner) { r.Config.Availability = 2 },
		"no point limit":   func(r *Runner) { r.MaxPoints = 0 },
		"negative lookbak": func(r *Runner) { r.Lookback = -time.Hour },
	} {
		rg := newRig(t)
		mut(rg.r)
		if rg.r.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// --- Fresh ---

func found(app string, start, end time.Time, open bool) Found {
	return Found{App: app, Label: app, Breach: Breach{Start: start, End: end, Open: open}}
}

func TestFresh(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	ago := func(m int) time.Time { return now.Add(-time.Duration(m) * time.Minute) }
	resolvedAt := func(m int) *time.Time { t := ago(m); return &t }
	existing := func(app string, state incident.State, started time.Time, src incident.Source) incident.Incident {
		return incident.Incident{App: app, State: state, StartedAt: started, UpdatedAt: started, Source: src}
	}
	cases := []struct {
		name string
		f    Found
		have []incident.Incident
		want bool
	}{
		{"nothing known: open it", found("a", ago(30), now, true), nil, true},
		{"ended long ago", found("a", ago(200), ago(120), false), nil, false},
		{"ended recently", found("a", ago(40), ago(10), false), nil, true},
		{"an active incident of the application covers it", found("a", ago(30), now, true), []incident.Incident{existing("a", incident.Investigating, ago(35), incident.Detected)}, false},
		{"an active one covers it though the start moved on", found("a", ago(100), now, true), []incident.Incident{existing("a", incident.Open, ago(400), incident.Detected)}, false},
		{"another application's incident does not", found("a", ago(30), now, true), []incident.Incident{existing("b", incident.Open, ago(35), incident.Detected)}, true},
		{"an incident a person opened does not", found("a", ago(30), now, true), []incident.Incident{existing("a", incident.Open, ago(35), incident.Manual)}, true},
		{"one resolved before the trouble began does not", found("a", ago(30), now, true), []incident.Incident{func() incident.Incident {
			i := existing("a", incident.Resolved, ago(300), incident.Detected)
			i.ResolvedAt = resolvedAt(200)
			return i
		}()}, true},
		{"one resolved after the trouble began covers it", found("a", ago(30), now, true), []incident.Incident{func() incident.Incident {
			i := existing("a", incident.Resolved, ago(60), incident.Detected)
			i.ResolvedAt = resolvedAt(5)
			return i
		}()}, false},
		{"one closed after the trouble began covers it", found("a", ago(30), now, true), []incident.Incident{func() incident.Incident {
			i := existing("a", incident.Closed, ago(60), incident.Detected)
			i.ResolvedAt, i.ClosedAt = resolvedAt(20), resolvedAt(2)
			return i
		}()}, false},
		{"an incident that began after this trouble ended does not cover it", found("a", ago(100), ago(25), false), []incident.Incident{existing("a", incident.Open, ago(5), incident.Detected)}, true},
	}
	for _, tc := range cases {
		got := Fresh([]Found{tc.f}, tc.have, now, Defaults(), 30*time.Minute) // join: five minutes
		if (len(got) == 1) != tc.want {
			t.Errorf("%s: kept = %v, want %v", tc.name, len(got) == 1, tc.want)
		}
	}
}

// --- Describe ---

func TestDescribeSeverityAndText(t *testing.T) {
	c := Defaults()
	start := time.Date(2026, 11, 10, 14, 2, 0, 0, time.UTC)
	f := Found{App: "/k8s/shop/web", Label: "shop/web", Breach: Breach{
		Kinds: []string{Availability, LatencyKind}, Start: start, End: start.Add(29 * time.Minute),
		PeakBurn:   map[string]float64{Availability: 20, LatencyKind: 40},
		Compliance: map[string]float64{Availability: 0.96, LatencyKind: 0.9}, Requests: 9310,
	}}
	in := Describe(f, c, 0.5)
	if in.Severity != incident.Critical { // 40 >= 2 * 14.4
		t.Fatalf("severity = %v", in.Severity)
	}
	if in.Title != "shop/web: availability and latency objective burning" {
		t.Fatalf("title = %q", in.Title)
	}
	for _, want := range []string{"from 14:02 UTC", "29m0s", "ended 14:31 UTC", "4% of requests failed", "9310 requests", "10% of requests took longer than 500 ms", "up to 40x faster"} {
		if !strings.Contains(in.Summary, want) {
			t.Errorf("summary lacks %q:\n%s", want, in.Summary)
		}
	}
	if in.Source != incident.Detected || !in.StartedAt.Equal(start) || in.App != f.App {
		t.Fatalf("input = %+v", in)
	}

	f.PeakBurn = map[string]float64{Availability: 20, LatencyKind: 28.7}
	if got := Describe(f, c, 0).Severity; got != incident.Warning {
		t.Fatalf("a burn just under twice the threshold is a warning, got %v", got)
	}
	f.PeakBurn[LatencyKind] = 28.8
	if got := Describe(f, c, 0).Severity; got != incident.Critical {
		t.Fatalf("exactly twice the threshold is critical, got %v", got)
	}
	if got := Describe(f, c, 1).Summary; !strings.Contains(got, "longer than 1000 ms") {
		t.Fatalf("the histogram's own bound is what is named:\n%s", got)
	}
}

func TestDescribeWithTooFewRequests(t *testing.T) {
	f := Found{App: "x", Label: "x", Breach: Breach{Kinds: []string{Availability}, Start: time.Now(), End: time.Now(), PeakBurn: map[string]float64{}}}
	if s := Describe(f, Defaults(), 0).Summary; !strings.Contains(s, "too few requests") {
		t.Fatalf("summary = %q", s)
	}
}

func TestDedupeKey(t *testing.T) {
	a := time.Unix(1_700_000_000, 0)
	k := DedupeKey("/k8s/shop/web", a)
	if k != DedupeKey("/k8s/shop/web", a) {
		t.Fatal("not stable")
	}
	if k == DedupeKey("/k8s/shop/web", a.Add(time.Minute)) || k == DedupeKey("/k8s/shop/api", a) {
		t.Fatal("must differ by start and by application")
	}
	// An application id the manager's key alphabet would refuse still gives a key it accepts.
	m := incident.NewManager(incident.NewMemStore(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, _, err := m.Create(bg, "gke", Actor, incident.CreateInput{Title: "t", DedupeKey: DedupeKey("/system.slice/we ird~name.service", a), Source: incident.Detected}); err != nil {
		t.Fatal(err)
	}
}

func TestTrim(t *testing.T) {
	for v, want := range map[float64]string{0: "0", 1: "1", 1.5: "1.5", 33.333333: "33.33", 0.001: "0", 100: "100", 14.4: "14.4", -0.001: "0"} {
		if got := trim(v); got != want {
			t.Errorf("trim(%v) = %q, want %q", v, got, want)
		}
	}
}

func TestActorIsAcceptedByTheManager(t *testing.T) {
	if !incident.ValidActor(Actor) {
		t.Fatalf("actor %q would be refused", Actor)
	}
}
