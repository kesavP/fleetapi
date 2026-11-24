package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-client/internal/fakehub"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-client/models"
)

type rig struct {
	hub *fakehub.Hub
	env map[string]string
}

func newRig(t *testing.T, key string) *rig {
	t.Helper()
	hub := fakehub.New(t, "../..")
	keyFile := filepath.Join(t.TempDir(), "read-key")
	if err := os.WriteFile(keyFile, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &rig{hub: hub, env: map[string]string{"FLEET_HUB": hub.URL, "FLEET_PROJECT": "us", "FLEET_READ_KEY_FILE": keyFile}}
}

func (r *rig) run(args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = run(args, func(k string) string { return r.env[k] }, &out, &errb)
	return code, out.String(), errb.String()
}

func TestWhoami(t *testing.T) {
	r := newRig(t, fakehub.Key)
	code, out, errs := r.run("whoami")
	if code != 0 || !strings.Contains(out, "project:  us") || !strings.Contains(out, "read key: ok (2 service(s)") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	if strings.Contains(out+errs, fakehub.Key) {
		t.Fatal("the key must never be printed")
	}
	code, out, _ = r.run("whoami", "-o", "json")
	if code != 0 || !strings.Contains(out, `"read_key":"ok"`) {
		t.Fatalf("%d %s", code, out)
	}
}

func TestWhoamiSaysWhatIsMissing(t *testing.T) {
	r := newRig(t, fakehub.Key)
	delete(r.env, "FLEET_READ_KEY_FILE")
	if code, _, errs := r.run("whoami"); code != exitUsage || !strings.Contains(errs, "no read key: set FLEET_READ_KEY_FILE") {
		t.Fatalf("%d %q", code, errs)
	}
	r.env["FLEET_READ_KEY"] = fakehub.Key // the value form works too
	if code, _, errs := r.run("whoami"); code != 0 {
		t.Fatalf("%d %q", code, errs)
	}
	delete(r.env, "FLEET_HUB")
	if code, _, errs := r.run("whoami"); code != exitUsage || !strings.Contains(errs, "no hub") {
		t.Fatalf("%d %q", code, errs)
	}
	r.env["FLEET_HUB"] = r.hub.URL
	r.env["FLEET_READ_KEY_FILE"] = "/nonexistent/key"
	if code, _, errs := r.run("whoami"); code != exitUsage || !strings.Contains(errs, "cannot read the key file /nonexistent/key") {
		t.Fatalf("%d %q", code, errs)
	}
}

func TestAWrongKeyIsTheHubsMessageAndExit1(t *testing.T) {
	r := newRig(t, "not-the-key")
	code, out, errs := r.run("logs", "--since", "15m")
	if code != exitAPI || out != "" || strings.TrimSpace(errs) != "fleetctl: the hub answered 403: this key cannot read that project" {
		t.Fatalf("%d %q %q", code, out, errs)
	}
}

func TestLogs(t *testing.T) {
	r := newRig(t, fakehub.Key)
	code, out, errs := r.run("logs", "--service", "/k8s/shop/checkout", "--severity", "error", "--grep", "timeout", "--since", "2h", "--limit", "5", "-o", "json")
	if code != 0 {
		t.Fatalf("%d %q", code, errs)
	}
	q := r.hub.Last().Query()
	if q.Get("service") != "/k8s/shop/checkout" || q.Get("severity") != "error" || q.Get("q") != "timeout" || q.Get("limit") != "5" {
		t.Fatalf("asked %v", q)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if len(rows) != 1 || len(rows[0]) != 4 || rows[0]["severity"] != "ERROR" || rows[0]["time"] != "2026-11-23T10:00:00.000Z" {
		t.Fatalf("the default fields are time, service, severity, body: %v", rows)
	}
	_, out, _ = r.run("logs", "-o", "json", "--fields", "body,trace_id")
	if !strings.Contains(out, `"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736"`) || strings.Contains(out, `"severity"`) {
		t.Fatalf("--fields picks the fields: %s", out)
	}
	if code, _, errs := r.run("logs", "-o", "json", "--fields", "body,password"); code != exitUsage || !strings.Contains(errs, `unknown field "password"`) {
		t.Fatalf("%d %q", code, errs)
	}
	code, out, _ = r.run("logs")
	if code != 0 || !strings.Contains(out, "TIME") || !strings.Contains(out, "payment failed: upstream timeout") {
		t.Fatalf("table: %q", out)
	}
	code, out, _ = r.run("logs", "services", "-o", "json")
	if code != 0 || strings.TrimSpace(out) != `["/k8s/shop/cart","/k8s/shop/checkout"]` {
		t.Fatalf("services: %q", out)
	}
}

func TestTracesAndTrace(t *testing.T) {
	r := newRig(t, fakehub.Key)
	code, out, errs := r.run("traces", "--status", "ERROR", "--min-duration", "500ms", "--root", "-o", "json")
	if code != 0 {
		t.Fatalf("%d %q", code, errs)
	}
	q := r.hub.Last().Query()
	if q.Get("status") != "error" || q.Get("min_duration_ms") != "500" || q.Get("root") != "true" {
		t.Fatalf("asked %v", q)
	}
	if !strings.Contains(out, `"duration_ms":1250`) || !strings.Contains(out, `"status":"ERROR"`) {
		t.Fatalf("%s", out)
	}
	code, out, _ = r.run("trace", "4BF92F3577B34DA6A3CE929D0E0E4736")
	if code != 0 || !strings.HasSuffix(r.hub.Last().Path, "/traces/4bf92f3577b34da6a3ce929d0e0e4736") {
		t.Fatalf("%d %s %v", code, out, r.hub.Last())
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "POST /checkout") || !strings.HasPrefix(lines[2], "  POST /pay") || !strings.Contains(lines[2], "+10.00ms") {
		t.Fatalf("the trace is a tree, child under parent:\n%s", out)
	}
	_, out, _ = r.run("trace", "4bf92f3577b34da6a3ce929d0e0e4736", "-o", "json")
	if !strings.Contains(out, `"depth":1`) || !strings.Contains(out, `"parent_span_id":"00f067aa0ba902b7"`) {
		t.Fatalf("%s", out)
	}
	if code, _, _ := r.run("trace"); code != exitUsage {
		t.Fatal("trace needs an id")
	}
}

func TestMetrics(t *testing.T) {
	r := newRig(t, fakehub.Key)
	if code, out, _ := r.run("metrics", "-o", "json"); code != 0 || !strings.Contains(out, "container_net_tcp_successful_connects_total") {
		t.Fatalf("%d %s", code, out)
	}
	code, out, errs := r.run("query", "container_resources_cpu_usage_seconds_total", "--match", "container_id=/k8s/shop/x", "--match", "job=a", "--step", "60", "--rate", "--since", "3h")
	if code != 0 {
		t.Fatalf("%d %q", code, errs)
	}
	q := r.hub.Last().Query()
	if q.Get("name") != "container_resources_cpu_usage_seconds_total" || len(q["match"]) != 2 || q.Get("step") != "60" || q.Get("rate") != "true" {
		t.Fatalf("asked %v", q)
	}
	if !strings.Contains(out, "0.25") || !strings.Contains(out, "0.5") || !strings.Contains(out, "2026-11-23T10:01:00Z") {
		t.Fatalf("table: %s", out)
	}
	_, out, _ = r.run("query", "x", "-o", "json")
	if !strings.Contains(out, `"points":[[`) {
		t.Fatalf("json points are [ms, value]: %s", out)
	}
	if code, _, errs := r.run("query", "x", "--rate"); code != exitUsage || !strings.Contains(errs, "--rate needs --step") {
		t.Fatalf("%d %q", code, errs)
	}
	if code, _, errs := r.run("series", "x", "--match", "nolabel"); code != exitUsage || !strings.Contains(errs, "label=value") {
		t.Fatalf("%d %q", code, errs)
	}
	if code, out, _ := r.run("series", "x"); code != 0 || !strings.Contains(out, `container_id="/k8s/shop/checkout-7d9f8-x2bq4/app"`) {
		t.Fatalf("%d %s", code, out)
	}
}

func TestUsage(t *testing.T) {
	r := newRig(t, fakehub.Key)
	if code, out, _ := r.run(); code != 0 || !strings.Contains(out, "fleetctl whoami") {
		t.Fatalf("%d %s", code, out)
	}
	if code, _, errs := r.run("restart", "checkout"); code != exitUsage || !strings.Contains(errs, `unknown command "restart"`) {
		t.Fatalf("%d %q", code, errs)
	}
	if code, _, _ := r.run("logs", "-o", "yaml"); code != exitUsage {
		t.Fatal("-o yaml is refused")
	}
	if code, _, _ := r.run("logs", "--since", "-5m"); code != exitUsage {
		t.Fatal("a negative --since is refused")
	}
}

func TestTreeOfHandlesOrphansAndCycles(t *testing.T) {
	sp := func(id, parent string) *models.Span { return &models.Span{SpanID: id, ParentSpanID: parent} }
	tree := treeOf([]*models.Span{sp("c", "b"), sp("a", ""), sp("b", "a"), sp("o", "missing"), sp("x", "y"), sp("y", "x"), nil})
	var got []string
	for _, n := range tree {
		got = append(got, strings.Repeat(">", n.depth)+n.span.SpanID)
	}
	if strings.Join(got, " ") != "a >b >>c o x >y" && strings.Join(got, " ") != "a >b >>c o y >x" {
		t.Fatalf("%v", got)
	}
}
