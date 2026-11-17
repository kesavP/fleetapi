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

// depRig: shop/web connects to the database db/mongo through its Service's address. The test says when the database stops accepting.
type depRig struct{ *rig }

func newDepRig(t *testing.T) *depRig {
	t.Helper()
	d := &depRig{newRig(t)}
	d.r.Dependencies = true
	d.r.Deps = DepDefaults()
	d.src.metrics["gke"] = []string{FailedConnectsMetric, ConnectsMetric, ListenMetric}
	d.src.series["gke"][ListenMetric] = []gen{{lab("container_id", mongoPod, "listen_addr", podAddr, "proxy", ""), func(int64) float64 { return 1 }}}
	return d
}

// refusing: web connected (2 a second) until `from`, and from then on every connect fails (1 a second), until `until` (zero: for good).
func (d *depRig) refusing(from, until time.Time) {
	d.src.series["gke"][ConnectsMetric] = []gen{{lab("container_id", webPod, "destination", svcAddr, "actual_destination", podAddr), func(t int64) float64 {
		if t < from.Unix() || (!until.IsZero() && t >= until.Unix()) {
			return 2
		}
		return 0
	}}}
	d.src.series["gke"][FailedConnectsMetric] = []gen{{lab("container_id", webPod, "destination", svcAddr), func(t int64) float64 {
		if t >= from.Unix() && (until.IsZero() || t < until.Unix()) {
			return 1
		}
		return 0
	}}}
}

