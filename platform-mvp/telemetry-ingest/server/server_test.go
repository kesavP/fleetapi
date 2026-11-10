package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang/snappy"
	"github.com/google/pprof/profile"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/model"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/pipeline"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/project"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/store"
	logsv1 "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	tracesv1 "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

const (
	usIngest = "us-ingest-key-0000001"
	usRead   = "us-read-key-00000001"
	euRead   = "eu-read-key-00000001"
)

func registry(t *testing.T) *project.Registry {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/p.json"
	cfg := `{"projects":[
	 {"name":"us","ingestKeys":["` + usIngest + `"],"readKeys":["` + usRead + `"]},
	 {"name":"eu","ingestKeys":["eu-ingest-key-0000001"],"readKeys":["` + euRead + `"]}]}`
	if err := writeFile(path, cfg); err != nil {
		t.Fatal(err)
	}
	r, err := project.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type fakeSink struct {
	mu      sync.Mutex
	err     error
	samples []model.Sample
	spans   []model.Span
	logs    []model.LogRecord
	profile []model.ProfileSample
}

func (f *fakeSink) AddMetrics(s []model.Sample, _ []model.Series, _ []model.MetricMeta) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.samples = append(f.samples, s...)
	return nil
}
func (f *fakeSink) AddSpans(s []model.Span) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.spans = append(f.spans, s...)
	return nil
}
func (f *fakeSink) AddLogs(s []model.LogRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.logs = append(f.logs, s...)
	return nil
}
func (f *fakeSink) AddProfile(s []model.ProfileSample, _ []model.Stack) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.profile = append(f.profile, s...)
	return nil
}

func ingestServer(t *testing.T, sink Sink) (*httptest.Server, *Counters) {
	t.Helper()
	mux := http.NewServeMux()
	c := NewCounters()
	(&Ingest{Projects: registry(t), Sink: sink, MaxBody: 1 << 20, Log: quiet, Count: c}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, c
}

func post(t *testing.T, url string, headers map[string]string, body []byte) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func remoteWriteBody() []byte {
	lbl := func(n, v string) []byte {
		var b []byte
		b = protowire.AppendTag(b, 1, protowire.BytesType)
		b = protowire.AppendString(b, n)
		b = protowire.AppendTag(b, 2, protowire.BytesType)
		return protowire.AppendString(b, v)
	}
	var ts []byte
	ts = protowire.AppendTag(ts, 1, protowire.BytesType)
	ts = protowire.AppendBytes(ts, lbl("__name__", "up"))
	var smp []byte
	smp = protowire.AppendTag(smp, 1, protowire.Fixed64Type)
	smp = protowire.AppendFixed64(smp, 0x3ff0000000000000) // 1.0
	smp = protowire.AppendTag(smp, 2, protowire.VarintType)
	smp = protowire.AppendVarint(smp, 1700000000000)
	ts = protowire.AppendTag(ts, 2, protowire.BytesType)
	ts = protowire.AppendBytes(ts, smp)
	var req []byte
	req = protowire.AppendTag(req, 1, protowire.BytesType)
	req = protowire.AppendBytes(req, ts)
	return snappy.Encode(nil, req)
}

func TestIngestAuth(t *testing.T) {
	sink := &fakeSink{}
	srv, _ := ingestServer(t, sink)
	body := remoteWriteBody()
	for name, h := range map[string]map[string]string{
		"no key":       {"Content-Encoding": "snappy"},
		"unknown key":  {"Content-Encoding": "snappy", "X-API-Key": "nope-nope-nope-nope"},
		"a read key":   {"Content-Encoding": "snappy", "X-API-Key": usRead},
		"empty bearer": {"Content-Encoding": "snappy", "Authorization": "Bearer "},
	} {
		if resp := post(t, srv.URL+"/v1/metrics", h, body); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, resp.StatusCode)
		}
	}
	if len(sink.samples) != 0 {
		t.Fatal("rows stored for an unauthenticated request")
	}
}

