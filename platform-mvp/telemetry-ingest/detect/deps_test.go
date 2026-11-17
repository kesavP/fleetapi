package detect

import (
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/incident"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/store"
)

// An hour of one-minute steps. The start is a multiple of the step, as the runner's grid is.
var depGrid = Grid{Start: 1_700_000_040, Step: 60, N: 60}

const (
	mongoPod  = "/k8s/db/mongo-0/mongo" // statefulset db/mongo
	mongoPod2 = "/k8s/db/mongo-1/mongo" // the same application's second pod
	webPod    = "/k8s/shop/web-5d6f8c7b9-xk2lp/app"
	apiPod    = "/k8s/shop/api-7c6d5b4f8-qwrtp/app"
	dnsPod    = "/k8s/kube-system/coredns-5d78c9869d-bcdfg/coredns"
	mongoApp  = "/k8s/db/mongo"
	webApp    = "/k8s/shop/web"
	apiApp    = "/k8s/shop/api"
	podIP     = "10.0.0.5"
	podAddr   = "10.0.0.5:27017"
	svcAddr   = "10.96.0.10:27017" // the Service's address, which callers dial
)

// ser is a series with one point per step at the rate f gives for that step.
func connSeries(labels map[string]string, f func(i int) float64) store.MetricSeries {
	s := store.MetricSeries{Labels: labels}
	for i := 0; i < depGrid.N; i++ {
		s.Points = append(s.Points, store.Point{T: (depGrid.Start + int64(i*depGrid.Step)) * 1000, V: f(i)})
	}
	return s
}

// serRange is a series that has points only for steps a up to but not including b: one that began, or stopped, being reported.
func serRange(labels map[string]string, a, b int, f func(i int) float64) store.MetricSeries {
	s := store.MetricSeries{Labels: labels}
	for i := a; i < b && i < depGrid.N; i++ {
		s.Points = append(s.Points, store.Point{T: (depGrid.Start + int64(i*depGrid.Step)) * 1000, V: f(i)})
	}
	return s
}

func always(v float64) func(int) float64 { return func(int) float64 { return v } }

// from: v from step i on, nothing before it.
func from(i int, v float64) func(int) float64 {
	return func(n int) float64 {
		if n >= i {
			return v
		}
		return 0
	}
}

// between: v for steps a up to but not including b.
func between(a, b int, v float64) func(int) float64 {
	return func(n int) float64 {
		if n >= a && n < b {
			return v
		}
		return 0
	}
}

// listens: a container that listens on addr (a series of the listen gauge, which is 1 while it does).
func listens(container, addr string) store.MetricSeries {
	return connSeries(lab("container_id", container, "listen_addr", addr, "proxy", ""), always(1))
}

func failedFrom(container, dest string, f func(int) float64) store.MetricSeries {
	return connSeries(lab("container_id", container, "destination", dest), f)
}

func okFrom(container, dest, actual string, f func(int) float64) store.MetricSeries {
	return connSeries(lab("container_id", container, "destination", dest, "actual_destination", actual), f)
}

func depByID(t *testing.T, deps []Dep, id string) Dep {
	t.Helper()
	for _, d := range deps {
		if d.ID == id {
			return d
		}
	}
	t.Fatalf("no dependency %q among %v", id, depIDs(deps))
	return Dep{}
}

func depIDs(deps []Dep) []string {
	var out []string
	for _, d := range deps {
		out = append(out, d.ID)
	}
	return out
}

func total(a []float64) float64 {
	var s float64
	for _, v := range a {
		s += v
	}
	return s
}

