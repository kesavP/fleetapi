package server

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/model"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/store"
)

// The read API's description (telemetry-client/api/swagger.json) and the client generated from it must match what this server does. These
// tests send every operation the spec describes to the real handlers, and keep each answer as the client's test fixture
// (telemetry-client/testdata), which the client's own tests decode: a field renamed on either side fails one of them.
//
// go test ./server -run APISpec -update-fixtures rewrites the fixtures after a deliberate change.

var updateFixtures = flag.Bool("update-fixtures", false, "rewrite telemetry-client/testdata from the handlers' answers")

const clientDir = "../../telemetry-client"

var specT0 = time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

// fixtureReader answers every read with fixed, filled-in rows, so the fixtures show every field.
type fixtureReader struct{ store.Reader }

func (fixtureReader) SearchLogs(context.Context, store.LogQuery) ([]model.LogRecord, error) {
	return []model.LogRecord{{
		Time: specT0, TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7", SeverityText: "ERROR", SeverityNumber: 17,
		Service: "/k8s/shop/checkout", Body: "payment failed: upstream timeout",
		ResourceAttrs: map[string]string{"container.id": "/k8s/shop/checkout-7d9f8-x2bq4/app"}, LogAttrs: map[string]string{"log.source": "stdout"},
	}}, nil
}
func (fixtureReader) LogServices(context.Context, string, time.Time, time.Time) ([]string, error) {
	return []string{"/k8s/shop/cart", "/k8s/shop/checkout"}, nil
}
func (fixtureReader) TraceServices(context.Context, string, time.Time, time.Time) ([]string, error) {
	return []string{"/k8s/shop/checkout"}, nil
}
func fixtureSpans() []model.Span {
	return []model.Span{
		{Time: specT0, TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7", Name: "POST /checkout", Kind: "SPAN_KIND_SERVER",
			Service: "/k8s/shop/checkout", DurationNs: 1_250_000_000, StatusCode: "ERROR", StatusMessage: "upstream timeout",
			ResourceAttrs: map[string]string{"container.id": "/k8s/shop/checkout-7d9f8-x2bq4/app"}, SpanAttrs: map[string]string{"http.status_code": "504"},
			EventTimes: []time.Time{specT0.Add(time.Second)}, EventNames: []string{"exception"}, EventAttrs: []map[string]string{{"exception.type": "Timeout"}}},
		{Time: specT0.Add(10 * time.Millisecond), TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "b7ad6b7169203331", ParentSpanID: "00f067aa0ba902b7",
			Name: "POST /pay", Kind: "SPAN_KIND_CLIENT", Service: "/k8s/shop/checkout", DurationNs: 1_200_000_000, StatusCode: "ERROR"},
	}
}
func (fixtureReader) SearchSpans(context.Context, store.SpanQuery) ([]model.Span, error) {
	return fixtureSpans(), nil
}
func (fixtureReader) GetTrace(context.Context, string, string) ([]model.Span, error) {
	return fixtureSpans(), nil
}
func (fixtureReader) ListMetrics(context.Context, string) ([]string, error) {
	return []string{"container_net_tcp_successful_connects_total", "container_resources_cpu_usage_seconds_total"}, nil
}
func (fixtureReader) ListSeries(context.Context, string, string, map[string]string, int) ([]store.SeriesInfo, error) {
	return []store.SeriesInfo{{Labels: map[string]string{"container_id": "/k8s/shop/checkout-7d9f8-x2bq4/app"}}}, nil
}
func (fixtureReader) QueryMetric(context.Context, store.MetricQuery) (store.MetricResult, error) {
	return store.MetricResult{Series: []store.MetricSeries{{Labels: map[string]string{"container_id": "/k8s/shop/checkout-7d9f8-x2bq4/app"},
		Points: []store.Point{{T: specT0.UnixMilli(), V: 0.25}, {T: specT0.Add(time.Minute).UnixMilli(), V: 0.5}}}}}, nil
}

type specOp struct {
	path, method, id string
	required         []string // required query parameters
}

func loadSpec(t *testing.T) []specOp {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(clientDir, "api", "swagger.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		BasePath string `json:"basePath"`
		Paths    map[string]map[string]struct {
			OperationID string `json:"operationId"`
			Parameters  []struct {
				Name     string `json:"name"`
				In       string `json:"in"`
				Required bool   `json:"required"`
			} `json:"parameters"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	var ops []specOp
	for p, methods := range spec.Paths {
		for m, o := range methods {
			op := specOp{path: spec.BasePath + p, method: strings.ToUpper(m), id: o.OperationID}
			for _, prm := range o.Parameters {
				if prm.In == "query" && prm.Required {
					op.required = append(op.required, prm.Name)
				}
			}
			ops = append(ops, op)
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].id < ops[j].id })
	return ops
}

var pathParam = regexp.MustCompile(`\{([a-z_]+)\}`)

func TestAPISpecOperationsAnswerAndMatchTheClientFixtures(t *testing.T) {
	ops := loadSpec(t)
	if len(ops) == 0 {
		t.Fatal("the spec has no operations")
	}
	base := fixtureServer(t)
	for _, op := range ops {
		url := pathParam.ReplaceAllStringFunc(op.path, func(m string) string {
			switch m {
			case "{project}":
				return "us"
			case "{trace_id}":
				return "4bf92f3577b34da6a3ce929d0e0e4736"
			}
			t.Fatalf("%s: no sample value for %s", op.id, m)
			return ""
		})
		q := []string{}
		for _, r := range op.required {
			q = append(q, r+"=up")
		}
		if len(q) > 0 {
			url += "?" + strings.Join(q, "&")
		}
		if op.method != "GET" {
			t.Fatalf("%s: only GET operations are described, not %s", op.id, op.method)
		}
		code, body := get(t, base+url, usRead)
		if code != 200 {
			t.Errorf("%s (%s): %d %s: the spec describes a route the server does not answer", op.id, url, code, body)
			continue
		}
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, []byte(body), "", "  "); err != nil {
			t.Fatal(err)
		}
		pretty.WriteByte('\n')
		file := filepath.Join(clientDir, "testdata", op.id+".json")
		if *updateFixtures {
			if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, pretty.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(file)
		if err != nil {
			t.Errorf("%s: no client fixture (%v); run go test ./server -run APISpec -update-fixtures", op.id, err)
			continue
		}
		if !bytes.Equal(want, pretty.Bytes()) {
			t.Errorf("%s: the answer differs from the client fixture %s; if the change is deliberate, update the spec and run -update-fixtures", op.id, file)
		}
	}
}

// fixtureServer is the real query handlers over fixtureReader.
func fixtureServer(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	(&Query{Projects: registry(t), Reader: fixtureReader{}, Log: quiet, DefaultRange: time.Hour, MaxSeries: 500, MaxPoints: 1000}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// The other way round: every logs, traces and metrics route this server registers is described in the spec.
func TestAPISpecDescribesEveryReadRoute(t *testing.T) {
	described := map[string]bool{}
	for _, op := range loadSpec(t) {
		described[op.method+" "+op.path] = true
	}
	src, err := os.ReadFile("query.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`mux\.HandleFunc\(base\+"((?:logs|traces|metrics)[^"]*)"`)
	found := re.FindAllStringSubmatch(string(src), -1)
	if len(found) < 8 {
		t.Fatalf("found %d logs/traces/metrics routes in query.go; the pattern no longer matches how they are registered", len(found))
	}
	for _, m := range found {
		route := "GET /api/v1/projects/{project}/" + m[1]
		if !described[route] {
			t.Errorf("%s is served but not in telemetry-client/api/swagger.json", route)
		}
	}
}
