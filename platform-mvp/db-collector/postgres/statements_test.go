package postgres

import (
	"strings"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/db-collector/collector"
)

// The columns of pg_stat_statements on real servers, as the catalog reported them.
const (
	cols13 = "userid,dbid,queryid,query,plans,total_plan_time,min_plan_time,max_plan_time,mean_plan_time,stddev_plan_time,calls,total_exec_time,min_exec_time,max_exec_time,mean_exec_time,stddev_exec_time,rows,shared_blks_hit,shared_blks_read,shared_blks_dirtied,shared_blks_written,local_blks_hit,local_blks_read,local_blks_dirtied,local_blks_written,temp_blks_read,temp_blks_written,blk_read_time,blk_write_time,wal_records,wal_fpi,wal_bytes"
	cols16 = "userid,dbid,toplevel,queryid,query,plans,total_plan_time,min_plan_time,max_plan_time,mean_plan_time,stddev_plan_time,calls,total_exec_time,min_exec_time,max_exec_time,mean_exec_time,stddev_exec_time,rows,shared_blks_hit,shared_blks_read,shared_blks_dirtied,shared_blks_written,local_blks_hit,local_blks_read,local_blks_dirtied,local_blks_written,temp_blks_read,temp_blks_written,blk_read_time,blk_write_time,temp_blk_read_time,temp_blk_write_time,wal_records,wal_fpi,wal_bytes,jit_functions"
	cols17 = "userid,dbid,toplevel,queryid,query,plans,total_plan_time,calls,total_exec_time,min_exec_time,rows,shared_blks_hit,shared_blks_read,shared_blks_dirtied,shared_blks_written,temp_blks_read,temp_blks_written,shared_blk_read_time,shared_blk_write_time,local_blk_read_time,wal_records,wal_fpi,wal_bytes,stats_since,minmax_stats_since"
	// PostgreSQL 12 and older: one time column, and the view is smaller
	cols12 = "userid,dbid,queryid,query,calls,total_time,min_time,max_time,mean_time,stddev_time,rows,shared_blks_hit,shared_blks_read,shared_blks_dirtied,shared_blks_written,local_blks_hit,temp_blks_read,temp_blks_written,blk_read_time,blk_write_time"
)

func set(csv string) map[string]bool {
	m := map[string]bool{}
	for _, c := range strings.Split(csv, ",") {
		m[c] = true
	}
	return m
}

func TestTheQueryFollowsTheColumnsOfTheServer(t *testing.T) {
	cases := []struct {
		name, cols string
		has        []string
		hasNot     []string
		extra      []string
	}{
		{"13", cols13, []string{"s.total_exec_time", "s.total_plan_time", "s.blk_read_time", "'t'", "''"}, []string{"toplevel", "stats_since", "shared_blk_read_time", "s.total_time"},
			[]string{"shared_blks_hit", "shared_blks_read", "shared_blks_dirtied", "shared_blks_written", "temp_blks_read", "temp_blks_written", "wal_bytes", "total_plan_time", "shared_blk_read_time", "shared_blk_write_time"}},
		{"16", cols16, []string{"s.toplevel::text", "s.total_exec_time"}, []string{"stats_since", "shared_blk_read_time"}, nil},
		{"17", cols17, []string{"s.toplevel::text", "s.stats_since::text", "s.shared_blk_read_time", "s.total_exec_time"}, []string{"s.blk_read_time", "s.total_time"}, nil},
		{"12", cols12, []string{"s.total_time::float8"}, []string{"total_exec_time", "toplevel", "wal_bytes", "total_plan_time"}, nil},
	}
	for _, c := range cases {
		sql, names, ok := StatementsQuery(set(c.cols), 4096)
		if !ok {
			t.Errorf("%s: no query", c.name)
			continue
		}
		for _, h := range c.has {
			if !strings.Contains(sql, h) {
				t.Errorf("%s: missing %q in\n%s", c.name, h, sql)
			}
		}
		for _, h := range c.hasNot {
			if strings.Contains(sql, h) {
				t.Errorf("%s: should not select %q in\n%s", c.name, h, sql)
			}
		}
		if c.extra != nil {
			if strings.Join(names, ",") != strings.Join(c.extra, ",") {
				t.Errorf("%s: extras %v", c.name, names)
			}
		}
		if !strings.HasPrefix(sql, "/* db-collector */") {
			t.Errorf("%s: the query does not carry the marker, so it would be reported as a slow query of the application", c.name)
		}
		if !strings.Contains(sql, "NOT LIKE '/* db-collector */%'") {
			t.Errorf("%s: it would read its own statements back", c.name)
		}
		// the number of selected expressions is the number of values a row will have: 9 fixed, then the extras
		want := 9 + len(names)
		if got := strings.Count(strings.SplitN(strings.TrimPrefix(sql, "/* db-collector */ SELECT "), " FROM ", 2)[0], ", ") + 1; got < want-3 {
			t.Errorf("%s: %d columns, wanted about %d", c.name, got, want)
		}
	}
}

