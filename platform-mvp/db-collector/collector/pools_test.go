package collector

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func counters(db string, xacts, queries, recv, sent, xms, qms, wms float64) map[string]map[string]float64 {
	return map[string]map[string]float64{db: {"xacts": xacts, "queries": queries, "bytes_in": recv, "bytes_out": sent, "xact_ms": xms, "query_ms": qms, "wait_ms": wms}}
}

func TestThePoolsAreReportedAtOnceAndTheCountersStartFromABaseline(t *testing.T) {
	var d PoolDiffer
	first, info := d.Diff(PoolSnapshot{Pools: []Pool{{Database: "shop", User: "app", ClActive: 2, ClWaiting: 5, MaxWaitMs: 1500}}, Counters: counters("shop", 1e6, 2e6, 9e9, 9e9, 9e9, 9e9, 9e9)})
	if !info.Primed || len(first.Stats) != 0 {
		t.Fatalf("the counters since the pooler started are not one interval: %+v %+v", first.Stats, info)
	}
	if len(first.Pools) != 1 || first.Waiting != 5 || first.Clients != 7 || first.MaxWaitMs != 1500 {
		t.Fatalf("the pools are gauges and are reported as they are: %+v", first)
	}
	second, _ := d.Diff(PoolSnapshot{Counters: counters("shop", 1e6+10, 2e6+30, 9e9+1000, 9e9+5000, 9e9+200, 9e9+300, 9e9+4000)})
	if len(second.Stats) != 1 {
		t.Fatalf("%+v", second.Stats)
	}
	s := second.Stats[0]
	if s.Database != "shop" || s.Xacts != 10 || s.Queries != 30 || s.BytesIn != 1000 || s.BytesOut != 5000 || s.XactMs != 200 || s.QueryMs != 300 || s.WaitMs != 4000 {
		t.Fatalf("%+v", s)
	}
}

func TestADatabaseThatSawNothingIsLeftOut(t *testing.T) {
	var d PoolDiffer
	snap := map[string]map[string]float64{"a": {"xacts": 5, "queries": 5}, "b": {"xacts": 9, "queries": 9}}
	d.Diff(PoolSnapshot{Counters: snap})
	out, info := d.Diff(PoolSnapshot{Counters: map[string]map[string]float64{"a": {"xacts": 5, "queries": 5}, "b": {"xacts": 12, "queries": 13}}})
	if len(out.Stats) != 1 || out.Stats[0].Database != "b" || out.Stats[0].Xacts != 3 || out.Stats[0].Queries != 4 || info.Changed != 1 {
		t.Fatalf("%+v %+v", out.Stats, info)
	}
}

func TestACounterThatGoesDownIsARestartOfThePooler(t *testing.T) {
	var d PoolDiffer
	d.Diff(PoolSnapshot{Counters: counters("shop", 1000, 5000, 1e6, 1e6, 1e6, 1e6, 1e6)})
	out, info := d.Diff(PoolSnapshot{Counters: counters("shop", 7, 20, 900, 800, 700, 600, 500)})
	if len(out.Stats) != 1 || out.Stats[0].Xacts != 7 || out.Stats[0].Queries != 20 || out.Stats[0].WaitMs != 500 || info.Resets != 1 {
		t.Fatalf("what it has counted since: %+v %+v", out.Stats, info)
	}
	// and the next interval is ordinary again
	out, _ = d.Diff(PoolSnapshot{Counters: counters("shop", 10, 25, 900, 800, 700, 600, 500)})
	if len(out.Stats) != 1 || out.Stats[0].Xacts != 3 || out.Stats[0].Queries != 5 {
		t.Fatalf("%+v", out.Stats)
	}
}

func TestANewDatabaseCountsFromZero(t *testing.T) {
	var d PoolDiffer
	d.Diff(PoolSnapshot{Counters: counters("a", 5, 5, 0, 0, 0, 0, 0)})
	both := counters("a", 5, 5, 0, 0, 0, 0, 0)
	both["b"] = map[string]float64{"xacts": 4, "queries": 8}
	out, _ := d.Diff(PoolSnapshot{Counters: both})
	if len(out.Stats) != 1 || out.Stats[0].Database != "b" || out.Stats[0].Xacts != 4 || out.Stats[0].Queries != 8 {
		t.Fatalf("%+v", out.Stats)
	}
}

func TestPoolsWithWaitingClientsComeFirstAndTheTotalsCoverThePoolsThatAreCut(t *testing.T) {
	var d PoolDiffer
	var pools []Pool
	for i := 0; i < MaxPools+40; i++ {
		pools = append(pools, Pool{Database: fmt.Sprintf("db%04d", i), User: "u", SvActive: 1, ClActive: 1})
	}
	pools = append(pools, Pool{Database: "zz-last-alphabetically", User: "u", ClWaiting: 9, MaxWaitMs: 2500, SvActive: 0})
	out, _ := d.Diff(PoolSnapshot{Pools: pools})
	if len(out.Pools) != MaxPools || out.Dropped != 41 {
		t.Fatalf("%d pools, %d dropped", len(out.Pools), out.Dropped)
	}
	if out.Pools[0].Database != "zz-last-alphabetically" {
		t.Fatalf("the pool where clients wait is the one that must not be cut: %+v", out.Pools[0])
	}
	if out.Waiting != 9 || out.MaxWaitMs != 2500 || out.Clients != MaxPools+40+9 {
		t.Fatalf("the totals are of every pool: %+v", out)
	}
}