func TestBuildDepsNamesTheApplicationAPodAddressBelongsTo(t *testing.T) {
	deps := BuildDeps(
		[]store.MetricSeries{failedFrom(webPod, podAddr, from(30, 0.5))},
		nil,
		[]store.MetricSeries{listens(mongoPod, podAddr)},
		depGrid, nil)
	if len(deps) != 1 {
		t.Fatalf("deps = %v", depIDs(deps))
	}
	d := deps[0]
	if d.ID != mongoApp || d.Label != "db/mongo" {
		t.Fatalf("dep = %q (%q)", d.ID, d.Label)
	}
	if got := total(d.Failed); math.Abs(got-15) > 1e-9 { // 30 steps at 0.5 a second
		t.Fatalf("failed rate sum = %v", got)
	}
	if len(d.CallerFailed) != 1 || total(d.CallerFailed[webApp]) != total(d.Failed) {
		t.Fatalf("callers = %v", d.CallerFailed)
	}
}

func TestBuildDepsLearnsWhereAServiceAddressLeadsFromTheConnectionsThatWorked(t *testing.T) {
	// callers dial the Service's address; before it went down they were connected to the pod behind it, which is how the address is known
	deps := BuildDeps(
		[]store.MetricSeries{failedFrom(webPod, svcAddr, from(30, 1))},
		[]store.MetricSeries{okFrom(webPod, svcAddr, podAddr, between(0, 30, 2))},
		[]store.MetricSeries{listens(mongoPod, podAddr)},
		depGrid, nil)
	d := depByID(t, deps, mongoApp)
	if total(d.Failed) != 30 || total(d.Ok) != 60 {
		t.Fatalf("failed %v ok %v, want 30 and 60", total(d.Failed), total(d.Ok))
	}
	// the successes were to the same dependency, so a share can be told
	if d.Ok[0] != 2 || d.Ok[30] != 0 || d.Failed[0] != 0 || d.Failed[30] != 1 {
		t.Fatalf("laid out wrongly: ok %v failed %v", d.Ok[:2], d.Failed[29:32])
	}
}

func TestBuildDepsLeavesOutWhatItCannotName(t *testing.T) {
	cases := map[string]struct {
		failed []store.MetricSeries
		ok     []store.MetricSeries
		listen []store.MetricSeries
	}{
		"an address nothing listens on": {failed: []store.MetricSeries{failedFrom(webPod, "10.9.9.9:5432", always(1))}, listen: []store.MetricSeries{listens(mongoPod, podAddr)}},
		"an address two applications listen on": {
			failed: []store.MetricSeries{failedFrom(webPod, podAddr, always(1))},
			listen: []store.MetricSeries{listens(mongoPod, podAddr), listens(apiPod, podAddr)},
		},
		"a listener on every address (not a place a caller can be said to have dialled)": {
			failed: []store.MetricSeries{failedFrom(webPod, podAddr, always(1))},
			listen: []store.MetricSeries{listens(mongoPod, "0.0.0.0:27017")},
		},
		"an address that is every address":               {failed: []store.MetricSeries{failedFrom(webPod, "0.0.0.0:27017", always(1))}, listen: []store.MetricSeries{listens(mongoPod, "0.0.0.0:27017")}},
		"the unspecified IPv6 address":                   {failed: []store.MetricSeries{failedFrom(webPod, "[::]:27017", always(1))}, listen: []store.MetricSeries{listens(mongoPod, "[::]:27017")}},
		"a loopback listener":                            {failed: []store.MetricSeries{failedFrom(webPod, "127.0.0.1:27017", always(1))}, listen: []store.MetricSeries{listens(mongoPod, "127.0.0.1:27017")}},
		"a caller that is no application":                {failed: []store.MetricSeries{failedFrom("/", podAddr, always(1))}, listen: []store.MetricSeries{listens(mongoPod, podAddr)}},
		"a destination that is not an address":           {failed: []store.MetricSeries{failedFrom(webPod, "not-an-address", always(1))}, listen: []store.MetricSeries{listens(mongoPod, podAddr)}},
		"an application calling itself":                  {failed: []store.MetricSeries{failedFrom(mongoPod2, podAddr, always(1))}, listen: []store.MetricSeries{listens(mongoPod, podAddr)}},
		"a Service address nothing was ever seen behind": {failed: []store.MetricSeries{failedFrom(webPod, svcAddr, always(1))}, listen: []store.MetricSeries{listens(mongoPod, podAddr)}},
	}
	for name, c := range cases {
		if deps := BuildDeps(c.failed, c.ok, c.listen, depGrid, nil); len(deps) != 0 {
			t.Errorf("%s: named %v", name, depIDs(deps))
		}
	}
}

