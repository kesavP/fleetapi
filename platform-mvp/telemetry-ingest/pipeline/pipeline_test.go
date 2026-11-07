package pipeline

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/model"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func opts() Options {
	return Options{BatchRows: 3, MaxBuffered: 5, FlushInterval: 20 * time.Millisecond, Retries: 2, Backoff: time.Millisecond}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestFlushesOnSizeAndOnInterval(t *testing.T) {
	var mu sync.Mutex
	var batches []int
	b := NewBatcher("t", Options{BatchRows: 3, MaxBuffered: 100, FlushInterval: time.Hour}, quiet,
		func(_ context.Context, rows []int) error {
			mu.Lock()
			batches = append(batches, len(rows))
			mu.Unlock()
			return nil
		})
	defer b.Close(context.Background())
	if err := b.Add([]int{1, 2, 3, 4}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "size flush", func() bool { mu.Lock(); defer mu.Unlock(); return len(batches) == 1 })
	mu.Lock()
	if batches[0] != 3 {
		t.Fatalf("batch of %d, want 3", batches[0])
	}
	mu.Unlock()

	b2 := NewBatcher("t2", Options{BatchRows: 100, MaxBuffered: 100, FlushInterval: 10 * time.Millisecond}, quiet,
		func(_ context.Context, rows []int) error {
			mu.Lock()
			batches = append(batches, len(rows))
			mu.Unlock()
			return nil
		})
	defer b2.Close(context.Background())
	_ = b2.Add([]int{1})
	waitFor(t, "interval flush", func() bool { return b2.Stats().Written == 1 })
}

func TestBackpressure(t *testing.T) {
	block := make(chan struct{})
	b := NewBatcher("t", opts(), quiet, func(ctx context.Context, rows []int) error { <-block; return nil })
	defer func() { close(block); _ = b.Close(context.Background()) }()
	if err := b.Add([]int{1, 2, 3}); err != nil { // fills a batch; the flush then blocks
		t.Fatal(err)
	}
	waitFor(t, "buffer drained into the blocked flush", func() bool { return b.Stats().Buffered == 0 })
	if err := b.Add([]int{1, 2, 3, 4}); err != nil {
		t.Fatal(err)
	}
	if err := b.Add([]int{9, 9}); !errors.Is(err, ErrOverloaded) {
		t.Fatalf("want ErrOverloaded, got %v", err)
	}
	b.Force([]int{9, 9}) // side tables are not refused
	if got := b.Stats().Buffered; got != 6 {
		t.Fatalf("buffered %d, want 6", got)
	}
}

func TestOversizedRequestIsAcceptedWhenEmpty(t *testing.T) {
	b := NewBatcher("t", opts(), quiet, func(context.Context, []int) error { return nil })
	defer b.Close(context.Background())
	if err := b.Add(make([]int, 50)); err != nil {
		t.Fatalf("a request larger than the buffer must not be refused forever: %v", err)
	}
}

func TestRetryThenSucceed(t *testing.T) {
	var calls atomic.Int32
	b := NewBatcher("t", opts(), quiet, func(context.Context, []int) error {
		if calls.Add(1) < 3 {
			return errors.New("down")
		}
		return nil
	})
	defer b.Close(context.Background())
	_ = b.Add([]int{1, 2, 3})
	waitFor(t, "write after retries", func() bool { return b.Stats().Written == 3 })
	if s := b.Stats(); s.Dropped != 0 || s.FlushErrors != 2 {
		t.Fatalf("stats: %+v", s)
	}
}

func TestDropAfterRetriesCallsOnDrop(t *testing.T) {
	b := NewBatcher("t", opts(), quiet, func(context.Context, []int) error { return errors.New("down") })
	var dropped atomic.Int32
	b.OnDrop(func(rows []int) { dropped.Add(int32(len(rows))) })
	defer b.Close(context.Background())
	_ = b.Add([]int{1, 2, 3})
	waitFor(t, "drop", func() bool { return dropped.Load() == 3 })
	if s := b.Stats(); s.Dropped != 3 || s.FlushErrors != 3 {
		t.Fatalf("stats: %+v", s)
	}
}

func TestCloseFlushes(t *testing.T) {
	var got atomic.Int32
	b := NewBatcher("t", Options{BatchRows: 100, MaxBuffered: 100, FlushInterval: time.Hour}, quiet,
		func(_ context.Context, rows []int) error { got.Add(int32(len(rows))); return nil })
	_ = b.Add([]int{1, 2})
	if err := b.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got.Load() != 2 {
		t.Fatalf("flushed %d on close, want 2", got.Load())
	}
	if err := b.Close(context.Background()); err != nil { // idempotent
		t.Fatal(err)
	}
}

