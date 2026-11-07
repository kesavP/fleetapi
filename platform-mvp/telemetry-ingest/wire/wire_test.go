package wire

import (
	"bytes"
	"compress/gzip"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/golang/snappy"
	"github.com/google/pprof/profile"
	logsv1 "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	tracesv1 "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
)

func appendBytesField(b []byte, num protowire.Number, v []byte) []byte {
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendBytes(b, v)
}

func encLabel(name, value string) []byte {
	var b []byte
	b = appendBytesField(b, 1, []byte(name))
	return appendBytesField(b, 2, []byte(value))
}

func encSample(v float64, ts int64) []byte {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.Fixed64Type)
	b = protowire.AppendFixed64(b, math.Float64bits(v))
	b = protowire.AppendTag(b, 2, protowire.VarintType)
	return protowire.AppendVarint(b, uint64(ts))
}

func encSeries(labels [][2]string, samples ...[]byte) []byte {
	var b []byte
	for _, l := range labels {
		b = appendBytesField(b, 1, encLabel(l[0], l[1]))
	}
	for _, s := range samples {
		b = appendBytesField(b, 2, s)
	}
	return b
}

func encMeta(typ uint64, family, help, unit string) []byte {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.VarintType)
	b = protowire.AppendVarint(b, typ)
	b = appendBytesField(b, 2, []byte(family))
	b = appendBytesField(b, 4, []byte(help))
	return appendBytesField(b, 5, []byte(unit))
}

func sampleRequest() []byte {
	var b []byte
	b = appendBytesField(b, 1, encSeries(
		[][2]string{{"__name__", "container_cpu_usage"}, {"container_id", "/k8s/ns/pod/c"}, {"a", "1"}},
		encSample(1.5, 1700000000123), encSample(2.5, 1700000015123)))
	b = appendBytesField(b, 1, encSeries([][2]string{{"no_name", "x"}}, encSample(1, 1)))   // skipped
	b = appendBytesField(b, 1, encSeries([][2]string{{"__name__", "empty"}}))               // no samples: skipped
	b = appendBytesField(b, 3, encMeta(2, "container_cpu_usage", "cpu seconds", "seconds")) // gauge
	return b
}

func TestRemoteWriteRoundTrip(t *testing.T) {
	rw, err := DecodeRemoteWrite(snappy.Encode(nil, sampleRequest()), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	samples, series, meta := MetricRows("us", rw)
	if len(samples) != 2 || len(series) != 1 || len(meta) != 1 {
		t.Fatalf("rows: %d samples, %d series, %d meta", len(samples), len(series), len(meta))
	}
	if samples[0].Name != "container_cpu_usage" || samples[0].Value != 1.5 || samples[0].Project != "us" {
		t.Fatalf("sample: %+v", samples[0])
	}
	if got := samples[0].Time.UnixMilli(); got != 1700000000123 {
		t.Fatalf("millisecond precision lost: %d", got)
	}
	if samples[0].Hash != samples[1].Hash || samples[0].Hash != series[0].Hash {
		t.Fatal("hash differs within one series")
	}
	if _, ok := series[0].Labels["__name__"]; ok || series[0].Labels["a"] != "1" || len(series[0].Labels) != 2 {
		t.Fatalf("labels: %v", series[0].Labels)
	}
	if meta[0].Type != "GAUGE" || meta[0].Unit != "seconds" {
		t.Fatalf("meta: %+v", meta[0])
	}
}

func TestHashIsOrderIndependentAndSeparatesFields(t *testing.T) {
	mk := func(l ...Label) uint64 { sortLabels(l); return HashLabels(l) }
	a := mk(Label{"a", "1"}, Label{"b", "2"})
	b := mk(Label{"b", "2"}, Label{"a", "1"})
	if a != b {
		t.Fatal("hash depends on label order")
	}
	if mk(Label{"ab", "c"}) == mk(Label{"a", "bc"}) {
		t.Fatal("hash does not separate name from value")
	}
}

func TestRemoteWriteRejects(t *testing.T) {
	if _, err := DecodeRemoteWrite([]byte("not snappy"), 1<<20); err == nil {
		t.Fatal("garbage accepted")
	}
	if _, err := DecodeRemoteWrite(snappy.Encode(nil, sampleRequest()), 8); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge, got %v", err)
	}
	if _, err := ParseRemoteWrite([]byte{0x0a, 0x7f, 0x01}); err == nil { // length runs past the end
		t.Fatal("truncated message accepted")
	}
	var many []byte
	var labels [][2]string
	for i := 0; i < maxLabelsPerSeries+1; i++ {
		labels = append(labels, [2]string{string(rune('a'+i%26)) + string(rune('a'+i/26)), "v"})
	}
	many = appendBytesField(many, 1, encSeries(labels, encSample(1, 1)))
	if _, err := ParseRemoteWrite(many); err == nil {
		t.Fatal("series over the label limit accepted")
	}
}

