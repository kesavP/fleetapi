package collector

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/cluster-collector/kube"
	"github.com/sojoner/kro-lab/platform-mvp/cluster-collector/wire"
)

var t0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func obj(kind, uid, name string, data map[string]any) wire.Object {
	ns := "shop"
	if wire.ClusterScoped[kind] {
		ns = ""
	}
	return wire.Object{Kind: kind, UID: uid, Namespace: ns, Name: name, Created: t0.Add(-time.Hour), Data: data}
}

func listed(kinds ...string) map[string]bool {
	m := map[string]bool{}
	for _, k := range kinds {
		m[k] = true
	}
	return m
}

func snap(objs []wire.Object, kinds ...string) kube.Snapshot {
	if len(kinds) == 0 {
		kinds = append(append([]string{}, wire.Kinds...), EventKey)
	}
	return kube.Snapshot{Objects: objs, Listed: listed(kinds...), Problems: map[string]error{}}
}

func uids(objs []wire.Object) string {
	var s []string
	for _, o := range objs {
		s = append(s, o.UID)
	}
	return strings.Join(s, ",")
}

func TestTheFirstLookSendsEverythingAndSweepsWhatItReadCompletely(t *testing.T) {
	d := NewDiffer()
	s := snap([]wire.Object{obj("pod", "p1", "web-0", map[string]any{"phase": "Running"}), obj("node", "n1", "node-a", map[string]any{"ready": true})}, "pod", "node")
	p := d.Plan(s, t0, true)
	if uids(p.Objects) != "p1,n1" || len(p.Deleted) != 0 || !p.Full {
		t.Fatalf("%+v", p)
	}
	if len(p.Sweeps) != 2 || p.Sweeps[0].Kind != "node" || p.Sweeps[1].Kind != "pod" || !p.Sweeps[0].Before.Equal(t0) {
		t.Fatalf("sweeps: %+v (only the kinds that were read, in the order of wire.Kinds, cut at the time of the look)", p.Sweeps)
	}
}

func TestNothingChangedIsNothingToSend(t *testing.T) {
	d := NewDiffer()
	s := snap([]wire.Object{obj("pod", "p1", "web-0", map[string]any{"phase": "Running", "restarts": int64(2)})})
	d.Commit(d.Plan(s, t0, true))
	p := d.Plan(s, t0.Add(30*time.Second), false)
	if !p.Empty() {
		t.Fatalf("%+v", p)
	}
	// the same data built in another order is the same data
	s2 := snap([]wire.Object{obj("pod", "p1", "web-0", map[string]any{"restarts": int64(2), "phase": "Running"})})
	if p := d.Plan(s2, t0.Add(time.Minute), false); !p.Empty() {
		t.Fatalf("a different key order was taken for a change: %+v", p)
	}
}

func TestChangesNewObjectsAndDeletionsAreSentAlone(t *testing.T) {
	d := NewDiffer()
	a, b, c := obj("pod", "p1", "web-0", map[string]any{"phase": "Running"}), obj("pod", "p2", "web-1", map[string]any{"phase": "Running"}), obj("deployment", "d1", "web", map[string]any{"desired": int64(3)})
	d.Commit(d.Plan(snap([]wire.Object{a, b, c}), t0, true))

	a2 := obj("pod", "p1", "web-0", map[string]any{"phase": "Failed"}) // changed
	e := obj("pod", "p3", "web-2", map[string]any{"phase": "Pending"}) // new
	// p2 is gone
	p := d.Plan(snap([]wire.Object{a2, c, e}), t0.Add(time.Minute), false)
	if uids(p.Objects) != "p1,p3" {
		t.Fatalf("objects: %s", uids(p.Objects))
	}
	if len(p.Deleted) != 1 || p.Deleted[0] != (wire.Ref{Kind: "pod", UID: "p2", Namespace: "shop", Name: "web-1"}) {
		t.Fatalf("deleted: %+v", p.Deleted)
	}
	if len(p.Sweeps) != 0 {
		t.Fatal("sweeps only come with a full resync")
	}

	// a label is a change too
	l := obj("deployment", "d1", "web", map[string]any{"desired": int64(3)})
	l.Labels = map[string]string{"tier": "front"}
	d.Commit(p)
	if p := d.Plan(snap([]wire.Object{a2, l, e}), t0.Add(2*time.Minute), false); uids(p.Objects) != "d1" {
		t.Fatalf("a label change: %s", uids(p.Objects))
	}
	// and so is scaling to zero
	z := obj("deployment", "d1", "web", map[string]any{"desired": int64(0)})
	z.Labels = map[string]string{"tier": "front"}
	d.Commit(d.Plan(snap([]wire.Object{a2, l, e}), t0.Add(2*time.Minute), false))
	if p := d.Plan(snap([]wire.Object{a2, z, e}), t0.Add(3*time.Minute), false); uids(p.Objects) != "d1" {
		t.Fatalf("scaled to zero: %s", uids(p.Objects))
	}
}

