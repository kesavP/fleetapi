package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/cluster-collector/kube"
)

// Reader is what the Runner looks at: the cluster.
type Reader interface {
	Snapshot(ctx context.Context) kube.Snapshot
}

// Runner looks at the cluster on a timer and reports what changed.
type Runner struct {
	Source   Reader
	Sink     Sink
	Cluster  string
	Interval time.Duration
	Resync   time.Duration // how often everything is sent again
	Log      *slog.Logger
	Now      func() time.Time

	differ     *Differ
	lastFull   time.Time
	mu         sync.Mutex
	lastOK     time.Time
	lastProbs  string
	lastReport Report
}

// Report is what the last successful look did.
type Report struct {
	At                       time.Time
	Full                     bool
	Objects, Deleted, Events int
	Batches                  int
	Skipped                  int
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) init() {
	if r.differ == nil {
		r.differ = NewDiffer()
	}
}

// Healthy reports whether a look has been reported within maxAge: what a readiness probe asks.
func (r *Runner) Healthy(maxAge time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.lastOK.IsZero() && r.now().Sub(r.lastOK) <= maxAge
}

// Last is the report of the last successful look.
func (r *Runner) Last() Report {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastReport
}

// Run looks every Interval until ctx ends, and once at the start. A look that fails is logged and tried again at the next one.
func (r *Runner) Run(ctx context.Context) {
	r.init()
	r.Log.Info("cluster collector running", "cluster", r.Cluster, "interval", r.Interval, "resync", r.Resync)
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		if err := r.Cycle(ctx); err != nil && ctx.Err() == nil {
			r.Log.Error("a look at the cluster was not reported", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Cycle makes one look and reports it. Nothing is remembered as reported unless every batch of it was accepted.
func (r *Runner) Cycle(ctx context.Context) error {
	r.init()
	at := r.now().UTC().Truncate(time.Millisecond)
	snap := r.Source.Snapshot(ctx)
	r.noteProblems(snap)
	if len(snap.Listed) == 0 {
		// Blind: reporting an empty cluster would say everything is gone, and a heartbeat would say all is well. Say nothing, and the service
		// shows this collector as silent.
		return errors.New("no kind of object could be read from the cluster: " + describeProblems(snap))
	}
	full := r.lastFull.IsZero() || at.Sub(r.lastFull) >= r.Resync
	plan := r.differ.Plan(snap, at, full)
	batches := plan.Batches(r.Cluster, at)
	for i, b := range batches {
		if err := r.Sink.Send(ctx, b); err != nil {
			return fmt.Errorf("batch %d of %d: %w", i+1, len(batches), err)
		}
	}
	r.differ.Commit(plan)
	if full {
		r.lastFull = at
	}
	r.mu.Lock()
	r.lastOK = r.now()
	r.lastReport = Report{At: at, Full: full, Objects: len(plan.Objects), Deleted: len(plan.Deleted), Events: len(plan.Events), Batches: len(batches), Skipped: snap.Skipped}
	r.mu.Unlock()
	if snap.Skipped > 0 {
		r.Log.Warn("objects were left out because they could not be reported", "count", snap.Skipped)
	}
	return nil
}

func describeProblems(s kube.Snapshot) string {
	var parts []string
	for k, err := range s.Problems {
		parts = append(parts, k+": "+err.Error())
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

// noteProblems logs the kinds that could not be read, when the set changes, not at every look.
func (r *Runner) noteProblems(s kube.Snapshot) {
	now := describeProblems(s)
	if now == r.lastProbs {
		return
	}
	if now == "" {
		r.Log.Info("every kind of object can be read again")
	} else {
		r.Log.Warn("some kinds of object could not be read, and are left as they were", "problems", now)
	}
	r.lastProbs = now
}