func TestDecompress(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(bytes.Repeat([]byte("a"), 4096))
	_ = zw.Close()
	out, err := Decompress("gzip", buf.Bytes(), 1<<20)
	if err != nil || len(out) != 4096 {
		t.Fatalf("gzip: %d %v", len(out), err)
	}
	if _, err := Decompress("gzip", buf.Bytes(), 1024); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("decompression bomb not stopped: %v", err)
	}
	if _, err := Decompress("br", []byte("x"), 10); err == nil {
		t.Fatal("unknown encoding accepted")
	}
	if _, err := Decompress("", make([]byte, 20), 10); !errors.Is(err, ErrTooLarge) {
		t.Fatal("oversized identity body accepted")
	}
}

func kv(k, v string) *commonv1.KeyValue {
	return &commonv1.KeyValue{Key: k, Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: v}}}
}

func TestSpans(t *testing.T) {
	start := uint64(time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC).UnixNano())
	req := &tracesv1.ExportTraceServiceRequest{ResourceSpans: []*tracev1.ResourceSpans{{
		Resource: &resourcev1.Resource{Attributes: []*commonv1.KeyValue{kv("service.name", "cart")}},
		ScopeSpans: []*tracev1.ScopeSpans{{Spans: []*tracev1.Span{{
			TraceId: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}, SpanId: []byte{1, 1, 1, 1, 1, 1, 1, 1},
			Name: "GET /cart", Kind: tracev1.Span_SPAN_KIND_SERVER,
			StartTimeUnixNano: start, EndTimeUnixNano: start + 5_000_000,
			Status:     &tracev1.Status{Code: tracev1.Status_STATUS_CODE_ERROR, Message: "boom"},
			Attributes: []*commonv1.KeyValue{kv("http.method", "GET")},
			Events:     []*tracev1.Span_Event{{TimeUnixNano: start + 1, Name: "retry"}},
		}}}},
	}}}
	got := Spans("eu", req)
	if len(got) != 1 {
		t.Fatalf("spans: %d", len(got))
	}
	s := got[0]
	if s.Service != "cart" || s.Kind != "SERVER" || s.StatusCode != "ERROR" || s.DurationNs != 5_000_000 || s.Project != "eu" {
		t.Fatalf("span: %+v", s)
	}
	if s.TraceID != "0102030405060708090a0b0c0d0e0f10" || s.ParentSpanID != "" {
		t.Fatalf("ids: %q %q", s.TraceID, s.ParentSpanID)
	}
	if len(s.EventNames) != 1 || s.EventAttrs[0] == nil {
		t.Fatalf("events: %+v", s)
	}
	if !s.Time.Equal(time.Unix(0, int64(start))) {
		t.Fatal("start time")
	}
}

func TestSpanWithoutServiceName(t *testing.T) {
	req := &tracesv1.ExportTraceServiceRequest{ResourceSpans: []*tracev1.ResourceSpans{{
		ScopeSpans: []*tracev1.ScopeSpans{{Spans: []*tracev1.Span{{Name: "x"}}}}}}}
	if got := Spans("p", req); len(got) != 1 || got[0].Service != unknownService {
		t.Fatalf("%+v", got)
	}
}

func TestLogs(t *testing.T) {
	ts := uint64(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).UnixNano())
	req := &logsv1.ExportLogsServiceRequest{ResourceLogs: []*logv1.ResourceLogs{{
		Resource: &resourcev1.Resource{Attributes: []*commonv1.KeyValue{kv("service.name", "api")}},
		ScopeLogs: []*logv1.ScopeLogs{{LogRecords: []*logv1.LogRecord{
			{TimeUnixNano: ts, SeverityNumber: logv1.SeverityNumber_SEVERITY_NUMBER_ERROR,
				Body: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: "oops"}}},
			{ObservedTimeUnixNano: ts, SeverityText: "warn",
				Body: &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: 7}}},
		}}},
	}}}
	got := Logs("us", req)
	if len(got) != 2 {
		t.Fatalf("logs: %d", len(got))
	}
	if got[0].SeverityText != "ERROR" || got[0].Body != "oops" || got[0].Service != "api" {
		t.Fatalf("log 0: %+v", got[0])
	}
	if got[1].SeverityText != "WARN" || got[1].Body != "7" || !got[1].Time.Equal(time.Unix(0, int64(ts))) {
		t.Fatalf("log 1: %+v", got[1])
	}
}

