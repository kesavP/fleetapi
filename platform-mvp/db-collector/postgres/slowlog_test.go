package postgres

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/db-collector/collector"
)

var fixedNow = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

func parseAll(lines ...string) []collector.SlowQuery {
	p := &LogParser{Now: func() time.Time { return fixedNow }}
	var out []collector.SlowQuery
	emit := func(q collector.SlowQuery) { out = append(out, q) }
	for _, l := range lines {
		p.Feed(l, emit)
	}
	p.Flush(emit)
	return out
}

// Lines as a real PostgreSQL 16 wrote them (see the captured log in the package documentation).
func TestParsesWhatARealServerWrote(t *testing.T) {
	got := parseAll(
		"2026-11-10 06:18:07.627 UTC [107] LOG:  duration: 301.604 ms  statement: SELECT pg_sleep(0.15), 'x'",
		"\t  FROM generate_series(1,2)",
		"\t  WHERE 1=1",
		"2026-11-10 06:18:07.780 UTC [114] LOG:  duration: 8.176 ms  statement: SET log_min_duration_statement=0; PREPARE p AS SELECT $1::int + 1; EXECUTE p(41); CREATE ROLE leaky PASSWORD 'hunter2'; SELECT 'it''s' AS a",
		"2026-11-10 06:18:07.780 UTC [114] DETAIL:  prepare: SET log_min_duration_statement=0; PREPARE p AS SELECT $1::int + 1; EXECUTE p(41); CREATE ROLE leaky PASSWORD 'hunter2'; SELECT 'it''s' AS a",
	)
	if len(got) != 2 {
		t.Fatalf("%d: %+v", len(got), got)
	}
	a, b := got[0], got[1]
	if a.DurationMs != 301.604 || a.Kind != "statement" || a.PID != 107 || !a.Time.Equal(time.Date(2026, 11, 10, 6, 18, 7, 627_000_000, time.UTC)) {
		t.Fatalf("%+v", a)
	}
	if a.Query != "SELECT pg_sleep(?), ? FROM generate_series(?,?) WHERE ?=?" {
		t.Fatalf("a statement of several lines is one: %q", a.Query)
	}
	if strings.Contains(b.Query, "hunter2") || strings.Contains(b.Query, "it") && strings.Contains(b.Query, "'s") {
		t.Fatalf("a secret reached the result: %q", b.Query)
	}
	if !strings.Contains(b.Query, "PASSWORD ?") {
		t.Fatalf("%q", b.Query)
	}
}

func TestOnlyStatementsAndExecutesAreSlowQueries(t *testing.T) {
	got := parseAll(
		"2026-01-01 10:00:00.000 UTC [1] LOG:  duration: 5.0 ms  parse <unnamed>: SELECT 1",
		"2026-01-01 10:00:00.001 UTC [1] LOG:  duration: 6.0 ms  bind <unnamed>: SELECT 1",
		"2026-01-01 10:00:00.002 UTC [1] LOG:  duration: 7.0 ms  execute <unnamed>: SELECT * FROM t WHERE id = $1",
		"2026-01-01 10:00:00.003 UTC [1] DETAIL:  parameters: $1 = 'alice@example.com'",
		"2026-01-01 10:00:00.004 UTC [1] LOG:  duration: 8.0 ms  execute S_1: SELECT 2",
		"2026-01-01 10:00:00.005 UTC [1] LOG:  duration: 1.2 ms",
		"2026-01-01 10:00:00.006 UTC [1] LOG:  duration: 9.0 ms  plan:",
		"\tQuery Text: SELECT secret FROM t",
		"\tSeq Scan on t",
		"2026-01-01 10:00:00.007 UTC [1] LOG:  checkpoint starting: time",
		"2026-01-01 10:00:00.008 UTC [1] ERROR:  relation \"x\" does not exist",
		"2026-01-01 10:00:00.009 UTC [1] STATEMENT:  SELECT * FROM x WHERE email = 'bob@example.com'",
	)
	if len(got) != 2 || got[0].Kind != "execute" || got[0].DurationMs != 7 || got[1].DurationMs != 8 {
		t.Fatalf("%+v", got)
	}
	for _, q := range got {
		if strings.Contains(q.Query, "alice") || strings.Contains(q.Query, "bob") || strings.Contains(q.Query, "secret") {
			t.Fatalf("%q", q.Query)
		}
	}
}

