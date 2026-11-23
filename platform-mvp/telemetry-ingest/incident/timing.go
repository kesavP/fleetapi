package incident

import (
	"math"
	"sort"
	"time"
)

// Timing is how long each stage of an incident took, in whole seconds, from its milestones. A stage that has not happened is absent.
//
//   - detect: from when the trouble began (StartedAt) to when the incident was opened (CreatedAt)
//   - acknowledge: from when it was opened to when someone first took it on
//   - mitigate: from when the trouble began to when it was first mitigated
//   - resolve: from when the trouble began to when it was (last) resolved; a reopened incident counts the time it was open again
//
// Resolve is the one MTTR is made of. Times are measured from StartedAt, the start of the trouble, because that is what users felt; an incident
// opened by hand without a start time has StartedAt equal to CreatedAt, so its detect time is 0. A milestone before the one it is measured from (a
// start given in the future, a clock that went back) counts as 0, never as a negative time.
type Timing struct {
	DetectSeconds      *int64 `json:"detect_seconds,omitempty"`
	AcknowledgeSeconds *int64 `json:"acknowledge_seconds,omitempty"`
	MitigateSeconds    *int64 `json:"mitigate_seconds,omitempty"`
	ResolveSeconds     *int64 `json:"resolve_seconds,omitempty"`
}

func seconds(from, to time.Time) *int64 {
	d := int64(to.Sub(from) / time.Second)
	if d < 0 {
		d = 0
	}
	return &d
}

// TimingOf works an incident's timing out from its milestones.
func TimingOf(inc Incident) Timing {
	start := inc.StartedAt
	if start.IsZero() {
		start = inc.CreatedAt
	}
	var t Timing
	if !inc.CreatedAt.IsZero() {
		t.DetectSeconds = seconds(start, inc.CreatedAt)
	}
	if inc.AcknowledgedAt != nil {
		t.AcknowledgeSeconds = seconds(inc.CreatedAt, *inc.AcknowledgedAt)
	}
	if inc.MitigatedAt != nil {
		t.MitigateSeconds = seconds(start, *inc.MitigatedAt)
	}
	if inc.ResolvedAt != nil {
		t.ResolveSeconds = seconds(start, *inc.ResolvedAt)
	}
	return t
}

// Durations summarises a set of durations in seconds. Count 0 leaves the rest out.
type Durations struct {
	Count  int    `json:"count"`
	Mean   *int64 `json:"mean_seconds,omitempty"`
	Median *int64 `json:"median_seconds,omitempty"`
	P90    *int64 `json:"p90_seconds,omitempty"`
	Max    *int64 `json:"max_seconds,omitempty"`
}

func durationsOf(xs []int64) Durations {
	if len(xs) == 0 {
		return Durations{}
	}
	s := append([]int64(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	var sum float64
	for _, x := range s {
		sum += float64(x)
	}
	// nearest rank: the smallest value with at least p of them at or below it
	rank := func(p float64) *int64 {
		i := int(math.Ceil(p*float64(len(s)))) - 1
		if i < 0 {
			i = 0
		}
		v := s[i]
		return &v
	}
	mean := int64(math.Round(sum / float64(len(s))))
	return Durations{Count: len(s), Mean: &mean, Median: rank(0.5), P90: rank(0.9), Max: &s[len(s)-1]}
}

// Stats is how a project's incidents went in a window: MTTD, MTTA, MTTM and MTTR, and how many were opened and resolved.
type Stats struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Opened were opened in the window; Resolved were resolved in it, and count towards MTTR; NotCounted were resolved in it as a false positive
	// or a duplicate, which are left out of MTTR because there was nothing to fix. StillOpen are not resolved or closed now, whenever they opened.
	Opened     int `json:"opened"`
	Resolved   int `json:"resolved"`
	NotCounted int `json:"not_counted"`
	StillOpen  int `json:"still_open"`
	Reopened   int `json:"reopened"` // of those resolved in the window, how many had been reopened

	// MTTD is over the detected incidents opened in the window (a person's has no detection time to speak of); MTTA over those first taken on in
	// the window; MTTM over those first mitigated in it; MTTR over those resolved in it, also by severity.
	MTTD           Durations              `json:"mttd"`
	MTTA           Durations              `json:"mtta"`
	MTTM           Durations              `json:"mttm"`
	MTTR           Durations              `json:"mttr"`
	MTTRBySeverity map[Severity]Durations `json:"mttr_by_severity"`

	// Partial is set when the incidents looked at did not reach back to From (only the newest MaxList are read), so the figures miss older ones.
	Partial bool `json:"partial"`
}

func in(t time.Time, from, to time.Time) bool { return !t.Before(from) && t.Before(to) }

// ComputeStats works out the stats of the window [from, to) from incidents (folded, in any order).
func ComputeStats(incs []Incident, from, to time.Time) Stats {
	st := Stats{From: from.UTC(), To: to.UTC(), MTTRBySeverity: map[Severity]Durations{}}
	var detect, ack, mitigate, resolve []int64
	bySev := map[Severity][]int64{}
	for _, inc := range incs {
		t := TimingOf(inc)
		if inc.State != Resolved && inc.State != Closed {
			st.StillOpen++
		}
		if in(inc.CreatedAt, from, to) {
			st.Opened++
			if inc.Source == Detected && t.DetectSeconds != nil {
				detect = append(detect, *t.DetectSeconds)
			}
		}
		if inc.AcknowledgedAt != nil && in(*inc.AcknowledgedAt, from, to) && t.AcknowledgeSeconds != nil {
			ack = append(ack, *t.AcknowledgeSeconds)
		}
		if inc.MitigatedAt != nil && in(*inc.MitigatedAt, from, to) && t.MitigateSeconds != nil {
			mitigate = append(mitigate, *t.MitigateSeconds)
		}
		if inc.ResolvedAt != nil && in(*inc.ResolvedAt, from, to) && t.ResolveSeconds != nil {
			if inc.Resolution == FalsePositive || inc.Resolution == Duplicate {
				st.NotCounted++
				continue
			}
			st.Resolved++
			if inc.Reopened > 0 {
				st.Reopened++
			}
			resolve = append(resolve, *t.ResolveSeconds)
			bySev[inc.Severity] = append(bySev[inc.Severity], *t.ResolveSeconds)
		}
	}
	st.MTTD, st.MTTA, st.MTTM, st.MTTR = durationsOf(detect), durationsOf(ack), durationsOf(mitigate), durationsOf(resolve)
	for sev, xs := range bySev {
		st.MTTRBySeverity[sev] = durationsOf(xs)
	}
	return st
}
