package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- fakes ----

type fakeSource struct {
	mu        sync.Mutex
	snaps     []Snapshot
	snapErr   error
	actErr    error
	capsErr   error
	capsCalls int
	closed    atomic.Bool
	acts      int
}

func (f *fakeSource) Close() { f.closed.Store(true) }
func (f *fakeSource) Capabilities(context.Context) (Capabilities, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.capsCalls++
	if f.capsErr != nil {
		return Capabilities{}, f.capsErr
	}
	return Capabilities{Engine: "fake", Version: "1", Statements: Capability{Available: true}}, nil
}
func (f *fakeSource) Snapshot(context.Context) (Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snapErr != nil {
		return Snapshot{}, f.snapErr
	}
	if len(f.snaps) == 0 {
		return Snapshot{Epoch: "e"}, nil
	}
	s := f.snaps[0]
	if len(f.snaps) > 1 {
		f.snaps = f.snaps[1:]
	}
	return s, nil
}
func (f *fakeSource) Sample(context.Context, int) (Activity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acts++
	if f.actErr != nil {
		return Activity{}, f.actErr
	}
	return Activity{Total: f.acts, ByState: map[string]int{"active": 1}}, nil
}
func (f *fakeSource) set(fn func(*fakeSource)) { f.mu.Lock(); fn(f); f.mu.Unlock() }
func (f *fakeSource) capsAsked() int           { f.mu.Lock(); defer f.mu.Unlock(); return f.capsCalls }

type fakeFollower struct {
	feed chan SlowQuery
	err  error
	mu   sync.Mutex
	onE  func(error)
}

func (f *fakeFollower) handler() func(error) { f.mu.Lock(); defer f.mu.Unlock(); return f.onE }

func (f *fakeFollower) Follow(ctx context.Context, emit func(SlowQuery)) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case q := <-f.feed:
			emit(q)
		}
	}
}
func (f *fakeFollower) SetOnError(fn func(error)) { f.mu.Lock(); f.onE = fn; f.mu.Unlock() }

type fakeEngine struct{ follower *fakeFollower }

func (fakeEngine) Name() string     { return "fake" }
func (fakeEngine) Signals() Signals { return Signals{Statements: true, Activity: true, SlowLog: true} }
func (fakeEngine) Open(context.Context, TargetConfig) (Source, error) {
	return nil, errors.New("not used: the runner is given its sources")
}
func (e fakeEngine) SlowLog(TargetConfig) (SlowLogSource, error) { return e.follower, nil }

type recordingSink struct {
	mu      sync.Mutex
	batches []Batch
}

