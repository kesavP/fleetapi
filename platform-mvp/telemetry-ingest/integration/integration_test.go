//go:build integration

// Runs the real handlers, pipeline and ClickHouse store together against a
// live ClickHouse (CLICKHOUSE_ADDR, CLICKHOUSE_USER, CLICKHOUSE_PASSWORD) and
// checks the result through the query API. hack/platform-mvp/test-telemetry-ingest.sh
// starts the database and runs this.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/golang/snappy"
	"github.com/google/pprof/profile"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/detect"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/enroll"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/incident"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/inventory"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/model"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/pipeline"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/project"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/server"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/store"
	logsv1 "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	tracesv1 "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

const (
	usIngest = "us-ingest-key-0000001"
	usRead   = "us-read-key-00000001"
	euIngest = "eu-ingest-key-0000001"
	euRead   = "eu-read-key-00000001"
)

func env(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Skipf("%s not set; run hack/platform-mvp/test-telemetry-ingest.sh", k)
	}
	return v
}

type rig struct {
	t   *testing.T
	srv *httptest.Server
	st  *store.Store
	db  string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	addr, user, pw := env(t, "CLICKHOUSE_ADDR"), env(t, "CLICKHOUSE_USER"), os.Getenv("CLICKHOUSE_PASSWORD")
	db := fmt.Sprintf("it_%d", time.Now().UnixNano())
	opts := store.Options{
		Addrs: []string{addr}, Database: db, User: user, Password: pw, CreateDatabase: true,
		TTL: store.TTLs{Metrics: 48 * time.Hour, Traces: 24 * time.Hour, Logs: 24 * time.Hour, Profiles: 24 * time.Hour, Databases: 12 * time.Hour},
	}
	ctx := context.Background()
	st, err := store.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := st.Migrate(ctx); err != nil { // a restart must be a no-op
		t.Fatalf("second migrate: %v", err)
	}

	dir := t.TempDir()
	cfgPath := dir + "/projects.json"
	_ = os.WriteFile(cfgPath, []byte(fmt.Sprintf(`{"projects":[
	 {"name":"us","ingestKeys":[%q],"readKeys":[%q]},
	 {"name":"eu","ingestKeys":[%q],"readKeys":[%q]}]}`, usIngest, usRead, euIngest, euRead)), 0o600)
	reg, err := project.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pipe := pipeline.New(st, pipeline.PipelineOptions{
		Options:       pipeline.Options{BatchRows: 1000, MaxBuffered: 100000, FlushInterval: 50 * time.Millisecond, Retries: 1, Backoff: 50 * time.Millisecond},
		SeriesRefresh: time.Minute,
	}, log)
	mux := http.NewServeMux()
	c := server.NewCounters()
	(&server.Ingest{Projects: reg, Sink: pipe, MaxBody: 8 << 20, Log: log, Count: c}).Register(mux)
	(&server.Query{Projects: reg, Reader: st, Log: log, DefaultRange: time.Hour, MaxSeries: 500, MaxPoints: 100000}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		srv.Close()
		_ = pipe.Close(context.Background())
		raw, err := clickhouse.Open(&clickhouse.Options{Addr: opts.Addrs, Auth: clickhouse.Auth{Database: "default", Username: user, Password: pw}})
		if err == nil {
			_ = raw.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db)
			_ = raw.Close()
		}
		_ = st.Close()
	})

	// Retention must be on the tables, not just requested.
	raw, err := clickhouse.Open(&clickhouse.Options{Addr: opts.Addrs, Auth: clickhouse.Auth{Database: db, Username: user, Password: pw}})
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, tbl := range []string{"metrics_samples", "metrics_series", "traces_spans", "logs_records", "profiling_samples", "profiling_stacks"} {
		var ddl string
		if err := raw.QueryRow(ctx, "SELECT engine_full FROM system.tables WHERE database = ? AND name = ?", db, tbl).Scan(&ddl); err != nil {
			t.Fatalf("%s: %v", tbl, err)
		}
		if !strings.Contains(ddl, "TTL") {
			t.Errorf("%s has no TTL: %s", tbl, ddl)
		}
	}
	// every migration recorded once, although Migrate ran twice
	var versions uint64
	if err := raw.QueryRow(ctx, "SELECT count() FROM schema_migrations").Scan(&versions); err != nil || versions != uint64(store.MigrationCount()) {
		t.Fatalf("schema_migrations rows: %d %v", versions, err)
	}
	var incidents uint64
	if err := raw.QueryRow(ctx, "SELECT count() FROM system.tables WHERE database = ? AND name = 'incident_events'", db).Scan(&incidents); err != nil || incidents != 1 {
		t.Fatalf("incident_events table: %d %v", incidents, err)
	}
	var ttl string
	if err := raw.QueryRow(ctx, "SELECT engine_full FROM system.tables WHERE database = ? AND name = 'incident_events'", db).Scan(&ttl); err != nil || strings.Contains(ttl, "TTL") {
		t.Fatalf("incidents are records and must never expire: %s %v", ttl, err)
	}
	return &rig{t: t, srv: srv, st: st, db: db}
}

// ---- request builders ----

func rwLabel(n, v string) []byte {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendString(b, n)
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	return protowire.AppendString(b, v)
}

func rwSeries(name string, labels [][2]string, ts []int64, vals []float64) []byte {
	var s []byte
	add := func(l []byte) { s = protowire.AppendTag(s, 1, protowire.BytesType); s = protowire.AppendBytes(s, l) }
	add(rwLabel("__name__", name))
	for _, l := range labels {
		add(rwLabel(l[0], l[1]))
	}
	for i := range ts {
		var smp []byte
		smp = protowire.AppendTag(smp, 1, protowire.Fixed64Type)
		smp = protowire.AppendFixed64(smp, math.Float64bits(vals[i]))
		smp = protowire.AppendTag(smp, 2, protowire.VarintType)
		smp = protowire.AppendVarint(smp, uint64(ts[i]))
		s = protowire.AppendTag(s, 2, protowire.BytesType)
		s = protowire.AppendBytes(s, smp)
	}
	return s
}

func rwBody(series ...[]byte) []byte {
	var b []byte
	for _, s := range series {
		b = protowire.AppendTag(b, 1, protowire.BytesType)
		b = protowire.AppendBytes(b, s)
	}
	var md []byte // metadata: gauge/counter family with help text
	md = protowire.AppendTag(md, 1, protowire.VarintType)
	md = protowire.AppendVarint(md, 1)
	md = protowire.AppendTag(md, 2, protowire.BytesType)
	md = protowire.AppendString(md, "container_net_tcp_successful_connects_total")
	b = protowire.AppendTag(b, 3, protowire.BytesType)
	b = protowire.AppendBytes(b, md)
	return snappy.Encode(nil, b)
}

func (r *rig) post(path string, headers map[string]string, body []byte) *http.Response {
	r.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, r.srv.URL+path, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp
}

func (r *rig) must(resp *http.Response, want int) {
	r.t.Helper()
	if resp.StatusCode != want {
		r.t.Fatalf("status %d, want %d", resp.StatusCode, want)
	}
}

func (r *rig) get(key, path string, out any) int {
	r.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, r.srv.URL+"/api/v1/projects/"+path, nil)
	req.Header.Set("X-API-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if s, ok := out.(*string); ok && resp.StatusCode == 200 {
		*s = string(b) // plain text (format=folded), not the JSON envelope
	} else if out != nil && resp.StatusCode == 200 {
		wrapper := struct{ Data json.RawMessage }{}
		if err := json.Unmarshal(b, &wrapper); err != nil {
			r.t.Fatalf("%s: %v\n%s", path, err, b)
		}
		if err := json.Unmarshal(wrapper.Data, out); err != nil {
			r.t.Fatalf("%s: %v\n%s", path, err, b)
		}
	}
	return resp.StatusCode
}

// eventually retries until check passes: rows reach ClickHouse after the
// pipeline's flush interval.
func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

type metricResult struct {
	Series []struct {
		Labels map[string]string `json:"labels"`
		Points []struct {
			T int64   `json:"t"`
			V float64 `json:"v"`
		} `json:"points"`
	} `json:"series"`
	Truncated bool `json:"truncated"`
}

