package collector

import "sort"

// Cumulative is one statement's counters since the server (or the statement) started counting: what pg_stat_statements and its relatives
// hold. They only ever go up, except when they are reset, which the Differ has to notice.
type Cumulative struct {
	Key      string // the identity: the same statement of the same user in the same database has the same key
	QueryID  string
	Database string
	User     string
	Query    string
	Calls    uint64
	TotalMs  float64
	Rows     uint64
	Extra    map[string]float64
	// Reset changes when the engine says this statement's counters started again (PostgreSQL 17's stats_since); empty when it does not say.
	Reset string
}

// Snapshot is every statement's counters at one moment.
type Snapshot struct {
	Stats []Cumulative
	// Epoch changes when the engine says ALL the counters started again (a reset of the statistics): pg_stat_statements_info.stats_reset.
	Epoch string
}

// DiffInfo says what happened in a Diff, for the collector's own metrics and logs.
type DiffInfo struct {
	Primed  bool // this was the first snapshot: it is the baseline, and there is nothing to report yet
	Resets  int  // statements whose counters had restarted
	Seen    int  // statements in the snapshot
	Changed int  // statements that ran in the interval
	Folded  int  // statements that ran but are only in the "other" row
}

// Differ turns successive snapshots of cumulative counters into what happened between them.
//
// The first snapshot is only a baseline: reporting the counters since the server started as if they were one interval's would put a spike
// of days of work into one minute. After that a statement's figures are the increase since the last snapshot, with three ways it can go wrong
// handled: the whole set was reset (the Epoch changed), one statement's counters restarted (its Reset token changed, or its calls went DOWN,
// as when pg_stat_statements evicts a statement and it comes back), and a statement that is new since the last snapshot (whose counters
// started from zero in the interval, so they ARE the interval's). Figures that would come out negative (floating-point drift in a total
// time) are zero.
type Differ struct {
	prev   map[string]Cumulative
	epoch  string
	primed bool
	// Top is how many statements to report by name, the busiest by time first; the rest are one "other" row. 0 means MaxStatements.
	Top int
}

func (d *Differ) Diff(s Snapshot) ([]Statement, DiffInfo) {
	info := DiffInfo{Seen: len(s.Stats)}
	next := make(map[string]Cumulative, len(s.Stats))
	for _, c := range s.Stats {
		next[c.Key] = c
	}
	defer func() { d.prev, d.epoch, d.primed = next, s.Epoch, true }()
	if !d.primed {
		info.Primed = true
		return nil, info
	}
	wholeReset := s.Epoch != d.epoch

	type delta struct {
		st  Statement
		key string
	}
	var deltas []delta
	for _, c := range s.Stats {
		p, had := d.prev[c.Key]
		fresh := wholeReset || !had || p.Reset != c.Reset || c.Calls < p.Calls
		if had && fresh {
			info.Resets++
		}
		st := Statement{QueryID: c.QueryID, Database: c.Database, User: c.User, Query: c.Query}
		if fresh {
			st.Calls, st.TotalMs, st.Rows = c.Calls, c.TotalMs, c.Rows
			st.Extra = copyExtra(c.Extra, nil)
		} else {
			st.Calls = c.Calls - p.Calls
			st.TotalMs = nonNegative(c.TotalMs - p.TotalMs)
			st.Rows = sub(c.Rows, p.Rows)
			st.Extra = copyExtra(c.Extra, p.Extra)
		}
		if st.Calls == 0 {
			continue
		}
		deltas = append(deltas, delta{st, c.Key})
	}
	info.Changed = len(deltas)
	sort.Slice(deltas, func(i, j int) bool {
		a, b := deltas[i].st, deltas[j].st
		if a.TotalMs != b.TotalMs {
			return a.TotalMs > b.TotalMs
		}
		if a.Calls != b.Calls {
			return a.Calls > b.Calls
		}
		return deltas[i].key < deltas[j].key
	})
	top := d.Top
	if top <= 0 || top > MaxStatements {
		top = MaxStatements
	}
	out := make([]Statement, 0, min(len(deltas), top)+1)
	var rest Statement
	for i, dl := range deltas {
		if i < top {
			out = append(out, dl.st)
			continue
		}
		info.Folded++
		rest.Calls += dl.st.Calls
		rest.TotalMs += dl.st.TotalMs
		rest.Rows += dl.st.Rows
		rest.Extra = addExtra(rest.Extra, dl.st.Extra)
	}
	if info.Folded > 0 {
		rest.Other, rest.Query = true, "(other statements)"
		out = append(out, rest)
	}
	return out, info
}

func nonNegative(f float64) float64 {
	if f < 0 {
		return 0
	}
	return f
}

func sub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

// copyExtra is the increase of each extra counter over the previous one (prev nil: the counters as they are).
func copyExtra(cur, prev map[string]float64) map[string]float64 {
	if len(cur) == 0 {
		return nil
	}
	out := make(map[string]float64, len(cur))
	for k, v := range cur {
		if prev != nil {
			v = nonNegative(v - prev[k])
		}
		if v != 0 {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func addExtra(into, add map[string]float64) map[string]float64 {
	if len(add) == 0 {
		return into
	}
	if into == nil {
		into = map[string]float64{}
	}
	for k, v := range add {
		into[k] += v
	}
	return into
}