func TestOneNameForTheSameCounterAcrossVersions(t *testing.T) {
	_, n13, _ := StatementsQuery(set(cols13), 100)
	_, n17, _ := StatementsQuery(set(cols17), 100)
	has := func(names []string, n string) int {
		c := 0
		for _, x := range names {
			if x == n {
				c++
			}
		}
		return c
	}
	for _, names := range [][]string{n13, n17} {
		if has(names, "shared_blk_read_time") != 1 {
			t.Errorf("the I/O time must be reported once, under the newest name: %v", names)
		}
		if has(names, "blk_read_time") != 0 {
			t.Errorf("an old name leaked: %v", names)
		}
	}
}

func TestAViewThatLacksWhatIsNeededGivesNoQuery(t *testing.T) {
	for _, drop := range []string{"queryid", "query", "calls", "rows", "total_exec_time"} {
		cols := set(cols16)
		delete(cols, drop)
		if _, _, ok := StatementsQuery(cols, 100); ok {
			t.Errorf("a view without %s gave a query", drop)
		}
	}
	if _, _, ok := StatementsQuery(map[string]bool{}, 100); ok {
		t.Error("no columns at all gave a query")
	}
}

func TestSnapshotRowsAreNormalizedKeyedAndSorted(t *testing.T) {
	rows := []statementRow{
		{Database: "shop", User: "app", QueryID: "2", Query: "SELECT * FROM users WHERE email = 'alice@example.com' AND id = 7", Calls: 10, TotalMs: 55.5, Rows: 10, TopLevel: "true", Extra: []float64{100, 0, 4}},
		{Database: "shop", User: "app", QueryID: "1", Query: "CREATE ROLE x PASSWORD 'hunter2'", Calls: 1, TotalMs: 1, Rows: 0, TopLevel: "true", Since: "2026-01-01 00:00:00+00"},
		{Database: "shop", User: "app", QueryID: "2", Query: "SELECT * FROM users WHERE email = 'alice@example.com' AND id = 7", Calls: 3, TotalMs: 3, Rows: 3, TopLevel: "false"},
	}
	s := BuildSnapshot(rows, []string{"shared_blks_hit", "shared_blks_read", "temp_blks_written"}, "epoch-1")
	if s.Epoch != "epoch-1" || len(s.Stats) != 3 {
		t.Fatalf("%+v", s)
	}
	keys := map[string]bool{}
	for _, c := range s.Stats {
		keys[c.Key] = true
		if strings.Contains(c.Query, "alice") || strings.Contains(c.Query, "hunter2") {
			t.Fatalf("a literal reached the snapshot: %q", c.Query)
		}
	}
	if len(keys) != 3 {
		t.Fatal("the same query at top level and nested is two statements: they must not share a key")
	}
	var users collector.Cumulative
	for _, c := range s.Stats {
		if c.QueryID == "2" && c.Calls == 10 {
			users = c
		}
	}
	if users.Query != "SELECT * FROM users WHERE email = ? AND id = ?" || users.TotalMs != 55.5 || users.Rows != 10 {
		t.Fatalf("%+v", users)
	}
	if len(users.Extra) != 2 || users.Extra["shared_blks_hit"] != 100 || users.Extra["temp_blks_written"] != 4 {
		t.Fatalf("a counter that is zero is left out: %v", users.Extra)
	}
	for i := 1; i < len(s.Stats); i++ {
		if s.Stats[i-1].Key > s.Stats[i].Key {
			t.Fatal("not in a fixed order")
		}
	}
	for _, c := range s.Stats {
		if c.QueryID == "1" && c.Reset != "2026-01-01 00:00:00+00" {
			t.Fatalf("the reset token is lost: %+v", c)
		}
	}
}