func TestAResyncSendsEverythingAgain(t *testing.T) {
	d := NewDiffer()
	s := snap([]wire.Object{obj("pod", "p1", "a", nil), obj("pod", "p2", "b", nil)})
	d.Commit(d.Plan(s, t0, true))
	p := d.Plan(s, t0.Add(time.Hour), true)
	if uids(p.Objects) != "p1,p2" || len(p.Sweeps) != len(wire.Kinds) {
		t.Fatalf("%s, %d sweeps", uids(p.Objects), len(p.Sweeps))
	}
}

// An empty answer from a call that failed is not "everything was deleted".
func TestAKindThatWasNotReadIsLeftAsItWas(t *testing.T) {
	d := NewDiffer()
	d.Commit(d.Plan(snap([]wire.Object{obj("pod", "p1", "a", nil), obj("node", "n1", "node-a", nil)}), t0, true))

	// pods could not be read this time
	s := snap([]wire.Object{obj("node", "n1", "node-a", nil)}, "node", EventKey)
	s.Problems["pod"] = fmt.Errorf("timeout")
	p := d.Plan(s, t0.Add(time.Hour), true)
	if len(p.Deleted) != 0 {
		t.Fatalf("pods were deleted because they could not be read: %+v", p.Deleted)
	}
	for _, sw := range p.Sweeps {
		if sw.Kind == "pod" {
			t.Fatal("a kind that was not read was swept")
		}
	}
	d.Commit(p)
	// they come back, unchanged: nothing to send
	if p := d.Plan(snap([]wire.Object{obj("pod", "p1", "a", nil), obj("node", "n1", "node-a", nil)}), t0.Add(time.Hour+30*time.Second), false); !p.Empty() {
		t.Fatalf("pods that were remembered through the outage were sent again: %+v", p)
	}
}

func TestAPlanThatWasNotCommittedIsMadeAgain(t *testing.T) {
	d := NewDiffer()
	s := snap([]wire.Object{obj("pod", "p1", "a", nil)})
	first := d.Plan(s, t0, true)
	// not committed: the batches did not get through
	again := d.Plan(s, t0.Add(time.Minute), true)
	if uids(first.Objects) != uids(again.Objects) || uids(again.Objects) != "p1" {
		t.Fatalf("%s %s", uids(first.Objects), uids(again.Objects))
	}
	nf := d.Plan(s, t0.Add(time.Minute), false)
	if uids(nf.Objects) != "p1" {
		t.Fatal("without a commit, what was never reported is still to be reported")
	}
}

func ev(uid string, count int, last time.Time) wire.Event {
	return wire.Event{UID: uid, Namespace: "shop", Kind: "Pod", Name: "web-0", Type: "Warning", Reason: "BackOff", Count: count, Last: last}
}

func TestEventsAreSentWhenNewOrChanged(t *testing.T) {
	d := NewDiffer()
	s := snap(nil)
	s.Events = []wire.Event{ev("e1", 1, t0), ev("e2", 1, t0)}
	p := d.Plan(s, t0, false)
	if len(p.Events) != 2 {
		t.Fatalf("%+v", p.Events)
	}
	d.Commit(p)
	if p := d.Plan(s, t0.Add(time.Minute), false); len(p.Events) != 0 {
		t.Fatalf("unchanged events were sent again: %+v", p.Events)
	}
	s.Events = []wire.Event{ev("e1", 4, t0.Add(time.Minute)), ev("e2", 1, t0), ev("e3", 1, t0)}
	p = d.Plan(s, t0.Add(time.Minute), false)
	if len(p.Events) != 2 || p.Events[0].UID != "e1" || p.Events[1].UID != "e3" {
		t.Fatalf("%+v", p.Events)
	}
	// the same count seen again later (an event that is repeated, as a series) is a change too
	d.Commit(p)
	s.Events = []wire.Event{ev("e1", 4, t0.Add(2*time.Minute)), ev("e2", 1, t0), ev("e3", 1, t0)}
	if q := d.Plan(s, t0.Add(2*time.Minute), false); len(q.Events) != 1 || q.Events[0].UID != "e1" {
		t.Fatalf("a later time with the same count: %+v", q.Events)
	}
	d.Commit(p)
	// events that have expired from the cluster are forgotten: if one came back it would be sent
	s.Events = []wire.Event{ev("e3", 1, t0)}
	d.Commit(d.Plan(s, t0.Add(2*time.Minute), false))
	s.Events = []wire.Event{ev("e1", 4, t0.Add(time.Minute))}
	if p := d.Plan(s, t0.Add(3*time.Minute), false); len(p.Events) != 1 {
		t.Fatalf("%+v", p.Events)
	}
	// events that could not be read leave what is known alone
	d2 := NewDiffer()
	s2 := snap(nil)
	s2.Events = []wire.Event{ev("e1", 1, t0)}
	d2.Commit(d2.Plan(s2, t0, false))
	blind := snap(nil, "pod")
	blind.Problems["event"] = fmt.Errorf("timeout")
	d2.Commit(d2.Plan(blind, t0.Add(time.Minute), false))
	if p := d2.Plan(s2, t0.Add(2*time.Minute), false); len(p.Events) != 0 {
		t.Fatalf("events remembered through an outage were sent again: %+v", p.Events)
	}
}