func TestEndToEnd(t *testing.T) {
	r := newRig(t)
	ingest := func(key string) map[string]string {
		return map[string]string{"X-API-Key": key, "Content-Encoding": "snappy", "Content-Type": "application/x-protobuf"}
	}

	// ---- metrics: a counter that resets, for two projects ----
	t0 := time.Now().Add(-5 * time.Minute).Truncate(time.Second).UnixMilli()
	ts := []int64{t0, t0 + 15_000, t0 + 30_000, t0 + 45_000, t0 + 60_000}
	const name = "container_net_tcp_successful_connects_total"
	usBody := rwBody(
		rwSeries(name, [][2]string{{"container_id", "/k8s/shop/cart/app"}, {"destination", "10.0.0.5:6379"}}, ts, []float64{100, 130, 160, 10, 40}),
		rwSeries(name, [][2]string{{"container_id", "/k8s/shop/web/app"}, {"destination", "10.0.0.6:80"}}, ts, []float64{1, 2, 3, 4, 5}),
		rwSeries("node_cpu_usage", nil, ts[:1], []float64{math.NaN()}), // stale marker: stored, never returned
	)
	r.must(r.post("/v1/metrics", ingest(usIngest), usBody), 204)
	euBody := rwBody(rwSeries(name, [][2]string{{"container_id", "/k8s/eu-only/x/app"}}, ts, []float64{7, 7, 7, 7, 7}))
	r.must(r.post("/v1/metrics", ingest(euIngest), euBody), 204)
	r.must(r.post("/v1/metrics", ingest(usIngest), usBody), 204) // an agent retry: the series row is not duplicated

	var names []string
	eventually(t, "metric names", func() bool {
		return r.get(usRead, "us/metrics", &names) == 200 && len(names) == 2
	})
	if names[0] != name || names[1] != "node_cpu_usage" {
		t.Fatalf("names: %v", names)
	}

	var series []struct{ Labels map[string]string }
	if code := r.get(usRead, "us/metrics/series?name="+name+"&match.container_id="+url.QueryEscape("/k8s/shop/cart/app"), &series); code != 200 || len(series) != 1 || series[0].Labels["destination"] != "10.0.0.5:6379" {
		t.Fatalf("series: %d %+v", code, series)
	}
	if code := r.get(usRead, "us/metrics/series?name="+name, &series); code != 200 || len(series) != 2 {
		t.Fatalf("both series (the retry must not add a third): %d %+v", code, series)
	}

	from, to := time.UnixMilli(t0-60_000).UTC().Format(time.RFC3339), time.UnixMilli(t0+120_000).UTC().Format(time.RFC3339)
	win := "&from=" + url.QueryEscape(from) + "&to=" + url.QueryEscape(to)
	var raw metricResult
	eventually(t, "raw samples", func() bool {
		return r.get(usRead, "us/metrics/query_range?name="+name+"&match.container_id="+url.QueryEscape("/k8s/shop/cart/app")+win, &raw) == 200 &&
			len(raw.Series) == 1 && len(raw.Series[0].Points) >= 5
	})
	if n := len(raw.Series[0].Points); n != 10 && n != 5 { // 10 if the retried batch was stored twice, which MergeTree allows
		t.Fatalf("points: %d", n)
	}
	if p := raw.Series[0].Points[0]; p.T != t0 || p.V != 100 {
		t.Fatalf("first point %+v, want t=%d v=100 (millisecond precision)", p, t0)
	}

	var rate metricResult
	if code := r.get(usRead, "us/metrics/query_range?name="+name+"&match.container_id="+url.QueryEscape("/k8s/shop/cart/app")+win+"&step=15&rate=true", &rate); code != 200 || len(rate.Series) != 1 {
		t.Fatalf("rate: %d %+v", code, rate)
	}
	// Buckets sit on the 15 s epoch grid, not on the first sample, so look at the
	// values: +30 per 15 s is 2/s; the drop from 160 to 10 is a counter reset, so
	// that bucket's increase is the new value, 10, which is 10/15 per second.
	var rates []float64
	for _, p := range rate.Series[0].Points {
		rates = append(rates, p.V)
	}
	has := func(want float64) bool {
		for _, v := range rates {
			if math.Abs(v-want) < 1e-6 {
				return true
			}
		}
		return false
	}
	if len(rates) != 4 || !has(2) || !has(10.0/15) {
		t.Errorf("rates %v, want four buckets holding 2 and %v", rates, 10.0/15)
	}
	for _, v := range rates {
		if v < 0 {
			t.Errorf("negative rate %v: a counter reset leaked through", v)
		}
	}

	var nan metricResult
	if code := r.get(usRead, "us/metrics/query_range?name=node_cpu_usage"+win, &nan); code != 200 || len(nan.Series) != 0 {
		t.Errorf("a NaN stale marker leaked into the response: %d %+v", code, nan)
	}

	// ---- project isolation ----
	var eu metricResult
	if code := r.get(euRead, "eu/metrics/query_range?name="+name+win, &eu); code != 200 || len(eu.Series) != 1 || eu.Series[0].Labels["container_id"] != "/k8s/eu-only/x/app" {
		t.Fatalf("eu data: %d %+v", code, eu)
	}
	if code := r.get(euRead, "us/metrics", nil); code != 403 {
		t.Errorf("eu key read us: %d", code)
	}
	if code := r.get(usIngest, "us/metrics", nil); code != 403 {
		t.Errorf("an ingest key read: %d", code)
	}

	// ---- traces ----
	traceID := bytes.Repeat([]byte{0xab}, 16)
	start := uint64(time.Now().Add(-2 * time.Minute).UnixNano())
	span := func(id byte, parent []byte, name, status string, dur uint64) *tracev1.Span {
		code := tracev1.Status_STATUS_CODE_OK
		if status == "ERROR" {
			code = tracev1.Status_STATUS_CODE_ERROR
		}
		return &tracev1.Span{TraceId: traceID, SpanId: bytes.Repeat([]byte{id}, 8), ParentSpanId: parent, Name: name,
			Kind: tracev1.Span_SPAN_KIND_SERVER, StartTimeUnixNano: start, EndTimeUnixNano: start + dur,
			Status:     &tracev1.Status{Code: code},
			Attributes: []*commonv1.KeyValue{{Key: "http.status_code", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: 500}}}},
			Events:     []*tracev1.Span_Event{{TimeUnixNano: start + 1, Name: "retry", Attributes: []*commonv1.KeyValue{{Key: "n", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "1"}}}}}}}
	}
	res := &resourcev1.Resource{Attributes: []*commonv1.KeyValue{{Key: "service.name", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "cart"}}}}}
	treq := &tracesv1.ExportTraceServiceRequest{ResourceSpans: []*tracev1.ResourceSpans{{Resource: res, ScopeSpans: []*tracev1.ScopeSpans{{Spans: []*tracev1.Span{
		span(1, nil, "GET /cart", "ERROR", 80_000_000),
		span(2, bytes.Repeat([]byte{1}, 8), "SELECT carts", "OK", 5_000_000),
	}}}}}}
	tb, _ := proto.Marshal(treq)
	r.must(r.post("/v1/traces", map[string]string{"X-API-Key": usIngest, "Content-Type": "application/x-protobuf"}, tb), 200)

	var spans []map[string]any
	eventually(t, "spans", func() bool {
		return r.get(usRead, "us/traces?service=cart", &spans) == 200 && len(spans) == 2
	})
	if code := r.get(usRead, "us/traces?service=cart&status=error&min_duration_ms=50&root=true", &spans); code != 200 || len(spans) != 1 || spans[0]["name"] != "GET /cart" {
		t.Fatalf("filtered spans: %d %+v", code, spans)
	}
	if code := r.get(euRead, "eu/traces?service=cart", &spans); code != 200 || len(spans) != 0 {
		t.Fatalf("eu must not see us spans: %d %+v", code, spans)
	}
	var tr []map[string]any
	if code := r.get(usRead, "us/traces/"+strings.Repeat("ab", 16), &tr); code != 200 || len(tr) != 2 || tr[0]["service"] != "cart" {
		t.Fatalf("trace: %d %+v", code, tr)
	}
	if ev, _ := tr[0]["event_names"].([]any); len(ev) != 1 || ev[0] != "retry" {
		t.Fatalf("events lost: %+v", tr[0])
	}
	var svcs []string
	if code := r.get(usRead, "us/traces/services", &svcs); code != 200 || len(svcs) != 1 || svcs[0] != "cart" {
		t.Fatalf("trace services: %d %v", code, svcs)
	}

	// ---- logs (OTLP/JSON) ----
	lreq := &logsv1.ExportLogsServiceRequest{ResourceLogs: []*logv1.ResourceLogs{{Resource: res, ScopeLogs: []*logv1.ScopeLogs{{LogRecords: []*logv1.LogRecord{
		{TimeUnixNano: uint64(time.Now().Add(-time.Minute).UnixNano()), SeverityText: "error", TraceId: traceID,
			Body: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "Connection REFUSED by redis"}}},
		{TimeUnixNano: uint64(time.Now().Add(-time.Minute).UnixNano()), SeverityText: "info",
			Body: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "started"}}},
	}}}}}}
	lb, _ := json.Marshal(json.RawMessage(mustProtoJSON(t, lreq)))
	r.must(r.post("/v1/logs", map[string]string{"X-API-Key": usIngest, "Content-Type": "application/json"}, lb), 200)
	var logs []map[string]any
	eventually(t, "logs", func() bool {
		return r.get(usRead, "us/logs?service=cart", &logs) == 200 && len(logs) == 2
	})
	if code := r.get(usRead, "us/logs?severity=error&q=refused", &logs); code != 200 || len(logs) != 1 || logs[0]["trace_id"] != strings.Repeat("ab", 16) {
		t.Fatalf("log search: %d %+v", code, logs)
	}

	// ---- profiles ----
	fnLeaf, fnRoot := &profile.Function{ID: 1, Name: "main.hot"}, &profile.Function{ID: 2, Name: "main.main"}
	locLeaf, locRoot := &profile.Location{ID: 1, Line: []profile.Line{{Function: fnLeaf}}}, &profile.Location{ID: 2, Line: []profile.Line{{Function: fnRoot}}}
	p := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		Function:   []*profile.Function{fnLeaf, fnRoot}, Location: []*profile.Location{locLeaf, locRoot},
		Sample: []*profile.Sample{
			{Location: []*profile.Location{locLeaf, locRoot}, Value: []int64{900}},
			{Location: []*profile.Location{locRoot}, Value: []int64{100}},
		},
		TimeNanos: time.Now().Add(-time.Minute).UnixNano(), DurationNanos: int64(10 * time.Second),
	}
	var pb bytes.Buffer
	if err := p.Write(&pb); err != nil {
		t.Fatal(err)
	}
	r.must(r.post("/v1/profiles?service.name=cart&pod=cart-1", map[string]string{"X-API-Key": usIngest}, pb.Bytes()), 200)
	var profs []map[string]any
	eventually(t, "profile services", func() bool {
		return r.get(usRead, "us/profiles", &profs) == 200 && len(profs) == 1
	})
	if profs[0]["service"] != "cart" || profs[0]["type"] != "cpu:nanoseconds" {
		t.Fatalf("profiles: %+v", profs)
	}
	var top []struct {
		Frames []string
		Value  int64
	}
	if code := r.get(usRead, "us/profiles/top?service=cart&type="+url.QueryEscape("cpu:nanoseconds"), &top); code != 200 || len(top) != 2 || top[0].Value != 900 || top[0].Frames[0] != "main.hot" {
		t.Fatalf("top stacks: %d %+v", code, top)
	}
	var folded string
	r.get(usRead, "us/profiles/top?service=cart&type="+url.QueryEscape("cpu:nanoseconds")+"&format=folded", &folded)
	if !strings.Contains(folded, "main.main;main.hot 900") || !strings.Contains(folded, "main.main 100") {
		t.Fatalf("folded:\n%s", folded)
	}
}