func TestNegativeAndHugeCountersAreSafe(t *testing.T) {
	s := BuildSnapshot([]statementRow{{QueryID: "1", Query: "SELECT 1", Calls: -5, Rows: -1, TotalMs: 1}, {QueryID: "2", Query: "SELECT 2", Calls: 1e19, Rows: 3}}, nil, "")
	if s.Stats[0].Calls != 0 || s.Stats[0].Rows != 0 {
		t.Fatalf("%+v", s.Stats[0])
	}
}

// ---- activity ----

func tm(base time.Time, ago time.Duration) *time.Time { t := base.Add(-ago); return &t }

func TestBuildActivity(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	rows := []activityRow{
		{Now: now, PID: 1, Database: "shop", User: "app", State: "active", QueryStart: tm(now, 5*time.Second), XactStart: tm(now, 6*time.Second), Query: "SELECT * FROM orders WHERE id = 42"},
		{Now: now, PID: 2, Database: "shop", User: "app", State: "active", QueryStart: tm(now, 20*time.Millisecond), Query: "SELECT 1"},
		{Now: now, PID: 3, Database: "shop", User: "app", State: "active", WaitType: "Lock", Wait: "relation", QueryStart: tm(now, 3*time.Second), BlockedBy: []int64{4}, Query: "UPDATE orders SET s = 'x' WHERE id = 9"},
		{Now: now, PID: 4, Database: "shop", User: "worker", State: "idle in transaction", XactStart: tm(now, 40*time.Second), StateChange: tm(now, 30*time.Second), Query: "SELECT * FROM orders WHERE id = 9 FOR UPDATE"},
		{Now: now, PID: 5, State: "idle", Query: "SELECT secret FROM vault WHERE k = 'abc'"},
		{Now: now, PID: 6, State: "idle"},
		{Now: now, PID: 7, State: "", Query: ""},
		{Now: now, PID: 8, State: "active", WaitType: "LWLock", Wait: "WALWrite", QueryStart: tm(now, 50*time.Millisecond), Query: "INSERT INTO t VALUES (1)"},
	}
	a := BuildActivity(rows, 100, 10)
	if a.Total != 8 || a.MaxConnections != 100 {
		t.Fatalf("%+v", a)
	}
	if a.ByState["active"] != 4 || a.ByState["idle"] != 2 || a.ByState["idle in transaction"] != 1 || a.ByState["unknown"] != 1 {
		t.Fatalf("%v", a.ByState)
	}
	if a.Waiting["Lock:relation"] != 1 || a.Waiting["LWLock:WALWrite"] != 1 || len(a.Waiting) != 2 {
		t.Fatalf("%v", a.Waiting)
	}
	if a.Blocked != 1 {
		t.Fatalf("blocked %d", a.Blocked)
	}
	if a.LongestQueryMs != 5000 || a.LongestXactMs != 40000 || a.LongestIdleXMs != 30000 {
		t.Fatalf("%v %v %v", a.LongestQueryMs, a.LongestXactMs, a.LongestIdleXMs)
	}
	// named: the one that blocks (4) first, then the long and the blocked; not the quick, the idle or the unknown
	var pids []int64
	for _, s := range a.Sessions {
		pids = append(pids, s.PID)
	}
	if len(pids) != 3 || pids[0] != 4 {
		t.Fatalf("%v", pids)
	}
	got := map[int64]collector.Session{}
	for _, s := range a.Sessions {
		got[s.PID] = s
	}
	if got[3].BlockedBy[0] != 4 || got[3].Wait != "relation" || got[3].QueryMs != 3000 {
		t.Fatalf("%+v", got[3])
	}
	if got[4].QueryMs != 30000 || got[4].XactMs != 40000 || got[4].Query != "SELECT * FROM orders WHERE id = ? FOR UPDATE" {
		t.Fatalf("%+v", got[4])
	}
	if got[1].Query != "SELECT * FROM orders WHERE id = ?" {
		t.Fatalf("a session's text is normalized: %q", got[1].Query)
	}
	for _, s := range a.Sessions {
		if strings.Contains(s.Query, "'x'") || strings.Contains(s.Query, "42") {
			t.Fatalf("a literal reached the sample: %q", s.Query)
		}
	}
}

