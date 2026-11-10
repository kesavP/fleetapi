package dbtelemetry

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func good() Batch {
	return Batch{
		Version: 1, Target: "orders-db", Engine: "postgres", CollectedAt: now.Add(-time.Second), IntervalSeconds: 60,
		Statements: []Statement{{QueryID: "42", Database: "shop", User: "app", Query: "SELECT * FROM t WHERE id = ?", Calls: 3, TotalMs: 1.5, Rows: 3, Extra: map[string]float64{"shared_blks_hit": 10}}},
	}
}

func TestAGoodBatch(t *testing.T) {
	b := good()
	if err := b.Validate(now); err != nil {
		t.Fatal(err)
	}
	// each kind alone is a batch
	for name, mk := range map[string]func() Batch{
		"activity": func() Batch {
			return Batch{Version: 1, Target: "a", Engine: "postgres", CollectedAt: now, Activity: &Activity{Total: 3}}
		},
		"slow": func() Batch {
			return Batch{Version: 1, Target: "a", Engine: "postgres", CollectedAt: now, SlowQueries: []SlowQuery{{Time: now, DurationMs: 5, Query: "SELECT ?"}}}
		},
		"capabilities": func() Batch {
			return Batch{Version: 1, Target: "a", Engine: "postgres", CollectedAt: now, Capabilities: &Capabilities{Engine: "postgres", Version: "16"}}
		},
	} {
		b := mk()
		if err := b.Validate(now); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestWhatIsRefused(t *testing.T) {
	tooMany := func() Batch {
		b := good()
		for len(b.Statements) <= MaxStatements {
			b.Statements = append(b.Statements, b.Statements[0])
		}
		return b
	}
	cases := map[string]func(*Batch){
		"a future version":       func(b *Batch) { b.Version = 2 },
		"no version":             func(b *Batch) { b.Version = 0 },
		"a target with a space":  func(b *Batch) { b.Target = "a b" },
		"a target with a slash":  func(b *Batch) { b.Target = "a/../b" },
		"no target":              func(b *Batch) { b.Target = "" },
		"an upper-case target":   func(b *Batch) { b.Target = "Orders" },
		"a long target":          func(b *Batch) { b.Target = strings.Repeat("a", 65) },
		"a bad engine":           func(b *Batch) { b.Engine = "Postgres 16" },
		"no time":                func(b *Batch) { b.CollectedAt = time.Time{} },
		"a time in the future":   func(b *Batch) { b.CollectedAt = now.Add(time.Hour) },
		"a time long ago":        func(b *Batch) { b.CollectedAt = now.Add(-48 * time.Hour) },
		"a negative interval":    func(b *Batch) { b.IntervalSeconds = -1 },
		"an infinite interval":   func(b *Batch) { b.IntervalSeconds = math.Inf(1) },
		"nothing in it":          func(b *Batch) { b.Statements = nil },
		"too many statements":    func(b *Batch) { *b = tooMany() },
		"a NaN time":             func(b *Batch) { b.Statements[0].TotalMs = math.NaN() },
		"a negative time":        func(b *Batch) { b.Statements[0].TotalMs = -1 },
		"a query with a literal": func(b *Batch) { b.Statements[0].Query = "SELECT * FROM t WHERE email = 'alice@example.com'" },
		"a dollar-quoted body":   func(b *Batch) { b.Statements[0].Query = "SELECT $$secret$$" },
		"a tagged body":          func(b *Batch) { b.Statements[0].Query = "SELECT $tag$secret$tag$" },
		"a long query":           func(b *Batch) { b.Statements[0].Query = strings.Repeat("a", MaxQueryBytes+1) },
		"a control character":    func(b *Batch) { b.Statements[0].Query = "SELECT\x00 1" },
		"invalid utf-8":          func(b *Batch) { b.Statements[0].Query = "SELECT \xff" },
		"a long query id":        func(b *Batch) { b.Statements[0].QueryID = strings.Repeat("1", 65) },
		"a bad extra name":       func(b *Batch) { b.Statements[0].Extra = map[string]float64{"a b": 1} },
		"a NaN extra":            func(b *Batch) { b.Statements[0].Extra = map[string]float64{"a": math.NaN()} },
		"too many extras": func(b *Batch) {
			b.Statements[0].Extra = map[string]float64{}
			for i := 0; i < MaxExtra+1; i++ {
				b.Statements[0].Extra[strings.Repeat("x", i+1)] = 1
			}
		},
		"a slow query with a literal": func(b *Batch) {
			b.Statements = nil
			b.SlowQueries = []SlowQuery{{Time: now, DurationMs: 1, Query: "SELECT 'x'"}}
		},
		"a slow query with no time": func(b *Batch) {
			b.Statements = nil
			b.SlowQueries = []SlowQuery{{DurationMs: 1, Query: "SELECT ?"}}
		},
		"a session with a literal": func(b *Batch) {
			b.Statements = nil
			b.Activity = &Activity{Sessions: []Session{{State: "active", Query: "UPDATE t SET a = 'x'"}}}
		},
		"too many sessions": func(b *Batch) {
			b.Statements = nil
			b.Activity = &Activity{Sessions: make([]Session, MaxSessions+1)}
		},
		"a negative total": func(b *Batch) { b.Statements = nil; b.Activity = &Activity{Total: -1} },
		"too many pools": func(b *Batch) {
			b.Statements = nil
			b.Pools = &Pools{Pools: make([]Pool, MaxPools+1)}
		},
		"a pool with no database": func(b *Batch) {
			b.Statements = nil
			b.Pools = &Pools{Pools: []Pool{{User: "a"}}}
		},
		"a pool with a negative count": func(b *Batch) {
			b.Statements = nil
			b.Pools = &Pools{Pools: []Pool{{Database: "shop", ClWaiting: -1}}}
		},
		"a pool with an absurd count": func(b *Batch) {
			b.Statements = nil
			b.Pools = &Pools{Pools: []Pool{{Database: "shop", SvActive: 1 << 30}}}
		},
		"a pool with a NaN wait": func(b *Batch) {
			b.Statements = nil
			b.Pools = &Pools{Pools: []Pool{{Database: "shop", MaxWaitMs: math.NaN()}}}
		},
		"stats with no database": func(b *Batch) {
			b.Statements = nil
			b.Pools = &Pools{Stats: []PoolStats{{Xacts: 1}}}
		},
		"stats with a negative time": func(b *Batch) {
			b.Statements = nil
			b.Pools = &Pools{Stats: []PoolStats{{Database: "shop", WaitMs: -1}}}
		},
		"a negative total of waiting": func(b *Batch) { b.Statements = nil; b.Pools = &Pools{Waiting: -1} },
		"too many settings": func(b *Batch) {
			b.Statements = nil
			b.Capabilities = &Capabilities{Engine: "postgres", Settings: map[string]string{}}
			for i := 0; i < 65; i++ {
				b.Capabilities.Settings[strings.Repeat("s", i+1)] = "v"
			}
		},
	}
	for name, mutate := range cases {
		b := good()
		mutate(&b)
		if err := b.Validate(now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTheTripwireLetsNormalizedTextThrough(t *testing.T) {
	for _, q := range []string{
		`SELECT * FROM "O'Brien" WHERE a = ?`, // a quoted identifier may hold a quote
		`SELECT $1, $2`,
		`SELECT "a$$b" FROM t`,
		`INSERT INTO t VALUES (?, ?)`,
		`SELECT ?::text || $1`,
		`SELECT a FROM t1$x`,
		``,
	} {
		if !Normalized(q) {
			t.Errorf("%q was refused", q)
		}
	}
	for _, q := range []string{`SELECT 'x'`, `SELECT $$x$$`, `SELECT $a$x$a$`, `WHERE a = 'it''s'`, `SELECT "ok" || 'bad'`} {
		if Normalized(q) {
			t.Errorf("%q was let through", q)
		}
	}
}

func TestTheJSONOfTheCollectorDecodes(t *testing.T) {
	// the collector's wire format, as db-collector writes it (with fields this service does not know)
	doc := `{"version":1,"target":"orders","engine":"postgres","collected_at":"2026-01-01T11:59:59Z","interval_seconds":60,
	 "statements":[{"query_id":"1","database":"shop","user":"app","query":"SELECT ?","calls":2,"total_ms":3.5,"rows":2,"extra":{"wal_bytes":5},"other":true,"future_field":1}],
	 "activity":{"total":4,"max_connections":100,"by_state":{"active":2},"waiting":{"Lock:relation":1},"blocked":1,"longest_query_ms":5000,"longest_xact_ms":6000,"longest_idle_in_xact_ms":0,
	   "sessions":[{"pid":9,"state":"active","query_ms":5000,"blocked_by":[3],"query":"UPDATE t SET a = ?"}]},
	 "slow_queries":[{"time":"2026-01-01T11:59:00Z","duration_ms":301.6,"kind":"statement","pid":7,"query":"SELECT ?"}],
	 "capabilities":{"engine":"postgres","version":"16.2","statements":{"available":true},"activity":{"available":true},"slow_log":{"available":false,"problems":["x"]},"settings":{"a":"b"}}}`
	var b Batch
	if err := json.Unmarshal([]byte(doc), &b); err != nil {
		t.Fatal(err)
	}
	if err := b.Validate(now); err != nil {
		t.Fatal(err)
	}
	if b.Statements[0].Calls != 2 || !b.Statements[0].Other || b.Activity.Sessions[0].BlockedBy[0] != 3 || b.SlowQueries[0].DurationMs != 301.6 || b.Capabilities.SlowLog.Problems[0] != "x" {
		t.Fatalf("%+v", b)
	}
}

func TestValidTarget(t *testing.T) {
	for _, ok := range []string{"a", "orders-db", "db.prod_1", "x1"} {
		if !ValidTarget(ok) {
			t.Errorf("%q", ok)
		}
	}
	for _, bad := range []string{"", "-a", "a-", "A", "a b", "a/b", "a'b", strings.Repeat("a", 65)} {
		if ValidTarget(bad) {
			t.Errorf("%q", bad)
		}
	}
}
