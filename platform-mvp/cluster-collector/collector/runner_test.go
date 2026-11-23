package collector

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/cluster-collector/kube"
	"github.com/sojoner/kro-lab/platform-mvp/cluster-collector/wire"
)

var bg = context.Background()

type fakeReader struct {
	mu   sync.Mutex
	snap kube.Snapshot
}

func (f *fakeReader) Snapshot(context.Context) kube.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}
func (f *fakeReader) set(s kube.Snapshot) { f.mu.Lock(); f.snap = s; f.mu.Unlock() }

type recSink struct {
	mu      sync.Mutex
	batches []wire.Batch
	fail    func(n int, b wire.Batch) error // n counts every attempt
	n       int
}

func (s *recSink) Send(_ context.Context, b wire.Batch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	if s.fail != nil {
		if err := s.fail(s.n, b); err != nil {
			return err
		}
	}
	s.batches = append(s.batches, b)
	return nil
}
func (s *recSink) take() []wire.Batch {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.batches
	s.batches = nil
	return b
}

type rig struct {
	r   *Runner
	src *fakeReader
	out *recSink
	now time.Time
}

func newRig() *rig {
	g := &rig{src: &fakeReader{}, out: &recSink{}, now: t0}
	g.r = &Runner{Source: g.src, Sink: g.out, Cluster: "gke", Interval: 30 * time.Second, Resync: 10 * time.Minute,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return g.now }}
	g.src.set(snap([]wire.Object{obj("pod", "p1", "web-0", map[string]any{"phase": "Running"})}))
	return g
}

func (g *rig) step(d time.Duration) error { g.now = g.now.Add(d); return g.r.Cycle(bg) }

func TestTheFirstLookIsReportedInFullThenOnlyChangesThenAHeartbeat(t *testing.T) {
	g := newRig()
	if err := g.r.Cycle(bg); err != nil {
		t.Fatal(err)
	}
	b := g.out.take()
	if len(b) != 1 || len(b[0].Objects) != 1 || len(b[0].Sweeps) != len(wire.Kinds) || b[0].Cluster != "gke" {
		t.Fatalf("first look: %+v", b)
	}
	if err := g.step(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	if b := g.out.take(); len(b) != 1 || len(b[0].Objects)+len(b[0].Sweeps)+len(b[0].Deleted) != 0 {
		t.Fatalf("a look with nothing new must be an empty heartbeat: %+v", b)
	}
	g.src.set(snap([]wire.Object{obj("pod", "p1", "web-0", map[string]any{"phase": "Failed"})}))
	if err := g.step(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	if b := g.out.take(); len(b) != 1 || len(b[0].Objects) != 1 || b[0].Objects[0].Data["phase"] != "Failed" || len(b[0].Sweeps) != 0 {
		t.Fatalf("a change: %+v", b)
	}
	g.src.set(snap(nil))
	if err := g.step(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	if b := g.out.take(); len(b) != 1 || len(b[0].Deleted) != 1 || b[0].Deleted[0].UID != "p1" {
		t.Fatalf("a deletion: %+v", b)
	}
}

func TestEverythingIsSentAgainEveryResync(t *testing.T) {
	g := newRig()
	g.r.Cycle(bg)
	g.out.take()
	g.step(5 * time.Minute)
	if b := g.out.take(); len(b[0].Objects) != 0 {
		t.Fatalf("five minutes in, before the resync: %+v", b)
	}
	g.step(6 * time.Minute) // 11 minutes since the first
	b := g.out.take()
	if len(b) != 1 || len(b[0].Objects) != 1 || len(b[0].Sweeps) != len(wire.Kinds) {
		t.Fatalf("the resync: %+v", b)
	}
	g.step(time.Minute)
	if b := g.out.take(); len(b[0].Objects) != 0 {
		t.Fatalf("the look after a resync is not one: %+v", b)
	}
}

func TestAFailedReportIsMadeAgainInFullAndNothingIsRememberedAsSent(t *testing.T) {
	g := newRig()
	g.out.fail = func(n int, _ wire.Batch) error { return errors.New("hub down") }
	if err := g.r.Cycle(bg); err == nil {
		t.Fatal("a failed report must be an error")
	}
	if g.r.Healthy(time.Hour) {
		t.Fatal("healthy with nothing reported")
	}
	g.out.fail = nil
	if err := g.step(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	b := g.out.take()
	if len(b) != 1 || len(b[0].Objects) != 1 || len(b[0].Sweeps) != len(wire.Kinds) {
		t.Fatalf("after the outage the first look is still to be reported, in full: %+v", b)
	}
	if !g.r.Healthy(time.Minute) {
		t.Fatal("not healthy after a report")
	}
}

func TestSweepsAreNotSentWhenAnEarlierBatchDidNotGetThrough(t *testing.T) {
	g := newRig()
	g.src.set(snap(many(700)))
	// the second of two batches is the one with the sweeps; the first is refused
	g.out.fail = func(n int, b wire.Batch) error {
		if n == 1 {
			return errors.New("refused")
		}
		return nil
	}
	if err := g.r.Cycle(bg); err == nil {
		t.Fatal("expected an error")
	}
	for _, b := range g.out.take() {
		if len(b.Sweeps) > 0 {
			t.Fatal("a sweep was sent although the batch before it failed: it would delete what that batch carried")
		}
	}
}

func TestACollectorThatCannotReadAnythingSaysNothing(t *testing.T) {
	g := newRig()
	g.r.Cycle(bg)
	g.out.take()
	blind := kube.Snapshot{Listed: map[string]bool{}, Problems: map[string]error{"pod": errors.New("forbidden"), "node": errors.New("forbidden")}}
	g.src.set(blind)
	err := g.step(time.Minute)
	if err == nil {
		t.Fatal("blind but no error")
	}
	if b := g.out.take(); len(b) != 0 {
		t.Fatalf("a blind collector reported: %+v (an empty report would say everything is gone, a heartbeat that all is well)", b)
	}
	if g.r.Healthy(30 * time.Second) {
		t.Fatal("a blind collector is not ready")
	}
}

func TestAPermanentRefusalIsSeenByTheCaller(t *testing.T) {
	g := newRig()
	g.out.fail = func(int, wire.Batch) error { return Permanent{errors.New("refused (400)")} }
	err := g.r.Cycle(bg)
	if !IsPermanent(err) {
		t.Fatalf("%v", err)
	}
}

func TestLastReport(t *testing.T) {
	g := newRig()
	g.r.Cycle(bg)
	r := g.r.Last()
	if !r.Full || r.Objects != 1 || r.Batches != 1 || !r.At.Equal(t0) {
		t.Fatalf("%+v", r)
	}
	g.src.set(kube.Snapshot{Objects: []wire.Object{obj("pod", "p1", "a", nil)}, Listed: listed("pod"), Problems: map[string]error{}, Skipped: 3})
	g.step(time.Minute)
	if g.r.Last().Skipped != 3 {
		t.Fatalf("%+v", g.r.Last())
	}
}

func TestRunStopsWhenCancelled(t *testing.T) {
	g := newRig()
	g.r.Interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { g.r.Run(ctx); close(done) }()
	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
	if len(g.out.take()) < 2 {
		t.Fatal("Run did not look repeatedly")
	}
}