func TestAQuickSessionThatBlocksIsStillNamed(t *testing.T) {
	now := time.Now()
	a := BuildActivity([]activityRow{
		{Now: now, PID: 10, State: "active", QueryStart: tm(now, time.Millisecond), Query: "LOCK TABLE t"},
		{Now: now, PID: 11, State: "active", QueryStart: tm(now, time.Millisecond), BlockedBy: []int64{10}, Query: "SELECT 1"},
	}, 0, 10)
	if len(a.Sessions) != 2 || a.Sessions[0].PID != 10 {
		t.Fatalf("%+v", a.Sessions)
	}
}

func TestTheSessionListIsCappedAndTheCountsAreNot(t *testing.T) {
	now := time.Now()
	var rows []activityRow
	for i := 0; i < 200; i++ {
		rows = append(rows, activityRow{Now: now, PID: int64(i + 1), State: "active", QueryStart: tm(now, time.Duration(2+i)*time.Second), Query: "SELECT 1"})
	}
	a := BuildActivity(rows, 0, 5)
	if len(a.Sessions) != 5 || a.Total != 200 || a.ByState["active"] != 200 {
		t.Fatalf("%d named, %d counted", len(a.Sessions), a.Total)
	}
	if a.Sessions[0].PID != 200 { // the longest running first
		t.Fatalf("%v", a.Sessions[0].PID)
	}
	if capped := BuildActivity(rows, 0, 100000); len(capped.Sessions) != collector.MaxSessions {
		t.Fatalf("%d", len(capped.Sessions))
	}
}

func TestSessionsTheRoleMayNotSeeHaveNoText(t *testing.T) {
	now := time.Now()
	a := BuildActivity([]activityRow{{Now: now, PID: 1, State: "active", QueryStart: tm(now, 5*time.Second), Query: insufficient}}, 0, 5)
	if len(a.Sessions) != 1 || a.Sessions[0].Query != "" {
		t.Fatalf("%+v", a.Sessions)
	}
}

func TestEmptyActivity(t *testing.T) {
	a := BuildActivity(nil, 0, 5)
	if a.Total != 0 || a.ByState == nil || a.Waiting == nil || a.Sessions != nil && len(a.Sessions) != 0 {
		t.Fatalf("%+v", a)
	}
}

func TestActivityQueryCarriesTheMarkerAndTheBound(t *testing.T) {
	q := ActivityQuery(4096)
	if !strings.HasPrefix(q, "/* db-collector */") || !strings.Contains(q, "left(COALESCE(a.query, ''), 4096)") || !strings.Contains(q, "pg_backend_pid()") {
		t.Fatal(q)
	}
}
