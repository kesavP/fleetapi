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
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/golang/snappy"
	"github.com/google/pprof/profile"
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
		TTL: store.TTLs{Metrics: 48 * time.Hour, Traces: 24 * time.Hour, Logs: 24 * time.Hour, Profiles: 24 * time.Hour},
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
	var versions uint64
	if err := raw.QueryRow(ctx, "SELECT count() FROM schema_migrations").Scan(&versions); err != nil || versions != uint64(1) {
		t.Fatalf("schema_migrations rows: %d %v", versions, err)
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