func TestMetricsIngestTagsProject(t *testing.T) {
	sink := &fakeSink{}
	srv, counters := ingestServer(t, sink)
	for _, h := range []map[string]string{
		{"Content-Encoding": "snappy", "X-API-Key": usIngest},
		{"Content-Encoding": "snappy", "Authorization": "Bearer " + usIngest},
	} {
		if resp := post(t, srv.URL+"/v1/metrics", h, remoteWriteBody()); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	if len(sink.samples) != 2 || sink.samples[0].Project != "us" || sink.samples[0].Name != "up" || sink.samples[0].Value != 1 {
		t.Fatalf("%+v", sink.samples)
	}
	rec := httptest.NewRecorder()
	MetricsHandler(counters, nil).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `telemetry_ingest_requests_total{signal="metrics",code="204"} 2`) {
		t.Fatalf("counters:\n%s", rec.Body.String())
	}
}

func TestMetricsBadRequests(t *testing.T) {
	srv, _ := ingestServer(t, &fakeSink{})
	key := map[string]string{"X-API-Key": usIngest}
	h := func(extra string) map[string]string {
		m := map[string]string{"X-API-Key": usIngest}
		if extra != "" {
			m["Content-Encoding"] = extra
		}
		return m
	}
	_ = key
	if r := post(t, srv.URL+"/v1/metrics", h(""), remoteWriteBody()); r.StatusCode != 400 {
		t.Errorf("missing snappy encoding: %d", r.StatusCode)
	}
	if r := post(t, srv.URL+"/v1/metrics", h("snappy"), []byte("garbage")); r.StatusCode != 400 {
		t.Errorf("garbage body: %d", r.StatusCode)
	}
	big := snappy.Encode(nil, bytes.Repeat([]byte{0}, 2<<20)) // decodes past MaxBody
	if r := post(t, srv.URL+"/v1/metrics", h("snappy"), big); r.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: %d", r.StatusCode)
	}
}

func TestOverloadIsRetryable(t *testing.T) {
	srv, _ := ingestServer(t, &fakeSink{err: pipeline.ErrOverloaded})
	resp := post(t, srv.URL+"/v1/metrics", map[string]string{"X-API-Key": usIngest, "Content-Encoding": "snappy"}, remoteWriteBody())
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("status %d retry-after %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	srv2, _ := ingestServer(t, &fakeSink{err: errors.New("boom")})
	if r := post(t, srv2.URL+"/v1/metrics", map[string]string{"X-API-Key": usIngest, "Content-Encoding": "snappy"}, remoteWriteBody()); r.StatusCode != 500 {
		t.Fatalf("internal error: %d", r.StatusCode)
	}
}

func TestTracesProtoAndJSON(t *testing.T) {
	sink := &fakeSink{}
	srv, _ := ingestServer(t, sink)
	req := &tracesv1.ExportTraceServiceRequest{ResourceSpans: []*tracev1.ResourceSpans{{
		ScopeSpans: []*tracev1.ScopeSpans{{Spans: []*tracev1.Span{{Name: "op", TraceId: make([]byte, 16), SpanId: make([]byte, 8)}}}}}}}
	b, _ := proto.Marshal(req)
	resp := post(t, srv.URL+"/v1/traces", map[string]string{"X-API-Key": usIngest, "Content-Type": "application/x-protobuf"}, b)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/x-protobuf" {
		t.Fatalf("proto: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	jsonBody := `{"resourceSpans":[{"scopeSpans":[{"spans":[{"name":"op2","unknownField":1}]}]}]}`
	resp = post(t, srv.URL+"/v1/traces", map[string]string{"X-API-Key": usIngest, "Content-Type": "application/json"}, []byte(jsonBody))
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("json: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if len(sink.spans) != 2 || sink.spans[0].Project != "us" || sink.spans[1].Name != "op2" {
		t.Fatalf("%+v", sink.spans)
	}
	if r := post(t, srv.URL+"/v1/traces", map[string]string{"X-API-Key": usIngest, "Content-Type": "text/plain"}, b); r.StatusCode != 415 {
		t.Fatalf("wrong media type: %d", r.StatusCode)
	}
	if r := post(t, srv.URL+"/v1/traces", map[string]string{"X-API-Key": usIngest, "Content-Type": "application/x-protobuf"}, []byte{0xff, 0xff}); r.StatusCode != 400 {
		t.Fatalf("garbage protobuf: %d", r.StatusCode)
	}
}

func TestRefusedOTLPIsNotAcknowledged(t *testing.T) {
	srv, _ := ingestServer(t, &fakeSink{err: pipeline.ErrOverloaded})
	req := &logsv1.ExportLogsServiceRequest{ResourceLogs: []*logv1.ResourceLogs{{ScopeLogs: []*logv1.ScopeLogs{{LogRecords: []*logv1.LogRecord{
		{Body: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "x"}}}}}}}}}
	b, _ := proto.Marshal(req)
	resp := post(t, srv.URL+"/v1/logs", map[string]string{"X-API-Key": usIngest, "Content-Type": "application/x-protobuf"}, b)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 503 || len(body) > 40 {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
}

func TestProfileUploadAnswers200(t *testing.T) {
	sink := &fakeSink{}
	srv, _ := ingestServer(t, sink)
	fn := &profile.Function{ID: 1, Name: "main.hot"}
	loc := &profile.Location{ID: 1, Line: []profile.Line{{Function: fn}}}
	p := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		Function:   []*profile.Function{fn}, Location: []*profile.Location{loc},
		Sample: []*profile.Sample{{Location: []*profile.Location{loc}, Value: []int64{5}}},
	}
	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil {
		t.Fatal(err)
	}
	resp := post(t, srv.URL+"/v1/profiles?service.name=api&container.id=/k8s/ns/pod/c", map[string]string{"X-API-Key": usIngest}, buf.Bytes())
	// The node-agent treats any status but 200 as a failed upload.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want exactly 200", resp.StatusCode)
	}
	if len(sink.profile) != 1 || sink.profile[0].Service != "api" || sink.profile[0].Labels["container.id"] != "/k8s/ns/pod/c" {
		t.Fatalf("%+v", sink.profile)
	}
}

