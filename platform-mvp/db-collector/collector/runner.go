package collector

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Defaults for how often each kind of data is collected.
const (
	DefaultStatementsEvery = 60 * time.Second
	DefaultActivityEvery   = 10 * time.Second
	DefaultPoolsEvery      = 10 * time.Second
	DefaultCapsEvery       = 10 * time.Minute
	DefaultSlowFlush       = 5 * time.Second
)

// Runner collects from every target, each in its own goroutines, and gives what it finds to the sink. One target failing, or one kind of data
// failing, never stops another: a database that is down is a log line, a counter and a gap in the data, and it is tried again.
type Runner struct {
	Targets []TargetConfig
	Sink    Sink
	Log     *slog.Logger
	Metrics *Metrics
	Now     func() time.Time

	CapsEvery  time.Duration
	SlowFlush  time.Duration
	CapsRetry  time.Duration // how soon to look again after the server could not be asked; default 15 s
	engineOpen func(ctx context.Context, t TargetConfig) (Source, Engine, error)
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) open(ctx context.Context, t TargetConfig) (Source, Engine, error) {
	if r.engineOpen != nil {
		return r.engineOpen(ctx, t)
	}
	e, err := Lookup(t.Engine)
	if err != nil {
		return nil, nil, err
	}
	s, err := e.Open(ctx, t)
	return s, e, err
}

// Run starts every target and returns when ctx ends. It returns an error at once, before collecting anything, if a target cannot be set up
// (an engine that does not exist, a connection string that is not valid): that is a mistake in the configuration, not a database being down.
func (r *Runner) Run(ctx context.Context) error {
	if r.Log == nil {
		r.Log = slog.Default()
	}
	if r.Metrics == nil {
		r.Metrics = NewMetrics()
	}
	type opened struct {
		t   TargetConfig
		src Source
		eng Engine
	}
	var all []opened
	closeAll := func() {
		for _, o := range all {
			o.src.Close()
		}
	}
	for _, t := range r.Targets {
		src, eng, err := r.open(ctx, t)
		if err != nil {
			closeAll()
			return fmt.Errorf("target %s: %w", t.Name, err)
		}
		all = append(all, opened{t, src, eng})
	}
	var wg sync.WaitGroup
	for _, o := range all {
		w := &worker{r: r, t: o.t, src: o.src, eng: o.eng, log: r.Log.With("target", o.t.Name), throttle: map[string]throttled{}}
		wg.Add(1)
		go func() { defer wg.Done(); w.run(ctx) }()
	}
	wg.Wait()
	closeAll()
	return nil
}

type throttled struct {
	msg string
	at  time.Time
}

type worker struct {
	r   *Runner
	t   TargetConfig
	src Source
	eng Engine
	log *slog.Logger

	tmu      sync.Mutex
	throttle map[string]throttled
}

func (w *worker) send(ctx context.Context, b Batch) {
	b.Version, b.Target, b.Engine = BatchVersion, w.t.Name, w.t.Engine
	if b.CollectedAt.IsZero() {
		b.CollectedAt = w.r.now().UTC()
	}
	if b.Empty() {
		return
	}
	_ = w.r.Sink.Send(ctx, b)
}

// problem logs an error the first time, when it changes, and every ten minutes while it lasts: a database that is down for a day is one
// line and a counter, not eight thousand lines.
func (w *worker) problem(kind string, err error) {
	w.r.Metrics.Inc("db_collector_errors_total", "target", w.t.Name, "signal", kind)
	w.tmu.Lock()
	defer w.tmu.Unlock()
	t, seen := w.throttle[kind]
	now := w.r.now()
	if seen && t.msg == err.Error() && now.Sub(t.at) < 10*time.Minute {
		return
	}
	w.throttle[kind] = throttled{err.Error(), now}
	w.log.Warn("collection failed", "signal", kind, "error", err)
}

func (w *worker) recovered(kind string) {
	w.tmu.Lock()
	defer w.tmu.Unlock()
	if _, was := w.throttle[kind]; was {
		delete(w.throttle, kind)
		w.log.Info("collection works again", "signal", kind)
	}
}

func every(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

func (w *worker) run(ctx context.Context) {
	var wg sync.WaitGroup
	start := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	start(func() { w.capabilities(ctx) })
	if ss, ok := w.src.(StatementSource); ok && w.t.Statements.Enabled {
		start(func() { w.statements(ctx, ss) })
	}
	if as, ok := w.src.(ActivitySource); ok && w.t.Activity.Enabled {
		start(func() { w.activity(ctx, as) })
	}
	if ps, ok := w.src.(PoolSource); ok && w.t.Pools.Enabled {
		start(func() { w.pools(ctx, ps) })
	}
	if w.t.SlowLog.Enabled {
		start(func() { w.slowLog(ctx) })
	}
	wg.Wait()
}

func (w *worker) capabilities(ctx context.Context) {
	period := every(w.r.CapsEvery, DefaultCapsEvery)
	retry := every(w.r.CapsRetry, 15*time.Second)
	for {
		caps, err := w.src.Capabilities(ctx)
		next := period
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			w.problem("capabilities", err)
			w.r.Metrics.Set("db_collector_target_up", 0, "target", w.t.Name)
			next = retry
		} else {
			w.recovered("capabilities")
			w.r.Metrics.Set("db_collector_target_up", 1, "target", w.t.Name)
			w.send(ctx, Batch{Capabilities: &caps})
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(next):
		}
	}
}