func (d *depRig) look(t *testing.T) int {
	t.Helper()
	n, err := d.r.Project(bg, "gke")
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestADependencyThatRefusesConnectionsOpensOneIncident(t *testing.T) {
	d := newDepRig(t)
	from := d.now.Add(-30 * time.Minute)
	d.refusing(from, time.Time{})

	if n := d.look(t); n != 1 {
		t.Fatalf("opened %d", n)
	}
	got := d.list(t)
	if len(got) != 1 {
		t.Fatalf("incidents = %d", len(got))
	}
	in := got[0]
	if in.Title != "db/mongo: connections failing" || in.App != mongoApp || in.Source != incident.Detected || in.State != incident.Open {
		t.Fatalf("incident = %+v", in)
	}
	if in.Severity != incident.Critical {
		t.Fatalf("severity = %v: every connection failed", in.Severity)
	}
	if !in.StartedAt.Equal(from) {
		t.Fatalf("started = %v, want %v", in.StartedAt, from)
	}
	for _, want := range []string{"Opened automatically", "Connections to db/mongo have been failing", "still going", "connection attempts failed (100%)", "shop/web"} {
		if !strings.Contains(in.Summary, want) {
			t.Errorf("summary lacks %q:\n%s", want, in.Summary)
		}
	}
	if len(in.Evidence) != 3 {
		t.Fatalf("evidence = %+v", in.Evidence)
	}
	if e := in.Evidence[0].Ref; e.Kind != incident.KindApplication || e.Service != mongoApp {
		t.Fatalf("the first piece points at the dependency: %+v", e)
	}
	if e := in.Evidence[1].Ref; e.Kind != incident.KindApplication || e.Service != webApp {
		t.Fatalf("the second at its busiest caller: %+v", e)
	}
	if e := in.Evidence[2].Ref; e.Kind != incident.KindLogs || e.Service != webApp || e.Severity != "error" || e.To == nil || !e.To.Equal(d.now) {
		t.Fatalf("the third at the caller's errors, up to now while it is going on: %+v", e)
	}
	for _, e := range in.Evidence {
		if e.AddedBy != Actor {
			t.Fatalf("added by %q", e.AddedBy)
		}
	}

	// looking again must not open it again
	if n := d.look(t); n != 0 || len(d.list(t)) != 1 {
		t.Fatalf("second look: opened %d, incidents %d", n, len(d.list(t)))
	}
}

func TestAnOutageLongerThanTheLookbackStaysOneIncident(t *testing.T) {
	d := newDepRig(t)
	d.refusing(d.now.Add(-5*time.Hour), time.Time{})
	if n := d.look(t); n != 1 {
		t.Fatalf("opened %d", n)
	}
	// the look-back slides on, so the stretch begins later each time it is found: it is still the one incident
	for i := 0; i < 3; i++ {
		d.now = d.now.Add(10 * time.Minute)
		if n := d.look(t); n != 0 {
			t.Fatalf("look %d opened %d", i+2, n)
		}
	}
	if got := d.list(t); len(got) != 1 {
		t.Fatalf("incidents = %d", len(got))
	}
}

func TestDependencyDetectionIsOffUnlessItIsSwitchedOn(t *testing.T) {
	d := newDepRig(t)
	d.r.Dependencies = false
	d.refusing(d.now.Add(-30*time.Minute), time.Time{})
	if n := d.look(t); n != 0 || d.src.queries != 0 {
		t.Fatalf("opened %d after %d queries", n, d.src.queries)
	}
}

func TestAHealthyDependencyOpensNothing(t *testing.T) {
	d := newDepRig(t)
	d.refusing(d.now.Add(time.Hour), time.Time{}) // starts refusing only in the future
	if n := d.look(t); n != 0 {
		t.Fatalf("opened %d", n)
	}
}

func TestNothingIsJudgedWithoutTheMetricsItNeeds(t *testing.T) {
	for name, have := range map[string][]string{
		"no failed connections": {ConnectsMetric, ListenMetric},
		"no listen addresses":   {FailedConnectsMetric, ConnectsMetric},
		"no metrics at all":     nil,
	} {
		d := newDepRig(t)
		d.refusing(d.now.Add(-30*time.Minute), time.Time{})
		d.src.metrics["gke"] = have
		if n, err := d.r.Project(bg, "gke"); n != 0 || err != nil {
			t.Errorf("%s: opened %d, err %v", name, n, err)
		}
	}
}

func TestADependencyThatCameBackIsStillReportedForAWhileAndThenIsHistory(t *testing.T) {
	d := newDepRig(t)
	d.refusing(d.now.Add(-40*time.Minute), d.now.Add(-10*time.Minute))
	if n := d.look(t); n != 1 {
		t.Fatalf("an outage that ended ten minutes ago: opened %d", n)
	}
	in := d.list(t)[0]
	if !strings.Contains(in.Summary, "ended") || strings.Contains(in.Summary, "still going") {
		t.Fatalf("summary:\n%s", in.Summary)
	}
	if e := in.Evidence[2].Ref; e.To == nil || !e.To.Before(d.now) {
		t.Fatalf("an ended breach's logs stop shortly after it: %+v", e.To)
	}

	old := newDepRig(t)
	old.refusing(old.now.Add(-150*time.Minute), old.now.Add(-120*time.Minute))
	if n := old.look(t); n != 0 {
		t.Fatalf("an outage that ended two hours ago: opened %d", n)
	}
}

func TestResolvingItWhileItIsStillRefusingDoesNotOpenAnother(t *testing.T) {
	d := newDepRig(t)
	d.refusing(d.now.Add(-30*time.Minute), time.Time{})
	d.look(t)
	id := d.list(t)[0].ID
	if _, err := d.m.Transition(bg, "gke", id, "ana", nil, incident.TransitionInput{To: incident.Resolved, Resolution: incident.Workaround, Note: "restarted"}); err != nil {
		t.Fatal(err)
	}
	if n := d.look(t); n != 0 {
		t.Fatalf("opened %d", n)
	}
}

func TestTheClusterCollectorSaysWhetherItWasMeant(t *testing.T) {
	cases := map[string]struct {
		row  store.InventoryObject
		want string
	}{
		"scaled to zero": {wl("statefulset", "db", "mongo", 0, 0), "set to run no replicas"},
		"not ready":      {wl("statefulset", "db", "mongo", 1, 0), "set to run 1 and 0 are ready"},
		"ready":          {wl("statefulset", "db", "mongo", 2, 2), "All 2 of its pods are ready"},
	}
	for name, c := range cases {
		d := newDepRig(t)
		d.refusing(d.now.Add(-30*time.Minute), time.Time{})
		d.src.summary = []store.InventoryCluster{{Cluster: "gke", LastReceived: d.now.Add(-20 * time.Second)}}
		d.src.rows = []store.InventoryObject{c.row, wl("deployment", "shop", "web", 2, 2)}
		d.look(t)
		if got := d.list(t); len(got) != 1 || !strings.Contains(got[0].Summary, c.want) {
			t.Errorf("%s: want %q in %+v", name, c.want, got)
		}
	}
	// a scaled-to-zero dependency is still reported: the callers still depend on it
	d := newDepRig(t)
	d.refusing(d.now.Add(-30*time.Minute), time.Time{})
	d.src.summary = []store.InventoryCluster{{Cluster: "gke", LastReceived: d.now.Add(-20 * time.Second)}}
	d.src.rows = []store.InventoryObject{wl("statefulset", "db", "mongo", 0, 0)}
	if n := d.look(t); n != 1 {
		t.Fatalf("a scaled-down dependency whose callers fail: opened %d", n)
	}
}

func TestWithoutAFreshInventoryTheIncidentSaysNothingAboutWhetherItWasMeant(t *testing.T) {
	d := newDepRig(t)
	d.refusing(d.now.Add(-30*time.Minute), time.Time{})
	d.src.rows = []store.InventoryObject{wl("statefulset", "db", "mongo", 0, 0)}
	d.look(t) // no summary: the collector has never reported
	if got := d.list(t); len(got) != 1 || strings.Contains(got[0].Summary, "replicas") {
		t.Fatalf("incidents = %+v", got)
	}

	stale := newDepRig(t)
	stale.refusing(stale.now.Add(-30*time.Minute), time.Time{})
	stale.src.summary = []store.InventoryCluster{{Cluster: "gke", LastReceived: stale.now.Add(-time.Hour)}}
	stale.src.rows = []store.InventoryObject{wl("statefulset", "db", "mongo", 0, 0)}
	stale.look(t)
	if got := stale.list(t); len(got) != 1 || strings.Contains(got[0].Summary, "replicas") {
		t.Fatalf("a collector that has gone quiet is not trusted: %+v", got)
	}

	broken := newDepRig(t)
	broken.refusing(broken.now.Add(-30*time.Minute), time.Time{})
	broken.src.invErr = errors.New("inventory unavailable")
	if n := broken.look(t); n != 1 { // the incident is worth more than the note
		t.Fatalf("an inventory that cannot be read stopped the incident: opened %d", n)
	}
}

func TestNamespacesLeftOutAreNotJudged(t *testing.T) {
	d := newDepRig(t)
	d.refusing(d.now.Add(-30*time.Minute), time.Time{})
	d.r.DepsExclude = regexp.MustCompile(`^db$`)
	if n := d.look(t); n != 0 {
		t.Fatalf("opened %d for a namespace that is left out", n)
	}
}

func TestADependencyThatIsAlsoBurningGetsOneOfEachKind(t *testing.T) {
	d := newDepRig(t)
	d.refusing(d.now.Add(-30*time.Minute), time.Time{})
	// the database application serves HTTP too, and its requests fail
	d.src.metrics["gke"] = append(d.src.metrics["gke"], RequestsMetric)
	d.src.series["gke"][RequestsMetric] = []gen{
		{lab("container_id", mongoPod, "status", "200"), func(int64) float64 { return 10 }},
		{lab("container_id", mongoPod, "status", "500"), func(t int64) float64 {
			if t >= d.now.Add(-30*time.Minute).Unix() {
				return 5
			}
			return 0
		}},
	}
	if n := d.look(t); n != 2 {
		t.Fatalf("opened %d, want one for the failing requests and one for the failing connections", n)
	}
	titles := map[string]bool{}
	for _, in := range d.list(t) {
		titles[in.Title] = true
	}
	if !titles["db/mongo: connections failing"] || !titles["db/mongo: availability objective burning"] {
		t.Fatalf("titles = %v", titles)
	}
	if n := d.look(t); n != 0 {
		t.Fatalf("a second look opened %d", n)
	}
}

func TestAFailureToReadTheConnectionsDoesNotStopTheOtherDetectors(t *testing.T) {
	d := newDepRig(t)
	d.refusing(d.now.Add(-30*time.Minute), time.Time{})
	d.src.metrics["gke"] = append(d.src.metrics["gke"], RequestsMetric)
	d.src.series["gke"][RequestsMetric] = []gen{
		{lab("container_id", webPod, "status", "200"), func(int64) float64 { return 10 }},
		{lab("container_id", webPod, "status", "500"), func(t int64) float64 {
			if t >= d.now.Add(-30*time.Minute).Unix() {
				return 5
			}
			return 0
		}},
	}
	d.src.queryErr = map[string]error{ListenMetric: errors.New("clickhouse is busy")}
	n, err := d.r.Project(bg, "gke")
	if n != 1 {
		t.Fatalf("opened %d: the burning application should still be opened", n)
	}
	if err == nil || !strings.Contains(err.Error(), "clickhouse is busy") {
		t.Fatalf("err = %v: the failure should be reported", err)
	}
}

func TestTheDependencySettingsAreCheckedWhenItIsOn(t *testing.T) {
	d := newDepRig(t)
	if err := d.r.Validate(); err != nil {
		t.Fatal(err)
	}
	d.r.Deps.Share = 0
	if err := d.r.Validate(); err == nil {
		t.Fatal("a share of zero was accepted")
	}
	d.r.Dependencies = false // not used, so not checked
	if err := d.r.Validate(); err != nil {
		t.Fatalf("settings that are not used were checked: %v", err)
	}
}