func TestNoPoolsIsAnEmptyListNotNull(t *testing.T) {
	var d PoolDiffer
	out, _ := d.Diff(PoolSnapshot{})
	if out.Pools == nil || len(out.Pools) != 0 {
		t.Fatalf("%+v", out)
	}
}

func TestPoolDifferDoesNotShareMapsWithItsInput(t *testing.T) {
	var d PoolDiffer
	in := counters("a", 5, 5, 0, 0, 0, 0, 0)
	d.Diff(PoolSnapshot{Counters: in})
	in["a"]["xacts"] = 1000000 // the engine reuses its map
	out, _ := d.Diff(PoolSnapshot{Counters: counters("a", 8, 8, 0, 0, 0, 0, 0)})
	if len(out.Stats) != 1 || out.Stats[0].Xacts != 3 {
		t.Fatalf("%+v", out.Stats)
	}
}

// ---- engine signals ----

type signalEngine struct {
	name string
	sig  Signals
}

func (e signalEngine) Name() string                                     { return e.name }
func (e signalEngine) Signals() Signals                                 { return e.sig }
func (signalEngine) Open(context.Context, TargetConfig) (Source, error) { return nil, nil }
func (signalEngine) SlowLog(TargetConfig) (SlowLogSource, error)        { return nil, nil }

func TestSupportedSignals(t *testing.T) {
	Register(signalEngine{"signals-test-pooler", Signals{Pools: true}})
	s, known := SupportedSignals("signals-test-pooler")
	if !known || !s.Pools || s.Statements || s.Activity || s.SlowLog {
		t.Fatalf("%+v %v", s, known)
	}
	if _, known := SupportedSignals("signals-test-none"); known {
		t.Fatal("an engine that is not registered is not known")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("registering a name twice must be refused")
			}
		}()
		Register(signalEngine{"signals-test-pooler", Signals{}})
	}()
}

// ---- the runner's pools loop ----

// withSource has the runner open the given source instead of the rig's own (it runs before the runner starts).
func withSource(src Source) func(*Runner) {
	return func(r *Runner) {
		r.engineOpen = func(context.Context, TargetConfig) (Source, Engine, error) { return src, fakeEngine{}, nil }
	}
}

type fakePoolSource struct {
	fakeSource
	mu2   sync.Mutex
	snaps []PoolSnapshot
	err   error
}

func (f *fakePoolSource) PoolSnapshot(context.Context) (PoolSnapshot, error) {
	f.mu2.Lock()
	defer f.mu2.Unlock()
	if f.err != nil {
		return PoolSnapshot{}, f.err
	}
	if len(f.snaps) == 0 {
		return PoolSnapshot{}, nil
	}
	s := f.snaps[0]
	if len(f.snaps) > 1 {
		f.snaps = f.snaps[1:]
	}
	return s, nil
}

func TestTheRunnerSendsPoolsWithTheirIntervalAndSaysWhoIsWaiting(t *testing.T) {
	src := &fakePoolSource{snaps: []PoolSnapshot{
		{Pools: []Pool{{Database: "shop", User: "app", ClWaiting: 4, SvActive: 2, PoolSize: 2}}, Counters: counters("shop", 100, 100, 0, 0, 0, 0, 0)},
		{Pools: []Pool{{Database: "shop", User: "app", ClWaiting: 4, SvActive: 2, PoolSize: 2}}, Counters: counters("shop", 130, 140, 0, 0, 0, 0, 0)},
	}}
	g := start(t, TargetConfig{Pools: SignalConfig{Enabled: true, Interval: 20 * time.Millisecond}}, withSource(src))
	eventually(t, "two samples", func() bool {
		return len(g.sink.where(func(b Batch) bool { return b.Pools != nil && len(b.Pools.Stats) > 0 })) >= 1
	})
	var withStats Batch
	for _, b := range g.sink.all() {
		if b.Pools != nil && len(b.Pools.Stats) > 0 {
			withStats = b
			break
		}
	}
	if withStats.IntervalSeconds <= 0 || withStats.Pools.Stats[0].Xacts != 30 || withStats.Pools.Stats[0].Queries != 40 || withStats.Pools.Waiting != 4 {
		t.Fatalf("%+v", withStats)
	}
	if v := g.r.Metrics.Value("db_collector_pool_clients_waiting", "target", "t1"); v != 4 {
		t.Fatalf("the waiting clients are in the collector's own metrics: %v", v)
	}
	first := g.sink.where(func(b Batch) bool { return b.Pools != nil })[0]
	if first.IntervalSeconds != 0 || len(first.Pools.Pools) != 1 {
		t.Fatalf("the first sample is the pools, with no interval for the counters: %+v", first)
	}
}

func TestAPoolerThatDoesNotAnswerIsLoggedOnceAndTheOthersCarryOn(t *testing.T) {
	src := &fakePoolSource{err: fmt.Errorf("the pooler is not allowing this user")}
	g := start(t, TargetConfig{Pools: SignalConfig{Enabled: true, Interval: 5 * time.Millisecond}, Activity: SignalConfig{Enabled: true, Interval: 5 * time.Millisecond}}, withSource(src))
	eventually(t, "activity keeps going", func() bool { return len(g.sink.where(func(b Batch) bool { return b.Activity != nil })) >= 4 })
	eventually(t, "failures counted", func() bool {
		return g.r.Metrics.Value("db_collector_errors_total", "target", "t1", "signal", "pools") >= 4
	})
	if n := strings.Count(g.log.String(), "not allowing this user"); n != 1 {
		t.Fatalf("%d lines", n)
	}
}
