package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/dbtelemetry"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/store"
)

type fakeDBWriter struct {
	mu      sync.Mutex
	err     error
	project []string
	batches []dbtelemetry.Batch
}

func (f *fakeDBWriter) WriteDatabaseBatch(_ context.Context, project string, b dbtelemetry.Batch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.project = append(f.project, project)
	f.batches = append(f.batches, b)
	return nil
}

func dbIngest(t *testing.T, w DatabaseWriter, maxBody int64) (*httptest.Server, *Counters) {
	t.Helper()
	mux := http.NewServeMux()
	c := NewCounters()
	(&Ingest{Projects: registry(t), Sink: &fakeSink{}, MaxBody: maxBody, Log: quiet, Count: c, Databases: w}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, c
}

func goodBatch() dbtelemetry.Batch {
	return dbtelemetry.Batch{
		Version: 1, Target: "orders-db", Engine: "postgres", CollectedAt: time.Now().Add(-time.Second), IntervalSeconds: 60,
		Statements: []dbtelemetry.Statement{{QueryID: "42", Database: "shop", User: "app", Query: "SELECT * FROM t WHERE id = ?", Calls: 3, TotalMs: 1.5, Rows: 3}},
	}
}

func postDB(t *testing.T, srv *httptest.Server, key string, body any, headers ...string) (int, string) {
	t.Helper()
	var rd *bytes.Reader
	switch b := body.(type) {
	case string:
		rd = bytes.NewReader([]byte(b))
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest("POST", srv.URL+"/v1/db", rd)
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i+1] == "-" {
			req.Header.Del(headers[i])
		} else {
			req.Header.Set(headers[i], headers[i+1])
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(res.Body)
	return res.StatusCode, buf.String()
}

func TestDatabaseBatchesAreStoredForTheKeysProject(t *testing.T) {
	w := &fakeDBWriter{}
	srv, c := dbIngest(t, w, 1<<20)
	code, body := postDB(t, srv, usIngest, goodBatch())
	if code != 200 || !strings.Contains(body, `"statements":1`) {
		t.Fatalf("%d %s", code, body)
	}
	if len(w.batches) != 1 || w.project[0] != "us" || w.batches[0].Target != "orders-db" || w.batches[0].Statements[0].Calls != 3 {
		t.Fatalf("%+v %v", w.batches, w.project)
	}
	// the project is the key's, whatever the body says
	var m map[string]any
	raw, _ := json.Marshal(goodBatch())
	_ = json.Unmarshal(raw, &m)
	m["project"] = "eu"
	if code, _ := postDB(t, srv, usIngest, m); code != 200 || w.project[1] != "us" {
		t.Fatalf("%d %v", code, w.project)
	}
	if !strings.Contains(metricsText(c), `telemetry_ingest_requests_total{signal="databases",code="200"} 2`) {
		t.Fatalf("%s", metricsText(c))
	}
}

func metricsText(c *Counters) string {
	rec := httptest.NewRecorder()
	MetricsHandler(c, nil).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func TestDatabaseBatchesNeedAnIngestKey(t *testing.T) {
	w := &fakeDBWriter{}
	srv, _ := dbIngest(t, w, 1<<20)
	for name, key := range map[string]string{"none": "", "a read key": usRead, "an unknown key": "nope-nope-nope-nope-0"} {
		if code, _ := postDB(t, srv, key, goodBatch()); code != 401 {
			t.Errorf("%s: %d", name, code)
		}
	}
	if len(w.batches) != 0 {
		t.Fatal("something was stored without a key")
	}
}

func TestDatabaseBatchesThatAreNotValidAreRefusedAndNothingIsStored(t *testing.T) {
	w := &fakeDBWriter{}
	srv, _ := dbIngest(t, w, 1<<20)
	mutate := func(f func(*dbtelemetry.Batch)) dbtelemetry.Batch { b := goodBatch(); f(&b); return b }
	for name, body := range map[string]any{
		"a literal that was not removed": mutate(func(b *dbtelemetry.Batch) {
			b.Statements[0].Query = "SELECT * FROM users WHERE email = 'alice@example.com'"
		}),
		"a future version":           mutate(func(b *dbtelemetry.Batch) { b.Version = 9 }),
		"a bad target":               mutate(func(b *dbtelemetry.Batch) { b.Target = "a/b" }),
		"nothing in it":              mutate(func(b *dbtelemetry.Batch) { b.Statements = nil }),
		"not json":                   `{"version":`,
		"empty":                      ``,
		"json that is not an object": `[1,2]`,
		"null":                       `null`,
	} {
		code, resp := postDB(t, srv, usIngest, body)
		if code != 400 {
			t.Errorf("%s: %d %s", name, code, resp)
		}
		if strings.Contains(resp, "alice@example.com") {
			t.Errorf("%s: the refusal echoes the data back: %s", name, resp)
		}
	}
	if len(w.batches) != 0 {
		t.Fatalf("%d batches stored", len(w.batches))
	}
}

func TestDatabaseBatchesNeedJSON(t *testing.T) {
	srv, _ := dbIngest(t, &fakeDBWriter{}, 1<<20)
	for _, ct := range []string{"text/plain", "application/x-protobuf", "-"} {
		if code, _ := postDB(t, srv, usIngest, goodBatch(), "Content-Type", ct); code != 415 {
			t.Errorf("%q: %d", ct, code)
		}
	}
	if code, _ := postDB(t, srv, usIngest, goodBatch(), "Content-Type", "application/json; charset=utf-8"); code != 200 {
		t.Errorf("with a charset: %d", code)
	}
}

func TestADatabaseBatchThatIsTooBigIsRefused(t *testing.T) {
	srv, _ := dbIngest(t, &fakeDBWriter{}, 1<<10) // 1 KiB
	b := goodBatch()
	for i := 0; i < 30; i++ {
		b.Statements = append(b.Statements, b.Statements[0])
	}
	if code, _ := postDB(t, srv, usIngest, b); code != 413 {
		t.Fatalf("%d", code)
	}
}

func TestAStorageFailureIsAnErrorTheCollectorRetriesAndNothingInternalLeaks(t *testing.T) {
	w := &fakeDBWriter{err: errors.New("clickhouse at 10.0.0.9:9000 refused the password hunter2")}
	srv, _ := dbIngest(t, w, 1<<20)
	code, body := postDB(t, srv, usIngest, goodBatch())
	if code != 500 || strings.Contains(body, "hunter2") || strings.Contains(body, "10.0.0.9") {
		t.Fatalf("%d %s", code, body)
	}
}

func TestTheDatabaseEndpointIsOnlyThereWhenThereIsAPlaceToStore(t *testing.T) {
	mux := http.NewServeMux()
	(&Ingest{Projects: registry(t), Sink: &fakeSink{}, MaxBody: 1 << 20, Log: quiet, Count: NewCounters()}).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if code, _ := postDB(t, srv, usIngest, goodBatch()); code != 404 && code != 405 {
		t.Fatalf("%d", code)
	}
	good, _ := dbIngest(t, &fakeDBWriter{}, 1<<20)
	res, _ := http.Get(good.URL + "/v1/db")
	if res.StatusCode != 405 {
		t.Fatalf("a GET: %d", res.StatusCode)
	}
}

// ---- reading ----

type fakeDBReader struct {
	store.Reader
	mu       sync.Mutex
	project  string
	target   string
	stmtQ    store.StatementQuery
	slowQ    store.SlowQueryFilter
	step     int
	from, to time.Time
}

func (f *fakeDBReader) ListDatabases(_ context.Context, p string) ([]store.DBTarget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.project = p
	return []store.DBTarget{{Target: "orders-db", Engine: "postgres", Version: "16.2", LastSeen: time.Unix(1700000000, 0).UTC(), Capabilities: json.RawMessage(`{"statements":{"available":true}}`)}}, nil
}
func (f *fakeDBReader) TopStatements(_ context.Context, q store.StatementQuery) (store.StatementsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stmtQ = q
	return store.StatementsResult{Statements: []store.StatementRow{{QueryID: "1", Query: "SELECT ?", Calls: 2}}, TotalCalls: 2}, nil
}
func (f *fakeDBReader) ActivitySeries(_ context.Context, p, target string, from, to time.Time, step int) (store.ActivityResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.project, f.target, f.from, f.to, f.step = p, target, from, to, step
	return store.ActivityResult{Points: []store.ActivityPoint{{T: 1000, Total: 5}}}, nil
}
func (f *fakeDBReader) PoolSeries(_ context.Context, p, target string, from, to time.Time, step int) (store.PoolsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.project, f.target, f.from, f.to, f.step = p, target, from, to, step
	return store.PoolsResult{Points: []store.PoolPoint{{T: 1000, Clients: 8, Waiting: 6}}, Stats: []store.PoolStatsRow{{Database: "shop", Xacts: 10}}}, nil
}
func (f *fakeDBReader) SlowQueries(_ context.Context, q store.SlowQueryFilter) ([]store.SlowQueryRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.slowQ = q
	return []store.SlowQueryRow{{DurationMs: 301, Query: "SELECT ?"}}, nil
}

func dbQueryServer(t *testing.T, r store.Reader) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	(&Query{Projects: registry(t), Reader: r, Log: quiet, DefaultRange: time.Hour, MaxSeries: 500, MaxPoints: 1000}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestDatabaseReadsAreScopedToTheKeysProject(t *testing.T) {
	fr := &fakeDBReader{}
	srv := dbQueryServer(t, fr)
	base := srv.URL + "/api/v1/projects/"
	for name, c := range map[string]struct {
		path, key string
		want      int
	}{
		"no key":          {"us/databases", "", 401},
		"ingest key":      {"us/databases", usIngest, 403},
		"another project": {"us/databases/orders-db/statements", euRead, 403},
		"unknown project": {"nosuch/databases/orders-db/slow", usRead, 403},
	} {
		if code, _ := get(t, base+c.path, c.key); code != c.want {
			t.Errorf("%s: %d", name, code)
		}
	}
	if fr.project != "" || fr.stmtQ.Project != "" {
		t.Fatal("the reader was asked for a rejected request")
	}
	code, body := get(t, base+"us/databases", usRead)
	if code != 200 || fr.project != "us" || !strings.Contains(body, `"target":"orders-db"`) || !strings.Contains(body, `"capabilities":{"statements"`) {
		t.Fatalf("%d %s", code, body)
	}
}

func TestStatementsQueryParameters(t *testing.T) {
	fr := &fakeDBReader{}
	srv := dbQueryServer(t, fr)
	base := srv.URL + "/api/v1/projects/us/databases/orders-db/statements"
	if code, body := get(t, base+"?from=1700000000&to=1700003600&order=calls&limit=10&database=shop&user=app", usRead); code != 200 || !strings.Contains(body, `"total_calls":2`) {
		t.Fatalf("%d %s", code, body)
	}
	q := fr.stmtQ
	if q.Project != "us" || q.Target != "orders-db" || q.Order != "calls" || q.Limit != 10 || q.Database != "shop" || q.User != "app" || q.To.Sub(q.From) != time.Hour {
		t.Fatalf("%+v", q)
	}
	if code, _ := get(t, base, usRead); code != 200 || fr.stmtQ.Order != "total_ms" || fr.stmtQ.Limit != 50 {
		t.Fatalf("the defaults: %+v", fr.stmtQ)
	}
	// a semicolon is not a separator Go reads in a query: the parameter is dropped and the default order is used, never the text given
	if code, _ := get(t, base+"?order=total_ms;DROP", usRead); code != 200 || fr.stmtQ.Order != "total_ms" {
		t.Errorf("a parameter with a semicolon: %d %q", code, fr.stmtQ.Order)
	}
	for _, bad := range []string{"?order=random", "?order=total_ms%3BDROP", "?limit=0", "?limit=201", "?limit=x", "?from=x", "?from=2000&to=1000"} {
		if code, _ := get(t, base+bad, usRead); code != 400 {
			t.Errorf("%s: %d", bad, code)
		}
	}
}

func TestATargetThatIsNotANameIsRefusedBeforeAnyQuery(t *testing.T) {
	fr := &fakeDBReader{}
	srv := dbQueryServer(t, fr)
	for _, target := range []string{"UPPER", "a%20b", "a'b", "..", "a%2Fb", strings.Repeat("a", 70)} {
		for _, p := range []string{"statements", "activity", "slow", "pools"} {
			code, _ := get(t, srv.URL+"/api/v1/projects/us/databases/"+target+"/"+p, usRead)
			if code != 400 && code != 404 {
				t.Errorf("%s/%s: %d", target, p, code)
			}
		}
	}
	if fr.stmtQ.Project != "" || fr.slowQ.Project != "" || fr.target != "" {
		t.Fatal("the store was asked about a name that is not a name")
	}
}

func TestActivityStepFollowsTheRange(t *testing.T) {
	fr := &fakeDBReader{}
	srv := dbQueryServer(t, fr)
	base := srv.URL + "/api/v1/projects/us/databases/orders-db/activity"
	if code, _ := get(t, base+"?from=1700000000&to=1700000600", usRead); code != 200 || fr.step != 10 {
		t.Fatalf("a short range is never finer than the 10 s the collector samples at: %d", fr.step)
	}
	if code, _ := get(t, base+"?from=1700000000&to=1700086400", usRead); code != 200 || fr.step < 280 || fr.step > 300 {
		t.Fatalf("a day is about 300 points: step %d", fr.step)
	}
	if code, _ := get(t, base+"?from=1700000000&to=1700003600&step=60", usRead); code != 200 || fr.step != 60 {
		t.Fatalf("%d", fr.step)
	}
	for _, bad := range []string{"&step=0", "&step=x", "&step=-5", "&step=99999"} {
		if code, _ := get(t, base+"?from=1700000000&to=1700003600"+bad, usRead); code != 400 {
			t.Errorf("%s: %d", bad, code)
		}
	}
}

func TestSlowQueryParameters(t *testing.T) {
	fr := &fakeDBReader{}
	srv := dbQueryServer(t, fr)
	base := srv.URL + "/api/v1/projects/us/databases/orders-db/slow"
	if code, body := get(t, base+"?min_ms=250&order=duration&limit=5", usRead); code != 200 || !strings.Contains(body, `"duration_ms":301`) {
		t.Fatalf("%d %s", code, body)
	}
	if f := fr.slowQ; f.Project != "us" || f.Target != "orders-db" || f.MinMs != 250 || !f.Slowest || f.Limit != 5 {
		t.Fatalf("%+v", f)
	}
	for _, bad := range []string{"?min_ms=-1", "?min_ms=x", "?order=random", "?limit=501", "?limit=0"} {
		if code, _ := get(t, base+bad, usRead); code != 400 {
			t.Errorf("%s: %d", bad, code)
		}
	}
}

func TestDatabaseReadsAreReadOnly(t *testing.T) {
	srv := dbQueryServer(t, &fakeDBReader{})
	for _, p := range []string{"databases", "databases/orders-db/statements", "databases/orders-db/slow", "databases/pgb/pools"} {
		req, _ := http.NewRequest("POST", srv.URL+"/api/v1/projects/us/"+p, strings.NewReader("{}"))
		req.Header.Set("X-API-Key", usRead)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 405 {
			t.Errorf("%s: %d", p, res.StatusCode)
		}
	}
}

func TestPoolsEndpoint(t *testing.T) {
	fr := &fakeDBReader{}
	srv := dbQueryServer(t, fr)
	base := srv.URL + "/api/v1/projects/"
	for name, c := range map[string]struct {
		path, key string
		want      int
	}{
		"no key":          {"us/databases/pgb/pools", "", 401},
		"ingest key":      {"us/databases/pgb/pools", usIngest, 403},
		"another project": {"us/databases/pgb/pools", euRead, 403},
	} {
		if code, _ := get(t, base+c.path, c.key); code != c.want {
			t.Errorf("%s: %d", name, code)
		}
	}
	if fr.project != "" {
		t.Fatal("the reader was asked for a rejected request")
	}
	code, body := get(t, base+"us/databases/pgb/pools?from=1700000000&to=1700003600", usRead)
	if code != 200 || fr.project != "us" || fr.target != "pgb" || !strings.Contains(body, `"waiting":6`) || !strings.Contains(body, `"xacts":10`) {
		t.Fatalf("%d %s (%q %q)", code, body, fr.project, fr.target)
	}
	if fr.step != 13 { // an hour is about 300 points: 3600/300 + 1
		t.Fatalf("step %d", fr.step)
	}
	if code, _ := get(t, base+"us/databases/pgb/pools?from=1700000000&to=1700003600&step=60", usRead); code != 200 || fr.step != 60 {
		t.Fatalf("%d", fr.step)
	}
	for _, bad := range []string{"&step=0", "&step=x", "&step=99999"} {
		if code, _ := get(t, base+"us/databases/pgb/pools?from=1700000000&to=1700003600"+bad, usRead); code != 400 {
			t.Errorf("%s: %d", bad, code)
		}
	}
	if code, _ := get(t, base+"us/databases/pgb/pools?from=2000&to=1000", usRead); code != 400 {
		t.Errorf("a range that is backwards: %d", code)
	}
}

func TestAPoolBatchIsStoredForTheKeysProject(t *testing.T) {
	w := &fakeDBWriter{}
	srv, _ := dbIngest(t, w, 1<<20)
	b := dbtelemetry.Batch{Version: 1, Target: "pgb", Engine: "pgbouncer", CollectedAt: time.Now().Add(-time.Second), IntervalSeconds: 10,
		Pools: &dbtelemetry.Pools{Pools: []dbtelemetry.Pool{{Database: "shop", User: "app", ClWaiting: 6, SvActive: 2, PoolSize: 2, MaxWaitMs: 1500}}, Clients: 8, Waiting: 6, MaxWaitMs: 1500}}
	if code, body := postDB(t, srv, usIngest, b); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if len(w.batches) != 1 || w.batches[0].Pools == nil || w.batches[0].Pools.Waiting != 6 || w.project[0] != "us" {
		t.Fatalf("%+v", w.batches)
	}
}
