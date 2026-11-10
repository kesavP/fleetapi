//go:build integration

// Runs the PostgreSQL engine against a real server (PG_DSN: the collector's own role; PG_ADMIN_DSN: a superuser the "application" uses).
// hack/platform-mvp/test-db-collector.sh starts the servers, one per version, and runs this in two phases: the first does the work and
// leaves a marker in the server's log; the second (PG_LOG set) reads that log, which only the server could have written.
package postgres_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sojoner/kro-lab/platform-mvp/db-collector/collector"
	"github.com/sojoner/kro-lab/platform-mvp/db-collector/postgres"
)

func env(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Skipf("%s not set; run hack/platform-mvp/test-db-collector.sh", k)
	}
	return v
}

func admin(t *testing.T) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), env(t, "PG_ADMIN_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

func open(t *testing.T, slowLogPath string) collector.Source {
	t.Helper()
	e, err := collector.Lookup("postgres")
	if err != nil {
		t.Fatal(err)
	}
	src, err := e.Open(context.Background(), collector.TargetConfig{Name: "it", Engine: "postgres", DSN: env(t, "PG_DSN"), SlowLog: collector.SlowLogConfig{Enabled: slowLogPath != "", Path: slowLogPath}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(src.Close)
	return src
}

var ctx = context.Background()

func TestCapabilitiesOfAWellConfiguredServer(t *testing.T) {
	src := open(t, "")
	caps, err := src.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("PostgreSQL %s: %+v", caps.Version, caps)
	if caps.Engine != "postgres" || caps.Version == "" {
		t.Fatalf("%+v", caps)
	}
	if !caps.Statements.Available || len(caps.Statements.Problems) != 0 {
		t.Fatalf("statements: %+v", caps.Statements)
	}
	if !caps.Activity.Available || len(caps.Activity.Problems) != 0 {
		t.Fatalf("activity: %+v", caps.Activity)
	}
	if caps.SlowLog.Available || len(caps.SlowLog.Problems) == 0 {
		t.Fatalf("a target with no slow log configured must say so: %+v", caps.SlowLog)
	}
	if !strings.Contains(caps.Settings["shared_preload_libraries"], "pg_stat_statements") || caps.Settings["log_min_duration_statement"] != "100ms" {
		t.Fatalf("%v", caps.Settings)
	}
}

func TestCapabilitiesSayWhatToChange(t *testing.T) {
	// a log path that is not there, and a slow log that is asked for
	src := open(t, "/nonexistent/postgresql.log")
	caps, err := src.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if caps.SlowLog.Available || !strings.Contains(strings.Join(caps.SlowLog.Problems, " "), "/nonexistent/postgresql.log") {
		t.Fatalf("%+v", caps.SlowLog)
	}
}

func TestTheStatementsOfARealServer(t *testing.T) {
	src := open(t, "").(collector.StatementSource)
	app := admin(t)
	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := app.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	mustExec("DROP TABLE IF EXISTS collector_it")
	mustExec("CREATE TABLE collector_it (id int primary key, email text)")

	var d collector.Differ
	base, err := src.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, info := d.Diff(base); !info.Primed {
		t.Fatal("the first snapshot is a baseline")
	}
	for i := 0; i < 25; i++ {
		mustExec("INSERT INTO collector_it VALUES ($1, $2)", i, "person"+string(rune('a'+i))+"@secret-example.com")
	}
	for i := 0; i < 10; i++ {
		var n int
		if err := app.QueryRow(ctx, "SELECT count(*) FROM collector_it WHERE email = $1", "alice@secret-example.com").Scan(&n); err != nil {
			t.Fatal(err)
		}
	}
	mustExec("CREATE ROLE collector_leaky PASSWORD 'hunter2-secret-password'")
	mustExec("DROP ROLE collector_leaky")

	snap, err := src.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out, info := d.Diff(snap)
	t.Logf("%d statements in the interval: %+v", len(out), info)

	var insert, selectQ *collector.Statement
	for i := range out {
		s := &out[i]
		switch {
		case strings.HasPrefix(s.Query, "INSERT INTO collector_it"):
			insert = s
		case strings.HasPrefix(s.Query, "SELECT count(*) FROM collector_it"):
			selectQ = s
		}
		if strings.Contains(s.Query, "secret") {
			t.Fatalf("a literal left the collector: %q", s.Query)
		}
		if strings.Contains(s.Query, "db-collector") || strings.Contains(s.Query, "pg_stat_statements") && s.User == "db_collector" {
			t.Fatalf("the collector reported its own statement: %q", s.Query)
		}
	}
	if insert == nil || insert.Calls != 25 || insert.Rows != 25 || insert.TotalMs <= 0 || insert.Database == "" || insert.User == "" || insert.QueryID == "" {
		t.Fatalf("insert: %+v", insert)
	}
	if selectQ == nil || selectQ.Calls != 10 || selectQ.Rows != 10 {
		t.Fatalf("select: %+v", selectQ)
	}
	if len(insert.Extra) == 0 {
		t.Fatalf("no buffer counters: %+v", insert)
	}

	// nothing ran: nothing is reported (the collector's own reads are not counted as work)
	snap, _ = src.Snapshot(ctx)
	out, _ = d.Diff(snap)
	for _, s := range out {
		if strings.Contains(s.Query, "collector_it") {
			t.Fatalf("a statement that did not run again is in the interval: %+v", s)
		}
	}

	// a reset of the statistics: the counters go back to zero, and the next interval is what ran since, not a negative number
	mustExec("SELECT pg_stat_statements_reset()")
	for i := 0; i < 3; i++ {
		mustExec("SELECT count(*) FROM collector_it WHERE id = $1", i)
	}
	snap, _ = src.Snapshot(ctx)
	out, info = d.Diff(snap)
	found := false
	for _, s := range out {
		if strings.HasPrefix(s.Query, "SELECT count(*) FROM collector_it WHERE id =") {
			found = true
			if s.Calls != 3 {
				t.Fatalf("after a reset: %+v", s)
			}
		}
	}
	if !found {
		t.Fatalf("nothing after the reset: %+v %+v", out, info)
	}
	mustExec("DROP TABLE collector_it")
}

func TestTheSessionsOfARealServer(t *testing.T) {
	src := open(t, "").(collector.ActivitySource)
	app := admin(t)
	if _, err := app.Exec(ctx, "DROP TABLE IF EXISTS collector_lock; CREATE TABLE collector_lock (id int primary key, v text); INSERT INTO collector_lock VALUES (1, 'a')"); err != nil {
		t.Fatal(err)
	}
	defer app.Exec(ctx, "DROP TABLE IF EXISTS collector_lock")

	// one session sits in a transaction holding a row; another waits for that row; a third just runs something long
	holder, _ := pgx.Connect(ctx, env(t, "PG_ADMIN_DSN"))
	defer holder.Close(ctx)
	waiter, _ := pgx.Connect(ctx, env(t, "PG_ADMIN_DSN"))
	defer waiter.Close(ctx)
	sleeper, _ := pgx.Connect(ctx, env(t, "PG_ADMIN_DSN"))
	defer sleeper.Close(ctx)
	if _, err := holder.Exec(ctx, "BEGIN; SELECT * FROM collector_lock WHERE id = 1 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = waiter.Exec(ctx, "UPDATE collector_lock SET v = 'secret-value-b' WHERE id = 1")
	}()
	go func() { defer wg.Done(); _, _ = sleeper.Exec(ctx, "SELECT pg_sleep(3), 'secret-sleeper'") }()
	time.Sleep(1700 * time.Millisecond) // past the one second a session needs to be named

	a, err := src.Sample(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", a)
	if a.MaxConnections <= 0 || a.Total < 4 {
		t.Fatalf("%+v", a)
	}
	if a.ByState["idle in transaction"] < 1 || a.ByState["active"] < 2 {
		t.Fatalf("%v", a.ByState)
	}
	if a.Blocked != 1 {
		t.Fatalf("the waiting update: %+v", a)
	}
	lock := 0
	for k, n := range a.Waiting {
		if strings.HasPrefix(k, "Lock:") {
			lock += n
		}
	}
	if lock != 1 {
		t.Fatalf("waiting: %v", a.Waiting)
	}
	if a.LongestQueryMs < 1000 || a.LongestIdleXMs < 1000 || a.LongestXactMs < 1000 {
		t.Fatalf("%v %v %v", a.LongestQueryMs, a.LongestIdleXMs, a.LongestXactMs)
	}
	var blocker, blocked *collector.Session
	for i := range a.Sessions {
		s := &a.Sessions[i]
		if len(s.BlockedBy) > 0 {
			blocked = s
		}
		if s.State == "idle in transaction" {
			blocker = s
		}
		if strings.Contains(s.Query, "secret") {
			t.Fatalf("a literal in a session's text: %q", s.Query)
		}
		if strings.Contains(s.Query, "db-collector") {
			t.Fatal("the collector listed itself")
		}
	}
	if blocker == nil || blocked == nil {
		t.Fatalf("the blocker and the blocked are named: %+v", a.Sessions)
	}
	if len(blocked.BlockedBy) != 1 || blocked.BlockedBy[0] != blocker.PID {
		t.Fatalf("%v should be blocked by %d", blocked.BlockedBy, blocker.PID)
	}
	if a.Sessions[0].PID != blocker.PID {
		t.Fatalf("the session that blocks others comes first: %+v", a.Sessions[0])
	}
	if !strings.Contains(blocked.Query, "UPDATE collector_lock SET v = ?") {
		t.Fatalf("%q", blocked.Query)
	}
	_, _ = holder.Exec(ctx, "ROLLBACK")
	wg.Wait()
}

// The slow work of the first phase: statements the server logs because they take longer than log_min_duration_statement, and a statement
// that carries a secret in its text and in its parameters.
func TestMakeSlowStatements(t *testing.T) {
	if os.Getenv("PG_LOG") != "" {
		t.Skip("the second phase reads the log")
	}
	app := admin(t)
	if _, err := app.Exec(ctx, "SELECT pg_sleep(0.3), 'marker-simple-secret-7421'"); err != nil {
		t.Fatal(err)
	}
	// the extended protocol: "execute <unnamed>", with the parameters in a DETAIL line
	if _, err := app.Exec(ctx, "SELECT pg_sleep($1::float8), $2::text", 0.3, "marker-param-secret-9908"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Exec(ctx, "SET log_min_duration_statement = 0"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Exec(ctx, "CREATE ROLE collector_pw_it PASSWORD 'hunter2-marker-password'"); err != nil {
		t.Fatal(err)
	}
	_, _ = app.Exec(ctx, "DROP ROLE collector_pw_it")
}

func TestReadTheServersOwnLog(t *testing.T) {
	path := env(t, "PG_LOG")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	p := &postgres.LogParser{}
	var got []collector.SlowQuery
	for _, line := range strings.Split(string(raw), "\n") {
		p.Feed(strings.TrimSuffix(line, "\r"), func(q collector.SlowQuery) { got = append(got, q) })
	}
	p.Flush(func(q collector.SlowQuery) { got = append(got, q) })
	t.Logf("%d slow statements in the log", len(got))

	var simple, extended, role bool
	for _, q := range got {
		for _, secret := range []string{"marker-simple-secret", "marker-param-secret", "hunter2"} {
			if strings.Contains(q.Query, secret) {
				t.Fatalf("a secret that the server logged reached a slow query: %q", q.Query)
			}
		}
		switch {
		case q.Kind == "statement" && strings.HasPrefix(q.Query, "SELECT pg_sleep(?), ?"):
			simple = q.DurationMs >= 300 && q.PID > 0 && !q.Time.IsZero()
		case q.Kind == "execute" && strings.HasPrefix(q.Query, "SELECT pg_sleep($1::float8), $2::text"):
			extended = q.DurationMs >= 300
		case strings.HasPrefix(q.Query, "CREATE ROLE collector_pw_it PASSWORD ?"):
			role = true
		}
		if strings.Contains(q.Query, "db-collector") {
			t.Fatalf("the collector's own statement was reported as slow: %q", q.Query)
		}
	}
	if !simple {
		t.Error("the simple-protocol slow statement was not found, or had no duration, pid or time")
	}
	if !extended {
		t.Error("the extended-protocol slow statement (execute) was not found")
	}
	if !role {
		t.Error("the CREATE ROLE statement was not found, or its password was not removed")
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "DETAIL:  parameters:") && strings.Contains(line, "marker-param-secret") {
			return // the server did log the parameters: and none of it was read
		}
	}
	t.Log("note: the server did not log the parameters this time")
}