func TestDifferentPrefixes(t *testing.T) {
	cases := []struct {
		name, line string
		user, db   string
		pid        int64
		when       time.Time
	}{
		{"the default", "2026-11-10 06:18:07.627 UTC [107] LOG:  duration: 1.5 ms  statement: SELECT 1", "", "", 107, time.Date(2026, 11, 10, 6, 18, 7, 627_000_000, time.UTC)},
		{"%t with seconds only", "2026-11-10 06:18:07 UTC [9] LOG:  duration: 1.5 ms  statement: SELECT 1", "", "", 9, time.Date(2026, 11, 10, 6, 18, 7, 0, time.UTC)},
		{"user@db", "2026-11-10 06:18:07.627 UTC [107] app@shop LOG:  duration: 1.5 ms  statement: SELECT 1", "app", "shop", 107, time.Date(2026, 11, 10, 6, 18, 7, 627_000_000, time.UTC)},
		{"user= and db=", "2026-11-10 06:18:07.627 UTC [107]: user=app,db=shop LOG:  duration: 1.5 ms  statement: SELECT 1", "app", "shop", 107, time.Date(2026, 11, 10, 6, 18, 7, 627_000_000, time.UTC)},
		{"unknown user", "2026-11-10 06:18:07.627 UTC [107] [unknown]@[unknown] LOG:  duration: 1.5 ms  statement: SELECT 1", "", "", 107, time.Date(2026, 11, 10, 6, 18, 7, 627_000_000, time.UTC)},
		{"a numeric offset", "2026-11-10 08:18:07.627 +02 [5] LOG:  duration: 1.5 ms  statement: SELECT 1", "", "", 5, time.Date(2026, 11, 10, 6, 18, 7, 627_000_000, time.UTC)},
		{"an offset with minutes", "2026-11-10 11:48:07.000 +0530 [5] LOG:  duration: 1.5 ms  statement: SELECT 1", "", "", 5, time.Date(2026, 11, 10, 6, 18, 7, 0, time.UTC)},
		{"a zone name that says no offset", "2026-11-10 08:18:07.627 CEST [5] LOG:  duration: 1.5 ms  statement: SELECT 1", "", "", 5, fixedNow},
		{"no prefix at all", "LOG:  duration: 1.5 ms  statement: SELECT 1", "", "", 0, fixedNow},
		{"a DEBUG level", "2026-11-10 06:18:07.627 UTC [1] DEBUG1:  duration: 1.5 ms  statement: SELECT 1", "", "", 1, time.Date(2026, 11, 10, 6, 18, 7, 627_000_000, time.UTC)},
	}
	for _, c := range cases {
		got := parseAll(c.line)
		if len(got) != 1 {
			t.Errorf("%s: %d records", c.name, len(got))
			continue
		}
		q := got[0]
		if q.User != c.user || q.Database != c.db || q.PID != c.pid || !q.Time.Equal(c.when) || q.DurationMs != 1.5 || q.Query != "SELECT ?" {
			t.Errorf("%s: %+v", c.name, q)
		}
	}
}

func TestOurOwnQueriesAreLeftOut(t *testing.T) {
	got := parseAll(
		"2026-01-01 10:00:00.000 UTC [1] LOG:  duration: 900.0 ms  statement: /* db-collector */ SELECT * FROM pg_stat_statements",
		"2026-01-01 10:00:00.001 UTC [1] LOG:  duration: 900.0 ms  execute <unnamed>: /* db-collector */ SELECT * FROM pg_stat_activity",
		"2026-01-01 10:00:00.002 UTC [1] LOG:  duration: 900.0 ms  statement: SELECT 1",
	)
	if len(got) != 1 || got[0].Query != "SELECT ?" {
		t.Fatalf("%+v", got)
	}
}