func TestBuildDepsTakesAnyPortOfAPodButNotOfAHost(t *testing.T) {
	// an IP is one pod's, and any port on it is that pod
	deps := BuildDeps([]store.MetricSeries{failedFrom(webPod, podIP+":9999", always(1))}, nil, []store.MetricSeries{listens(mongoPod, podAddr)}, depGrid, nil)
	if len(deps) != 1 || deps[0].ID != mongoApp {
		t.Fatalf("a pod's other port: %v", depIDs(deps))
	}
	// on a plain host one IP carries many services, so only the exact address says which
	host := "/system.slice/mongod.service"
	deps = BuildDeps([]store.MetricSeries{failedFrom(webPod, "192.168.1.5:9999", always(1))}, nil, []store.MetricSeries{listens(host, "192.168.1.5:27017")}, depGrid, nil)
	if len(deps) != 0 {
		t.Fatalf("a host's other port was taken for the service: %v", depIDs(deps))
	}
	deps = BuildDeps([]store.MetricSeries{failedFrom(webPod, "192.168.1.5:27017", always(1))}, nil, []store.MetricSeries{listens(host, "192.168.1.5:27017")}, depGrid, nil)
	if len(deps) != 1 || deps[0].ID != host {
		t.Fatalf("a host's exact address: %v", depIDs(deps))
	}
}

func TestBuildDepsAddsTheCallersTogetherAndKeepsThemApart(t *testing.T) {
	deps := BuildDeps(
		[]store.MetricSeries{failedFrom(webPod, podAddr, always(1)), failedFrom(apiPod, podAddr, always(2)), failedFrom("/k8s/shop/web-5d6f8c7b9-zzzzz/app", podAddr, always(1))},
		nil, []store.MetricSeries{listens(mongoPod, podAddr), listens(mongoPod2, "10.0.0.6:27017")}, depGrid, nil)
	d := depByID(t, deps, mongoApp)
	if d.Failed[0] != 4 {
		t.Fatalf("failed rate = %v, want the three callers' 1+2+1", d.Failed[0])
	}
	if d.CallerFailed[webApp][0] != 2 || d.CallerFailed[apiApp][0] != 2 {
		t.Fatalf("callers = %v: two pods of one application are one caller", d.CallerFailed)
	}
}

func TestBuildDepsLeavesOutTheNamespacesItIsToldTo(t *testing.T) {
	deps := BuildDeps(
		[]store.MetricSeries{failedFrom(webPod, "10.0.0.9:53", always(1)), failedFrom(webPod, podAddr, always(1))},
		nil, []store.MetricSeries{listens(dnsPod, "10.0.0.9:53"), listens(mongoPod, podAddr)}, depGrid, regexp.MustCompile(`^kube-`))
	if got := depIDs(deps); len(got) != 1 || got[0] != mongoApp {
		t.Fatalf("deps = %v", got)
	}
}

func TestBuildDepsOnlyReturnsWhatFailed(t *testing.T) {
	deps := BuildDeps(nil, []store.MetricSeries{okFrom(webPod, svcAddr, podAddr, always(3))}, []store.MetricSeries{listens(mongoPod, podAddr)}, depGrid, nil)
	if len(deps) != 0 {
		t.Fatalf("a dependency that only worked: %v", depIDs(deps))
	}
}

func TestBuildDepsAnAddressClaimedAgainLaterGoesToTheLatest(t *testing.T) {
	// the pod IP was reused: the first application stopped reporting it long before the second began
	old := serRange(lab("container_id", mongoPod, "listen_addr", podAddr, "proxy", ""), 0, 10, always(1))
	cur := serRange(lab("container_id", apiPod, "listen_addr", podAddr, "proxy", ""), 40, depGrid.N, always(1))
	deps := BuildDeps([]store.MetricSeries{failedFrom(webPod, podAddr, always(1))}, nil, []store.MetricSeries{old, cur}, depGrid, nil)
	if got := depIDs(deps); len(got) != 1 || got[0] != apiApp {
		t.Fatalf("deps = %v", got)
	}
}

