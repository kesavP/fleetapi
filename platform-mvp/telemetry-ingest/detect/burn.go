// Package detect finds incidents on its own: stretches of time in which an application burned its error budget too fast, judged from the
// node-agent's inbound HTTP metrics, and opens them through the incident manager. The finding itself (this file) is pure; the timeline it
// reads is built in timeline.go and the loop that runs it, in runner.go.
//
// It is the same algorithm as the incident page's own (telemetry-ui/src/incidents/detect.js), so the page and the detector agree on what an
// incident is. An objective such as "99.9% of requests should not fail" leaves a budget of 0.1%: the share of requests allowed to be bad.
// The BURN RATE is how many times faster than allowed the budget is being used: a window in which 1.44% of requests fail burns it at 14.4x.
// A breach is declared when a short window (5 minutes) AND a long one (an hour) both burn at the threshold or more: the long window says
// it matters, the short one says it is still happening, so an incident ends soon after the trouble does (the usual multi-window alert).
package detect

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Config is what an objective and its detection are tuned by. The zero value is not usable: start from Defaults.
type Config struct {
	Availability   float64 // share of requests that should not fail
	Latency        float64 // share of requests that should be faster than LatencySeconds
	LatencySeconds float64
	BurnThreshold  float64
	ShortSeconds   int
	LongSeconds    int
	MinRequests    float64 // requests a window must hold before its share means anything
	JoinSeconds    int     // two breaches closer than this are one incident
	MinSeconds     int     // a breach shorter than this is noise
}

// Defaults are the page's defaults.
func Defaults() Config {
	return Config{
		Availability: 0.999, Latency: 0.999, LatencySeconds: 0.5, BurnThreshold: 14.4,
		ShortSeconds: 300, LongSeconds: 3600, MinRequests: 20, JoinSeconds: 300, MinSeconds: 120,
	}
}

// Validate says what is unusable in c, or nil.
func (c Config) Validate() error {
	obj := func(name string, v float64) error {
		if !(v > 0 && v < 1) {
			return fmt.Errorf("%s objective must be between 0 and 1 (exclusive), such as 0.999", name)
		}
		return nil
	}
	if err := obj("availability", c.Availability); err != nil {
		return err
	}
	if err := obj("latency", c.Latency); err != nil {
		return err
	}
	switch {
	case !(c.LatencySeconds > 0):
		return fmt.Errorf("latency threshold must be positive")
	case !(c.BurnThreshold > 0):
		return fmt.Errorf("burn threshold must be positive")
	case c.ShortSeconds <= 0 || c.LongSeconds < c.ShortSeconds:
		return fmt.Errorf("the short window must be positive and no longer than the long one")
	case c.MinRequests < 0 || c.JoinSeconds < 0 || c.MinSeconds < 0:
		return fmt.Errorf("minimum requests, join and minimum duration must not be negative")
	}
	return nil
}

// Grid is the time axis every array of an application is laid out on: N steps of Step seconds from Start (epoch seconds).
type Grid struct {
	Start int64
	Step  int
	N     int
}

// App is one application's requests per second at each step of a Grid. Slow is nil when nothing can be said about slow requests.
type App struct {
	ID, Label           string
	Total, Errors, Slow []float64
}

const (
	Availability = "availability"
	LatencyKind  = "latency"
)

// Breach is one incident found in an application: the first and last step that were themselves burning, and the time they span.
type Breach struct {
	Kinds       []string  // Availability and/or LatencyKind
	First, Last int       // step indexes, inclusive
	Start, End  time.Time // Start is the beginning of the first step, End the end of the last
	Open        bool      // still breaching at the last step of the data
	// PeakBurn is, per kind, the highest burn rate of a short window ending inside the breach.
	PeakBurn map[string]float64
	// Compliance is, per kind, the share of requests that were good across the breach (absent when there were too few to say).
	Compliance map[string]float64
	Requests   float64 // requests served across the breach
}

func sum(a []float64, from, to int) float64 {
	var s float64
	for i := max(from, 0); i <= to && i < len(a); i++ {
		s += a[i]
	}
	return s
}

// BurnRate is the burn rate of the window of `steps` steps ending at step i: its bad share over the budget; 0 when it holds too few
// requests to judge.
func BurnRate(total, bad []float64, i, steps, step int, objective, minRequests float64) float64 {
	t := sum(total, i-steps+1, i) * float64(step)
	if t < minRequests || t <= 0 {
		return 0
	}
	b := sum(bad, i-steps+1, i) * float64(step)
	return b / t / (1 - objective)
}

