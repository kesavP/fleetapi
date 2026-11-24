package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/dbtelemetry"
)

// WriteDatabaseBatch stores what a database collector sent. The batch has been validated (dbtelemetry.Batch.Validate). The collector sends
// one kind of data per batch, so a failure part way cannot be retried into duplicates; a batch that carries several kinds is written in a
// fixed order and a retry after a partial failure repeats the kinds that had succeeded.
func (s *Store) WriteDatabaseBatch(ctx context.Context, project string, b dbtelemetry.Batch) error {
	at := b.CollectedAt.UTC()
	if len(b.Statements) > 0 {
		interval := uint32(b.IntervalSeconds * 1000)
		err := insert(ctx, s, "INSERT INTO db_statements (Project, Target, Time, IntervalMs, QueryID, Database, User, Query, Calls, TotalMs, Rows, Other, Extra)", b.Statements,
			func(st dbtelemetry.Statement) []any {
				var other uint8
				if st.Other {
					other = 1
				}
				return []any{project, b.Target, at, interval, st.QueryID, st.Database, st.User, st.Query, st.Calls, st.TotalMs, st.Rows, other, mpf(st.Extra)}
			})
		if err != nil {
			return err
		}
	}
	if a := b.Activity; a != nil {
		sessions, _ := json.Marshal(nonNilSessions(a.Sessions))
		err := insert(ctx, s, "INSERT INTO db_activity (Project, Target, Time, Total, MaxConnections, Active, Idle, IdleInTx, Blocked, LongestQueryMs, LongestXactMs, LongestIdleInTxMs, ByState, Waiting, Sessions)", []dbtelemetry.Activity{*a},
			func(a dbtelemetry.Activity) []any {
				idleInTx := a.ByState["idle in transaction"] + a.ByState["idle in transaction (aborted)"]
				return []any{project, b.Target, at, uint32(a.Total), uint32(a.MaxConnections), uint32(a.ByState["active"]), uint32(a.ByState["idle"]), uint32(idleInTx), uint32(a.Blocked),
					a.LongestQueryMs, a.LongestXactMs, a.LongestIdleXMs, mpu(a.ByState), mpu(a.Waiting), string(sessions)}
			})
		if err != nil {
			return err
		}
	}
	if p := b.Pools; p != nil {
		interval := uint32(b.IntervalSeconds * 1000)
		err := insert(ctx, s, "INSERT INTO db_pool_samples (Project, Target, Time, IntervalMs, Clients, Waiting, MaxWaitMs, MaxClientConn, Dropped)", []dbtelemetry.Pools{*p},
			func(p dbtelemetry.Pools) []any {
				return []any{project, b.Target, at, interval, uint32(p.Clients), uint32(p.Waiting), p.MaxWaitMs, uint32(p.MaxClientConn), uint32(p.Dropped)}
			})
		if err != nil {
			return err
		}
		err = insert(ctx, s, "INSERT INTO db_pools (Project, Target, Time, Database, User, Mode, ClActive, ClWaiting, SvActive, SvIdle, SvUsed, SvTested, SvLogin, MaxWaitMs, PoolSize, ReservePool, Paused, Disabled)", p.Pools,
			func(q dbtelemetry.Pool) []any {
				return []any{project, b.Target, at, q.Database, q.User, q.Mode, uint32(q.ClActive), uint32(q.ClWaiting), uint32(q.SvActive), uint32(q.SvIdle), uint32(q.SvUsed), uint32(q.SvTested), uint32(q.SvLogin),
					q.MaxWaitMs, uint32(q.PoolSize), uint32(q.ReservePool), flag(q.Paused), flag(q.Disabled)}
			})
		if err != nil {
			return err
		}
		err = insert(ctx, s, "INSERT INTO db_pool_stats (Project, Target, Time, IntervalMs, Database, Xacts, Queries, BytesIn, BytesOut, XactMs, QueryMs, WaitMs)", p.Stats,
			func(st dbtelemetry.PoolStats) []any {
				return []any{project, b.Target, at, interval, st.Database, st.Xacts, st.Queries, st.BytesIn, st.BytesOut, st.XactMs, st.QueryMs, st.WaitMs}
			})
		if err != nil {
			return err
		}
	}
	if len(b.SlowQueries) > 0 {
		err := insert(ctx, s, "INSERT INTO db_slow_queries (Project, Target, Time, DurationMs, Database, User, Kind, PID, Query)", b.SlowQueries,
			func(q dbtelemetry.SlowQuery) []any {
				return []any{project, b.Target, q.Time.UTC(), q.DurationMs, q.Database, q.User, q.Kind, uint32(q.PID), q.Query}
			})
		if err != nil {
			return err
		}
	}
	if c := b.Capabilities; c != nil {
		raw, _ := json.Marshal(c)
		err := insert(ctx, s, "INSERT INTO db_targets (Project, Target, Time, Engine, Version, Capabilities)", []dbtelemetry.Capabilities{*c},
			func(c dbtelemetry.Capabilities) []any {
				return []any{project, b.Target, at, b.Engine, c.Version, string(raw)}
			})
		if err != nil {
			return err
		}
	}
	return nil
}

