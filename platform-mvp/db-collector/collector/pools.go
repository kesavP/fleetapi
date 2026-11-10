package collector

import "sort"

// PoolSnapshot is a pooler's pools now, and its counters since it started counting: per database, by the name this package uses for each
// (xacts, queries, bytes_in, bytes_out, xact_ms, query_ms, wait_ms).
type PoolSnapshot struct {
	Pools         []Pool
	Counters      map[string]map[string]float64
	MaxClientConn int
}

// PoolDiffer turns successive snapshots into a Pools: the pools as they are, and the counters as what went through in the interval.
//
// Like the statement Differ, the first snapshot is only a baseline for the counters (the pools themselves are reported at once: they are
// gauges, true as they are). A counter that has gone DOWN means the pooler restarted (or reset its statistics), so that database's figures are
// what it has counted since; a database that is new counts from zero. A restart that is followed by more traffic than the counters had before
// is indistinguishable from an ordinary interval, which the pooler gives no way to tell apart: the interval after a restart can read low.
type PoolDiffer struct {
	prev   map[string]map[string]float64
	primed bool
}

var counterNames = []string{"xacts", "queries", "bytes_in", "bytes_out", "xact_ms", "query_ms", "wait_ms"}

func (d *PoolDiffer) Diff(s PoolSnapshot) (Pools, DiffInfo) {
	info := DiffInfo{Seen: len(s.Counters)}
	out := Pools{MaxClientConn: s.MaxClientConn}
	all := append([]Pool(nil), s.Pools...)
	for _, p := range all {
		out.Clients += p.ClActive + p.ClWaiting
		out.Waiting += p.ClWaiting
		if p.MaxWaitMs > out.MaxWaitMs {
			out.MaxWaitMs = p.MaxWaitMs
		}
	}
	// the pools that matter first: the ones with clients waiting, then the busiest
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.ClWaiting != b.ClWaiting {
			return a.ClWaiting > b.ClWaiting
		}
		if a.SvActive != b.SvActive {
			return a.SvActive > b.SvActive
		}
		if a.Database != b.Database {
			return a.Database < b.Database
		}
		return a.User < b.User
	})
	if len(all) > MaxPools {
		out.Dropped = len(all) - MaxPools
		all = all[:MaxPools]
	}
	out.Pools = all
	if out.Pools == nil {
		out.Pools = []Pool{}
	}

	next := make(map[string]map[string]float64, len(s.Counters))
	for db, c := range s.Counters {
		next[db] = copyCounters(c)
	}
	defer func() { d.prev, d.primed = next, true }()
	if !d.primed {
		info.Primed = true
		return out, info
	}
	dbs := make([]string, 0, len(s.Counters))
	for db := range s.Counters {
		dbs = append(dbs, db)
	}
	sort.Strings(dbs)
	for _, db := range dbs {
		cur, was := s.Counters[db], d.prev[db]
		reset := was == nil
		for _, n := range counterNames {
			if cur[n] < was[n] {
				reset = true
			}
		}
		if was != nil && reset {
			info.Resets++
		}
		get := func(n string) float64 {
			if reset {
				return cur[n]
			}
			return nonNegative(cur[n] - was[n])
		}
		st := PoolStats{
			Database: db, Xacts: uint64(get("xacts")), Queries: uint64(get("queries")), BytesIn: uint64(get("bytes_in")), BytesOut: uint64(get("bytes_out")),
			XactMs: get("xact_ms"), QueryMs: get("query_ms"), WaitMs: get("wait_ms"),
		}
		if st.Xacts == 0 && st.Queries == 0 && st.BytesIn == 0 && st.BytesOut == 0 {
			continue // nothing went through this database
		}
		info.Changed++
		out.Stats = append(out.Stats, st)
	}
	return out, info
}

func copyCounters(c map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(c))
	for k, v := range c {
		out[k] = v
	}
	return out
}