func TestARecordIsReportedWhenTheNextOneStartsOrOnFlush(t *testing.T) {
	p := &LogParser{}
	var n int
	emit := func(collector.SlowQuery) { n++ }
	p.Feed("2026-01-01 10:00:00.000 UTC [1] LOG:  duration: 1.0 ms  statement: SELECT 1", emit)
	if n != 0 {
		t.Fatal("reported before the record could be known to be whole")
	}
	p.Feed("\tFROM t", emit)
	p.Feed("2026-01-01 10:00:00.001 UTC [1] LOG:  duration: 1.0 ms  statement: SELECT 2", emit)
	if n != 1 {
		t.Fatal(n)
	}
	p.Flush(emit)
	p.Flush(emit) // flushing twice reports nothing twice
	if n != 2 {
		t.Fatal(n)
	}
}

func TestAContinuationWithNoRecordIsIgnored(t *testing.T) {
	if got := parseAll("\tQuery Text: SELECT 1", "", "garbage", "\t"); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestAHugeStatementIsCutAndStillRedacted(t *testing.T) {
	lines := []string{"2026-01-01 10:00:00.000 UTC [1] LOG:  duration: 1.0 ms  statement: SELECT * FROM t WHERE a = 'start"}
	for i := 0; i < 5000; i++ {
		lines = append(lines, "\t"+strings.Repeat("secret-continuation-", 5))
	}
	got := parseAll(lines...)
	if len(got) != 1 || len(got[0].Query) > collector.MaxQueryBytes+3 || strings.Contains(got[0].Query, "secret") {
		t.Fatalf("%d bytes, %.80q", len(got[0].Query), got[0].Query)
	}
}

func TestNoisyInputNeverPanics(t *testing.T) {
	for _, l := range []string{"LOG:  duration: ms  statement:", "LOG:  duration: 1.2.3 ms  statement: x", "LOG:  duration: 99999999999999999999 ms  statement: SELECT 1", "[x]", "\x00\x01", strings.Repeat("LOG:  duration: 1 ms  statement: ", 1000)} {
		_ = parseAll(l, "\tmore")
	}
}

// ---- the file follower ----

type collected struct {
	mu  sync.Mutex
	got []collector.SlowQuery
}

func (c *collected) add(q collector.SlowQuery) { c.mu.Lock(); c.got = append(c.got, q); c.mu.Unlock() }
func (c *collected) queries() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, q := range c.got {
		out = append(out, q.Query)
	}
	return out
}

func slowLine(n int) string {
	return "2026-01-01 10:00:00.000 UTC [1] LOG:  duration: 1.0 ms  statement: SELECT " + strings.Repeat("x", n) + "\n"
}

func follow(t *testing.T, path string, fromStart bool) (*collected, func(), *[]error) {
	t.Helper()
	col := &collected{}
	var errs []error
	var emu sync.Mutex
	tl := &Tailer{Path: path, FromStart: fromStart, Poll: 5 * time.Millisecond, Quiet: 30 * time.Millisecond, OnError: func(e error) { emu.Lock(); errs = append(errs, e); emu.Unlock() }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = tl.Follow(ctx, col.add); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return col, cancel, &errs
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func appendTo(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func TestFollowStartsAtTheEndAndSeesWhatIsAppended(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pg.log")
	appendTo(t, path, slowLine(1)+slowLine(2)) // already there: not reported
	col, _, _ := follow(t, path, false)
	time.Sleep(60 * time.Millisecond)
	appendTo(t, path, slowLine(3))
	appendTo(t, path, "2026-01-01 10:00:00.001 UTC [1] LOG:  checkpoint starting\n")
	eventually(t, "the new line", func() bool { return len(col.queries()) == 1 })
	if q := col.queries()[0]; q != "SELECT xxx" {
		t.Fatal(q)
	}
}

func TestFollowFromTheStartReadsTheWholeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pg.log")
	appendTo(t, path, slowLine(1)+slowLine(2))
	col, _, _ := follow(t, path, true)
	eventually(t, "both", func() bool { return len(col.queries()) == 2 })
}

func TestAWriteThatStopsInTheMiddleOfALineIsWaitedFor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pg.log")
	appendTo(t, path, "")
	col, _, _ := follow(t, path, true)
	appendTo(t, path, "2026-01-01 10:00:00.000 UTC [1] LOG:  duration: 1.0 ms  stat")
	time.Sleep(80 * time.Millisecond)
	if len(col.queries()) != 0 {
		t.Fatal("a half line was reported")
	}
	appendTo(t, path, "ement: SELECT whole\n")
	eventually(t, "the finished line", func() bool { return len(col.queries()) == 1 })
	if col.queries()[0] != "SELECT whole" {
		t.Fatal(col.queries())
	}
}

func TestAStatementOfSeveralLinesIsReportedOnceTheLogIsQuiet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pg.log")
	appendTo(t, path, "")
	col, _, _ := follow(t, path, true)
	appendTo(t, path, "2026-01-01 10:00:00.000 UTC [1] LOG:  duration: 1.0 ms  statement: SELECT a\n\t, b\n\tFROM t\n")
	eventually(t, "the last record, with nothing after it to end it", func() bool { return len(col.queries()) == 1 })
	if col.queries()[0] != "SELECT a , b FROM t" {
		t.Fatal(col.queries())
	}
}

func TestRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pg.log")
	appendTo(t, path, "")
	col, _, _ := follow(t, path, true)
	appendTo(t, path, slowLine(1))
	eventually(t, "before", func() bool { return len(col.queries()) == 1 })
	// rotated: the old file is renamed and a new one takes the name
	if err := os.Rename(path, filepath.Join(dir, "pg.log.1")); err != nil {
		t.Fatal(err)
	}
	appendTo(t, path, slowLine(2)+slowLine(3))
	eventually(t, "after the rotation, from the start of the new file", func() bool { return len(col.queries()) == 3 })
}

func TestTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pg.log")
	appendTo(t, path, slowLine(10)+slowLine(11))
	col, _, _ := follow(t, path, true)
	eventually(t, "both", func() bool { return len(col.queries()) == 2 })
	if err := os.WriteFile(path, []byte(slowLine(1)), 0o600); err != nil { // emptied and written again: shorter than where it was read to
		t.Fatal(err)
	}
	eventually(t, "the line after the truncation", func() bool { return len(col.queries()) == 3 })
}

func TestAFileThatAppearsLate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "later", "pg.log")
	col, _, errs := follow(t, path, false)
	time.Sleep(50 * time.Millisecond)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	appendTo(t, path, slowLine(1))
	// a new file after the follower started is read from its start: nothing in it was seen before
	eventually(t, "the line", func() bool { return len(col.queries()) == 1 })
	if len(*errs) != 1 {
		t.Fatalf("a missing file is said once, not once a poll: %d", len(*errs))
	}
}

func TestALineThatIsTooLongIsCutAndTheNextOneRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pg.log")
	appendTo(t, path, "")
	col, _, _ := follow(t, path, true)
	appendTo(t, path, "2026-01-01 10:00:00.000 UTC [1] LOG:  duration: 1.0 ms  statement: SELECT * FROM t WHERE a = 'secret-"+strings.Repeat("a", 2<<20)+"\n")
	appendTo(t, path, slowLine(7))
	eventually(t, "the line after the huge one", func() bool {
		for _, q := range col.queries() {
			if q == "SELECT xxxxxxx" {
				return true
			}
		}
		return false
	})
	qs := col.queries()
	if len(qs) != 2 || qs[0] != "SELECT * FROM t WHERE a = ?" {
		t.Fatalf("the huge statement is kept, cut and redacted: %.100v", qs)
	}
}

func TestStopsWhenAskedAndReportsWhatItHad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pg.log")
	appendTo(t, path, "")
	col := &collected{}
	tl := &Tailer{Path: path, FromStart: true, Poll: 5 * time.Millisecond, Quiet: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tl.Follow(ctx, col.add) }()
	appendTo(t, path, slowLine(1))
	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not stop")
	}
	if len(col.queries()) != 1 {
		t.Fatalf("the record being assembled was lost at shutdown: %v", col.queries())
	}
}