func (s *recordingSink) Send(_ context.Context, b Batch) error {
	s.mu.Lock()
	s.batches = append(s.batches, b)
	s.mu.Unlock()
	return nil
}
func (s *recordingSink) all() []Batch {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Batch(nil), s.batches...)
}
func (s *recordingSink) where(f func(Batch) bool) []Batch {
	var out []Batch
	for _, b := range s.all() {
		if f(b) {
			out = append(out, b)
		}
	}
	return out
}

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *logBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(3 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type rig struct {
	r      *Runner
	src    *fakeSource
	sink   *recordingSink
	log    *logBuf
	follow *fakeFollower
	cancel context.CancelFunc
	done   chan error
}

func start(t *testing.T, tc TargetConfig, mod func(*Runner)) *rig {
	t.Helper()
	g := &rig{src: &fakeSource{}, sink: &recordingSink{}, log: &logBuf{}, follow: &fakeFollower{feed: make(chan SlowQuery, 1000)}}
	if tc.Name == "" {
		tc.Name = "t1"
	}
	tc.Engine = "fake"
	g.r = &Runner{
		Targets: []TargetConfig{tc}, Sink: g.sink, Metrics: NewMetrics(),
		Log:       slog.New(slog.NewTextHandler(g.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
		CapsEvery: time.Hour, CapsRetry: 20 * time.Millisecond, SlowFlush: 15 * time.Millisecond,
	}
	g.r.engineOpen = func(context.Context, TargetConfig) (Source, Engine, error) { return g.src, fakeEngine{g.follow}, nil }
	if mod != nil {
		mod(g.r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	g.cancel = cancel
	g.done = make(chan error, 1)
	go func() { g.done <- g.r.Run(ctx) }()
	t.Cleanup(func() { g.cancel(); <-g.done }) // g's, not this function's: a restart replaces them
	return g
}

func (g *rig) stop(t *testing.T) {
	t.Helper()
	g.cancel()
	select {
	case err := <-g.done:
		if err != nil {
			t.Fatal(err)
		}
		g.done <- nil
	case <-time.After(3 * time.Second):
		t.Fatal("did not stop")
	}
}

func cum(key string, calls uint64, ms float64) Cumulative {
	return Cumulative{Key: key, QueryID: key, Query: "SELECT " + key, Calls: calls, TotalMs: ms, Rows: calls}
}

// ---- tests ----

func TestRunnerCollectsEveryKindAndLabelsTheBatches(t *testing.T) {
	g := start(t, TargetConfig{Statements: SignalConfig{Enabled: true, Interval: 20 * time.Millisecond}, Activity: SignalConfig{Enabled: true, Interval: 20 * time.Millisecond}}, nil)
	g.src.set(func(f *fakeSource) {
		f.snaps = []Snapshot{{Epoch: "e", Stats: []Cumulative{cum("a", 10, 100)}}, {Epoch: "e", Stats: []Cumulative{cum("a", 15, 160)}}}
	})
	eventually(t, "capabilities", func() bool { return len(g.sink.where(func(b Batch) bool { return b.Capabilities != nil })) >= 1 })
	eventually(t, "statements", func() bool { return len(g.sink.where(func(b Batch) bool { return len(b.Statements) > 0 })) >= 1 })
	eventually(t, "activity", func() bool { return len(g.sink.where(func(b Batch) bool { return b.Activity != nil })) >= 2 })

	st := g.sink.where(func(b Batch) bool { return len(b.Statements) > 0 })[0]
	if st.Version != BatchVersion || st.Target != "t1" || st.Engine != "fake" || st.CollectedAt.IsZero() {
		t.Fatalf("%+v", st)
	}
	if s := st.Statements[0]; s.Calls != 5 || s.TotalMs != 60 {
		t.Fatalf("the first snapshot is a baseline; the next is the interval: %+v", s)
	}
	if st.IntervalSeconds <= 0 {
		t.Fatalf("interval %v", st.IntervalSeconds)
	}
	for _, b := range g.sink.all() {
		if b.Empty() {
			t.Fatal("an empty batch was sent")
		}
	}
	if g.r.Metrics.Value("db_collector_target_up", "target", "t1") != 1 {
		t.Fatal("target_up")
	}
}

func TestTheFirstStatementsSnapshotSendsNothing(t *testing.T) {
	g := start(t, TargetConfig{Statements: SignalConfig{Enabled: true, Interval: time.Hour}}, nil)
	g.src.set(func(f *fakeSource) { f.snaps = []Snapshot{{Epoch: "e", Stats: []Cumulative{cum("a", 1000000, 9e9)}}} })
	eventually(t, "the scrape", func() bool {
		return g.r.Metrics.Value("db_collector_scrapes_total", "target", "t1", "signal", "statements") >= 1
	})
	if n := len(g.sink.where(func(b Batch) bool { return len(b.Statements) > 0 })); n != 0 {
		t.Fatalf("days of work sent as an interval: %d batches", n)
	}
}

func TestADisabledKindIsNeverAsked(t *testing.T) {
	g := start(t, TargetConfig{Statements: SignalConfig{Enabled: true, Interval: 10 * time.Millisecond}}, nil) // activity off
	time.Sleep(80 * time.Millisecond)
	g.src.mu.Lock()
	acts := g.src.acts
	g.src.mu.Unlock()
	if acts != 0 {
		t.Fatalf("activity was sampled %d times", acts)
	}
}

func TestOneKindFailingDoesNotStopTheOthersAndIsLoggedOnce(t *testing.T) {
	g := start(t, TargetConfig{Statements: SignalConfig{Enabled: true, Interval: 5 * time.Millisecond}, Activity: SignalConfig{Enabled: true, Interval: 5 * time.Millisecond}}, nil)
	g.src.set(func(f *fakeSource) { f.snapErr = errors.New("permission denied for view pg_stat_statements") })
	eventually(t, "activity keeps working", func() bool { return len(g.sink.where(func(b Batch) bool { return b.Activity != nil })) >= 5 })
	eventually(t, "several failures counted", func() bool {
		return g.r.Metrics.Value("db_collector_errors_total", "target", "t1", "signal", "statements") >= 5
	})
	if n := strings.Count(g.log.String(), "permission denied"); n != 1 {
		t.Fatalf("the same failure was logged %d times, not once:\n%s", n, g.log.String())
	}
	// it recovers, and says so
	g.src.set(func(f *fakeSource) { f.snapErr = nil })
	eventually(t, "recovery", func() bool { return strings.Contains(g.log.String(), "collection works again") })
	// a different failure is a new line
	g.src.set(func(f *fakeSource) { f.snapErr = errors.New("connection refused") })
	eventually(t, "a new failure", func() bool { return strings.Contains(g.log.String(), "connection refused") })
}

func TestADatabaseThatIsDownIsRetriedAndMarkedDownThenUp(t *testing.T) {
	g := start(t, TargetConfig{}, nil)
	g.src.set(func(f *fakeSource) { f.capsErr = errors.New("connection refused") })
	eventually(t, "down", func() bool {
		return g.r.Metrics.Value("db_collector_target_up", "target", "t1") == 0 && g.src.capsAsked() >= 3
	})
	if len(g.sink.all()) != 0 {
		t.Fatal("something was sent for a database that did not answer: the gap in the data is the signal")
	}
	g.src.set(func(f *fakeSource) { f.capsErr = nil })
	eventually(t, "up again, soon, not after the long interval", func() bool { return g.r.Metrics.Value("db_collector_target_up", "target", "t1") == 1 })
	eventually(t, "its capabilities sent", func() bool { return len(g.sink.where(func(b Batch) bool { return b.Capabilities != nil })) == 1 })
}

func TestSlowQueriesAreBatchedAndTheOverflowIsCounted(t *testing.T) {
	g := start(t, TargetConfig{SlowLog: SlowLogConfig{Enabled: true, Path: "/x"}}, nil)
	for i := 0; i < MaxSlowQueries+30; i++ {
		g.follow.feed <- SlowQuery{Time: time.Now(), DurationMs: float64(100 + i), Kind: "statement", Query: fmt.Sprintf("SELECT %d", i)}
	}
	eventually(t, "the batch", func() bool {
		n := 0
		for _, b := range g.sink.all() {
			n += len(b.SlowQueries)
		}
		return n >= MaxSlowQueries
	})
	time.Sleep(60 * time.Millisecond)
	total, dropped := 0, 0
	for _, b := range g.sink.where(func(b Batch) bool { return len(b.SlowQueries) > 0 }) {
		if len(b.SlowQueries) > MaxSlowQueries {
			t.Fatalf("a batch of %d is over the cap", len(b.SlowQueries))
		}
		total += len(b.SlowQueries)
		dropped += b.SlowDropped
	}
	if total+dropped != MaxSlowQueries+30 {
		t.Fatalf("every slow query is either sent or counted as dropped: %d + %d", total, dropped)
	}
}

func TestSlowLogProblemsAreReportedThroughTheTargetsLog(t *testing.T) {
	g := start(t, TargetConfig{SlowLog: SlowLogConfig{Enabled: true, Path: "/x"}}, nil)
	eventually(t, "the follower is wired", func() bool { return g.follow.handler() != nil })
	g.follow.handler()(errors.New("open /x: no such file or directory"))
	eventually(t, "reported", func() bool { return strings.Contains(g.log.String(), "no such file") })
}

func TestRunRefusesAMisconfiguredTargetBeforeCollectingAnything(t *testing.T) {
	good := &fakeSource{}
	r := &Runner{Targets: []TargetConfig{{Name: "ok", Engine: "fake"}, {Name: "bad", Engine: "nosuch"}}, Sink: &recordingSink{}}
	r.engineOpen = func(_ context.Context, tc TargetConfig) (Source, Engine, error) {
		if tc.Engine == "nosuch" {
			return nil, nil, errors.New("unknown database engine")
		}
		return good, fakeEngine{}, nil
	}
	err := r.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "target bad") {
		t.Fatalf("%v", err)
	}
	if !good.closed.Load() {
		t.Fatal("the target that did open was left open")
	}
}

func TestRunStopsAndClosesItsSources(t *testing.T) {
	g := start(t, TargetConfig{Statements: SignalConfig{Enabled: true, Interval: 5 * time.Millisecond}, SlowLog: SlowLogConfig{Enabled: true, Path: "/x"}}, nil)
	time.Sleep(30 * time.Millisecond)
	g.stop(t)
	if !g.src.closed.Load() {
		t.Fatal("the source was not closed")
	}
}

// ---- the queue and the sink ----

type flakySink struct {
	mu    sync.Mutex
	fails int
	err   error
	got   []Batch
}

func (s *flakySink) Send(_ context.Context, b Batch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fails > 0 {
		s.fails--
		return s.err
	}
	s.got = append(s.got, b)
	return nil
}
func (s *flakySink) targets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, b := range s.got {
		out = append(out, b.Target)
	}
	return out
}

func newQ(sink Sink, size, retries int) (*Queue, context.CancelFunc, *Metrics) {
	m := NewMetrics()
	q := NewQueue(sink, size, retries, time.Millisecond, slog.New(slog.NewTextHandler(&logBuf{}, nil)), m)
	ctx, cancel := context.WithCancel(context.Background())
	q.Start(ctx)
	return q, cancel, m
}

func TestTheQueueDeliversInOrderAndNeverBlocksTheSender(t *testing.T) {
	sink := &flakySink{}
	q, cancel, m := newQ(sink, 100, 3)
	defer func() { cancel(); q.Wait() }()
	for i := 0; i < 20; i++ {
		_ = q.Send(context.Background(), Batch{Target: fmt.Sprint(i)})
	}
	eventually(t, "all sent", func() bool { return len(sink.targets()) == 20 })
	for i, tg := range sink.targets() {
		if tg != fmt.Sprint(i) {
			t.Fatalf("out of order: %v", sink.targets())
		}
	}
	if m.Value("db_collector_batches_sent_total", "target", "3") != 1 {
		t.Fatal("not counted")
	}
}

func TestTheQueueRetriesATransientFailure(t *testing.T) {
	sink := &flakySink{fails: 2, err: errors.New("503")}
	q, cancel, _ := newQ(sink, 10, 5)
	defer func() { cancel(); q.Wait() }()
	_ = q.Send(context.Background(), Batch{Target: "a"})
	eventually(t, "delivered after two failures", func() bool { return len(sink.targets()) == 1 })
}

func TestTheQueueGivesUpAfterTheRetriesAndMovesOn(t *testing.T) {
	sink := &flakySink{fails: 1000, err: errors.New("down")}
	q, cancel, m := newQ(sink, 10, 2)
	defer func() { cancel(); q.Wait() }()
	_ = q.Send(context.Background(), Batch{Target: "a"})
	eventually(t, "dropped", func() bool { return m.Value("db_collector_batches_dropped_total", "reason", "unreachable") == 1 })
	sink.mu.Lock()
	sink.fails = 0
	sink.mu.Unlock()
	_ = q.Send(context.Background(), Batch{Target: "b"})
	eventually(t, "the next one goes", func() bool { return len(sink.targets()) == 1 && sink.targets()[0] == "b" })
}

func TestAPermanentRefusalIsNotRetried(t *testing.T) {
	calls := 0
	var mu sync.Mutex
	sink := sinkFunc(func(context.Context, Batch) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return permanent{errors.New("403")}
	})
	q, cancel, m := newQ(sink, 10, 5)
	defer func() { cancel(); q.Wait() }()
	_ = q.Send(context.Background(), Batch{Target: "a"})
	eventually(t, "dropped", func() bool { return m.Value("db_collector_batches_dropped_total", "reason", "refused") == 1 })
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("%d attempts at something that cannot work", calls)
	}
}

type sinkFunc func(context.Context, Batch) error

func (f sinkFunc) Send(ctx context.Context, b Batch) error { return f(ctx, b) }

func TestAFullQueueDropsTheOldestNotTheNewest(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	var got []string
	var mu sync.Mutex
	sink := sinkFunc(func(_ context.Context, b Batch) error {
		<-release
		mu.Lock()
		got = append(got, b.Target)
		mu.Unlock()
		return nil
	})
	q, cancel, m := newQ(sink, 3, 0)
	defer func() { open(); cancel(); q.Wait() }()
	_ = q.Send(context.Background(), Batch{Target: "in-flight"})
	eventually(t, "taken", func() bool { return q.Len() == 0 })
	for _, x := range []string{"1", "2", "3", "4", "5"} {
		_ = q.Send(context.Background(), Batch{Target: x})
	}
	if q.Len() != 3 || m.Value("db_collector_batches_dropped_total", "reason", "queue_full") != 2 {
		t.Fatalf("%d held, %v dropped", q.Len(), m.Value("db_collector_batches_dropped_total", "reason", "queue_full"))
	}
	open()
	eventually(t, "the rest is delivered", func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 4 })
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, ",") != "in-flight,3,4,5" {
		t.Fatalf("the newest are what is kept: %v", got)
	}
}