type fakeWriter struct {
	mu      sync.Mutex
	samples []model.Sample
	series  []model.Series
	stacks  []model.Stack
	fail    atomic.Bool
}

func (f *fakeWriter) err() error {
	if f.fail.Load() {
		return errors.New("down")
	}
	return nil
}
func (f *fakeWriter) WriteSamples(_ context.Context, r []model.Sample) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.err(); err != nil {
		return err
	}
	f.samples = append(f.samples, r...)
	return nil
}
func (f *fakeWriter) WriteSeries(_ context.Context, r []model.Series) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.err(); err != nil {
		return err
	}
	f.series = append(f.series, r...)
	return nil
}
func (f *fakeWriter) WriteStacks(_ context.Context, r []model.Stack) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.err(); err != nil {
		return err
	}
	f.stacks = append(f.stacks, r...)
	return nil
}
func (f *fakeWriter) WriteMetricMeta(context.Context, []model.MetricMeta) error { return f.err() }
func (f *fakeWriter) WriteSpans(context.Context, []model.Span) error            { return f.err() }
func (f *fakeWriter) WriteLogs(context.Context, []model.LogRecord) error        { return f.err() }
func (f *fakeWriter) WriteProfileSamples(context.Context, []model.ProfileSample) error {
	return f.err()
}

func newPipe(w Writer) *Pipeline {
	return New(w, PipelineOptions{Options: Options{BatchRows: 1000, MaxBuffered: 5000, FlushInterval: 10 * time.Millisecond, Retries: 1, Backoff: time.Millisecond}, SeriesRefresh: time.Hour}, quiet)
}

func TestSeriesWrittenOncePerRefresh(t *testing.T) {
	w := &fakeWriter{}
	p := newPipe(w)
	defer p.Close(context.Background())
	s := []model.Sample{{Project: "us", Name: "m", Hash: 7}}
	ser := []model.Series{{Project: "us", Name: "m", Hash: 7}}
	for i := 0; i < 3; i++ {
		if err := p.AddMetrics(s, ser, nil); err != nil {
			t.Fatal(err)
		}
	}
	other := []model.Series{{Project: "eu", Name: "m", Hash: 7}} // same hash, other project
	_ = p.AddMetrics([]model.Sample{{Project: "eu", Name: "m", Hash: 7}}, other, nil)
	waitFor(t, "writes", func() bool { w.mu.Lock(); defer w.mu.Unlock(); return len(w.samples) == 4 && len(w.series) == 2 })
}

func TestDroppedSeriesIsWrittenAgain(t *testing.T) {
	w := &fakeWriter{}
	w.fail.Store(true)
	p := newPipe(w)
	defer p.Close(context.Background())
	ser := []model.Series{{Project: "us", Name: "m", Hash: 1}}
	_ = p.AddMetrics(nil, ser, nil)
	waitFor(t, "drop", func() bool { return p.Stats()["metric_series"].Dropped == 1 })
	w.fail.Store(false)
	_ = p.AddMetrics(nil, ser, nil)
	waitFor(t, "rewrite", func() bool { w.mu.Lock(); defer w.mu.Unlock(); return len(w.series) == 1 })
}

func TestRefusedRequestLeavesNoSeriesBehind(t *testing.T) {
	w := &fakeWriter{}
	w.fail.Store(true)
	o := PipelineOptions{Options: Options{BatchRows: 2, MaxBuffered: 3, FlushInterval: time.Hour, Retries: 0, Backoff: time.Hour}, SeriesRefresh: time.Hour}
	o.FlushTimeout = time.Second
	p := New(w, o, quiet)
	defer p.Close(context.Background())
	_ = p.AddMetrics([]model.Sample{{}, {}}, nil, nil) // fills the buffer; the flush fails
	waitFor(t, "first flush attempt", func() bool { return p.Stats()["metrics"].FlushErrors >= 1 })
	_ = p.AddMetrics([]model.Sample{{}, {}, {}}, nil, nil)
	err := p.AddMetrics([]model.Sample{{}, {}, {}}, []model.Series{{Project: "us", Hash: 9}}, nil)
	if !errors.Is(err, ErrOverloaded) {
		t.Fatalf("want ErrOverloaded, got %v", err)
	}
	if !p.seenSeries.Fresh("us", 9, time.Now()) {
		t.Fatal("series of a refused request was marked as written")
	}
}