// stretches returns the [first, last] step indexes of each run where flags hold, joining runs at most `join` steps apart.
func stretches(flags []bool, join int) [][2]int {
	var out [][2]int
	for i, f := range flags {
		if !f {
			continue
		}
		if n := len(out); n > 0 && i-out[n-1][1] <= join+1 {
			out[n-1][1] = i
		} else {
			out = append(out, [2]int{i, i})
		}
	}
	return out
}

func roundSteps(seconds, step int) int { return int(math.Round(float64(seconds) / float64(step))) }

// FindBreaches returns the incidents of one application over the grid, oldest first.
func FindBreaches(app App, g Grid, c Config) []Breach {
	if g.N <= 0 || g.Step <= 0 {
		return nil
	}
	shortN := max(roundSteps(c.ShortSeconds, g.Step), 1)
	longN := max(roundSteps(c.LongSeconds, g.Step), shortN)
	join := roundSteps(c.JoinSeconds, g.Step)

	type kindDef struct {
		name      string
		bad       []float64
		objective float64
	}
	kinds := []kindDef{{Availability, app.Errors, c.Availability}}
	if app.Slow != nil {
		kinds = append(kinds, kindDef{LatencyKind, app.Slow, c.Latency})
	}

	type run struct {
		kind        string
		first, last int
	}
	var runs []run
	for _, k := range kinds {
		hot := func(i, steps int) bool {
			return BurnRate(app.Total, k.bad, i, steps, g.Step, k.objective, c.MinRequests) >= c.BurnThreshold
		}
		breach := make([]bool, g.N)
		for i := range breach {
			breach[i] = hot(i, shortN) && hot(i, longN)
		}
		// A step is "burning" when its own few steps are: that is where an incident really begins and ends.
		burning := func(i int) bool {
			if i < 0 || i >= g.N {
				return false
			}
			return BurnRate(app.Total, k.bad, i, 1, g.Step, k.objective, 0) >= c.BurnThreshold && at(app.Total, i)*float64(g.Step) >= 1
		}
		for _, s := range stretches(breach, join) {
			first, last := s[0], s[1]
			for first-1 >= max(s[0]-shortN, 0) && burning(first-1) {
				first--
			}
			for first < last && !burning(first) {
				first++
			}
			for last > first && !burning(last) {
				last--
			}
			runs = append(runs, run{k.name, first, last})
		}
	}

	// Availability and latency stretches that overlap are one incident with both kinds.
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].first < runs[j].first })
	type merged struct {
		first, last int
		kinds       []string
	}
	var ms []*merged
	for _, r := range runs {
		if n := len(ms); n > 0 && r.first <= ms[n-1].last+join+1 {
			m := ms[n-1]
			m.last = max(m.last, r.last)
			if !contains(m.kinds, r.kind) {
				m.kinds = append(m.kinds, r.kind)
			}
			continue
		}
		ms = append(ms, &merged{first: r.first, last: r.last, kinds: []string{r.kind}})
	}

	var out []Breach
	for _, m := range ms {
		if (m.last-m.first+1)*g.Step < c.MinSeconds {
			continue
		}
		b := Breach{
			Kinds: m.kinds, First: m.first, Last: m.last,
			Start:      time.Unix(g.Start+int64(m.first*g.Step), 0).UTC(),
			End:        time.Unix(g.Start+int64((m.last+1)*g.Step), 0).UTC(),
			Open:       m.last >= g.N-1,
			PeakBurn:   map[string]float64{},
			Compliance: map[string]float64{},
			Requests:   sum(app.Total, m.first, m.last) * float64(g.Step),
		}
		for _, k := range kinds {
			if !contains(m.kinds, k.name) {
				continue
			}
			var peak float64
			for i := m.first; i <= m.last; i++ {
				peak = max(peak, BurnRate(app.Total, k.bad, i, shortN, g.Step, k.objective, c.MinRequests))
			}
			b.PeakBurn[k.name] = peak
		}
		for _, k := range kinds {
			if b.Requests >= 1 {
				b.Compliance[k.name] = 1 - sum(k.bad, m.first, m.last)*float64(g.Step)/b.Requests
			}
		}
		out = append(out, b)
	}
	return out
}

func at(a []float64, i int) float64 {
	if i < 0 || i >= len(a) {
		return 0
	}
	return a[i]
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