func TestProfilesNeedServiceName(t *testing.T) {
	srv, _ := ingestServer(t, &fakeSink{})
	h := map[string]string{"X-API-Key": usIngest}
	if r := post(t, srv.URL+"/v1/profiles", h, []byte("x")); r.StatusCode != 400 {
		t.Fatalf("no service.name: %d", r.StatusCode)
	}
	if r := post(t, srv.URL+"/v1/profiles?service.name=api", h, []byte("not a profile")); r.StatusCode != 400 {
		t.Fatalf("bad profile: %d", r.StatusCode)
	}
}

// ---- query API ----

type fakeReader struct {
	store.Reader
	gotShare   bool
	gotProject string
	gotMetric  store.MetricQuery
	gotSpan    store.SpanQuery
}

func (f *fakeReader) ListMetrics(_ context.Context, p string) ([]string, error) {
	f.gotProject = p
	return []string{"a", "b"}, nil
}
func (f *fakeReader) QueryMetric(_ context.Context, q store.MetricQuery) (store.MetricResult, error) {
	f.gotMetric = q
	return store.MetricResult{Series: []store.MetricSeries{{Labels: map[string]string{"x": "y"}, Points: []store.Point{{T: 1, V: 2}}}}}, nil
}
func (f *fakeReader) SearchSpans(_ context.Context, q store.SpanQuery) ([]model.Span, error) {
	f.gotSpan = q
	return nil, nil
}
func (f *fakeReader) GetTrace(context.Context, string, string) ([]model.Span, error) {
	return []model.Span{{Name: "x"}}, nil
}
func (f *fakeReader) TopStacks(context.Context, store.ProfileQuery) ([]store.StackValue, error) {
	return []store.StackValue{{Frames: []string{"leaf", "mid", "root"}, Value: 7}}, nil
}

func (f *fakeReader) ClickHouseInfo(_ context.Context, p string, share bool) (store.ClickHouseInfo, error) {
	f.gotProject, f.gotShare = p, share
	return store.ClickHouseInfo{
		Connection: store.ChConnection{Reachable: true, Protocol: "native", Database: "telemetry", Version: "24.8.1", PingMs: 1.5},
		Storage:    []store.ChStorage{{Type: "logs", Rows: 3}},
		Warnings:   []string{},
	}, nil
}