func TestTheQueueSendsWhatItHasWhenItIsStopped(t *testing.T) {
	sink := &flakySink{}
	m := NewMetrics()
	q := NewQueue(sink, 10, 3, time.Hour, slog.New(slog.NewTextHandler(&logBuf{}, nil)), m)
	ctx, cancel := context.WithCancel(context.Background())
	q.Start(ctx)
	// stop first, then queue: the drain is what delivers it
	cancel()
	q.Wait()
	_ = q.Send(context.Background(), Batch{Target: "late"})
	q.drain()
	if len(sink.targets()) != 1 {
		t.Fatalf("%v", sink.targets())
	}
}

func TestHTTPSink(t *testing.T) {
	var gotKey, gotType string
	var got Batch
	status := 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotType = r.Header.Get("X-API-Key"), r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
		fmt.Fprint(w, `{"error":"nope"}`)
	}))
	defer srv.Close()
	s := &HTTPSink{URL: srv.URL + "/v1/db", Key: "ingest-key-0123456789"}
	b := Batch{Version: 1, Target: "orders", Engine: "postgres", Statements: []Statement{{QueryID: "1", Query: "SELECT ?", Calls: 2}}}
	if err := s.Send(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if gotKey != "ingest-key-0123456789" || gotType != "application/json" || got.Target != "orders" || got.Statements[0].Calls != 2 {
		t.Fatalf("%q %q %+v", gotKey, gotType, got)
	}
	for code, perm := range map[int]bool{400: true, 401: true, 403: true, 413: true, 422: true, 429: false, 500: false, 503: false, 408: false} {
		status = code
		err := s.Send(context.Background(), b)
		if err == nil || IsPermanent(err) != perm {
			t.Errorf("%d: %v, permanent %v want %v", code, err, IsPermanent(err), perm)
		}
		if err != nil && strings.Contains(err.Error(), "ingest-key") {
			t.Errorf("the key is in the error: %v", err)
		}
	}
}

