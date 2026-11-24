package fleet_test

import (
	"errors"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-client/client/logs"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-client/client/metrics"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-client/client/traces"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-client/fleet"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-client/internal/fakehub"
)

func connect(t *testing.T, key string) (*fleet.Client, *fakehub.Hub) {
	t.Helper()
	hub := fakehub.New(t, "..")
	c, err := fleet.New(hub.URL, "us", key)
	if err != nil {
		t.Fatal(err)
	}
	return c, hub
}

// The generated client decodes what the real handlers answer (the fixtures), field by field.
func TestRoundTrip(t *testing.T) {
	c, hub := connect(t, fakehub.Key)
	from, to := fleet.Since(time.Hour)

	lr, err := c.Logs.SearchLogs(logs.NewSearchLogsParams().WithProject(c.Project).WithService(fleet.String("/k8s/shop/checkout")).
		WithSeverity(fleet.String("error")).WithFrom(from).WithTo(to).WithLimit(fleet.Int64(10)), nil)
	if err != nil {
		t.Fatal(err)
	}
	l := lr.Payload.Data[0]
	if l.Service != "/k8s/shop/checkout" || l.Severity != "ERROR" || l.SeverityNumber != 17 || l.Body != "payment failed: upstream timeout" ||
		l.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || l.LogAttributes["log.source"] != "stdout" ||
		!time.Time(l.Time).Equal(time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("log %+v", l)
	}
	q := hub.Last().Query()
	if q.Get("service") != "/k8s/shop/checkout" || q.Get("severity") != "error" || q.Get("limit") != "10" || q.Get("from") == "" || q.Get("to") == "" {
		t.Fatalf("asked %v", q)
	}
	if hub.Keys[len(hub.Keys)-1] != fakehub.Key {
		t.Fatal("the key is sent in X-API-Key")
	}

	sr, err := c.Traces.SearchSpans(traces.NewSearchSpansParams().WithProject(c.Project).WithStatus(fleet.String("error")), nil)
	if err != nil {
		t.Fatal(err)
	}
	s := sr.Payload.Data[0]
	if s.Name != "POST /checkout" || s.DurationNs != 1_250_000_000 || s.StatusCode != "ERROR" || s.SpanAttributes["http.status_code"] != "504" ||
		len(s.EventNames) != 1 || s.EventAttributes[0]["exception.type"] != "Timeout" {
		t.Fatalf("span %+v", s)
	}
	tr, err := c.Traces.GetTrace(traces.NewGetTraceParams().WithProject(c.Project).WithTraceID("4bf92f3577b34da6a3ce929d0e0e4736"), nil)
	if err != nil || len(tr.Payload.Data) != 2 || tr.Payload.Data[1].ParentSpanID != "00f067aa0ba902b7" {
		t.Fatalf("trace %v %v", tr, err)
	}

	mr, err := c.Metrics.QueryRange(metrics.NewQueryRangeParams().WithProject(c.Project).WithName("up").
		WithMatch([]string{fleet.Match("container_id", "/k8s/shop/checkout-7d9f8-x2bq4/app"), fleet.Match("job", "x")}).WithStep(fleet.Int64(60)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if ms := mr.Payload.Data.Series; len(ms) != 1 || len(ms[0].Points) != 2 || ms[0].Points[1].V != 0.5 || ms[0].Labels["container_id"] == "" {
		t.Fatalf("metric %+v", mr.Payload.Data)
	}
	if m := hub.Last().Query()["match"]; len(m) != 2 || m[0] != "container_id=/k8s/shop/checkout-7d9f8-x2bq4/app" || m[1] != "job=x" {
		t.Fatalf("matchers are sent as repeated match=label=value: %v", m)
	}

	for name, call := range map[string]func() (int, error){
		"log services": func() (int, error) {
			r, e := c.Logs.ListLogServices(logs.NewListLogServicesParams().WithProject(c.Project), nil)
			return lenOr(r, e, func() int { return len(r.Payload.Data) })
		},
		"trace services": func() (int, error) {
			r, e := c.Traces.ListTraceServices(traces.NewListTraceServicesParams().WithProject(c.Project), nil)
			return lenOr(r, e, func() int { return len(r.Payload.Data) })
		},
		"metrics": func() (int, error) {
			r, e := c.Metrics.ListMetrics(metrics.NewListMetricsParams().WithProject(c.Project), nil)
			return lenOr(r, e, func() int { return len(r.Payload.Data) })
		},
		"series": func() (int, error) {
			r, e := c.Metrics.ListSeries(metrics.NewListSeriesParams().WithProject(c.Project).WithName("up"), nil)
			return lenOr(r, e, func() int { return len(r.Payload.Data) })
		},
	} {
		if n, err := call(); err != nil || n == 0 {
			t.Errorf("%s: %d %v", name, n, err)
		}
	}
}

func lenOr[T any](r *T, err error, n func() int) (int, error) {
	if err != nil {
		return 0, err
	}
	return n(), nil
}

func TestTheHubsRefusalIsExplained(t *testing.T) {
	c, _ := connect(t, "a-wrong-key")
	_, err := c.Logs.SearchLogs(logs.NewSearchLogsParams().WithProject(c.Project), nil)
	var ae *fleet.APIError
	if !errors.As(fleet.Explain(err), &ae) || ae.Status != 403 || ae.Message != "this key cannot read that project" {
		t.Fatalf("%v", fleet.Explain(err))
	}
	if fleet.Explain(nil) != nil {
		t.Fatal("no error stays no error")
	}
}

func TestNewChecksItsArguments(t *testing.T) {
	for _, c := range []struct{ hub, project, key string }{
		{"hub.example.com", "us", "k"}, {"ftp://hub", "us", "k"}, {"https://hub?x=1", "us", "k"},
		{"https://hub", "", "k"}, {"https://hub", "us", " "},
	} {
		if _, err := fleet.New(c.hub, c.project, c.key); err == nil {
			t.Errorf("New(%q, %q, key) should fail", c.hub, c.project)
		}
	}
	if _, err := fleet.New("http://localhost:8080/telemetry/", "us", "k"); err != nil {
		t.Fatalf("a path prefix is allowed: %v", err)
	}
}

func TestHelpers(t *testing.T) {
	from, to := fleet.Since(time.Hour)
	f, _ := time.Parse(time.RFC3339Nano, *from)
	tt, _ := time.Parse(time.RFC3339Nano, *to)
	if d := tt.Sub(f); d < 59*time.Minute || d > 61*time.Minute {
		t.Fatalf("Since(1h) spans %v", d)
	}
	if fleet.Match("job", "a=b") != "job=a=b" {
		t.Fatal("Match")
	}
}