func queryServer(t *testing.T, r *fakeReader) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	(&Query{Projects: registry(t), Reader: r, Log: quiet, DefaultRange: time.Hour, MaxSeries: 500, MaxPoints: 1000}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url, key string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestQueryIsScopedToTheKeysProject(t *testing.T) {
	fr := &fakeReader{}
	srv := queryServer(t, fr)
	base := srv.URL + "/api/v1/projects/"
	if code, _ := get(t, base+"us/metrics", ""); code != 401 {
		t.Errorf("no key: %d", code)
	}
	if code, _ := get(t, base+"us/metrics", usIngest); code != 403 {
		t.Errorf("an ingest key must not read: %d", code)
	}
	if code, _ := get(t, base+"us/metrics", euRead); code != 403 {
		t.Errorf("eu key on us: %d", code)
	}
	if code, _ := get(t, base+"nosuch/metrics", usRead); code != 403 {
		t.Errorf("unknown project must look like forbidden: %d", code)
	}
	if fr.gotProject != "" {
		t.Fatal("the reader was called for a rejected request")
	}
	code, body := get(t, base+"us/metrics", usRead)
	if code != 200 || fr.gotProject != "us" || !strings.Contains(body, `"data":["a","b"]`) {
		t.Fatalf("%d %s (project %q)", code, body, fr.gotProject)
	}
}

func TestQueryRangeParams(t *testing.T) {
	fr := &fakeReader{}
	srv := queryServer(t, fr)
	u := srv.URL + "/api/v1/projects/us/metrics/query_range"
	code, body := get(t, u+"?name=up&match.pod=a&match.ns=b&from=1700000000&to=1700003600&step=60&rate=true", usRead)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	q := fr.gotMetric
	if q.Project != "us" || q.Name != "up" || q.Match["pod"] != "a" || q.Match["ns"] != "b" || q.StepSec != 60 || !q.Rate ||
		q.From.Unix() != 1700000000 || q.To.Unix() != 1700003600 || q.MaxPoints != 1000 {
		t.Fatalf("%+v", q)
	}
	for name, qs := range map[string]string{
		"from_zero needs rate": "?name=up&step=15&from_zero=true",
		"no name":              "",
		"rate needs step":      "?name=up&rate=true",
		"bad step":             "?name=up&step=-1",
		"huge step":            "?name=up&step=999999",
		"bad from":             "?name=up&from=yesterday",
		"from after to":        "?name=up&from=200&to=100",
	} {
		if code, _ := get(t, u+qs, usRead); code != 400 {
			t.Errorf("%s: %d, want 400", name, code)
		}
	}
}

func TestSpanAndTraceParams(t *testing.T) {
	fr := &fakeReader{}
	srv := queryServer(t, fr)
	base := srv.URL + "/api/v1/projects/us/traces"
	if code, _ := get(t, base+"?service=cart&status=error&min_duration_ms=5&root=true&limit=10", usRead); code != 200 {
		t.Fatal(code)
	}
	if q := fr.gotSpan; q.Service != "cart" || q.Status != "ERROR" || q.MinDurationNs != 5_000_000 || !q.RootOnly || q.Limit != 10 {
		t.Fatalf("%+v", q)
	}
	if code, _ := get(t, base+"?status=weird", usRead); code != 400 {
		t.Errorf("bad status: %d", code)
	}
	if code, _ := get(t, base+"?limit=100000", usRead); code != 400 {
		t.Errorf("limit above max: %d", code)
	}
	id := strings.Repeat("ab", 16)
	if code, body := get(t, base+"/"+id, usRead); code != 200 || !strings.Contains(body, `"name":"x"`) {
		t.Errorf("trace: %d %s", code, body)
	}
	if code, _ := get(t, base+"/not-hex", usRead); code != 400 {
		t.Errorf("bad trace id: %d", code)
	}
}

func TestFoldedProfile(t *testing.T) {
	srv := queryServer(t, &fakeReader{})
	code, body := get(t, srv.URL+"/api/v1/projects/us/profiles/top?service=api&type=cpu:nanoseconds&format=folded", usRead)
	if code != 200 || body != "root;mid;leaf 7\n" {
		t.Fatalf("%d %q", code, body)
	}
	var out struct{ Data []store.StackValue }
	code, body = get(t, srv.URL+"/api/v1/projects/us/profiles/top?service=api&type=cpu:nanoseconds", usRead)
	if err := json.Unmarshal([]byte(body), &out); code != 200 || err != nil || len(out.Data) != 1 || out.Data[0].Frames[0] != "leaf" {
		t.Fatalf("%d %s %v", code, body, err)
	}
	if code, _ := get(t, srv.URL+"/api/v1/projects/us/profiles/top?service=api", usRead); code != 400 {
		t.Errorf("missing type: %d", code)
	}
}

func TestTracesJSONWithHexIDs(t *testing.T) {
	sink := &fakeSink{}
	srv, _ := ingestServer(t, sink)
	body := `{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"0101010101010101","name":"op"}]}]}]}`
	resp := post(t, srv.URL+"/v1/traces", map[string]string{"X-API-Key": usIngest, "Content-Type": "application/json"}, []byte(body))
	if resp.StatusCode != 200 || len(sink.spans) != 1 || sink.spans[0].TraceID != "0102030405060708090a0b0c0d0e0f10" || sink.spans[0].SpanID != "0101010101010101" {
		t.Fatalf("status %d, spans %+v", resp.StatusCode, sink.spans)
	}
}

func TestEmptyResultsAreListsNotNull(t *testing.T) {
	srv := queryServer(t, &fakeReader{}) // its SearchSpans returns a nil slice
	code, body := get(t, srv.URL+"/api/v1/projects/us/traces?service=none", usRead)
	if code != 200 || strings.TrimSpace(body) != `{"data":[]}` {
		t.Fatalf("%d %q: an empty result must be [], not null", code, body)
	}
}

func TestFromZeroIsPassedToTheReader(t *testing.T) {
	fr := &fakeReader{}
	srv := queryServer(t, fr)
	u := srv.URL + "/api/v1/projects/us/metrics/query_range?name=up&step=15&rate=true"
	if code, body := get(t, u+"&from_zero=true", usRead); code != 200 || !fr.gotMetric.FromZero || !fr.gotMetric.Rate {
		t.Fatalf("%d %s %+v", code, body, fr.gotMetric)
	}
	if code, _ := get(t, u, usRead); code != 200 || fr.gotMetric.FromZero {
		t.Fatalf("from_zero must be off unless asked for: %+v", fr.gotMetric)
	}
}

func TestClickHouseInfoIsScopedToTheKeysProject(t *testing.T) {
	fr := &fakeReader{}
	srv := queryServer(t, fr)
	base := srv.URL + "/api/v1/projects/"
	for name, c := range map[string]struct {
		path, key string
		want      int
	}{
		"no key":          {"us/clickhouse", "", 401},
		"ingest key":      {"us/clickhouse", usIngest, 403},
		"another project": {"us/clickhouse", euRead, 403},
		"unknown project": {"nosuch/clickhouse", usRead, 403},
	} {
		if code, _ := get(t, base+c.path, c.key); code != c.want {
			t.Errorf("%s: %d", name, code)
		}
	}
	if fr.gotProject != "" {
		t.Fatal("the reader was called for a rejected request")
	}
	code, body := get(t, base+"us/clickhouse", usRead)
	if code != 200 || fr.gotProject != "us" || !strings.Contains(body, `"version":"24.8.1"`) || !strings.Contains(body, `"data":{`) {
		t.Fatalf("%d %s (project %q)", code, body, fr.gotProject)
	}
}

func TestClickHouseInfoSharesClusterDetailsOnlyWhenTheOperatorAsked(t *testing.T) {
	for _, share := range []bool{false, true} {
		fr := &fakeReader{}
		mux := http.NewServeMux()
		(&Query{Projects: registry(t), Reader: fr, Log: quiet, DefaultRange: time.Hour, MaxSeries: 500, MaxPoints: 1000, ShareClusterInfo: share}).Register(mux)
		srv := httptest.NewServer(mux)
		if code, body := get(t, srv.URL+"/api/v1/projects/us/clickhouse", usRead); code != 200 {
			t.Fatalf("%d %s", code, body)
		}
		if fr.gotShare != share {
			t.Errorf("share %v reached the reader as %v", share, fr.gotShare)
		}
		srv.Close()
	}
}

func TestClickHouseInfoIsReadOnly(t *testing.T) {
	srv := queryServer(t, &fakeReader{})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/projects/us/clickhouse", nil)
	req.Header.Set("Authorization", "Bearer "+usRead)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("a project key must not be able to change the database settings: %d", resp.StatusCode)
	}
}