func TestValueKinds(t *testing.T) {
	arr := &commonv1.AnyValue{Value: &commonv1.AnyValue_ArrayValue{ArrayValue: &commonv1.ArrayValue{Values: []*commonv1.AnyValue{
		{Value: &commonv1.AnyValue_StringValue{StringValue: "a"}}, {Value: &commonv1.AnyValue_BoolValue{BoolValue: true}}}}}}
	if got := Value(arr); got != `["a","true"]` {
		t.Fatalf("array: %s", got)
	}
	if got := Value(&commonv1.AnyValue{Value: &commonv1.AnyValue_DoubleValue{DoubleValue: math.Inf(1)}}); got != "+Inf" {
		t.Fatalf("inf: %s", got)
	}
	if got := Value(nil); got != "" {
		t.Fatalf("nil: %q", got)
	}
}

func TestProfileRows(t *testing.T) {
	fnA := &profile.Function{ID: 1, Name: "main.work"}
	fnB := &profile.Function{ID: 2, Name: "main.main"}
	locA := &profile.Location{ID: 1, Line: []profile.Line{{Function: fnA}}}
	locB := &profile.Location{ID: 2, Line: []profile.Line{{Function: fnB}}}
	p := &profile.Profile{
		SampleType:    []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}, {Type: "samples", Unit: "count"}},
		Function:      []*profile.Function{fnA, fnB},
		Location:      []*profile.Location{locA, locB},
		Sample:        []*profile.Sample{{Location: []*profile.Location{locA, locB}, Value: []int64{10, 1}}, {Location: []*profile.Location{locA, locB}, Value: []int64{5, 0}}, {Value: []int64{9, 9}}},
		TimeNanos:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano(),
		DurationNanos: int64(10 * time.Second),
	}
	rows, stacks := ProfileRows("us", "api", map[string]string{"pod": "x"}, p)
	if len(stacks) != 1 || stacks[0].Frames[0] != "main.work" || stacks[0].Frames[1] != "main.main" {
		t.Fatalf("stacks (leaf first expected): %+v", stacks)
	}
	if len(rows) != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	byType := map[string]int64{}
	for _, r := range rows {
		byType[r.Type] = r.Value
		if r.StackHash != stacks[0].Hash || r.Service != "api" || r.End.Sub(r.Start) != 10*time.Second {
			t.Fatalf("row: %+v", r)
		}
	}
	if byType["cpu:nanoseconds"] != 15 || byType["samples:count"] != 1 {
		t.Fatalf("sums: %v", byType)
	}
}

func TestNormalizeOTLPJSONHexIDs(t *testing.T) {
	in := `{"resourceSpans":[{"scopeSpans":[{"spans":[{
	  "traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"0101010101010101","parentSpanId":"",
	  "name":"x","links":[{"traceId":"ffffffffffffffffffffffffffffffff","spanId":"0202020202020202"}],
	  "attributes":[{"key":"traceId","value":{"stringValue":"0102030405060708090a0b0c0d0e0f10"}}]}]}]}]}`
	out, err := NormalizeOTLPJSON([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	req := &tracesv1.ExportTraceServiceRequest{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(out, req); err != nil {
		t.Fatal(err)
	}
	spans := Spans("p", req)
	if len(spans) != 1 || spans[0].TraceID != "0102030405060708090a0b0c0d0e0f10" || spans[0].SpanID != "0101010101010101" || spans[0].ParentSpanID != "" {
		t.Fatalf("hex ids were not preserved: %+v", spans)
	}
	if spans[0].LinkTraceIDs[0] != "ffffffffffffffffffffffffffffffff" || spans[0].LinkSpanIDs[0] != "0202020202020202" {
		t.Fatalf("link ids: %+v", spans[0])
	}
	if spans[0].SpanAttrs["traceId"] != "0102030405060708090a0b0c0d0e0f10" {
		t.Fatalf("an attribute that happens to be named traceId was rewritten: %v", spans[0].SpanAttrs)
	}
}

func TestNormalizeOTLPJSONLeavesBase64Alone(t *testing.T) {
	b64 := "AQIDBAUGBwgJCgsMDQ4PEA==" // the same trace id as base64, 24 characters
	out, err := NormalizeOTLPJSON([]byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"` + b64 + `"}]}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req := &tracesv1.ExportTraceServiceRequest{}
	if err := (protojson.UnmarshalOptions{}).Unmarshal(out, req); err != nil {
		t.Fatal(err)
	}
	if got := Spans("p", req)[0].TraceID; got != "0102030405060708090a0b0c0d0e0f10" {
		t.Fatalf("base64 id changed: %s", got)
	}
	if _, err := NormalizeOTLPJSON([]byte("{")); err == nil {
		t.Fatal("invalid json accepted")
	}
}
