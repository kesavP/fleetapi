package detect

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/incident"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/store"
)

const (
	ctr = "/k8s/shop/%s-5d6f8c7b9-xk2lp/app"
)

func container(name string) string { return strings.Replace(ctr, "%s", name, 1) }

// silenceRig: a cluster whose frontend is sending, whose cart was scaled to zero, whose payment service wants three and has none, whose ad
// service is ready but stopped sending, and whose recommendation service is ready and has never sent. Time runs in the past, so that the
// incident manager (which refuses a start in the future by its own clock) is never asked for one.
type silenceRig struct {
	*rig
	base   time.Time // when the stale ones last sent
	sender bool      // is anything sending
}

func newSilenceRig(t *testing.T) *silenceRig {
	t.Helper()
	g := &silenceRig{rig: newRig(t), sender: true}
	g.now = time.Now().UTC().Truncate(time.Minute).Add(-3 * time.Hour)
	g.base = g.now.Add(-20 * time.Minute)
	g.r.SilenceAfter = 5 * time.Minute
	g.r.SilenceExclude = regexp.MustCompile(`^kube-`)
	g.src.metrics["gke"] = []string{RSSMetric}
	g.src.rows = []store.InventoryObject{
		wl("deployment", "shop", "frontend", 2, 2), wl("deployment", "shop", "cartservice", 0, 0), wl("deployment", "shop", "paymentservice", 3, 0),
		wl("deployment", "shop", "adservice", 1, 1), wl("deployment", "shop", "recommendationservice", 2, 2), wl("cronjob", "shop", "cleanup", 0, 0),
		wl("daemonset", "kube-system", "agent", 3, 0),
		podRow("shop", "paymentservice", map[string]any{"phase": "Running", "ready": false, "containers": []any{map[string]any{"state": "waiting", "reason": "CrashLoopBackOff", "last_reason": "OOMKilled"}}}),
		podRow("shop", "paymentservice", map[string]any{"phase": "Running", "ready": false, "containers": []any{map[string]any{"state": "waiting", "reason": "CrashLoopBackOff", "last_reason": "OOMKilled"}}}),
	}
	g.refresh()
	return g
}

// refresh brings what is "now" up to date: the collector has just reported, and the frontend has just sent.
func (g *silenceRig) refresh() {
	g.src.summary = []store.InventoryCluster{{Cluster: "gke", LastReceived: g.now.Add(-20 * time.Second)}}
	g.src.rss = []rssSeries{
		{map[string]string{"container_id": container("cartservice")}, g.base},
		{map[string]string{"container_id": container("paymentservice")}, g.base},
		{map[string]string{"container_id": container("adservice")}, g.base},
	}
	if g.sender {
		g.src.rss = append(g.src.rss, rssSeries{map[string]string{"container_id": container("frontend")}, g.now.Add(-time.Minute)})
	}
}

func (g *silenceRig) advance(d time.Duration) { g.now = g.now.Add(d); g.refresh() }

