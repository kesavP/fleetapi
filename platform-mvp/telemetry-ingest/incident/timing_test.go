package incident

import (
	"testing"
	"time"
)

func secs(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

func TestTimingFromTheMilestones(t *testing.T) {
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	inc, err := Fold([]Event{
		created(at(0)), // started 5 minutes before it was opened
		ev(2, EvStateChanged, StateChanged{From: Open, To: Acknowledged}, at(2)),
		ev(3, EvStateChanged, StateChanged{From: Acknowledged, To: Mitigated}, at(10)),
		ev(4, EvStateChanged, StateChanged{From: Mitigated, To: Resolved, Resolution: Fixed}, at(30)),
	})
	if err != nil {
		t.Fatal(err)
	}
	tm := inc.Timing
	if secs(tm.DetectSeconds) != 300 || secs(tm.AcknowledgeSeconds) != 120 || secs(tm.MitigateSeconds) != 900 || secs(tm.ResolveSeconds) != 2100 {
		t.Fatalf("detect %d ack %d mitigate %d resolve %d", secs(tm.DetectSeconds), secs(tm.AcknowledgeSeconds), secs(tm.MitigateSeconds), secs(tm.ResolveSeconds))
	}
}

func TestTimingLeavesOutWhatHasNotHappenedAndNeverGoesNegative(t *testing.T) {
	inc, err := Fold([]Event{created(t0)})
	if err != nil {
		t.Fatal(err)
	}
	if inc.Timing.AcknowledgeSeconds != nil || inc.Timing.MitigateSeconds != nil || inc.Timing.ResolveSeconds != nil {
		t.Fatalf("%+v", inc.Timing)
	}
	future := Incident{StartedAt: t0.Add(time.Hour), CreatedAt: t0}
	if secs(TimingOf(future).DetectSeconds) != 0 {
		t.Fatal("a start after the opening counts as 0")
	}
	noStart := Incident{CreatedAt: t0, ResolvedAt: ptr(t0.Add(time.Minute))}
	if secs(TimingOf(noStart).DetectSeconds) != 0 || secs(TimingOf(noStart).ResolveSeconds) != 60 {
		t.Fatalf("without a start, times count from the opening: %+v", TimingOf(noStart))
	}
}

func TestReopenedIncidentsCountTheWholeTime(t *testing.T) {
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	inc, err := Fold([]Event{
		created(at(0)),
		ev(2, EvStateChanged, StateChanged{From: Open, To: Resolved, Resolution: Fixed}, at(10)),
		ev(3, EvStateChanged, StateChanged{From: Resolved, To: Open, Note: "back"}, at(20)),
		ev(4, EvStateChanged, StateChanged{From: Open, To: Resolved, Resolution: Fixed}, at(40)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if secs(inc.Timing.ResolveSeconds) != 45*60 {
		t.Fatalf("resolve %d, want the time to the last resolution", secs(inc.Timing.ResolveSeconds))
	}
}

func ptr(t time.Time) *time.Time { return &t }

func resolvedInc(sev Severity, res Resolution, start time.Time, minutes int) Incident {
	r := start.Add(time.Duration(minutes) * time.Minute)
	ack := start.Add(time.Minute)
	return Incident{Severity: sev, Source: Detected, State: Resolved, Resolution: res, StartedAt: start, CreatedAt: start.Add(30 * time.Second),
		AcknowledgedAt: &ack, ResolvedAt: &r}
}

func TestComputeStats(t *testing.T) {
	from, to := t0, t0.Add(7*24*time.Hour)
	incs := []Incident{
		resolvedInc(Critical, Fixed, t0.Add(time.Hour), 10),
		resolvedInc(Critical, Fixed, t0.Add(2*time.Hour), 30),
		resolvedInc(Warning, Workaround, t0.Add(3*time.Hour), 60),
		resolvedInc(Warning, FalsePositive, t0.Add(4*time.Hour), 5),                                                       // not counted in MTTR
		resolvedInc(Critical, Fixed, t0.Add(-48*time.Hour), 20),                                                           // before the window
		{Severity: Info, Source: Manual, State: Open, StartedAt: t0.Add(5 * time.Hour), CreatedAt: t0.Add(5 * time.Hour)}, // still open
	}
	incs[1].Reopened = 1
	st := ComputeStats(incs, from, to)
	if st.Opened != 5 || st.Resolved != 3 || st.NotCounted != 1 || st.StillOpen != 1 || st.Reopened != 1 {
		t.Fatalf("counts %+v", st)
	}
	if st.MTTR.Count != 3 || secs(st.MTTR.Median) != 1800 || secs(st.MTTR.Max) != 3600 || secs(st.MTTR.Mean) != 2000 || secs(st.MTTR.P90) != 3600 {
		t.Fatalf("mttr %+v", st.MTTR)
	}
	if c := st.MTTRBySeverity[Critical]; c.Count != 2 || secs(c.Median) != 600 {
		t.Fatalf("critical %+v", c)
	}
	if st.MTTA.Count != 4 || secs(st.MTTA.Median) != 30 {
		t.Fatalf("mtta %+v (ack a minute after the start, 30 s after the opening)", st.MTTA)
	}
	if st.MTTD.Count != 4 || secs(st.MTTD.Median) != 30 {
		t.Fatalf("mttd %+v: only detected incidents opened in the window", st.MTTD)
	}
	if st.MTTM.Count != 0 || st.MTTM.Median != nil {
		t.Fatalf("no mitigations: %+v", st.MTTM)
	}
}

func TestStatsReadsTheStoreAndFlagsAPartialWindow(t *testing.T) {
	r := newRig()
	inc := r.create(t, CreateInput{Severity: Critical})
	r.tick(10 * time.Minute)
	if _, err := r.m.Transition(ctx, "gke", inc.ID, "ana", nil, TransitionInput{To: Resolved, Resolution: Fixed}); err != nil {
		t.Fatal(err)
	}
	st, skipped, err := r.m.Stats(ctx, "gke", r.clock.Add(-time.Hour), r.clock.Add(time.Minute))
	if err != nil || skipped != 0 {
		t.Fatal(err, skipped)
	}
	if st.Resolved != 1 || secs(st.MTTR.Median) != 600 || st.Partial {
		t.Fatalf("%+v", st)
	}
}