func nonNilSessions(s []dbtelemetry.Session) []dbtelemetry.Session {
	if s == nil {
		return []dbtelemetry.Session{}
	}
	return s
}

func mpf(m map[string]float64) map[string]float64 {
	if m == nil {
		return map[string]float64{}
	}
	return m
}

func mpu(m map[string]int) map[string]uint32 {
	out := make(map[string]uint32, len(m))
	for k, v := range m {
		if v > 0 {
			out[k] = uint32(v)
		}
	}
	return out
}

// ---- reads ----

type DBTarget struct {
	Target       string          `json:"target"`
	Engine       string          `json:"engine"`
	Version      string          `json:"version"`
	LastSeen     time.Time       `json:"last_seen"`
	Capabilities json.RawMessage `json:"capabilities"`
}

// ListDatabases: the targets that have reported what they allow, with the latest report. A target's last_seen is when it last reported
// (every ten minutes or so), so it is how recently the collector could ask the database, not how recently it sent statistics.
func (s *Store) ListDatabases(ctx context.Context, project string) ([]DBTarget, error) {
	rows, err := s.conn.Query(ctx, `SELECT Target, argMax(Engine, Time), argMax(Version, Time), max(Time), argMax(Capabilities, Time)
FROM db_targets WHERE Project = ? GROUP BY Target ORDER BY Target`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DBTarget{}
	for rows.Next() {
		var t DBTarget
		var caps string
		if err := rows.Scan(&t.Target, &t.Engine, &t.Version, &t.LastSeen, &caps); err != nil {
			return nil, err
		}
		t.LastSeen = t.LastSeen.UTC()
		t.Capabilities = json.RawMessage(caps)
		if !json.Valid(t.Capabilities) {
			t.Capabilities = json.RawMessage("null")
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

type StatementQuery struct {
	Project, Target string
	From, To        time.Time
	Order           string // total_ms, calls, mean_ms, rows
	Limit           int
	Database, User  string
}

type StatementRow struct {
	QueryID  string             `json:"query_id"`
	Database string             `json:"database"`
	User     string             `json:"user"`
	Query    string             `json:"query"`
	Calls    uint64             `json:"calls"`
	TotalMs  float64            `json:"total_ms"`
	MeanMs   float64            `json:"mean_ms"`
	Rows     uint64             `json:"rows"`
	Extra    map[string]float64 `json:"extra"`
}

type StatementsResult struct {
	Statements []StatementRow `json:"statements"`
	// The whole of the window, the "other" row included, so that a statement's share of the database's time can be told.
	TotalCalls   uint64  `json:"total_calls"`
	TotalTotalMs float64 `json:"total_ms"`
}

var statementOrders = map[string]string{
	"total_ms": "sum(TotalMs)",
	"calls":    "sum(Calls)",
	"rows":     "sum(Rows)",
	"mean_ms":  "sum(TotalMs) / sum(Calls)",
}

// StatementOrders lists the orders a caller may ask for.
func StatementOrders() []string { return []string{"total_ms", "calls", "mean_ms", "rows"} }

func (s *Store) TopStatements(ctx context.Context, q StatementQuery) (StatementsResult, error) {
	order, ok := statementOrders[q.Order]
	if !ok {
		return StatementsResult{}, fmt.Errorf("unknown order %q", q.Order)
	}
	where := "Project = ? AND Target = ? AND Time >= ? AND Time < ?"
	args := []any{q.Project, q.Target, q.From, q.To}
	if q.Database != "" {
		where += " AND Database = ?"
		args = append(args, q.Database)
	}
	if q.User != "" {
		where += " AND User = ?"
		args = append(args, q.User)
	}
	res := StatementsResult{Statements: []StatementRow{}}
	var calls uint64
	var ms float64
	if err := s.conn.QueryRow(ctx, "SELECT sum(Calls), sum(TotalMs) FROM db_statements WHERE "+where, args...).Scan(&calls, &ms); err != nil {
		return res, err
	}
	res.TotalCalls, res.TotalTotalMs = calls, ms
	rows, err := s.conn.Query(ctx, `SELECT QueryID, Database, User, argMax(Query, Time), sum(Calls), sum(TotalMs), sum(Rows), sumMap(Extra)
FROM db_statements WHERE `+where+` AND Other = 0 GROUP BY QueryID, Database, User ORDER BY `+order+` DESC, QueryID LIMIT ?`, append(args, q.Limit)...)
	if err != nil {
		return res, err
	}
	defer rows.Close()
	for rows.Next() {
		var r StatementRow
		if err := rows.Scan(&r.QueryID, &r.Database, &r.User, &r.Query, &r.Calls, &r.TotalMs, &r.Rows, &r.Extra); err != nil {
			return res, err
		}
		if r.Calls > 0 {
			r.MeanMs = r.TotalMs / float64(r.Calls)
		}
		res.Statements = append(res.Statements, r)
	}
	return res, rows.Err()
}

type ActivityPoint struct {
	T                 int64   `json:"t"` // unix milliseconds
	Total             uint32  `json:"total"`
	MaxConnections    uint32  `json:"max_connections"`
	Active            uint32  `json:"active"`
	IdleInTx          uint32  `json:"idle_in_transaction"`
	Blocked           uint32  `json:"blocked"`
	LongestQueryMs    float64 `json:"longest_query_ms"`
	LongestIdleInTxMs float64 `json:"longest_idle_in_transaction_ms"`
}

type ActivityResult struct {
	Points []ActivityPoint `json:"points"`
	// Latest is the newest sample in the window: when it was taken, and the sessions that were named in it.
	Latest *LatestActivity `json:"latest"`
}

type LatestActivity struct {
	Time     time.Time         `json:"time"`
	Waiting  map[string]uint32 `json:"waiting"`
	Sessions json.RawMessage   `json:"sessions"`
}

// ActivitySeries gives the PEAK of each figure within each `step` seconds: a session pile-up that lasts a few seconds must not be averaged away.
func (s *Store) ActivitySeries(ctx context.Context, project, target string, from, to time.Time, step int) (ActivityResult, error) {
	res := ActivityResult{Points: []ActivityPoint{}}
	rows, err := s.conn.Query(ctx, `SELECT toInt64(toUnixTimestamp(toStartOfInterval(Time, INTERVAL ? SECOND))) * 1000, max(Total), max(MaxConnections), max(Active), max(IdleInTx), max(Blocked), max(LongestQueryMs), max(LongestIdleInTxMs)
FROM db_activity WHERE Project = ? AND Target = ? AND Time >= ? AND Time < ? GROUP BY 1 ORDER BY 1`, step, project, target, from, to)
	if err != nil {
		return res, err
	}
	defer rows.Close()
	for rows.Next() {
		var p ActivityPoint
		var t int64
		if err := rows.Scan(&t, &p.Total, &p.MaxConnections, &p.Active, &p.IdleInTx, &p.Blocked, &p.LongestQueryMs, &p.LongestIdleInTxMs); err != nil {
			return res, err
		}
		p.T = t
		res.Points = append(res.Points, p)
	}
	if err := rows.Err(); err != nil {
		return res, err
	}
	var l LatestActivity
	var sessions string
	err = s.conn.QueryRow(ctx, `SELECT Time, Waiting, Sessions FROM db_activity WHERE Project = ? AND Target = ? AND Time >= ? AND Time < ? ORDER BY Time DESC LIMIT 1`, project, target, from, to).Scan(&l.Time, &l.Waiting, &sessions)
	if err == nil {
		l.Time = l.Time.UTC()
		l.Sessions = json.RawMessage(sessions)
		if !json.Valid(l.Sessions) {
			l.Sessions = json.RawMessage("[]")
		}
		res.Latest = &l
	} else if !isNoRows(err) {
		return res, err
	}
	return res, nil
}

type SlowQueryFilter struct {
	Project, Target string
	From, To        time.Time
	MinMs           float64
	Slowest         bool // order by duration, not by time
	Limit           int
}

type SlowQueryRow struct {
	Time       time.Time `json:"time"`
	DurationMs float64   `json:"duration_ms"`
	Database   string    `json:"database"`
	User       string    `json:"user"`
	Kind       string    `json:"kind"`
	PID        uint32    `json:"pid"`
	Query      string    `json:"query"`
}

func (s *Store) SlowQueries(ctx context.Context, f SlowQueryFilter) ([]SlowQueryRow, error) {
	order := "Time DESC"
	if f.Slowest {
		order = "DurationMs DESC, Time DESC"
	}
	rows, err := s.conn.Query(ctx, `SELECT Time, DurationMs, Database, User, Kind, PID, Query FROM db_slow_queries
WHERE Project = ? AND Target = ? AND Time >= ? AND Time < ? AND DurationMs >= ? ORDER BY `+order+` LIMIT ?`, f.Project, f.Target, f.From, f.To, f.MinMs, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SlowQueryRow{}
	for rows.Next() {
		var r SlowQueryRow
		if err := rows.Scan(&r.Time, &r.DurationMs, &r.Database, &r.User, &r.Kind, &r.PID, &r.Query); err != nil {
			return nil, err
		}
		r.Time = r.Time.UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

func flag(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

// ---- pools ----

type PoolPoint struct {
	T             int64   `json:"t"` // unix milliseconds
	Clients       uint32  `json:"clients"`
	Waiting       uint32  `json:"waiting"`
	MaxWaitMs     float64 `json:"max_wait_ms"`
	MaxClientConn uint32  `json:"max_client_conn"`
}

type PoolRow struct {
	Database    string  `json:"database"`
	User        string  `json:"user"`
	Mode        string  `json:"mode"`
	ClActive    uint32  `json:"cl_active"`
	ClWaiting   uint32  `json:"cl_waiting"`
	SvActive    uint32  `json:"sv_active"`
	SvIdle      uint32  `json:"sv_idle"`
	SvUsed      uint32  `json:"sv_used"`
	SvTested    uint32  `json:"sv_tested"`
	SvLogin     uint32  `json:"sv_login"`
	MaxWaitMs   float64 `json:"max_wait_ms"`
	PoolSize    uint32  `json:"pool_size"`
	ReservePool uint32  `json:"reserve_pool"`
	Paused      bool    `json:"paused"`
	Disabled    bool    `json:"disabled"`
}

type LatestPools struct {
	Time  time.Time `json:"time"`
	Pools []PoolRow `json:"pools"`
}

// PoolStatsRow is what went through one database of a pooler over the window, with the averages that say what a pool costs the application:
// how long a transaction took, and how long its client waited for a server connection first. Over a window the counts (taken when a
// transaction arrives) and the times (taken when it finishes) agree; over a single short sample they may fall in different ones.
type PoolStatsRow struct {
	Database  string  `json:"database"`
	Xacts     uint64  `json:"xacts"`
	Queries   uint64  `json:"queries"`
	BytesIn   uint64  `json:"bytes_in"`
	BytesOut  uint64  `json:"bytes_out"`
	XactMs    float64 `json:"xact_ms"`
	QueryMs   float64 `json:"query_ms"`
	WaitMs    float64 `json:"wait_ms"`
	AvgXactMs float64 `json:"avg_xact_ms"`
	AvgWaitMs float64 `json:"avg_wait_ms"`
	WaitShare float64 `json:"wait_share"` // of the time clients spent on a transaction, the share that was spent waiting for a server connection
}

type PoolsResult struct {
	Points []PoolPoint    `json:"points"`
	Latest *LatestPools   `json:"latest"`
	Stats  []PoolStatsRow `json:"stats"`
}

// PoolSeries: the PEAK of the totals in each `step` seconds (a queue that lasts a few seconds must not be averaged away), the pools as they
// were in the newest sample of the window, and what went through each database over the whole window.
func (s *Store) PoolSeries(ctx context.Context, project, target string, from, to time.Time, step int) (PoolsResult, error) {
	res := PoolsResult{Points: []PoolPoint{}, Stats: []PoolStatsRow{}}
	rows, err := s.conn.Query(ctx, `SELECT toInt64(toUnixTimestamp(toStartOfInterval(Time, INTERVAL ? SECOND))) * 1000, max(Clients), max(Waiting), max(MaxWaitMs), max(MaxClientConn)
FROM db_pool_samples WHERE Project = ? AND Target = ? AND Time >= ? AND Time < ? GROUP BY 1 ORDER BY 1`, step, project, target, from, to)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var p PoolPoint
		if err := rows.Scan(&p.T, &p.Clients, &p.Waiting, &p.MaxWaitMs, &p.MaxClientConn); err != nil {
			rows.Close()
			return res, err
		}
		res.Points = append(res.Points, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return res, err
	}
	rows.Close()

	// The pools of the newest sample that has any (a pooler with nothing connected lists no pool at all). The sample is found by a subquery and
	// not by a time read back and sent again: a time that has made the round trip through a Go time.Time and a query parameter has lost its
	// milliseconds and matches nothing.
	pr, err := s.conn.Query(ctx, `SELECT Time, Database, User, Mode, ClActive, ClWaiting, SvActive, SvIdle, SvUsed, SvTested, SvLogin, MaxWaitMs, PoolSize, ReservePool, Paused, Disabled
FROM db_pools WHERE Project = ? AND Target = ? AND Time = (SELECT max(Time) FROM db_pools WHERE Project = ? AND Target = ? AND Time >= ? AND Time < ?)
ORDER BY ClWaiting DESC, SvActive DESC, Database, User`, project, target, project, target, from, to)
	if err != nil {
		return res, err
	}
	for pr.Next() {
		var r PoolRow
		var at time.Time
		var paused, disabled uint8
		if err := pr.Scan(&at, &r.Database, &r.User, &r.Mode, &r.ClActive, &r.ClWaiting, &r.SvActive, &r.SvIdle, &r.SvUsed, &r.SvTested, &r.SvLogin, &r.MaxWaitMs, &r.PoolSize, &r.ReservePool, &paused, &disabled); err != nil {
			pr.Close()
			return res, err
		}
		r.Paused, r.Disabled = paused == 1, disabled == 1
		if res.Latest == nil {
			res.Latest = &LatestPools{Time: at.UTC(), Pools: []PoolRow{}}
		}
		res.Latest.Pools = append(res.Latest.Pools, r)
	}
	err = pr.Err()
	pr.Close()
	if err != nil {
		return res, err
	}

	sr, err := s.conn.Query(ctx, `SELECT Database, sum(Xacts), sum(Queries), sum(BytesIn), sum(BytesOut), sum(XactMs), sum(QueryMs), sum(WaitMs)
FROM db_pool_stats WHERE Project = ? AND Target = ? AND Time >= ? AND Time < ? GROUP BY Database ORDER BY sum(WaitMs) DESC, Database`, project, target, from, to)
	if err != nil {
		return res, err
	}
	defer sr.Close()
	for sr.Next() {
		var r PoolStatsRow
		if err := sr.Scan(&r.Database, &r.Xacts, &r.Queries, &r.BytesIn, &r.BytesOut, &r.XactMs, &r.QueryMs, &r.WaitMs); err != nil {
			return res, err
		}
		if r.Xacts > 0 {
			r.AvgXactMs = r.XactMs / float64(r.Xacts)
			r.AvgWaitMs = r.WaitMs / float64(r.Xacts)
		}
		if t := r.XactMs + r.WaitMs; t > 0 {
			r.WaitShare = r.WaitMs / t
		}
		res.Stats = append(res.Stats, r)
	}
	return res, sr.Err()
}
