package pipeline

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/model"
)

// Writer is the storage the pipeline inserts into. The ClickHouse
// implementation is in package store; tests use a fake.
type Writer interface {
	WriteSamples(ctx context.Context, rows []model.Sample) error
	WriteSeries(ctx context.Context, rows []model.Series) error
	WriteMetricMeta(ctx context.Context, rows []model.MetricMeta) error
	WriteSpans(ctx context.Context, rows []model.Span) error
	WriteLogs(ctx context.Context, rows []model.LogRecord) error
	WriteProfileSamples(ctx context.Context, rows []model.ProfileSample) error
	WriteStacks(ctx context.Context, rows []model.Stack) error
}

type PipelineOptions struct {
	Options
	// SeriesRefresh is how often an unchanged series or stack row is written
	// again, so its retention clock keeps moving while it is still reported.
	SeriesRefresh time.Duration
}

type Pipeline struct {
	samples *Batcher[model.Sample]
	series  *Batcher[model.Series]
	meta    *Batcher[model.MetricMeta]
	spans   *Batcher[model.Span]
	logs    *Batcher[model.LogRecord]
	psamp   *Batcher[model.ProfileSample]
	stacks  *Batcher[model.Stack]

	seenSeries *seenSet
	seenStacks *seenSet
}

func New(w Writer, o PipelineOptions, log *slog.Logger) *Pipeline {
	p := &Pipeline{
		samples:    NewBatcher("metrics", o.Options, log, w.WriteSamples),
		series:     NewBatcher("metric_series", o.Options, log, w.WriteSeries),
		meta:       NewBatcher("metric_metadata", o.Options, log, w.WriteMetricMeta),
		spans:      NewBatcher("traces", o.Options, log, w.WriteSpans),
		logs:       NewBatcher("logs", o.Options, log, w.WriteLogs),
		psamp:      NewBatcher("profile_samples", o.Options, log, w.WriteProfileSamples),
		stacks:     NewBatcher("profile_stacks", o.Options, log, w.WriteStacks),
		seenSeries: newSeenSet(o.SeriesRefresh, 2_000_000),
		seenStacks: newSeenSet(o.SeriesRefresh, 2_000_000),
	}
	// A series or stack row that is given up on must be written again the
	// next time it is reported, not skipped until the refresh interval ends.
	p.series.OnDrop(func(rows []model.Series) {
		for _, r := range rows {
			p.seenSeries.Forget(r.Project, r.Hash)
		}
	})
	p.stacks.OnDrop(func(rows []model.Stack) {
		for _, r := range rows {
			p.seenStacks.Forget(r.Project, r.Hash)
		}
	})
	return p
}

// AddMetrics queues samples first: if the buffer is full the whole request
// is refused and nothing is half-accepted.
func (p *Pipeline) AddMetrics(samples []model.Sample, series []model.Series, meta []model.MetricMeta) error {
	if err := p.samples.Add(samples); err != nil {
		return err
	}
	now := time.Now()
	fresh := series[:0:0]
	for _, s := range series {
		if p.seenSeries.Fresh(s.Project, s.Hash, now) {
			fresh = append(fresh, s)
		}
	}
	p.series.Force(fresh)
	p.meta.Force(meta)
	return nil
}

func (p *Pipeline) AddSpans(rows []model.Span) error     { return p.spans.Add(rows) }
func (p *Pipeline) AddLogs(rows []model.LogRecord) error { return p.logs.Add(rows) }

func (p *Pipeline) AddProfile(samples []model.ProfileSample, stacks []model.Stack) error {
	if err := p.psamp.Add(samples); err != nil {
		return err
	}
	now := time.Now()
	fresh := stacks[:0:0]
	for _, s := range stacks {
		if p.seenStacks.Fresh(s.Project, s.Hash, now) {
			fresh = append(fresh, s)
		}
	}
	p.stacks.Force(fresh)
	return nil
}

func (p *Pipeline) Stats() map[string]Stats {
	return map[string]Stats{
		"metrics": p.samples.Stats(), "metric_series": p.series.Stats(), "metric_metadata": p.meta.Stats(),
		"traces": p.spans.Stats(), "logs": p.logs.Stats(),
		"profile_samples": p.psamp.Stats(), "profile_stacks": p.stacks.Stats(),
	}
}

// Close flushes every buffer, waiting no longer than ctx allows.
func (p *Pipeline) Close(ctx context.Context) error {
	var wg sync.WaitGroup
	errs := make(chan error, 7)
	for _, c := range []func(context.Context) error{
		p.samples.Close, p.series.Close, p.meta.Close, p.spans.Close, p.logs.Close, p.psamp.Close, p.stacks.Close,
	} {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- c(ctx) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

type seenKey struct {
	project string
	hash    uint64
}

// seenSet remembers which series and stacks were written recently, so a busy
// agent does not rewrite the same label set with every sample.
type seenSet struct {
	mu  sync.Mutex
	m   map[seenKey]time.Time
	ttl time.Duration
	max int
}

func newSeenSet(ttl time.Duration, max int) *seenSet {
	return &seenSet{m: map[seenKey]time.Time{}, ttl: ttl, max: max}
}

// Fresh reports whether the entry should be written, and marks it written.
func (s *seenSet) Fresh(project string, hash uint64, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := seenKey{project, hash}
	if at, ok := s.m[k]; ok && now.Sub(at) < s.ttl {
		return false
	}
	if len(s.m) >= s.max {
		for key, at := range s.m {
			if now.Sub(at) >= s.ttl {
				delete(s.m, key)
			}
		}
		if len(s.m) >= s.max {
			s.m = map[seenKey]time.Time{} // over budget: rewriting is cheap, unbounded memory is not
		}
	}
	s.m[k] = now
	return true
}

func (s *seenSet) Forget(project string, hash uint64) {
	s.mu.Lock()
	delete(s.m, seenKey{project, hash})
	s.mu.Unlock()
}
