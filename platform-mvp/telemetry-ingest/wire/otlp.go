package wire

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/model"
	logsv1 "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	tracesv1 "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
)

const unknownService = "unknown"

func Attrs(kv []*commonv1.KeyValue) map[string]string {
	if len(kv) == 0 {
		return nil
	}
	m := make(map[string]string, len(kv))
	for _, a := range kv {
		m[a.GetKey()] = Value(a.GetValue())
	}
	return m
}

// Value renders an OTLP AnyValue as text. Arrays and maps become JSON-ish
// text so nothing is lost on the way into a Map(String, String) column.
func Value(v *commonv1.AnyValue) string {
	switch x := v.GetValue().(type) {
	case nil:
		return ""
	case *commonv1.AnyValue_StringValue:
		return x.StringValue
	case *commonv1.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	case *commonv1.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonv1.AnyValue_DoubleValue:
		switch {
		case math.IsNaN(x.DoubleValue):
			return "NaN"
		case math.IsInf(x.DoubleValue, 1):
			return "+Inf"
		case math.IsInf(x.DoubleValue, -1):
			return "-Inf"
		}
		return strconv.FormatFloat(x.DoubleValue, 'f', -1, 64)
	case *commonv1.AnyValue_BytesValue:
		return hex.EncodeToString(x.BytesValue)
	case *commonv1.AnyValue_ArrayValue:
		parts := make([]string, 0, len(x.ArrayValue.GetValues()))
		for _, e := range x.ArrayValue.GetValues() {
			parts = append(parts, strconv.Quote(Value(e)))
		}
		return "[" + strings.Join(parts, ",") + "]"
	case *commonv1.AnyValue_KvlistValue:
		parts := make([]string, 0, len(x.KvlistValue.GetValues()))
		for _, e := range x.KvlistValue.GetValues() {
			parts = append(parts, strconv.Quote(e.GetKey())+":"+strconv.Quote(Value(e.GetValue())))
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	return fmt.Sprintf("unsupported attribute type %T", v.GetValue())
}

func serviceName(r *resourcev1.Resource) (string, map[string]string) {
	attrs := Attrs(r.GetAttributes())
	if s := attrs["service.name"]; s != "" {
		return s, attrs
	}
	return unknownService, attrs
}

func nanos(n uint64) time.Time { return time.Unix(0, int64(n)).UTC() }

func Spans(project string, req *tracesv1.ExportTraceServiceRequest) []model.Span {
	var out []model.Span
	for _, rs := range req.GetResourceSpans() {
		service, resAttrs := serviceName(rs.GetResource())
		for _, ss := range rs.GetScopeSpans() {
			for _, s := range ss.GetSpans() {
				sp := model.Span{
					Project:       project,
					Time:          nanos(s.GetStartTimeUnixNano()),
					TraceID:       hex.EncodeToString(s.GetTraceId()),
					SpanID:        hex.EncodeToString(s.GetSpanId()),
					ParentSpanID:  hex.EncodeToString(s.GetParentSpanId()),
					TraceState:    s.GetTraceState(),
					Name:          s.GetName(),
					Kind:          strings.TrimPrefix(s.GetKind().String(), "SPAN_KIND_"),
					Service:       service,
					StatusCode:    strings.TrimPrefix(s.GetStatus().GetCode().String(), "STATUS_CODE_"),
					StatusMessage: s.GetStatus().GetMessage(),
					ResourceAttrs: resAttrs,
					SpanAttrs:     Attrs(s.GetAttributes()),
				}
				if end := s.GetEndTimeUnixNano(); end > s.GetStartTimeUnixNano() {
					sp.DurationNs = end - s.GetStartTimeUnixNano()
				}
				for _, e := range s.GetEvents() {
					sp.EventTimes = append(sp.EventTimes, nanos(e.GetTimeUnixNano()))
					sp.EventNames = append(sp.EventNames, e.GetName())
					sp.EventAttrs = append(sp.EventAttrs, orEmpty(Attrs(e.GetAttributes())))
				}
				for _, l := range s.GetLinks() {
					sp.LinkTraceIDs = append(sp.LinkTraceIDs, hex.EncodeToString(l.GetTraceId()))
					sp.LinkSpanIDs = append(sp.LinkSpanIDs, hex.EncodeToString(l.GetSpanId()))
					sp.LinkAttrs = append(sp.LinkAttrs, orEmpty(Attrs(l.GetAttributes())))
				}
				out = append(out, sp)
			}
		}
	}
	return out
}

func Logs(project string, req *logsv1.ExportLogsServiceRequest) []model.LogRecord {
	var out []model.LogRecord
	now := time.Now().UTC()
	for _, rl := range req.GetResourceLogs() {
		service, resAttrs := serviceName(rl.GetResource())
		for _, sl := range rl.GetScopeLogs() {
			for _, l := range sl.GetLogRecords() {
				ts := l.GetTimeUnixNano()
				if ts == 0 {
					ts = l.GetObservedTimeUnixNano()
				}
				t := now
				if ts != 0 {
					t = nanos(ts)
				}
				sev := l.GetSeverityText()
				if sev == "" {
					sev = strings.TrimPrefix(l.GetSeverityNumber().String(), "SEVERITY_NUMBER_")
				}
				out = append(out, model.LogRecord{
					Project:        project,
					Time:           t,
					TraceID:        hex.EncodeToString(l.GetTraceId()),
					SpanID:         hex.EncodeToString(l.GetSpanId()),
					SeverityText:   strings.ToUpper(sev),
					SeverityNumber: int32(l.GetSeverityNumber()),
					Service:        service,
					Body:           Value(l.GetBody()),
					ResourceAttrs:  resAttrs,
					LogAttrs:       Attrs(l.GetAttributes()),
				})
			}
		}
	}
	return out
}

// ClickHouse Map columns reject nil maps inside arrays.
func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// NormalizeOTLPJSON rewrites the hex trace and span ids that OTLP/JSON uses
// into the base64 that protojson expects for bytes fields. Without it a hex
// id would be read as base64 and silently decode to the wrong bytes: 32 hex
// characters are also valid base64. The lengths tell the two apart (a
// trace id is 32 hex or 24 base64 characters, a span id 16 or 12), so ids
// that are already base64 pass through untouched.
func NormalizeOTLPJSON(data []byte) ([]byte, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	fixIDs(v)
	return json.Marshal(v)
}

func fixIDs(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			s, isString := val.(string)
			if !isString {
				fixIDs(val)
				continue
			}
			if (k == "traceId" && len(s) == 32) || ((k == "spanId" || k == "parentSpanId") && len(s) == 16) {
				if b, err := hex.DecodeString(s); err == nil {
					x[k] = base64.StdEncoding.EncodeToString(b)
				}
			}
		}
	case []any:
		for _, e := range x {
			fixIDs(e)
		}
	}
}