// ---- finding the stretches ----

func depOf(failed, ok func(int) float64) Dep {
	d := Dep{ID: mongoApp, Label: "db/mongo", Failed: make([]float64, depGrid.N), Ok: make([]float64, depGrid.N), CallerFailed: map[string][]float64{webApp: make([]float64, depGrid.N)}}
	for i := 0; i < depGrid.N; i++ {
		d.Failed[i], d.Ok[i] = failed(i), ok(i)
		d.CallerFailed[webApp][i] = failed(i)
	}
	return d
}

func TestADependencyThatStoppedAcceptingConnectionsIsABreach(t *testing.T) {
	d := depOf(from(30, 1), between(0, 30, 2))
	got := FindDepBreaches(d, depGrid, DepDefaults())
	if len(got) != 1 {
		t.Fatalf("breaches = %+v", got)
	}
	b := got[0]
	if b.First != 30 || b.Last != 59 || !b.Open {
		t.Fatalf("first %d last %d open %v: it began at the first step with a failed connect and is still going", b.First, b.Last, b.Open)
	}
	if want := time.Unix(depGrid.Start+30*60, 0).UTC(); !b.Start.Equal(want) {
		t.Fatalf("start = %v, want %v", b.Start, want)
	}
	if want := time.Unix(depGrid.Start+60*60, 0).UTC(); !b.End.Equal(want) {
		t.Fatalf("end = %v, want %v", b.End, want)
	}
	if b.Failed != 30*60 || b.Attempts != 30*60 {
		t.Fatalf("failed %v of %v attempts: nothing connected in the stretch", b.Failed, b.Attempts)
	}
	if b.PeakShare != 1 {
		t.Fatalf("peak share = %v", b.PeakShare)
	}
}

func TestABreachThatEndedIsClosedWhereTheFailuresStopped(t *testing.T) {
	d := depOf(between(10, 21, 1), between(21, 60, 2))
	got := FindDepBreaches(d, depGrid, DepDefaults())
	if len(got) != 1 {
		t.Fatalf("breaches = %+v", got)
	}
	// the window that judges it reaches five minutes past the last failure, but the breach is not made longer by that
	if b := got[0]; b.First != 10 || b.Last != 20 || b.Open {
		t.Fatalf("first %d last %d open %v", b.First, b.Last, b.Open)
	}
}

func TestSomeFailuresAmongManySuccessesAreNotABreach(t *testing.T) {
	// 0.5 a second fail against 20 that succeed: 2.4% of attempts
	if got := FindDepBreaches(depOf(always(0.5), always(20)), depGrid, DepDefaults()); len(got) != 0 {
		t.Fatalf("breaches = %+v", got)
	}
	// at the share the rule names it is
	c := DepDefaults()
	c.Share = 0.02
	if got := FindDepBreaches(depOf(always(0.5), always(20)), depGrid, c); len(got) != 1 {
		t.Fatalf("at a 2%% share: %+v", got)
	}
}

func TestTooFewFailedConnectionsAreNotEnoughToSayAnything(t *testing.T) {
	// two failed connects in each window of five minutes, all hour long, with nothing else connecting: every one of the attempts failed, but two
	// is a trickle, not an outage
	d := depOf(always(1.0/150), always(0))
	if got := FindDepBreaches(d, depGrid, DepDefaults()); len(got) != 0 {
		t.Fatalf("breaches = %+v", got)
	}
	// at the number the rule names it is one
	c := DepDefaults()
	c.MinFailed = 2
	if got := FindDepBreaches(d, depGrid, c); len(got) != 1 {
		t.Fatalf("at a minimum of two: %+v", got)
	}
}