// ---- cutting a plan into batches ----

func many(n int) []wire.Object {
	out := make([]wire.Object, n)
	for i := range out {
		out[i] = obj("pod", fmt.Sprintf("p%05d", i), fmt.Sprintf("web-%d", i), map[string]any{"phase": "Running"})
	}
	return out
}

func TestAnEmptyPlanIsAHeartbeat(t *testing.T) {
	bs := (&Plan{}).Batches("gke", t0)
	if len(bs) != 1 || len(bs[0].Objects)+len(bs[0].Deleted)+len(bs[0].Events)+len(bs[0].Sweeps) != 0 || bs[0].Cluster != "gke" || !bs[0].CollectedAt.Equal(t0) || bs[0].Version != wire.Version {
		t.Fatalf("%+v", bs)
	}
}

func TestBatchesAreWithinTheServicesLimitsAndSweepsComeLast(t *testing.T) {
	p := &Plan{Objects: many(1200), Sweeps: []wire.Sweep{{Kind: "pod", Before: t0}}, Full: true}
	bs := p.Batches("gke", t0)
	if len(bs) != 3 || len(bs[0].Objects) != 500 || len(bs[1].Objects) != 500 || len(bs[2].Objects) != 200 {
		t.Fatalf("%d batches", len(bs))
	}
	for i, b := range bs {
		if !b.CollectedAt.Equal(t0) {
			t.Errorf("batch %d is stamped %v: all of a look must carry the time of the look, which is what a sweep is measured against", i, b.CollectedAt)
		}
		if (len(b.Sweeps) > 0) != (i == len(bs)-1) {
			t.Errorf("batch %d has %d sweeps: they ride on the last batch, after every object", i, len(b.Sweeps))
		}
	}
	del := &Plan{}
	for i := 0; i < 2500; i++ {
		del.Deleted = append(del.Deleted, wire.Ref{Kind: "pod", UID: fmt.Sprintf("u%d", i)})
	}
	for i := 0; i < 1100; i++ {
		del.Events = append(del.Events, ev(fmt.Sprintf("e%d", i), 1, t0))
	}
	bs = del.Batches("gke", t0)
	for _, b := range bs {
		if len(b.Deleted) > wire.MaxDeleted || len(b.Events) > wire.MaxEvents || len(b.Objects) > wire.MaxObjects {
			t.Fatalf("a batch is over the limits: %d %d %d", len(b.Objects), len(b.Deleted), len(b.Events))
		}
	}
	var nd, ne int
	for _, b := range bs {
		nd, ne = nd+len(b.Deleted), ne+len(b.Events)
	}
	if nd != 2500 || ne != 1100 {
		t.Fatalf("%d deleted and %d events in the batches", nd, ne)
	}
}

func TestBatchesAreCutBySizeToo(t *testing.T) {
	var objs []wire.Object
	for i := 0; i < 450; i++ {
		objs = append(objs, obj("pod", fmt.Sprintf("p%d", i), fmt.Sprintf("w%d", i), map[string]any{"phase": strings.Repeat("x", 15000)}))
	}
	bs := (&Plan{Objects: objs}).Batches("gke", t0) // 6.8 MB: under 500 objects, over the size a batch may be
	if len(bs) < 2 {
		t.Fatalf("%d batches for 6.8 MB", len(bs))
	}
	n := 0
	for _, b := range bs {
		n += len(b.Objects)
	}
	if n != 450 {
		t.Fatalf("%d objects in the batches", n)
	}
}

func TestSweepsAloneAreOneBatch(t *testing.T) {
	bs := (&Plan{Sweeps: []wire.Sweep{{Kind: "pod", Before: t0}}, Full: true}).Batches("gke", t0)
	if len(bs) != 1 || len(bs[0].Sweeps) != 1 {
		t.Fatalf("%+v", bs)
	}
	// and when the last batch is full of deletions, the sweeps still go with the last
	p := &Plan{Sweeps: []wire.Sweep{{Kind: "pod", Before: t0}}}
	for i := 0; i < wire.MaxDeleted; i++ {
		p.Deleted = append(p.Deleted, wire.Ref{Kind: "pod", UID: fmt.Sprintf("u%d", i)})
	}
	bs = p.Batches("gke", t0)
	if len(bs) != 1 || len(bs[0].Sweeps) != 1 || len(bs[0].Deleted) != wire.MaxDeleted {
		t.Fatalf("%d batches", len(bs))
	}
}