func mustProtoJSON(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := protoJSON(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A counter's first appearance. The agent creates an error counter at the first error, so
// its first stored value is the increase; a rate needs two readings and would drop it.
// from_zero=true counts it, but only for a series born inside the window: a counter that
// already existed holds an accumulated value that must never turn into a spike.
func TestFromZero(t *testing.T) {
	r := newRig(t)
	hdr := map[string]string{"X-API-Key": usIngest, "Content-Encoding": "snappy", "Content-Type": "application/x-protobuf"}
	now := time.Now()
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	send := func(name string, ts []int64, vals []float64) {
		r.must(r.post("/v1/metrics", hdr, rwBody(rwSeries(name, [][2]string{{"container_id", "/k8s/shop/web/app"}}, ts, vals))), 204)
	}
	send("fz_born_once_total", []int64{ms(-120 * time.Second)}, []float64{1})
	send("fz_born_grows_total", []int64{ms(-120 * time.Second), ms(-105 * time.Second), ms(-90 * time.Second)}, []float64{3, 3, 5})
	send("fz_old_total", []int64{ms(-2 * time.Hour), ms(-120 * time.Second)}, []float64{5000, 5003})

	window := fmt.Sprintf("&from=%d&to=%d&step=15&rate=true", now.Add(-10*time.Minute).Unix(), now.Unix())
	query := func(name string, fromZero bool) metricResult {
		var res metricResult
		path := "us/metrics/query_range?name=" + name + window
		if fromZero {
			path += "&from_zero=true"
		}
		if code := r.get(usRead, path, &res); code != 200 {
			t.Fatalf("%s: status %d", path, code)
		}
		return res
	}
	// the increase a result accounts for: each point is a rate over one 15 s step
	increase := func(res metricResult) float64 {
		var sum float64
		for _, s := range res.Series {
			for _, p := range s.Points {
				sum += p.V * 15
			}
		}
		return sum
	}
	// rows reach ClickHouse after the flush interval
	eventually(t, "the three series to be stored", func() bool {
		var names []string
		return r.get(usRead, "us/metrics", &names) == 200 && len(names) >= 3
	})
	eventually(t, "the samples to be stored", func() bool {
		return len(query("fz_old_total", false).Series) == 0 && len(query("fz_born_grows_total", false).Series) == 1
	})

	// 1. born inside the window with a single reading: invisible without from_zero, one increase with it
	if res := query("fz_born_once_total", false); len(res.Series) != 0 {
		t.Fatalf("a lone first reading has no rate, yet got %+v", res)
	}
	res := query("fz_born_once_total", true)
	if got := increase(res); len(res.Series) != 1 || math.Abs(got-1) > 1e-6 {
		t.Fatalf("from_zero: the first failure was not counted: increase %v in %+v", got, res)
	}

	// 2. born inside the window and then growing: the first value is part of the total
	if got := increase(query("fz_born_grows_total", false)); math.Abs(got-2) > 1e-6 {
		t.Fatalf("without from_zero only the later rise counts: %v, want 2", got)
	}
	if got := increase(query("fz_born_grows_total", true)); math.Abs(got-5) > 1e-6 {
		t.Fatalf("from_zero counts the birth value too: %v, want 5 (3 at birth, then +2)", got)
	}

	// 3. existed long before the window: its accumulated 5003 must not become an increase
	for _, fz := range []bool{false, true} {
		res := query("fz_old_total", fz)
		for _, s := range res.Series {
			for _, p := range s.Points {
				if p.V*15 > 1 {
					t.Fatalf("from_zero=%v turned an old counter's accumulated value into a spike: %+v", fz, p)
				}
			}
		}
		if got := increase(res); got != 0 {
			t.Fatalf("from_zero=%v: an old counter with one reading in the window has no measurable increase, got %v", fz, got)
		}
	}

	// 4. from_zero without rate is refused
	if code := r.get(usRead, "us/metrics/query_range?name=fz_old_total&step=15&from_zero=true", nil); code != 400 {
		t.Fatalf("from_zero without rate: %d, want 400", code)
	}
}

// ---- the ClickHouse information page ----

// queryServerWith serves the read API over the rig's store, with cluster details shared or not.
func queryServerWith(t *testing.T, r *rig, share bool) *httptest.Server {
	t.Helper()
	cfg := t.TempDir() + "/projects.json"
	_ = os.WriteFile(cfg, []byte(fmt.Sprintf(`{"projects":[
	 {"name":"us","ingestKeys":[%q],"readKeys":[%q]},
	 {"name":"eu","ingestKeys":[%q],"readKeys":[%q]}]}`, usIngest, usRead, euIngest, euRead)), 0o600)
	reg, err := project.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&server.Query{Projects: reg, Reader: r.st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DefaultRange: time.Hour, MaxSeries: 500, MaxPoints: 100000, ShareClusterInfo: share}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func getInfo(t *testing.T, srv *httptest.Server, project, key string) (store.ClickHouseInfo, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/projects/"+project+"/clickhouse", nil)
	req.Header.Set("X-API-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	var env struct {
		Data store.ClickHouseInfo `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return env.Data, string(raw)
}

func storageOf(t *testing.T, info store.ClickHouseInfo, kind string) store.ChStorage {
	t.Helper()
	for _, s := range info.Storage {
		if s.Type == kind {
			return s
		}
	}
	t.Fatalf("no %q in %+v", kind, info.Storage)
	return store.ChStorage{}
}

func TestClickHouseInfo(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// us has 50 log lines, the oldest 30 minutes ago; eu has 150; us has 10 spans and some samples; nobody has profiles.
	var logs []model.LogRecord
	for i := 0; i < 50; i++ {
		logs = append(logs, model.LogRecord{Project: "us", Time: now.Add(-30*time.Minute + time.Duration(i)*time.Second), Service: "api", SeverityText: "INFO", Body: fmt.Sprintf("line %d", i)})
	}
	for i := 0; i < 150; i++ {
		logs = append(logs, model.LogRecord{Project: "eu", Time: now.Add(-time.Duration(i) * time.Second), Service: "api", SeverityText: "INFO", Body: fmt.Sprintf("eu line %d", i)})
	}
	if err := r.st.WriteLogs(ctx, logs); err != nil {
		t.Fatal(err)
	}
	var spans []model.Span
	for i := 0; i < 10; i++ {
		spans = append(spans, model.Span{Project: "us", Time: now.Add(-time.Duration(i) * time.Minute), TraceID: fmt.Sprintf("%032x", i+1), SpanID: fmt.Sprintf("%016x", i+1), Name: "GET /", Kind: "SERVER", Service: "api", DurationNs: 1_000_000, StatusCode: "OK"})
	}
	if err := r.st.WriteSpans(ctx, spans); err != nil {
		t.Fatal(err)
	}
	if err := r.st.WriteSamples(ctx, []model.Sample{{Project: "us", Name: "up", Hash: 1, Time: now, Value: 1}, {Project: "us", Name: "up", Hash: 1, Time: now.Add(-time.Minute), Value: 1}}); err != nil {
		t.Fatal(err)
	}

	private := queryServerWith(t, r, false)
	info, raw := getInfo(t, private, "us", usRead)

	c := info.Connection
	if !c.Reachable || c.Protocol != "native" || c.Database != r.db || c.TLS || c.Version == "" || c.PingMs < 0 {
		t.Errorf("connection: %+v", c)
	}
	logsRow := storageOf(t, info, "logs")
	if logsRow.Rows != 50 { // this project's, not the table's 200
		t.Errorf("us logs: %d", logsRow.Rows)
	}
	if logsRow.Oldest == nil || !logsRow.Oldest.Equal(now.Add(-30*time.Minute)) {
		t.Errorf("oldest %v, want %v", logsRow.Oldest, now.Add(-30*time.Minute))
	}
	if logsRow.TTLSeconds != 24*3600 {
		t.Errorf("ttl %d", logsRow.TTLSeconds)
	}
	if logsRow.EstimatedBytes == nil || *logsRow.EstimatedBytes <= 0 || logsRow.Compression <= 0 {
		t.Errorf("a project with data should have a size and a compression ratio: %+v", logsRow)
	}
	if got := storageOf(t, info, "traces").Rows; got != 10 {
		t.Errorf("us spans: %d", got)
	}
	if got := storageOf(t, info, "metrics").Rows; got != 2 {
		t.Errorf("us samples: %d", got)
	}
	if p := storageOf(t, info, "profiles"); p.Rows != 0 || p.Oldest != nil {
		t.Errorf("no profiles: %+v", p)
	}
	if info.Cluster != nil || logsRow.TableBytes != nil || logsRow.TableRows != nil || strings.Contains(raw, "table_bytes") {
		t.Errorf("another project's volume and the disks must not be shared unless asked: %s", raw)
	}

	// A different key sees a different project's numbers.
	eu, _ := getInfo(t, private, "eu", euRead)
	if got := storageOf(t, eu, "logs").Rows; got != 150 {
		t.Errorf("eu logs: %d", got)
	}
	if got := storageOf(t, eu, "traces").Rows; got != 0 {
		t.Errorf("eu has no spans: %d", got)
	}

	// The operator may share the platform's sizes and the server's disks.
	shared, _ := getInfo(t, queryServerWith(t, r, true), "us", usRead)
	sl := storageOf(t, shared, "logs")
	if sl.TableRows == nil || *sl.TableRows != 200 || sl.TableBytes == nil || *sl.TableBytes <= 0 {
		t.Errorf("the whole table is 200 rows from both projects: %+v", sl)
	}
	if shared.Cluster == nil || len(shared.Cluster.Servers) != 1 || shared.Cluster.Servers[0].Host == "" || len(shared.Cluster.Servers[0].Disks) == 0 {
		t.Fatalf("cluster: %+v", shared.Cluster)
	}
	for _, d := range shared.Cluster.Servers[0].Disks {
		if d.Name == "" || d.Used < 0 || (!d.Unlimited && (d.Total <= 0 || d.Free < 0 || d.Free > d.Total)) {
			t.Errorf("disk %+v", d)
		}
	}

	// A project's own rows never include another's, even though the sizes are the table's.
	if *sl.EstimatedBytes >= *sl.TableBytes {
		t.Errorf("a quarter of the rows cannot be all of the bytes: %d of %d", *sl.EstimatedBytes, *sl.TableBytes)
	}

	// An unreachable database is reported with a fixed sentence: the driver's message can carry the address, and so could the password.
	// The database is reached through a forwarder that is then shut down, so the store's own connections really fail.
	fwd := newForwarder(t, env(t, "CLICKHOUSE_ADDR"))
	lost, err := store.Open(ctx, store.Options{Addrs: []string{fwd.addr}, Database: r.db, User: env(t, "CLICKHOUSE_USER"), Password: os.Getenv("CLICKHOUSE_PASSWORD")})
	if err != nil {
		t.Fatal(err)
	}
	defer lost.Close()
	fwd.stop()
	down, rawDown := getInfo(t, queryServerWith(t, &rig{t: t, st: lost, db: r.db}, false), "us", usRead)
	if down.Connection.Reachable || down.Connection.Error != "ClickHouse did not answer." {
		t.Errorf("%+v", down.Connection)
	}
	for _, secret := range []string{os.Getenv("CLICKHOUSE_ADDR"), os.Getenv("CLICKHOUSE_PASSWORD"), fwd.addr} {
		if len(secret) >= 4 && strings.Contains(rawDown, secret) {
			t.Errorf("%q leaked into %s", secret, rawDown)
		}
	}
}

// forwarder relays TCP to a target until stopped, then drops every connection and stops listening.
type forwarder struct {
	addr  string
	ln    net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func newForwarder(t *testing.T, target string) *forwarder {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &forwarder{addr: ln.Addr().String(), ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			f.mu.Lock()
			f.conns = append(f.conns, c, up)
			f.mu.Unlock()
			go func() { _, _ = io.Copy(up, c); _ = up.Close() }()
			go func() { _, _ = io.Copy(c, up); _ = c.Close() }()
		}
	}()
	t.Cleanup(f.stop)
	return f
}

func (f *forwarder) stop() {
	_ = f.ln.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conns {
		_ = c.Close()
	}
	f.conns = nil
}

// ---- tiered storage ----

// The server is started with a policy "tiered" (a second local disk stands in for the bucket): see testdata/tiered-storage.xml.
func TestTieredStorage(t *testing.T) {
	if os.Getenv("CLICKHOUSE_TIERED") == "" {
		t.Skip("needs the storage policy from testdata/tiered-storage.xml (hack/platform-mvp/test-telemetry-ingest.sh sets it up)")
	}
	addr, user, pw := env(t, "CLICKHOUSE_ADDR"), env(t, "CLICKHOUSE_USER"), os.Getenv("CLICKHOUSE_PASSWORD")
	db := fmt.Sprintf("it_tier_%d", time.Now().UnixNano())
	ctx := context.Background()
	opts := store.Options{
		Addrs: []string{addr}, Database: db, User: user, Password: pw, CreateDatabase: true,
		TTL:  store.TTLs{Metrics: 48 * time.Hour, Traces: 48 * time.Hour, Logs: 48 * time.Hour, Profiles: 48 * time.Hour},
		Tier: store.Tiering{Policy: "tiered", Volume: "cold", After: 2 * time.Hour},
	}
	st, err := store.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := clickhouse.Open(&clickhouse.Options{Addr: []string{addr}, Auth: clickhouse.Auth{Database: db, Username: user, Password: pw}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = raw.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db+" SYNC")
		_ = raw.Close()
		_ = st.Close()
	})

	// Created without a policy, as the tables of a deployment that has been running: the policy is applied to them afterwards.
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate with tiering: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("a second migrate must be a no-op: %v", err)
	}

	policyOf := func(table string) string {
		var p string
		if err := raw.QueryRow(ctx, "SELECT storage_policy FROM system.tables WHERE database = ? AND name = ?", db, table).Scan(&p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, big := range []string{"metrics_samples", "traces_spans", "logs_records", "profiling_samples"} {
		if got := policyOf(big); got != "tiered" {
			t.Errorf("%s: policy %q", big, got)
		}
	}
	for _, small := range []string{"metrics_series", "profiling_stacks", "profiling_services"} {
		if got := policyOf(small); got != "default" {
			t.Errorf("%s is a small lookup table and stays on the default policy: %q", small, got)
		}
	}
	var ddl string
	if err := raw.QueryRow(ctx, "SELECT engine_full FROM system.tables WHERE database = ? AND name = ?", db, "logs_records").Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	// ClickHouse prints the delete rule without its default action word.
	if !strings.Contains(ddl, "toIntervalSecond(7200) TO VOLUME 'cold'") || !strings.Contains(ddl, "toIntervalSecond(172800)") {
		t.Errorf("logs_records TTL should move after 2h, then delete after 48h: %s", ddl)
	}

	// Data 5 h old is past the 2 h move and inside the 48 h retention; fresh data is neither. A move is decided per part, by the
	// part's newest row, so the two are written as separate parts and merging is stopped so that they stay separate.
	if err := raw.Exec(ctx, "SYSTEM STOP MERGES logs_records"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	var oldLogs, newLogs []model.LogRecord
	for i := 0; i < 20; i++ {
		oldLogs = append(oldLogs, model.LogRecord{Project: "us", Time: now.Add(-5*time.Hour + time.Duration(i)*time.Second), Service: "api", SeverityText: "INFO", Body: fmt.Sprintf("old %d", i)})
		newLogs = append(newLogs, model.LogRecord{Project: "us", Time: now.Add(-time.Duration(i) * time.Second), Service: "api", SeverityText: "INFO", Body: fmt.Sprintf("new %d", i)})
	}
	for _, batch := range [][]model.LogRecord{oldLogs, newLogs} {
		if err := st.WriteLogs(ctx, batch); err != nil {
			t.Fatal(err)
		}
	}
	// Existing parts are moved by a background task, which a MATERIALIZE TTL asks for at once.
	if err := raw.Exec(ctx, "ALTER TABLE logs_records MATERIALIZE TTL"); err != nil {
		t.Fatal(err)
	}
	rowsOn := func(disk string) uint64 {
		var n uint64
		if err := raw.QueryRow(ctx, "SELECT sum(rows) FROM system.parts WHERE database = ? AND table = 'logs_records' AND active AND disk_name = ?", db, disk).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	deadline := time.Now().Add(60 * time.Second)
	for rowsOn("cold_disk") < 20 && time.Now().Before(deadline) {
		time.Sleep(time.Second)
	}
	if cold, hot := rowsOn("cold_disk"), rowsOn("default"); cold != 20 || hot != 20 {
		t.Fatalf("the 20 old rows should be on the cold disk and the 20 new on the default one: cold %d, default %d", cold, hot)
	}

	// Moved data is still read through the same table, by the same API.
	var n uint64
	if err := raw.QueryRow(ctx, "SELECT count() FROM logs_records WHERE Project = 'us'").Scan(&n); err != nil || n != 40 {
		t.Fatalf("count %d %v", n, err)
	}
}

// ---- incidents ----

const (
	usManageKey = "us-manage-key-000001"
	euManageKey = "eu-manage-key-000001"
)

// incidentServer serves the incident API over the rig's real store, with a manage key for each project.
func incidentServer(t *testing.T, st *store.Store) *httptest.Server {
	t.Helper()
	cfg := t.TempDir() + "/projects.json"
	_ = os.WriteFile(cfg, []byte(fmt.Sprintf(`{"projects":[
	 {"name":"us","ingestKeys":[%q],"readKeys":[%q],"manageKeys":[%q]},
	 {"name":"eu","ingestKeys":[%q],"readKeys":[%q],"manageKeys":[%q]}]}`, usIngest, usRead, usManageKey, euIngest, euRead, euManageKey)), 0o600)
	reg, err := project.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	(&server.Incidents{Projects: reg, Manager: incident.NewManager(st.Incidents(), log), Log: log}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func incCall(t *testing.T, srv *httptest.Server, method, path, key string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rd)
	req.Header.Set("X-API-Key", key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Actor", "it")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func TestIncidentsInClickHouse(t *testing.T) {
	r := newRig(t)
	srv := incidentServer(t, r.st)
	const base = "/api/v1/projects/us/incidents"

	// create (twice with one key: one incident), move, comment, attach
	code, out := incCall(t, srv, "POST", base, usManageKey, map[string]any{"title": "frontend errors", "app": "/k8s/shop/frontend", "severity": "critical", "source": "detected", "dedupe_key": "/k8s/shop/frontend|1800000000"})
	if code != 201 {
		t.Fatalf("%d %v", code, out)
	}
	id := out["data"].(map[string]any)["id"].(string)
	if code, out = incCall(t, srv, "POST", base, usManageKey, map[string]any{"title": "again", "dedupe_key": "/k8s/shop/frontend|1800000000"}); code != 200 || out["data"].(map[string]any)["id"] != id {
		t.Fatalf("the dedupe key did not find the incident in ClickHouse: %d %v", code, out)
	}
	for _, step := range []map[string]any{{"to": "acknowledged"}, {"to": "investigating", "note": "redis"}, {"to": "resolved", "resolution": "fixed"}} {
		if code, out = incCall(t, srv, "POST", base+"/"+id+"/transitions", usManageKey, step); code != 200 {
			t.Fatalf("%v: %d %v", step, code, out)
		}
	}
	if code, _ = incCall(t, srv, "POST", base+"/"+id+"/transitions", usManageKey, map[string]any{"to": "acknowledged"}); code != 409 {
		t.Fatalf("a barred move: %d", code)
	}
	if code, _ = incCall(t, srv, "POST", base+"/"+id+"/evidence", usManageKey, map[string]any{"kind": "trace", "trace_id": strings.Repeat("a", 32)}); code != 201 {
		t.Fatal(code)
	}

	// it is all there, as folded from the stored events
	code, out = incCall(t, srv, "GET", base+"/"+id, usRead, nil)
	d := out["data"].(map[string]any)
	if code != 200 || d["state"] != "resolved" || d["resolution"] != "fixed" || d["version"].(float64) != 5 || len(d["evidence"].([]any)) != 1 || d["dedupe_key"] != "/k8s/shop/frontend|1800000000" {
		t.Fatalf("%d %v", code, d)
	}
	_, out = incCall(t, srv, "GET", base+"/"+id+"/events", usRead, nil)
	evs := out["data"].([]any)
	if len(evs) != 5 || evs[0].(map[string]any)["type"] != "created" || evs[4].(map[string]any)["type"] != "evidence_added" || evs[1].(map[string]any)["actor"] != "it" {
		t.Fatalf("%v", evs)
	}
	for i, e := range evs {
		if e.(map[string]any)["seq"].(float64) != float64(i+1) {
			t.Fatalf("out of order: %v", evs)
		}
	}

	// a restart: a new server over the same database sees the same incident
	again := incidentServer(t, r.st)
	if code, out = incCall(t, again, "GET", base+"/"+id, usRead, nil); code != 200 || out["data"].(map[string]any)["state"] != "resolved" {
		t.Fatalf("after a restart: %d %v", code, out)
	}

	// another project sees nothing, and cannot reach it
	if code, _ = incCall(t, srv, "GET", "/api/v1/projects/eu/incidents/"+id, euRead, nil); code != 404 {
		t.Fatalf("%d", code)
	}
	if code, out = incCall(t, srv, "GET", "/api/v1/projects/eu/incidents", euRead, nil); code != 200 || len(out["data"].([]any)) != 0 {
		t.Fatalf("%d %v", code, out)
	}
	if code, _ = incCall(t, srv, "POST", base+"/"+id+"/comments", euManageKey, map[string]any{"text": "x"}); code != 403 {
		t.Fatalf("%d", code)
	}

	// listing: newest first, filtered by state, in ClickHouse's own order
	incCall(t, srv, "POST", base, usManageKey, map[string]any{"title": "second", "severity": "warning"})
	time.Sleep(10 * time.Millisecond)
	incCall(t, srv, "POST", base, usManageKey, map[string]any{"title": "third", "severity": "info"})
	_, out = incCall(t, srv, "GET", base, usRead, nil)
	list := out["data"].([]any)
	if len(list) != 3 || list[0].(map[string]any)["title"] != "third" || list[2].(map[string]any)["title"] != "frontend errors" {
		t.Fatalf("%v", list)
	}
	_, out = incCall(t, srv, "GET", base+"?state=open", usRead, nil)
	if got := out["data"].([]any); len(got) != 2 {
		t.Fatalf("open: %v", got)
	}
	_, out = incCall(t, srv, "GET", base+"?state=resolved,closed&app=/k8s/shop/frontend", usRead, nil)
	if got := out["data"].([]any); len(got) != 1 || got[0].(map[string]any)["id"] != id {
		t.Fatalf("resolved: %v", got)
	}
}

// A second event with a sequence number that is already in the log is refused by the store, which is what stops a second writer
// from forking an incident's history.
func TestIncidentLogRefusesARepeatedSequence(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	inc := r.st.Incidents()
	mk := func(seq uint32) incident.Event {
		return incident.Event{Project: "us", IncidentID: "abcd1234", Seq: seq, ID: fmt.Sprintf("e_%d", seq), Time: time.Now().UTC(), Type: incident.EvCommented, Actor: "it", Data: []byte(`{"text":"x"}`)}
	}
	created := incident.Event{Project: "us", IncidentID: "abcd1234", Seq: 1, ID: "e_0", Time: time.Now().UTC(), Type: incident.EvCreated, Actor: "it", DedupeKey: "k1",
		Data: []byte(`{"title":"t","severity":"warning","source":"manual","started_at":"2026-01-01T00:00:00Z"}`)}
	if err := inc.Append(ctx, created); err != nil {
		t.Fatal(err)
	}
	if err := inc.Append(ctx, mk(2)); err != nil {
		t.Fatal(err)
	}
	if err := inc.Append(ctx, mk(2)); !errors.Is(err, store.ErrSequenceTaken) {
		t.Fatalf("a repeated sequence number was accepted: %v", err)
	}
	evs, err := inc.Events(ctx, "us", "abcd1234")
	if err != nil || len(evs) != 2 {
		t.Fatalf("%v %d", err, len(evs))
	}
	// the same sequence in another project is another log
	other := mk(2)
	other.Project = "eu"
	if err := inc.Append(ctx, other); err != nil {
		t.Fatal(err)
	}
	if id, err := inc.FindByDedupeKey(ctx, "us", "k1"); err != nil || id != "abcd1234" {
		t.Fatalf("%q %v", id, err)
	}
	if id, err := inc.FindByDedupeKey(ctx, "us", "nope"); err != nil || id != "" {
		t.Fatalf("an unknown key finds nothing, without an error: %q %v", id, err)
	}
	if id, err := inc.FindByDedupeKey(ctx, "eu", "k1"); err != nil || id != "" {
		t.Fatalf("keys belong to a project: %q %v", id, err)
	}
	// what a ClickHouse round trip gives back is what was put in
	if evs[0].Time.Sub(created.Time).Abs() > time.Millisecond || string(evs[0].Data) != string(created.Data) || evs[0].Actor != "it" || evs[0].Type != incident.EvCreated {
		t.Fatalf("%+v", evs[0])
	}
}

// Many people commenting at once on one incident: every comment is kept, in an order with no gap and no repeat.
func TestIncidentsUnderConcurrentWriters(t *testing.T) {
	r := newRig(t)
	srv := incidentServer(t, r.st)
	const base = "/api/v1/projects/us/incidents"
	_, out := incCall(t, srv, "POST", base, usManageKey, map[string]any{"title": "busy"})
	id := out["data"].(map[string]any)["id"].(string)
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if code, out := incCall(t, srv, "POST", base+"/"+id+"/comments", usManageKey, map[string]any{"text": fmt.Sprintf("c%d", i)}); code != 201 {
				t.Errorf("%d %v", code, out)
			}
		}(i)
	}
	wg.Wait()
	_, out = incCall(t, srv, "GET", base+"/"+id+"/events?limit=500", usRead, nil)
	evs := out["data"].([]any)
	if len(evs) != n+1 {
		t.Fatalf("%d events", len(evs))
	}
	for i, e := range evs {
		if e.(map[string]any)["seq"].(float64) != float64(i+1) {
			t.Fatalf("a gap or a repeat at %d", i)
		}
	}
	if code, out := incCall(t, srv, "GET", base+"/"+id, usRead, nil); code != 200 || out["data"].(map[string]any)["version"].(float64) != n+1 {
		t.Fatalf("%d %v", code, out)
	}
}

// ---- what database collectors report ----

func dbServer(t *testing.T, st *store.Store) *httptest.Server {
	t.Helper()
	cfg := t.TempDir() + "/projects.json"
	_ = os.WriteFile(cfg, []byte(fmt.Sprintf(`{"projects":[
	 {"name":"us","ingestKeys":[%q],"readKeys":[%q]},
	 {"name":"eu","ingestKeys":[%q],"readKeys":[%q]}]}`, usIngest, usRead, euIngest, euRead)), 0o600)
	reg, err := project.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	(&server.Ingest{Projects: reg, Sink: nopSink{}, MaxBody: 8 << 20, Log: log, Count: server.NewCounters(), Databases: st}).Register(mux)
	(&server.Query{Projects: reg, Reader: st, Log: log, DefaultRange: time.Hour, MaxSeries: 500, MaxPoints: 100000}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type nopSink struct{}

func (nopSink) AddMetrics([]model.Sample, []model.Series, []model.MetricMeta) error { return nil }
func (nopSink) AddSpans([]model.Span) error                                         { return nil }
func (nopSink) AddLogs([]model.LogRecord) error                                     { return nil }
func (nopSink) AddProfile([]model.ProfileSample, []model.Stack) error               { return nil }

func dbCall(t *testing.T, srv *httptest.Server, method, path, key string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rd)
	req.Header.Set("X-API-Key", key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func TestDatabaseTelemetryInClickHouse(t *testing.T) {
	r := newRig(t)
	srv := dbServer(t, r.st)
	now := time.Now().UTC().Truncate(time.Second)
	at := func(ago time.Duration) string { return now.Add(-ago).Format(time.RFC3339) }
	stmt := func(id, q string, calls int, ms float64, rows int, extra map[string]float64) map[string]any {
		return map[string]any{"query_id": id, "database": "shop", "user": "app", "query": q, "calls": calls, "total_ms": ms, "rows": rows, "extra": extra}
	}
	post := func(b map[string]any) (int, map[string]any) {
		b["version"], b["target"], b["engine"] = 1, "orders-db", "postgres"
		return dbCall(t, srv, "POST", "/v1/db", usIngest, b)
	}

	// what the target allows
	if code, out := post(map[string]any{"collected_at": at(10 * time.Minute), "capabilities": map[string]any{"engine": "postgres", "version": "16.2", "statements": map[string]any{"available": true}, "activity": map[string]any{"available": true}, "slow_log": map[string]any{"available": false, "problems": []string{"log_min_duration_statement is off"}}, "settings": map[string]string{"log_min_duration_statement": "-1"}}}); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	// two intervals of statements, with a row for everything not named
	for i, ago := range []time.Duration{3 * time.Minute, 2 * time.Minute} {
		code, out := post(map[string]any{"collected_at": at(ago), "interval_seconds": 60, "statements": []any{
			stmt("1", "SELECT * FROM orders WHERE id = ?", 100*(i+1), 500*float64(i+1), 100*(i+1), map[string]float64{"shared_blks_read": 10}),
			stmt("2", "UPDATE orders SET s = ? WHERE id = ?", 10, 900, 10, nil),
			map[string]any{"query_id": "", "query": "(other statements)", "calls": 1000, "total_ms": 50, "rows": 0, "other": true},
		}})
		if code != 200 {
			t.Fatalf("%d %v", code, out)
		}
	}
	// sessions: a pile-up of a few seconds in the middle of an otherwise quiet minute
	for i, a := range []struct {
		ago     time.Duration
		blocked int
		longest float64
	}{{150 * time.Second, 0, 20}, {145 * time.Second, 7, 9000}, {140 * time.Second, 0, 30}} {
		code, out := post(map[string]any{"collected_at": at(a.ago), "activity": map[string]any{"total": 20 + i, "max_connections": 100, "by_state": map[string]int{"active": 3, "idle": 15, "idle in transaction": 2}, "waiting": map[string]int{"Lock:relation": a.blocked},
			"blocked": a.blocked, "longest_query_ms": a.longest, "longest_xact_ms": a.longest, "longest_idle_in_xact_ms": 100, "sessions": []any{map[string]any{"pid": 9, "state": "active", "query_ms": a.longest, "query": "UPDATE t SET a = ?"}}}})
		if code != 200 {
			t.Fatalf("%d %v", code, out)
		}
	}
	for _, q := range []struct {
		ms  float64
		sql string
	}{{301.5, "SELECT pg_sleep(?)"}, {1200, "SELECT * FROM big"}, {450, "UPDATE t SET a = ?"}} {
		code, out := post(map[string]any{"collected_at": at(time.Minute), "slow_queries": []any{map[string]any{"time": at(time.Minute), "duration_ms": q.ms, "kind": "statement", "pid": 7, "database": "shop", "user": "app", "query": q.sql}}})
		if code != 200 {
			t.Fatalf("%d %v", code, out)
		}
	}

	const base = "/api/v1/projects/us/databases"
	// the targets
	code, out := dbCall(t, srv, "GET", base, usRead, nil)
	list := out["data"].([]any)
	if code != 200 || len(list) != 1 {
		t.Fatalf("%d %v", code, out)
	}
	tg := list[0].(map[string]any)
	caps := tg["capabilities"].(map[string]any)
	if tg["target"] != "orders-db" || tg["engine"] != "postgres" || tg["version"] != "16.2" || caps["slow_log"].(map[string]any)["problems"].([]any)[0] != "log_min_duration_statement is off" {
		t.Fatalf("%v", tg)
	}

	// statements: summed over the window, the unnamed row left out of the list and kept in the totals
	code, out = dbCall(t, srv, "GET", base+"/orders-db/statements?order=total_ms", usRead, nil)
	d := out["data"].(map[string]any)
	rows := d["statements"].([]any)
	if code != 200 || len(rows) != 2 {
		t.Fatalf("%d %v", code, out)
	}
	first := rows[0].(map[string]any)
	if first["query_id"] != "2" || first["calls"].(float64) != 20 || first["total_ms"].(float64) != 1800 || first["mean_ms"].(float64) != 90 {
		t.Fatalf("by time, the update is first: %v", first)
	}
	second := rows[1].(map[string]any)
	if second["calls"].(float64) != 300 || second["total_ms"].(float64) != 1500 || second["rows"].(float64) != 300 || second["extra"].(map[string]any)["shared_blks_read"].(float64) != 20 {
		t.Fatalf("the select is summed over both intervals: %v", second)
	}
	if d["total_calls"].(float64) != 300+20+2000 || d["total_ms"].(float64) != 1500+1800+100 {
		t.Fatalf("the totals include the unnamed row: %v", d)
	}
	_, out = dbCall(t, srv, "GET", base+"/orders-db/statements?order=calls&limit=1", usRead, nil)
	if got := out["data"].(map[string]any)["statements"].([]any); len(got) != 1 || got[0].(map[string]any)["query_id"] != "1" {
		t.Fatalf("by calls, the select: %v", got)
	}
	_, out = dbCall(t, srv, "GET", base+"/orders-db/statements?user=nobody", usRead, nil)
	if got := out["data"].(map[string]any)["statements"].([]any); len(got) != 0 {
		t.Fatalf("%v", got)
	}

	// activity: the peak within each bucket, so the pile-up is not averaged away, and the latest sessions
	code, out = dbCall(t, srv, "GET", base+"/orders-db/activity?step=60", usRead, nil)
	if code != 200 {
		t.Fatalf("activity: %d %v", code, out)
	}
	d = out["data"].(map[string]any)
	pts := d["points"].([]any)
	if code != 200 || len(pts) == 0 {
		t.Fatalf("%d %v", code, out)
	}
	peak := 0.0
	for _, p := range pts {
		if b := p.(map[string]any)["blocked"].(float64); b > peak {
			peak = b
		}
	}
	if peak != 7 {
		t.Fatalf("the peak of blocked sessions is 7: %v", pts)
	}
	latest := d["latest"].(map[string]any)
	if latest["sessions"].([]any)[0].(map[string]any)["query"] != "UPDATE t SET a = ?" {
		t.Fatalf("%v", latest)
	}

	// slow statements
	_, out = dbCall(t, srv, "GET", base+"/orders-db/slow?order=duration", usRead, nil)
	slow := out["data"].([]any)
	if len(slow) != 3 || slow[0].(map[string]any)["duration_ms"].(float64) != 1200 || slow[2].(map[string]any)["duration_ms"].(float64) != 301.5 {
		t.Fatalf("%v", slow)
	}
	_, out = dbCall(t, srv, "GET", base+"/orders-db/slow?min_ms=400", usRead, nil)
	if got := out["data"].([]any); len(got) != 2 {
		t.Fatalf("%v", got)
	}

	// a collector that sends a literal is refused, and nothing is stored
	code, out = post(map[string]any{"collected_at": at(time.Second), "statements": []any{stmt("9", "SELECT * FROM users WHERE email = 'alice@example.com'", 1, 1, 1, nil)}})
	if code != 400 || strings.Contains(fmt.Sprint(out), "alice") {
		t.Fatalf("%d %v", code, out)
	}
	_, out = dbCall(t, srv, "GET", base+"/orders-db/statements", usRead, nil)
	for _, row := range out["data"].(map[string]any)["statements"].([]any) {
		if row.(map[string]any)["query_id"] == "9" {
			t.Fatal("a refused batch was stored")
		}
	}

	// another project sees nothing of it, and cannot read it with the first project's key
	code, out = dbCall(t, srv, "GET", "/api/v1/projects/eu/databases", euRead, nil)
	if code != 200 || len(out["data"].([]any)) != 0 {
		t.Fatalf("%d %v", code, out)
	}
	_, out = dbCall(t, srv, "GET", "/api/v1/projects/eu/databases/orders-db/statements", euRead, nil)
	if len(out["data"].(map[string]any)["statements"].([]any)) != 0 {
		t.Fatalf("%v", out)
	}
	if code, _ := dbCall(t, srv, "GET", "/api/v1/projects/eu/databases", usRead, nil); code != 403 {
		t.Fatalf("%d", code)
	}
	// a read key cannot send
	if code, _ := dbCall(t, srv, "POST", "/v1/db", usRead, map[string]any{"version": 1}); code != 401 {
		t.Fatalf("%d", code)
	}
}

func TestDatabaseTablesHaveRetentionAndAppearInStorage(t *testing.T) {
	r := newRig(t)
	raw, err := clickhouse.Open(&clickhouse.Options{Addr: []string{os.Getenv("CLICKHOUSE_ADDR")}, Auth: clickhouse.Auth{Database: r.db, Username: os.Getenv("CLICKHOUSE_USER"), Password: os.Getenv("CLICKHOUSE_PASSWORD")}})
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, tbl := range []string{"db_targets", "db_statements", "db_activity", "db_slow_queries", "db_pool_samples", "db_pools", "db_pool_stats"} {
		var ddl string
		if err := raw.QueryRow(context.Background(), "SELECT engine_full FROM system.tables WHERE database = ? AND name = ?", r.db, tbl).Scan(&ddl); err != nil {
			t.Fatalf("%s: %v", tbl, err)
		}
		if !strings.Contains(ddl, "TTL") || !strings.Contains(ddl, "toIntervalSecond(43200)") {
			t.Errorf("%s keeps data for ever, or for the wrong time: %s", tbl, ddl)
		}
	}
	srv := queryServerWith(t, r, true)
	info, _ := getInfo(t, srv, "us", usRead)
	if d := storageOf(t, info, "databases"); d.TTLSeconds != 12*3600 {
		t.Fatalf("%+v", d)
	}
}

func TestPoolTelemetryInClickHouse(t *testing.T) {
	r := newRig(t)
	srv := dbServer(t, r.st)
	now := time.Now().UTC().Truncate(time.Second)
	// with milliseconds, as a collector's times have: a query that matches a time at whole-second precision finds nothing
	at := func(ago time.Duration) string {
		return now.Add(-ago).Add(668 * time.Millisecond).Format(time.RFC3339Nano)
	}
	pool := func(db, user string, waiting, active, idle int, maxWait float64) map[string]any {
		return map[string]any{"database": db, "user": user, "mode": "transaction", "cl_active": active, "cl_waiting": waiting, "sv_active": active, "sv_idle": idle, "max_wait_ms": maxWait, "pool_size": 2, "reserve_pool": 1}
	}
	post := func(b map[string]any) {
		t.Helper()
		b["version"], b["target"], b["engine"] = 1, "pgb", "pgbouncer"
		if code, out := dbCall(t, srv, "POST", "/v1/db", usIngest, b); code != 200 {
			t.Fatalf("%d %v", code, out)
		}
	}
	post(map[string]any{"collected_at": at(10 * time.Minute), "capabilities": map[string]any{"engine": "pgbouncer", "version": "1.26.1", "pools": map[string]any{"available": true}}})
	// a quiet sample, a queue of a few seconds (clients waiting at the pooler while the database sees two connections), a quiet sample again
	for i, s := range []struct {
		ago     time.Duration
		waiting int
		maxWait float64
	}{{200 * time.Second, 0, 0}, {190 * time.Second, 6, 2500}, {180 * time.Second, 0, 0}} {
		post(map[string]any{"collected_at": at(s.ago), "interval_seconds": 10, "pools": map[string]any{
			"pools":           []any{pool("shop", "app", s.waiting, 2, 0, s.maxWait), pool("reports", "ro", 0, 1, 1, 0)},
			"stats":           []any{map[string]any{"database": "shop", "xacts": 10 * (i + 1), "queries": 20 * (i + 1), "bytes_in": 1000, "bytes_out": 5000, "xact_ms": 400.0 * float64(i+1), "query_ms": 300.0, "wait_ms": 100.0 * float64(i+1)}},
			"max_client_conn": 100, "clients": 2 + s.waiting + 1, "waiting": s.waiting, "max_wait_ms": s.maxWait,
		}})
	}

	const base = "/api/v1/projects/us/databases/pgb/pools"
	code, out := dbCall(t, srv, "GET", base+"?step=10", usRead, nil)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	d := out["data"].(map[string]any)
	pts := d["points"].([]any)
	peakWaiting, peakWait := 0.0, 0.0
	for _, p := range pts {
		m := p.(map[string]any)
		if w := m["waiting"].(float64); w > peakWaiting {
			peakWaiting = w
		}
		if w := m["max_wait_ms"].(float64); w > peakWait {
			peakWait = w
		}
		if m["max_client_conn"].(float64) != 100 {
			t.Fatalf("%v", m)
		}
	}
	if len(pts) != 3 || peakWaiting != 6 || peakWait != 2500 {
		t.Fatalf("the queue of a few seconds is a peak, not an average: %d points, peak %v, %v ms: %v", len(pts), peakWaiting, peakWait, pts)
	}
	// the pools as they were in the newest sample: quiet again
	latest := d["latest"].(map[string]any)["pools"].([]any)
	if len(latest) != 2 || latest[0].(map[string]any)["cl_waiting"].(float64) != 0 || latest[0].(map[string]any)["pool_size"].(float64) != 2 || latest[0].(map[string]any)["mode"] != "transaction" {
		t.Fatalf("%v", latest)
	}
	// what went through, over the window, and what the waiting cost
	stats := d["stats"].([]any)
	if len(stats) != 1 {
		t.Fatalf("%v", stats)
	}
	s := stats[0].(map[string]any)
	if s["database"] != "shop" || s["xacts"].(float64) != 60 || s["queries"].(float64) != 120 || s["xact_ms"].(float64) != 2400 || s["wait_ms"].(float64) != 600 {
		t.Fatalf("%v", s)
	}
	if s["avg_xact_ms"].(float64) != 40 || s["avg_wait_ms"].(float64) != 10 || s["wait_share"].(float64) != 0.2 {
		t.Fatalf("600 of 3000 ms: a fifth of the time went on waiting for a server connection: %v", s)
	}
	// a window with nothing in it: empty lists, and no "latest"
	code, out = dbCall(t, srv, "GET", base+"?from="+fmt.Sprint(now.Add(-48*time.Hour).Unix())+"&to="+fmt.Sprint(now.Add(-47*time.Hour).Unix()), usRead, nil)
	d = out["data"].(map[string]any)
	if code != 200 || len(d["points"].([]any)) != 0 || len(d["stats"].([]any)) != 0 || d["latest"] != nil {
		t.Fatalf("%d %v", code, d)
	}
	// another project sees none of it
	_, out = dbCall(t, srv, "GET", "/api/v1/projects/eu/databases/pgb/pools", euRead, nil)
	if d := out["data"].(map[string]any); len(d["points"].([]any)) != 0 || d["latest"] != nil {
		t.Fatalf("%v", d)
	}
	// the pooler is a target in the list, with what it allows
	_, out = dbCall(t, srv, "GET", "/api/v1/projects/us/databases", usRead, nil)
	list := out["data"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["engine"] != "pgbouncer" || list[0].(map[string]any)["capabilities"].(map[string]any)["pools"].(map[string]any)["available"] != true {
		t.Fatalf("%v", list)
	}
}

// The detector on counters as the node-agent stores them: cumulative, one reading every 15 seconds, turned into rates by the store's own
// query. A web application serves 10 requests a second and, for the last 30 minutes, fails 5 a second more.
func TestDetectorOnStoredCounters(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	failFrom := now.Add(-30 * time.Minute)
	web := "/k8s/shop/web-5d6f8c7b9-xk2lp/app"
	counters := []struct {
		hash   uint64
		status string
		rate   func(time.Time) float64
	}{
		{1, "200", func(time.Time) float64 { return 10 }},
		{2, "500", func(at time.Time) float64 {
			if at.Before(failFrom) {
				return 0
			}
			return 5
		}},
	}
	var samples []model.Sample
	var series []model.Series
	for _, c := range counters {
		series = append(series, model.Series{Project: "us", Name: detect.RequestsMetric, Hash: c.hash, Labels: map[string]string{"container_id": web, "status": c.status}, Seen: now})
		total := 0.0
		for at := now.Add(-210 * time.Minute); !at.After(now); at = at.Add(15 * time.Second) {
			total += c.rate(at) * 15
			samples = append(samples, model.Sample{Project: "us", Name: detect.RequestsMetric, Hash: c.hash, Time: at, Value: total})
		}
	}
	if err := r.st.WriteSeries(ctx, series); err != nil {
		t.Fatal(err)
	}
	if err := r.st.WriteSamples(ctx, samples); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := incident.NewManager(r.st.Incidents(), log)
	run := &detect.Runner{
		Projects: func() []string { return []string{"us"} }, Source: r.st, Incidents: mgr, Config: detect.Defaults(), Log: log,
		Interval: time.Minute, Lookback: 3 * time.Hour, MaxAge: 30 * time.Minute, Step: 60, MaxSeries: 100, MaxPoints: 100000,
	}
	if err := run.Validate(); err != nil {
		t.Fatal(err)
	}
	n, err := run.Project(ctx, "us")
	if err != nil || n != 1 {
		t.Fatalf("opened %d, err %v", n, err)
	}
	// a second look, and one by a new manager over the same database (a restart): still one
	if n, err = run.Project(ctx, "us"); err != nil || n != 0 {
		t.Fatalf("second look opened %d, err %v", n, err)
	}
	run.Incidents = incident.NewManager(r.st.Incidents(), log)
	if n, err = run.Project(ctx, "us"); err != nil || n != 0 {
		t.Fatalf("after a restart opened %d, err %v", n, err)
	}

	got, _, err := mgr.List(ctx, "us", incident.Filter{App: "/k8s/shop/web"})
	if err != nil || len(got) != 1 {
		t.Fatalf("incidents = %d, err %v", len(got), err)
	}
	in := got[0]
	if in.Source != incident.Detected || in.Severity != incident.Critical || in.State != incident.Open || in.Title != "shop/web: availability objective burning" {
		t.Fatalf("incident = %+v", in)
	}
	if d := in.StartedAt.Sub(failFrom); d < -2*time.Minute || d > 2*time.Minute {
		t.Fatalf("started %v, the trouble began %v", in.StartedAt, failFrom)
	}
	if len(in.Evidence) != 2 || !strings.Contains(in.Summary, "of requests failed") {
		t.Fatalf("evidence %d, summary %q", len(in.Evidence), in.Summary)
	}
	// another project's data is not looked at: "eu" has none
	if n, err = (&detect.Runner{Projects: func() []string { return []string{"eu"} }, Source: r.st, Incidents: mgr, Config: detect.Defaults(), Log: log,
		Interval: time.Minute, Lookback: 3 * time.Hour, MaxAge: 30 * time.Minute, Step: 60, MaxSeries: 100, MaxPoints: 100000}).Project(ctx, "eu"); err != nil || n != 0 {
		t.Fatalf("eu opened %d, err %v", n, err)
	}
}

// What a cluster collector reports, as ClickHouse keeps it: versions of each object, what is current, what a delete and a sweep do, events
// that change their count, the heartbeat, and the walls between projects and between clusters.
func TestInventoryInClickHouse(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	t0 := time.Now().UTC().Truncate(time.Millisecond).Add(-10 * time.Minute)
	dep := func(uid, name string, desired int) inventory.Object {
		return inventory.Object{Kind: inventory.Deployment, UID: uid, Namespace: "shop", Name: name, Labels: map[string]string{"app": name}, Created: t0.Add(-time.Hour),
			Data: map[string]any{"desired": desired, "ready": desired}}
	}
	send := func(project string, b inventory.Batch) {
		t.Helper()
		if err := b.Validate(time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := r.st.WriteInventoryBatch(ctx, project, b); err != nil {
			t.Fatal(err)
		}
	}
	list := func(project, cluster string, kinds ...string) map[string]store.InventoryObject {
		t.Helper()
		rows, err := r.st.ListInventory(ctx, store.InventoryQuery{Project: project, Cluster: cluster, Kinds: kinds, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]store.InventoryObject{}
		for _, o := range rows {
			out[o.Name] = o
		}
		return out
	}

	// a first, full report: three deployments and a node
	node := inventory.Object{Kind: inventory.Node, UID: "node-1", Name: "node-a", Created: t0.Add(-24 * time.Hour), Data: map[string]any{"ready": true}}
	send("us", inventory.Batch{Version: 1, Cluster: "gke", CollectedAt: t0, Objects: []inventory.Object{dep("d1", "cartservice", 3), dep("d2", "adservice", 2), dep("d3", "paymentservice", 1), node}})
	got := list("us", "", inventory.Deployment)
	if len(got) != 3 || string(got["cartservice"].Data) != `{"desired":3,"ready":3}` || got["cartservice"].Labels["app"] != "cartservice" || got["cartservice"].Namespace != "shop" {
		t.Fatalf("after the first report: %+v", got)
	}
	if n := list("us", "", inventory.Node); len(n) != 1 || n["node-a"].Namespace != "" {
		t.Fatalf("nodes: %+v", n)
	}

	// one is scaled to zero: its newest version is what is current, and the history keeps the old one
	send("us", inventory.Batch{Version: 1, Cluster: "gke", CollectedAt: t0.Add(time.Minute), Objects: []inventory.Object{dep("d1", "cartservice", 0)}})
	if got = list("us", "", inventory.Deployment); len(got) != 3 || string(got["cartservice"].Data) != `{"desired":0,"ready":0}` || !got["cartservice"].Observed.Equal(t0.Add(time.Minute)) {
		t.Fatalf("after the change: %+v", got)
	}
	hist, err := r.st.InventoryHistory(ctx, "us", "gke", inventory.Deployment, "d1", 10)
	if err != nil || len(hist) != 2 || string(hist[0].Data) != `{"desired":0,"ready":0}` || string(hist[1].Data) != `{"desired":3,"ready":3}` {
		t.Fatalf("history: %+v %v", hist, err)
	}
	// the same report sent again (a retry) is one version, not two
	send("us", inventory.Batch{Version: 1, Cluster: "gke", CollectedAt: t0.Add(time.Minute), Objects: []inventory.Object{dep("d1", "cartservice", 0)}})
	if hist, _ = r.st.InventoryHistory(ctx, "us", "gke", inventory.Deployment, "d1", 10); len(hist) != 2 {
		t.Fatalf("a retried batch made %d versions", len(hist))
	}

	// a delete
	send("us", inventory.Batch{Version: 1, Cluster: "gke", CollectedAt: t0.Add(2 * time.Minute), Deleted: []inventory.Ref{{Kind: inventory.Deployment, UID: "d2", Namespace: "shop", Name: "adservice"}}})
	if got = list("us", "", inventory.Deployment); len(got) != 2 || got["adservice"].UID != "" {
		t.Fatalf("after the delete: %+v", got)
	}
	if hist, _ = r.st.InventoryHistory(ctx, "us", "gke", inventory.Deployment, "d2", 10); len(hist) != 2 || !hist[0].Deleted || hist[1].Deleted {
		t.Fatalf("the deleted object's history: %+v", hist)
	}

	// a resync that re-sends cartservice only, then sweeps: paymentservice was not re-sent, so it is gone
	cut := t0.Add(10 * time.Minute)
	send("us", inventory.Batch{Version: 1, Cluster: "gke", CollectedAt: cut, Objects: []inventory.Object{dep("d1", "cartservice", 0)}, Sweeps: []inventory.Sweep{{Kind: inventory.Deployment, Before: cut}}})
	if got = list("us", "", inventory.Deployment); len(got) != 1 || got["cartservice"].UID != "d1" {
		t.Fatalf("after the sweep: %+v", got)
	}
	// the sweep of one kind leaves the others alone
	if n := list("us", "", inventory.Node); len(n) != 1 {
		t.Fatalf("a deployment sweep removed a node: %+v", n)
	}
	// and an object that comes back after being swept is current again
	send("us", inventory.Batch{Version: 1, Cluster: "gke", CollectedAt: cut.Add(time.Minute), Objects: []inventory.Object{dep("d3", "paymentservice", 1)}})
	if got = list("us", "", inventory.Deployment); len(got) != 2 {
		t.Fatalf("after it came back: %+v", got)
	}

	// filters
	rows, _ := r.st.ListInventory(ctx, store.InventoryQuery{Project: "us", Kinds: []string{inventory.Deployment}, Namespace: "shop", Name: "paymentservice", Limit: 10})
	if len(rows) != 1 {
		t.Fatalf("by name: %+v", rows)
	}
	if rows, _ = r.st.ListInventory(ctx, store.InventoryQuery{Project: "us", Kinds: []string{inventory.Deployment}, Namespace: "other", Limit: 10}); len(rows) != 0 {
		t.Fatalf("by another namespace: %+v", rows)
	}
	if rows, _ = r.st.ListInventory(ctx, store.InventoryQuery{Project: "us", Kinds: []string{inventory.Deployment}, Limit: 1}); len(rows) != 1 {
		t.Fatalf("a limit of one: %+v", rows)
	}
	if _, err := r.st.ListInventory(ctx, store.InventoryQuery{Project: "us", Kinds: []string{"x'; DROP TABLE inventory_objects; --"}, Limit: 1}); err == nil {
		t.Fatal("a kind that is not one of the known was spliced into a query")
	}

	// walls: another project, and another cluster of this one
	if n := list("eu", "", inventory.Deployment); len(n) != 0 {
		t.Fatalf("eu sees us's objects: %+v", n)
	}
	send("us", inventory.Batch{Version: 1, Cluster: "aks", CollectedAt: cut.Add(2 * time.Minute), Objects: []inventory.Object{dep("a1", "frontend", 2)}})
	if n := list("us", "aks", inventory.Deployment); len(n) != 1 || n["frontend"].Cluster != "aks" {
		t.Fatalf("by cluster: %+v", n)
	}
	if n := list("us", "gke", inventory.Deployment); len(n) != 2 || n["frontend"].UID != "" {
		t.Fatalf("a cluster's own: %+v", n)
	}
	// a sweep in one cluster does not touch another's objects
	send("us", inventory.Batch{Version: 1, Cluster: "aks", CollectedAt: cut.Add(3 * time.Minute), Sweeps: []inventory.Sweep{{Kind: inventory.Deployment, Before: cut.Add(3 * time.Minute)}}})
	if n := list("us", "gke", inventory.Deployment); len(n) != 2 {
		t.Fatalf("aks's sweep removed gke's objects: %+v", n)
	}
	if n := list("us", "aks", inventory.Deployment); len(n) != 0 {
		t.Fatalf("aks's sweep did not remove its own: %+v", n)
	}
	// no cluster named: the project's name is used
	send("eu", inventory.Batch{Version: 1, CollectedAt: cut, Objects: []inventory.Object{dep("e1", "web", 1)}})
	if n := list("eu", "eu", inventory.Deployment); len(n) != 1 {
		t.Fatalf("the default cluster name: %+v", n)
	}

	// events: the same event again with a higher count is one event, as it last stood
	ev := func(count int, at time.Time) inventory.Event {
		return inventory.Event{UID: "ev-1", Namespace: "shop", Kind: "Pod", Name: "cart-0", Type: "Warning", Reason: "BackOff", Message: "Back-off restarting failed container", Count: count, First: t0, Last: at, Source: "kubelet"}
	}
	send("us", inventory.Batch{Version: 1, Cluster: "gke", CollectedAt: cut, Events: []inventory.Event{ev(1, t0.Add(time.Minute)), {UID: "ev-2", Namespace: "shop", Kind: "Deployment", Name: "cartservice", Type: "Normal", Reason: "ScalingReplicaSet", Message: "Scaled down", Count: 1, Last: t0.Add(2 * time.Minute)}}})
	send("us", inventory.Batch{Version: 1, Cluster: "gke", CollectedAt: cut.Add(time.Minute), Events: []inventory.Event{ev(5, t0.Add(5*time.Minute))}})
	evs, err := r.st.ListInventoryEvents(ctx, store.InventoryEventQuery{Project: "us", From: t0.Add(-time.Hour), To: time.Now(), Limit: 10})
	if err != nil || len(evs) != 2 || evs[0].UID != "ev-1" || evs[0].Count != 5 || !evs[0].Last.Equal(t0.Add(5*time.Minute)) || !evs[0].First.Equal(t0) || evs[0].Source != "kubelet" {
		t.Fatalf("events: %+v %v", evs, err)
	}
	if evs, _ = r.st.ListInventoryEvents(ctx, store.InventoryEventQuery{Project: "us", Type: "Warning", Reason: "BackOff", Kind: "Pod", Namespace: "shop", Name: "cart-0", From: t0.Add(-time.Hour), To: time.Now(), Limit: 10}); len(evs) != 1 {
		t.Fatalf("filtered events: %+v", evs)
	}
	if evs, _ = r.st.ListInventoryEvents(ctx, store.InventoryEventQuery{Project: "eu", From: t0.Add(-time.Hour), To: time.Now(), Limit: 10}); len(evs) != 0 {
		t.Fatalf("eu sees us's events: %+v", evs)
	}
	if evs, _ = r.st.ListInventoryEvents(ctx, store.InventoryEventQuery{Project: "us", From: t0.Add(-time.Hour), To: t0.Add(90 * time.Second), Limit: 10}); len(evs) != 1 || evs[0].UID != "ev-1" {
		t.Fatalf("by time: %+v", evs)
	}

	// the summary: every cluster that has reported, when, and what is in it now
	sum, err := r.st.InventorySummary(ctx, "us")
	if err != nil || len(sum) != 2 || sum[0].Cluster != "aks" || sum[1].Cluster != "gke" {
		t.Fatalf("summary: %+v %v", sum, err)
	}
	gke := sum[1]
	if gke.Counts[inventory.Deployment] != 2 || gke.Counts[inventory.Node] != 1 || time.Since(gke.LastReceived) > time.Minute || !gke.LastCollected.Equal(cut.Add(time.Minute)) {
		t.Fatalf("gke: %+v", gke)
	}
	if sum, _ = r.st.InventorySummary(ctx, "nobody"); len(sum) != 0 || sum == nil {
		t.Fatalf("an unknown project: %+v", sum)
	}
}

// Silence on what ClickHouse holds: the cluster collector's inventory beside the node-agent's memory gauge. The payment service wants three and has
// none (and stopped sending twenty minutes ago), the ad service is ready and stopped, the cart was scaled to zero, the frontend is sending.
func TestDetectorSilenceOnStoredInventory(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Minute)
	clock := base
	dep := func(name string, desired, ready int) inventory.Object {
		return inventory.Object{Kind: inventory.Deployment, UID: "d-" + name, Namespace: "shop", Name: name, Created: base.Add(-24 * time.Hour), Data: map[string]any{"desired": desired, "ready": ready}}
	}
	pod := func(workload string) inventory.Object {
		return inventory.Object{Kind: inventory.Pod, UID: "p-" + workload, Namespace: "shop", Name: workload + "-5d6f8c7b9-xk2lp", Created: base.Add(-time.Hour), Data: map[string]any{
			"phase": "Running", "ready": false, "workload_kind": "deployment", "workload_name": workload,
			"containers": []any{map[string]any{"name": "app", "state": "waiting", "reason": "CrashLoopBackOff", "last_reason": "OOMKilled"}}}}
	}
	batch := inventory.Batch{Version: 1, Cluster: "gke", CollectedAt: base, Objects: []inventory.Object{
		dep("frontend", 2, 2), dep("cartservice", 0, 0), dep("paymentservice", 3, 0), dep("adservice", 1, 1), pod("paymentservice"),
	}}
	if err := batch.Validate(time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := r.st.WriteInventoryBatch(ctx, "us", batch); err != nil {
		t.Fatal(err)
	}

	// the memory gauge: one reading a minute; the frontend up to now and beyond (so it is still sending when the clock moves), the others until twenty minutes ago
	var samples []model.Sample
	var series []model.Series
	gauge := func(hash uint64, container string, until time.Time) {
		series = append(series, model.Series{Project: "us", Name: detect.RSSMetric, Hash: hash, Labels: map[string]string{"container_id": container}, Seen: base})
		for at := base.Add(-50 * time.Minute); !at.After(until); at = at.Add(time.Minute) {
			samples = append(samples, model.Sample{Project: "us", Name: detect.RSSMetric, Hash: hash, Time: at, Value: 1 << 20})
		}
	}
	gauge(1, "/k8s/shop/frontend-5d6f8c7b9-xk2lp/app", base.Add(10*time.Minute))
	gauge(2, "/k8s/shop/cartservice-5d6f8c7b9-xk2lp/app", base.Add(-20*time.Minute))
	gauge(3, "/k8s/shop/paymentservice-5d6f8c7b9-xk2lp/app", base.Add(-20*time.Minute))
	gauge(4, "/k8s/shop/adservice-5d6f8c7b9-xk2lp/app", base.Add(-20*time.Minute))
	if err := r.st.WriteSeries(ctx, series); err != nil {
		t.Fatal(err)
	}
	if err := r.st.WriteSamples(ctx, samples); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := incident.NewManager(r.st.Incidents(), log)
	newRunner := func(m *incident.Manager) *detect.Runner {
		return &detect.Runner{
			Projects: func() []string { return []string{"us"} }, Source: r.st, Incidents: m, Config: detect.Defaults(), Log: log,
			Interval: time.Minute, Lookback: 3 * time.Hour, MaxAge: 30 * time.Minute, Step: 60, MaxSeries: 100, MaxPoints: 100000,
			SilenceAfter: 5 * time.Minute, Now: func() time.Time { return clock },
		}
	}
	run := newRunner(mgr)
	if err := run.Validate(); err != nil {
		t.Fatal(err)
	}
	if n, err := run.Project(ctx, "us"); err != nil || n != 0 {
		t.Fatalf("the first look opened %d, err %v", n, err)
	}
	clock = base.Add(6 * time.Minute)
	n, err := run.Project(ctx, "us")
	if err != nil || n != 2 {
		t.Fatalf("after the delay: opened %d, err %v", n, err)
	}
	if n, err = run.Project(ctx, "us"); err != nil || n != 0 {
		t.Fatalf("a second look opened %d, err %v", n, err)
	}
	// a restart: a new runner over the same database starts its count again, and does not open what is already open
	run2 := newRunner(incident.NewManager(r.st.Incidents(), log))
	if n, err = run2.Project(ctx, "us"); err != nil || n != 0 {
		t.Fatalf("after a restart: %d, %v", n, err)
	}
	clock = base.Add(12 * time.Minute)
	if n, err = run2.Project(ctx, "us"); err != nil || n != 0 {
		t.Fatalf("after a restart and the delay: %d, %v (they are open already)", n, err)
	}

	got := map[string]incident.Incident{}
	list, _, err := mgr.List(ctx, "us", incident.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range list {
		got[i.App] = i
	}
	pay, ad := got["/k8s/shop/paymentservice"], got["/k8s/shop/adservice"]
	if len(got) != 2 || pay.Severity != incident.Critical || pay.Title != "shop/paymentservice: not ready (0 of 3)" || ad.Severity != incident.Warning || ad.Title != "shop/adservice: ready but sending no data" {
		t.Fatalf("incidents: %+v", got)
	}
	if d := pay.StartedAt.Sub(base.Add(-20 * time.Minute)); d < -time.Minute || d > time.Minute {
		t.Fatalf("it began at its last reading (%v), not %v", base.Add(-20*time.Minute), pay.StartedAt)
	}
	if !strings.Contains(pay.Summary, "CrashLoopBackOff (OOMKilled)") || len(pay.Evidence) != 2 {
		t.Fatalf("summary %q, evidence %d", pay.Summary, len(pay.Evidence))
	}
}

// Enrollment against ClickHouse: a token made with a manage key, redeemed once for an ingest key of the cluster's own, which the real ingest
// handler then accepts; and what survives a restart, what a revoke does, and that neither the token nor the key is ever in the table.
func TestEnrollmentInClickHouse(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	cfg := t.TempDir() + "/projects.json"
	_ = os.WriteFile(cfg, []byte(fmt.Sprintf(`{"projects":[
	 {"name":"us","ingestKeys":[%q],"readKeys":[%q],"manageKeys":[%q]},
	 {"name":"eu","ingestKeys":[%q],"readKeys":[%q],"manageKeys":[%q]}]}`, usIngest, usRead, usManageKey, euIngest, euRead, euManageKey)), 0o600)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// a service: what main wires, built from the same pieces, so that a "restart" is building it again over the same database
	start := func() (*httptest.Server, *project.Registry, *enroll.Manager) {
		reg, err := project.Load(cfg)
		if err != nil {
			t.Fatal(err)
		}
		m := enroll.NewManager(r.st.Enrollments())
		refresh := func() {
			keys, err := m.Keys(ctx)
			if err != nil {
				t.Fatal(err)
			}
			issued := map[[32]byte]string{}
			for _, k := range keys {
				issued[k.Hash] = k.Project
			}
			reg.SetIssuedIngest(issued)
		}
		m.OnKeys = refresh
		refresh()
		mux := http.NewServeMux()
		(&server.Enroll{Projects: reg, Manager: m, Log: log, Count: server.NewCounters()}).Register(mux)
		(&server.Ingest{Projects: reg, Sink: nopSink{}, MaxBody: 1 << 20, Log: log, Count: server.NewCounters(), Inventory: r.st}).Register(mux)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return srv, reg, m
	}
	call := func(srv *httptest.Server, method, path, key string, body any) (int, string) {
		var rd io.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			rd = bytes.NewReader(raw)
		}
		req, _ := http.NewRequest(method, srv.URL+path, rd)
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(raw)
	}
	heartbeat := func(srv *httptest.Server, key string) int {
		code, _ := call(srv, "POST", "/v1/inventory", key, map[string]any{"version": 1, "cluster": "gke", "collected_at": time.Now().UTC().Format(time.RFC3339Nano)})
		return code
	}
	field := func(body, name string) string {
		var m struct{ Data map[string]any }
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatalf("%v: %s", err, body)
		}
		s, _ := m.Data[name].(string)
		return s
	}

	srv, _, _ := start()
	code, body := call(srv, "POST", "/api/v1/projects/us/enrollments", usManageKey, map[string]any{"cluster": "gke"})
	if code != 201 {
		t.Fatalf("%d %s", code, body)
	}
	token, id := field(body, "token"), field(body, "id")

	// nothing but the token is needed, and it makes a manifest the cluster applies
	code, manifest := call(srv, "POST", "/v1/enroll?format=secret", "", map[string]any{"token": token})
	if code != 200 || !strings.Contains(manifest, "kind: Secret") {
		t.Fatalf("%d %s", code, manifest)
	}
	key := strings.Split(strings.Split(manifest, `apiKey: "`)[1], `"`)[0]
	if !strings.HasPrefix(key, "ing_") {
		t.Fatalf("key %q", key)
	}
	if code := heartbeat(srv, key); code != 200 {
		t.Fatalf("the issued key was not accepted by the ingest handler: %d", code)
	}
	if code := heartbeat(srv, "ing_"+strings.Repeat("Z", 43)); code != 401 {
		t.Fatalf("a key that was never issued: %d", code)
	}
	if code, _ := call(srv, "POST", "/v1/enroll", "", map[string]any{"token": token}); code != 401 {
		t.Fatalf("the token redeemed twice: %d", code)
	}

	// neither secret is anywhere in the table
	events, err := r.st.Enrollments().All(ctx)
	if err != nil || len(events) != 2 {
		t.Fatalf("%d events, %v", len(events), err)
	}
	for _, e := range events {
		for _, f := range []string{e.Project, e.ID, e.Actor, e.Cluster, e.TokenHash, e.KeyHash} {
			if strings.Contains(f, strings.TrimPrefix(token, "enr_")) || strings.Contains(f, strings.TrimPrefix(key, "ing_")) {
				t.Fatalf("a secret is stored: %+v", e)
			}
		}
	}
	if events[0].Type != enroll.EvCreated || events[1].Type != enroll.EvRedeemed || len(events[0].TokenHash) != 64 || len(events[1].KeyHash) != 64 {
		t.Fatalf("%+v", events)
	}

	// a restart: a new service over the same database takes the issued key up again, and the token is still spent
	srv2, reg2, _ := start()
	if p, ok := reg2.IngestProject(key); !ok || p != "us" {
		t.Fatalf("after a restart the issued key is not accepted: %q %v", p, ok)
	}
	if code := heartbeat(srv2, key); code != 200 {
		t.Fatalf("after a restart: %d", code)
	}
	if code, _ := call(srv2, "POST", "/v1/enroll", "", map[string]any{"token": token}); code != 401 {
		t.Fatalf("a spent token after a restart: %d", code)
	}
	// and the listing says what happened
	code, list := call(srv2, "GET", "/api/v1/projects/us/enrollments", usRead, nil)
	if code != 200 || !strings.Contains(list, `"status":"redeemed"`) || !strings.Contains(list, `"cluster":"gke"`) || strings.Contains(list, token) {
		t.Fatalf("%d %s", code, list)
	}
	// another project sees none of it
	if code, list := call(srv2, "GET", "/api/v1/projects/eu/enrollments", euRead, nil); code != 200 || strings.Contains(list, id) {
		t.Fatalf("%d %s", code, list)
	}

	// a revoke cuts the key off, here and after another restart
	if code, body := call(srv2, "DELETE", "/api/v1/projects/us/enrollments/"+id, usManageKey, nil); code != 200 || !strings.Contains(body, `"status":"revoked"`) {
		t.Fatalf("%d %s", code, body)
	}
	if code := heartbeat(srv2, key); code != 401 {
		t.Fatalf("a revoked key was accepted: %d", code)
	}
	srv3, _, _ := start()
	if code := heartbeat(srv3, key); code != 401 {
		t.Fatalf("a revoked key was accepted after a restart: %d", code)
	}
	// the key from the projects file never stopped working
	if code := heartbeat(srv3, usIngest); code != 200 {
		t.Fatalf("the file's own key: %d", code)
	}

	// many at once: one token, twenty clusters, one key
	_, body = call(srv3, "POST", "/api/v1/projects/us/enrollments", usManageKey, map[string]any{"cluster": "aks"})
	token2 := field(body, "token")
	var wg sync.WaitGroup
	var won atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, _ := call(srv3, "POST", "/v1/enroll", "", map[string]any{"token": token2}); code == 200 {
				won.Add(1)
			}
		}()
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("%d redeemed one token", won.Load())
	}
	events, _ = r.st.Enrollments().All(ctx)
	var redeemed int
	for _, e := range events {
		if e.Type == enroll.EvRedeemed {
			redeemed++
		}
	}
	if redeemed != 2 {
		t.Fatalf("%d redemptions recorded (two enrollments)", redeemed)
	}
}