func TestABlipShorterThanTheMinimumIsLeftOut(t *testing.T) {
	if got := FindDepBreaches(depOf(between(30, 31, 1), always(0)), depGrid, DepDefaults()); len(got) != 0 {
		t.Fatalf("one minute: %+v", got)
	}
	if got := FindDepBreaches(depOf(between(30, 32, 1), always(0)), depGrid, DepDefaults()); len(got) != 1 {
		t.Fatalf("two minutes: %+v", got)
	}
}

func TestBreachesCloserThanTheJoinAreOneAndFartherAreTwo(t *testing.T) {
	two := func(gap int) func(int) float64 {
		return func(i int) float64 {
			if (i >= 5 && i < 10) || (i >= 10+gap && i < 15+gap) {
				return 1
			}
			return 0
		}
	}
	if got := FindDepBreaches(depOf(two(3), always(0)), depGrid, DepDefaults()); len(got) != 1 {
		t.Fatalf("a gap of three steps: %+v", got)
	}
	if got := FindDepBreaches(depOf(two(12), always(0)), depGrid, DepDefaults()); len(got) != 2 {
		t.Fatalf("a gap of twelve steps: %+v", got)
	}
}

func TestNothingFailingIsNothing(t *testing.T) {
	if got := FindDepBreaches(depOf(always(0), always(5)), depGrid, DepDefaults()); len(got) != 0 {
		t.Fatalf("breaches = %+v", got)
	}
	if got := FindDepBreaches(depOf(always(1), always(1)), Grid{}, DepDefaults()); len(got) != 0 {
		t.Fatalf("an empty grid: %+v", got)
	}
}

func TestCallersInABreachAreTheBusiestFirst(t *testing.T) {
	d := depOf(from(30, 3), always(0))
	d.CallerFailed = map[string][]float64{
		webApp: make([]float64, depGrid.N), apiApp: make([]float64, depGrid.N),
	}
	for i := 30; i < depGrid.N; i++ {
		d.CallerFailed[webApp][i] = 1
		d.CallerFailed[apiApp][i] = 2
	}
	got := d.CallersIn(30, 59, depGrid.Step)
	if len(got) != 2 || got[0].ID != apiApp || got[0].Failed != 30*2*60 || got[1].ID != webApp || got[1].Failed != 30*60 {
		t.Fatalf("callers = %+v", got)
	}
	if got[0].Label != "shop/api" {
		t.Fatalf("label = %q", got[0].Label)
	}
	if only := d.CallersIn(0, 10, depGrid.Step); len(only) != 0 {
		t.Fatalf("callers before anything failed: %+v", only)
	}
}

// ---- the incident ----

func TestAnIncidentSaysWhichApplicationCannotBeReachedByWhomAndHowOften(t *testing.T) {
	d := depOf(from(30, 1), between(0, 30, 2))
	b := FindDepBreaches(d, depGrid, DepDefaults())[0]
	in := DescribeDep(d, b, DepDefaults(), depGrid, "")
	if in.Title != "db/mongo: connections failing" || in.App != mongoApp {
		t.Fatalf("title %q app %q", in.Title, in.App)
	}
	if in.Source != incident.Detected || in.Severity != incident.Critical {
		t.Fatalf("source %v severity %v: every connection failed", in.Source, in.Severity)
	}
	if !in.StartedAt.Equal(b.Start) {
		t.Fatalf("started = %v", in.StartedAt)
	}
	for _, want := range []string{"Opened automatically", "Connections to db/mongo have been failing", "still going", "1800 of 1800 connection attempts failed (100%)", "from 1 caller: shop/web (1800)"} {
		if !strings.Contains(in.Summary, want) {
			t.Errorf("summary lacks %q:\n%s", want, in.Summary)
		}
	}
	if !strings.HasPrefix(in.DedupeKey, "deps:") {
		t.Fatalf("dedupe key = %q: the prefix is what tells it from the other detectors' incidents", in.DedupeKey)
	}
}