func (w *worker) statements(ctx context.Context, src StatementSource) {
	d := &Differ{Top: w.t.Statements.Top}
	every := every(w.t.Statements.Interval, DefaultStatementsEvery)
	var last time.Time
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		snap, err := src.Snapshot(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			w.problem("statements", err)
		} else {
			w.recovered("statements")
			now := w.r.now()
			out, info := d.Diff(snap)
			w.r.Metrics.Set("db_collector_statements_tracked", float64(info.Seen), "target", w.t.Name)
			if info.Resets > 0 {
				w.r.Metrics.Add("db_collector_statement_resets_total", float64(info.Resets), "target", w.t.Name)
			}
			if !info.Primed {
				w.send(ctx, Batch{CollectedAt: now.UTC(), IntervalSeconds: now.Sub(last).Seconds(), Statements: out})
			}
			last = now
			w.r.Metrics.Inc("db_collector_scrapes_total", "target", w.t.Name, "signal", "statements")
			w.r.Metrics.Set("db_collector_last_scrape_timestamp_seconds", float64(now.Unix()), "target", w.t.Name, "signal", "statements")
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (w *worker) activity(ctx context.Context, src ActivitySource) {
	tick := time.NewTicker(every(w.t.Activity.Interval, DefaultActivityEvery))
	defer tick.Stop()
	for {
		a, err := src.Sample(ctx, w.t.Activity.Top)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			w.problem("activity", err)
		} else {
			w.recovered("activity")
			w.send(ctx, Batch{Activity: &a})
			w.r.Metrics.Inc("db_collector_scrapes_total", "target", w.t.Name, "signal", "activity")
			w.r.Metrics.Set("db_collector_last_scrape_timestamp_seconds", float64(w.r.now().Unix()), "target", w.t.Name, "signal", "activity")
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (w *worker) pools(ctx context.Context, src PoolSource) {
	var d PoolDiffer
	tick := time.NewTicker(every(w.t.Pools.Interval, DefaultPoolsEvery))
	defer tick.Stop()
	var last time.Time
	for {
		snap, err := src.PoolSnapshot(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			w.problem("pools", err)
		} else {
			w.recovered("pools")
			now := w.r.now()
			p, info := d.Diff(snap)
			if info.Resets > 0 {
				w.r.Metrics.Add("db_collector_statement_resets_total", float64(info.Resets), "target", w.t.Name)
			}
			w.r.Metrics.Set("db_collector_pool_clients_waiting", float64(p.Waiting), "target", w.t.Name)
			b := Batch{CollectedAt: now.UTC(), Pools: &p}
			if !last.IsZero() {
				b.IntervalSeconds = now.Sub(last).Seconds()
			}
			w.send(ctx, b)
			last = now
			w.r.Metrics.Inc("db_collector_scrapes_total", "target", w.t.Name, "signal", "pools")
			w.r.Metrics.Set("db_collector_last_scrape_timestamp_seconds", float64(now.Unix()), "target", w.t.Name, "signal", "pools")
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// slowLog follows the log and sends what it finds every SlowFlush: slow statements can come in bursts, and one batch of a few hundred is
// better than a request each. Over the cap of a batch, the rest are counted and dropped.
func (w *worker) slowLog(ctx context.Context) {
	flush := every(w.r.SlowFlush, DefaultSlowFlush)
	for ctx.Err() == nil {
		follower, err := w.eng.SlowLog(w.t)
		if err != nil {
			w.problem("slowlog", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Minute):
				continue
			}
		}
		if h, ok := follower.(interface{ SetOnError(func(error)) }); ok {
			h.SetOnError(func(e error) { w.problem("slowlog", e) })
		}
		var mu sync.Mutex
		var pending []SlowQuery
		dropped := 0
		emit := func(q SlowQuery) {
			mu.Lock()
			defer mu.Unlock()
			if len(pending) >= MaxSlowQueries {
				dropped++
				return
			}
			pending = append(pending, q)
		}
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			tick := time.NewTicker(flush)
			defer tick.Stop()
			send := func() {
				mu.Lock()
				p, d := pending, dropped
				pending, dropped = nil, 0
				mu.Unlock()
				if len(p) > 0 {
					w.r.Metrics.Add("db_collector_slow_queries_total", float64(len(p)), "target", w.t.Name)
					w.send(ctx, Batch{SlowQueries: p, SlowDropped: d})
				}
				if d > 0 {
					w.r.Metrics.Add("db_collector_slow_queries_dropped_total", float64(d), "target", w.t.Name)
				}
			}
			for {
				select {
				case <-stop:
					send()
					return
				case <-tick.C:
					send()
				}
			}
		}()
		err = follower.Follow(ctx, emit)
		close(stop)
		<-done
		if err != nil && ctx.Err() == nil {
			w.problem("slowlog", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}