func (g *silenceRig) look(t *testing.T) int {
	t.Helper()
	n, err := g.r.Project(bg, "gke")
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (g *silenceRig) byApp(t *testing.T) map[string]incident.Incident {
	t.Helper()
	out := map[string]incident.Incident{}
	for _, i := range g.list(t) {
		out[i.App] = i
	}
	return out
}

func TestSilentApplicationsOpenIncidentsOnlyAfterTheDelayAndOnlyTheOnesWorthIt(t *testing.T) {
	g := newSilenceRig(t)
	if n := g.look(t); n != 0 {
		t.Fatalf("the first look opened %d: a rollout or a blip would have", n)
	}
	g.advance(4 * time.Minute)
	if n := g.look(t); n != 0 {
		t.Fatalf("four minutes in, opened %d", n)
	}
	g.advance(2 * time.Minute) // six since the first look
	if n := g.look(t); n != 2 {
		t.Fatalf("after the delay: opened %d, want the payment service and the ad service", n)
	}
	got := g.byApp(t)
	if len(got) != 2 {
		t.Fatalf("incidents for %v", got)
	}

	pay := got["/k8s/shop/paymentservice"]
	if pay.Title != "shop/paymentservice: not ready (0 of 3)" || pay.Severity != incident.Critical || pay.Source != incident.Detected || pay.State != incident.Open {
		t.Fatalf("payment: %+v", pay)
	}
	if !pay.StartedAt.Equal(g.base) {
		t.Fatalf("it began when it last sent (%v), not when the detector first noticed (%v): %v", g.base, g.base.Add(20*time.Minute), pay.StartedAt)
	}
	for _, want := range []string{"set to run 3 and 0 are ready", "CrashLoopBackOff (OOMKilled) ×2", "not a scale-down"} {
		if !strings.Contains(pay.Summary, want) {
			t.Errorf("summary lacks %q:\n%s", want, pay.Summary)
		}
	}
	if len(pay.Evidence) != 2 || pay.Evidence[0].Ref.Kind != incident.KindApplication || pay.Evidence[0].Ref.Service != "/k8s/shop/paymentservice" || pay.Evidence[1].Ref.Kind != incident.KindLogs {
		t.Fatalf("evidence: %+v", pay.Evidence)
	}
	if !strings.HasPrefix(pay.DedupeKey, "silence:") {
		t.Fatalf("key %q", pay.DedupeKey)
	}

	ad := got["/k8s/shop/adservice"]
	if ad.Title != "shop/adservice: ready but sending no data" || ad.Severity != incident.Warning {
		t.Fatalf("ad: %+v", ad)
	}

	// scaled to zero, never seen, a cron job and the platform's own are not incidents
	for _, app := range []string{"/k8s/shop/cartservice", "/k8s/shop/recommendationservice", "/k8s-cronjob/shop/cleanup", "/k8s/kube-system/agent", "/k8s/shop/frontend"} {
		if _, has := got[app]; has {
			t.Errorf("an incident for %s", app)
		}
	}

	// looking again opens nothing
	g.advance(time.Minute)
	if n := g.look(t); n != 0 || len(g.list(t)) != 2 {
		t.Fatalf("a repeated look: opened %d, incidents %d", n, len(g.list(t)))
	}
}

func TestResolvingASilentApplicationWhileItIsStillSilentDoesNotOpenAnother(t *testing.T) {
	g := newSilenceRig(t)
	g.look(t)
	g.advance(6 * time.Minute)
	g.look(t)
	pay := g.byApp(t)["/k8s/shop/paymentservice"]
	if _, err := g.m.Transition(bg, "gke", pay.ID, "ana", nil, incident.TransitionInput{To: incident.Resolved, Resolution: incident.Workaround, Note: "scaled it up"}); err != nil {
		t.Fatal(err)
	}
	g.advance(2 * time.Minute)
	if n := g.look(t); n != 0 {
		t.Fatalf("opened %d: it was resolved by a person a moment ago", n)
	}
}

func TestAnApplicationThatComesBackStartsTheCountAgain(t *testing.T) {
	g := newSilenceRig(t)
	g.look(t)
	g.advance(3 * time.Minute)
	// the payment service starts sending, and is ready
	g.src.rows[2] = wl("deployment", "shop", "paymentservice", 3, 3)
	g.src.mu.Lock()
	g.src.rss = append(g.src.rss, rssSeries{map[string]string{"container_id": container("paymentservice")}, g.now.Add(-time.Minute)})
	g.src.mu.Unlock()
	g.look(t)
	// it goes quiet again: the five minutes it was silent before do not count
	g.src.rows[2] = wl("deployment", "shop", "paymentservice", 3, 0)
	g.advance(3 * time.Minute)
	g.look(t) // first seen silent again
	g.advance(3 * time.Minute)
	g.look(t)
	if _, has := g.byApp(t)["/k8s/shop/paymentservice"]; has {
		t.Fatal("it was opened on the count from before it recovered")
	}
	g.advance(3 * time.Minute) // six since it went quiet again
	g.look(t)
	if _, has := g.byApp(t)["/k8s/shop/paymentservice"]; !has {
		t.Fatal("it was not opened after the delay")
	}
}

func TestNothingIsJudgedWithoutAnInventoryOrWithAStaleOne(t *testing.T) {
	g := newSilenceRig(t)
	g.src.summary = nil
	g.look(t)
	g.advance(10 * time.Minute)
	g.src.summary = nil
	if n := g.look(t); n != 0 {
		t.Fatalf("without a collector: opened %d (stopped sending cannot be told from scaled down)", n)
	}
	// a collector that has not reported for eleven minutes is not trusted
	g.src.summary = []store.InventoryCluster{{Cluster: "gke", LastReceived: g.now.Add(-11 * time.Minute)}}
	g.advance(10 * time.Minute)
	g.src.summary = []store.InventoryCluster{{Cluster: "gke", LastReceived: g.now.Add(-11 * time.Minute)}}
	if n := g.look(t); n != 0 {
		t.Fatalf("with a stale collector: opened %d", n)
	}
	// still stale six minutes later, long enough that a count that had been running would have opened them
	g.advance(6 * time.Minute)
	g.src.summary = []store.InventoryCluster{{Cluster: "gke", LastReceived: g.now.Add(-11 * time.Minute)}}
	if n := g.look(t); n != 0 {
		t.Fatalf("with a collector stale for six minutes more: opened %d", n)
	}
	// and the count does not run through it: when it is back, the delay starts from the first look
	g.refresh()
	if n := g.look(t); n != 0 {
		t.Fatalf("the look after it came back opened %d", n)
	}
	g.advance(6 * time.Minute)
	if n := g.look(t); n != 2 {
		t.Fatalf("after the delay with a current collector: %d", n)
	}
}

func TestOnlyTheClustersThatReportedLatelyAreJudged(t *testing.T) {
	g := newSilenceRig(t)
	for i := range g.src.rows {
		g.src.rows[i].Cluster = "dead"
	}
	g.look(t)
	g.advance(10 * time.Minute)
	if n := g.look(t); n != 0 {
		t.Fatalf("the rows of a cluster that is not reporting were judged: %d", n)
	}
}

func TestNothingSendingMeansTheAgentNotTheApplications(t *testing.T) {
	g := newSilenceRig(t)
	g.sender = false
	g.refresh()
	g.look(t)
	g.advance(10 * time.Minute)
	if n := g.look(t); n != 0 {
		t.Fatalf("with nothing sending at all, opened %d", n)
	}
}

func TestSilenceNeedsTheMemoryGaugeAndCanBeSwitchedOff(t *testing.T) {
	g := newSilenceRig(t)
	g.src.metrics["gke"] = []string{"node_cpu_seconds_total"}
	g.look(t)
	g.advance(10 * time.Minute)
	if n := g.look(t); n != 0 {
		t.Fatalf("with no memory gauge: %d", n)
	}
	g.src.metrics["gke"] = []string{RSSMetric}
	g.r.SilenceAfter = 0
	g.look(t)
	g.advance(10 * time.Minute)
	if n := g.look(t); n != 0 {
		t.Fatalf("with silence switched off: %d", n)
	}
	g.r.SilenceAfter = 30 * time.Second
	if g.r.Validate() == nil {
		t.Fatal("a delay under a minute was accepted")
	}
	g.r.SilenceAfter = -time.Minute
	if g.r.Validate() == nil {
		t.Fatal("a negative delay was accepted")
	}
	g.r.SilenceAfter = 5 * time.Minute
	if err := g.r.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestWithoutAnExclusionThePlatformsOwnWorkloadsAreJudged(t *testing.T) {
	g := newSilenceRig(t)
	g.r.SilenceExclude = nil
	g.look(t)
	g.advance(6 * time.Minute)
	if n := g.look(t); n != 3 {
		t.Fatalf("opened %d, want the two and the daemon set of kube-system", n)
	}
}

func TestBurningAndSilenceDoNotStopEachOther(t *testing.T) {
	// the inventory failing is an error for the project, but the burn-rate half still ran
	g := newSilenceRig(t)
	g.src.metrics["gke"] = []string{RSSMetric, RequestsMetric}
	g.failing(g.now.Add(-30*time.Minute), time.Time{})
	g.src.invErr = errors.New("clickhouse is down")
	n, err := g.r.Project(bg, "gke")
	if err == nil || !strings.Contains(err.Error(), "inventory") {
		t.Fatalf("err = %v", err)
	}
	if n != 1 {
		t.Fatalf("the burning application was not opened: %d", n)
	}
	// and the other way: with the inventory back, an application that already has a burn incident still gets its silence incident
	g.src.invErr = nil
	g.advance(time.Minute)
	g.look(t)
	g.advance(6 * time.Minute)
	g.src.mu.Lock()
	g.src.series["gke"][RequestsMetric] = nil // the traffic stops: the burn is over
	g.src.mu.Unlock()
	if n := g.look(t); n != 2 {
		t.Fatalf("silence incidents: %d", n)
	}
}