func TestHTTPSinkErrorsNeverCarryTheKeyOrTheAddress(t *testing.T) {
	s := &HTTPSink{URL: "http://127.0.0.1:1/v1/db?token=very-secret-token", Key: "ingest-key-0123456789"}
	err := s.Send(context.Background(), Batch{Target: "x"})
	if err == nil || IsPermanent(err) {
		t.Fatalf("an unreachable service is a transient failure: %v", err)
	}
	if strings.Contains(err.Error(), "very-secret-token") || strings.Contains(err.Error(), "ingest-key") {
		t.Fatalf("%v", err)
	}
	bad := &HTTPSink{URL: "http://exa mple.com/\x7f", Key: "k"}
	if err := bad.Send(context.Background(), Batch{}); err == nil || !IsPermanent(err) {
		t.Fatalf("a bad address cannot be fixed by waiting: %v", err)
	}
}

func TestMetricsText(t *testing.T) {
	m := NewMetrics()
	m.Inc("a_total", "target", "x")
	m.Add("a_total", 2, "target", "x")
	m.Set("g", 5)
	var b bytes.Buffer
	m.Write(&b)
	if b.String() != "a_total{target=\"x\"} 3\ng 5\n" {
		t.Fatalf("%q", b.String())
	}
	if m.Value("a_total", "target", "x") != 3 || m.Value("nope") != 0 {
		t.Fatal("Value")
	}
}