func TestSomeConnectionsWorkingMakesItAWarningAndAnEndedOneSaysSo(t *testing.T) {
	d := depOf(between(10, 20, 1), func(i int) float64 {
		if i >= 10 && i < 20 {
			return 3 // 1 of 4 attempts fails
		}
		return 0
	})
	b := FindDepBreaches(d, depGrid, DepDefaults())[0]
	in := DescribeDep(d, b, DepDefaults(), depGrid, "")
	if in.Severity != incident.Warning {
		t.Fatalf("severity = %v: a quarter of connections failing is not all of them", in.Severity)
	}
	for _, want := range []string{"600 of 2400 connection attempts failed (25%)", "ended"} {
		if !strings.Contains(in.Summary, want) {
			t.Errorf("summary lacks %q:\n%s", want, in.Summary)
		}
	}
	if strings.Contains(in.Summary, "still going") {
		t.Errorf("an ended breach is not still going:\n%s", in.Summary)
	}
}

func TestTheNoteIsPutAfterWhatWasMeasured(t *testing.T) {
	d := depOf(from(30, 1), always(0))
	b := FindDepBreaches(d, depGrid, DepDefaults())[0]
	in := DescribeDep(d, b, DepDefaults(), depGrid, "It is set to run no replicas.")
	if !strings.HasSuffix(in.Summary, "\nIt is set to run no replicas.") {
		t.Fatalf("summary:\n%s", in.Summary)
	}
}

func TestOnlyTheBusiestCallersAreNamedAndTheRestAreCounted(t *testing.T) {
	d := depOf(from(30, 1), always(0))
	d.CallerFailed = map[string][]float64{}
	for i, name := range []string{"a", "b", "c", "d", "e"} {
		d.CallerFailed["/k8s/shop/"+name] = make([]float64, depGrid.N)
		for s := 30; s < depGrid.N; s++ {
			d.CallerFailed["/k8s/shop/"+name][s] = float64(10 - i)
		}
	}
	b := FindDepBreaches(d, depGrid, DepDefaults())[0]
	in := DescribeDep(d, b, DepDefaults(), depGrid, "")
	if !strings.Contains(in.Summary, "from 5 callers: shop/a (18000), shop/b (16200), shop/c (14400) and 2 more") {
		t.Fatalf("summary:\n%s", in.Summary)
	}
}

func TestDepKeysDifferByApplicationAndStartAndAreNotTheOtherDetectorsKeys(t *testing.T) {
	a := time.Unix(1_700_000_000, 0)
	if DepKey(mongoApp, a) == DepKey(webApp, a) || DepKey(mongoApp, a) == DepKey(mongoApp, a.Add(time.Minute)) {
		t.Fatal("two breaches share a key")
	}
	if DepKey(mongoApp, a) != DepKey(mongoApp, a) {
		t.Fatal("the same breach has two keys")
	}
	if DepKey(mongoApp, a) == SilenceKey(mongoApp, a) || DepKey(mongoApp, a) == DedupeKey(mongoApp, a) {
		t.Fatal("a dependency breach shares a key with another kind of incident")
	}
}

func TestDepConfigIsCheckedBeforeItIsUsed(t *testing.T) {
	if err := DepDefaults().Validate(); err != nil {
		t.Fatalf("the defaults: %v", err)
	}
	for name, mod := range map[string]func(*DepConfig){
		"no window":              func(c *DepConfig) { c.WindowSeconds = 0 },
		"a share of zero":        func(c *DepConfig) { c.Share = 0 },
		"a share over one":       func(c *DepConfig) { c.Share = 1.5 },
		"no failed connections":  func(c *DepConfig) { c.MinFailed = 0 },
		"critical below warning": func(c *DepConfig) { c.CriticalShare = c.Share / 2 },
		"a negative join":        func(c *DepConfig) { c.JoinSeconds = -1 },
		"a negative minimum":     func(c *DepConfig) { c.MinSeconds = -1 },
	} {
		c := DepDefaults()
		mod(&c)
		if c.Validate() == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
