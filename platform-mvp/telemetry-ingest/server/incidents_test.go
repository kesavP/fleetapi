package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/incident"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/project"
)

const (
	incUsManage = "us-manage-key-000001"
	incUsRead   = "us-read-key-00000001"
	incUsIngest = "us-ingest-key-0000001"
	incEuManage = "eu-manage-key-000001"
	incEuRead   = "eu-read-key-00000001"
)

type incRig struct {
	t   *testing.T
	srv *httptest.Server
	mem *incident.MemStore
}

func newIncRig(t *testing.T) *incRig {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "projects.json")
	data := `{"projects":[
	 {"name":"us","ingestKeys":["` + incUsIngest + `"],"readKeys":["` + incUsRead + `"],"manageKeys":["` + incUsManage + `"]},
	 {"name":"eu","readKeys":["` + incEuRead + `"],"manageKeys":["` + incEuManage + `"]}]}`
	if err := os.WriteFile(cfg, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := project.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mem := incident.NewMemStore()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	(&Incidents{Projects: reg, Manager: incident.NewManager(mem, log), Log: log}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &incRig{t: t, srv: srv, mem: mem}
}

type resp struct {
	Status int
	Header http.Header
	Body   map[string]any
	Raw    string
}

func (r *incRig) do(method, path, key string, body any, headers ...string) resp {
	r.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, r.srv.URL+path, rd)
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i+1] == "-" {
			req.Header.Del(headers[i])
		} else {
			req.Header.Set(headers[i], headers[i+1])
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := resp{Status: res.StatusCode, Header: res.Header, Raw: string(raw)}
	_ = json.Unmarshal(raw, &out.Body)
	return out
}

const incBase = "/api/v1/projects/us/incidents"

func (r *incRig) create(body map[string]any) map[string]any {
	r.t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	if _, ok := body["title"]; !ok {
		body["title"] = "frontend errors"
	}
	got := r.do("POST", incBase, incUsManage, body)
	if got.Status != 201 && got.Status != 200 {
		r.t.Fatalf("create: %d %s", got.Status, got.Raw)
	}
	return got.Body["data"].(map[string]any)
}

func (r *incRig) step(id string, body map[string]any, want int) resp {
	r.t.Helper()
	got := r.do("POST", incBase+"/"+id+"/transitions", incUsManage, body)
	if got.Status != want {
		r.t.Fatalf("transition %v: %d %s", body, got.Status, got.Raw)
	}
	return got
}

func TestIncidentKeys(t *testing.T) {
	r := newIncRig(t)
	inc := r.create(nil)
	id := inc["id"].(string)
	body := map[string]any{"text": "hi"}
	type c struct {
		name, method, path, key string
		body                    any
		want                    int
	}
	for _, k := range []c{
		{"no key reads", "GET", incBase, "", nil, 401},
		{"no key writes", "POST", incBase, "", map[string]any{"title": "x"}, 401},
		{"a read key reads", "GET", incBase, incUsRead, nil, 200},
		{"a read key reads one", "GET", incBase + "/" + id, incUsRead, nil, 200},
		{"a read key reads the timeline", "GET", incBase + "/" + id + "/events", incUsRead, nil, 200},
		{"a manage key reads", "GET", incBase, incUsManage, nil, 200},
		{"a read key cannot create", "POST", incBase, incUsRead, map[string]any{"title": "x"}, 403},
		{"a read key cannot move", "POST", incBase + "/" + id + "/transitions", incUsRead, map[string]any{"to": "acknowledged"}, 403},
		{"a read key cannot comment", "POST", incBase + "/" + id + "/comments", incUsRead, body, 403},
		{"a read key cannot edit", "PATCH", incBase + "/" + id, incUsRead, map[string]any{"title": "y"}, 403},
		{"a read key cannot attach", "POST", incBase + "/" + id + "/evidence", incUsRead, map[string]any{"kind": "application", "service": "x"}, 403},
		{"a read key cannot detach", "DELETE", incBase + "/" + id + "/evidence/ev_x", incUsRead, nil, 403},
		{"an ingest key cannot read", "GET", incBase, incUsIngest, nil, 403},
		{"an ingest key cannot write", "POST", incBase, incUsIngest, map[string]any{"title": "x"}, 403},
		{"an unknown key", "GET", incBase, "not-a-key-at-all-0000", nil, 403},
		{"another project's manage key cannot read", "GET", incBase, incEuManage, nil, 403},
		{"another project's manage key cannot write", "POST", incBase + "/" + id + "/comments", incEuManage, body, 403},
		{"another project's read key", "GET", incBase + "/" + id, incEuRead, nil, 403},
		{"a project that is not there answers like one that is", "GET", "/api/v1/projects/nope/incidents", incUsManage, nil, 403},
		{"and for a write", "POST", "/api/v1/projects/nope/incidents", incUsManage, map[string]any{"title": "x"}, 403},
	} {
		if got := r.do(k.method, k.path, k.key, k.body); got.Status != k.want {
			t.Errorf("%s: %d %s", k.name, got.Status, got.Raw)
		}
	}
	// the key in the Authorization header works as it does elsewhere
	req, _ := http.NewRequest("GET", r.srv.URL+incBase, nil)
	req.Header.Set("Authorization", "Bearer "+incUsRead)
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != 200 {
		t.Errorf("bearer: %d", res.StatusCode)
	}
	res.Body.Close()
	// nothing was changed by the refused writes
	if got := r.do("GET", incBase+"/"+id, incUsRead, nil); got.Body["data"].(map[string]any)["version"].(float64) != 1 {
		t.Fatal("a refused request changed the incident")
	}
}

func TestIncidentsOfOneProjectAreInvisibleToAnother(t *testing.T) {
	r := newIncRig(t)
	id := r.create(nil)["id"].(string)
	if got := r.do("GET", "/api/v1/projects/eu/incidents/"+id, incEuRead, nil); got.Status != 404 {
		t.Fatalf("%d %s", got.Status, got.Raw)
	}
	if got := r.do("POST", "/api/v1/projects/eu/incidents/"+id+"/comments", incEuManage, map[string]any{"text": "x"}); got.Status != 404 {
		t.Fatalf("%d %s", got.Status, got.Raw)
	}
	list := r.do("GET", "/api/v1/projects/eu/incidents", incEuRead, nil)
	if d := list.Body["data"].([]any); len(d) != 0 {
		t.Fatalf("%v", d)
	}
}

func TestCreate(t *testing.T) {
	r := newIncRig(t)
	got := r.do("POST", incBase, incUsManage, map[string]any{"title": "frontend errors", "app": "/k8s/shop/frontend", "severity": "critical", "summary": "5xx", "started_at": "2026-01-01T10:00:00Z"}, "X-Actor", "ana")
	if got.Status != 201 {
		t.Fatalf("%d %s", got.Status, got.Raw)
	}
	inc := got.Body["data"].(map[string]any)
	if inc["state"] != "open" || inc["severity"] != "critical" || inc["source"] != "manual" || inc["version"].(float64) != 1 || inc["started_at"] != "2026-01-01T10:00:00Z" {
		t.Fatalf("%v", inc)
	}
	if loc := got.Header.Get("Location"); loc != incBase+"/"+inc["id"].(string) {
		t.Fatalf("Location %q", loc)
	}
	if got.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("an incident may be cached")
	}
	if len(inc["allowed_transitions"].([]any)) != 4 {
		t.Fatalf("%v", inc["allowed_transitions"])
	}
	evs := r.do("GET", incBase+"/"+inc["id"].(string)+"/events", incUsRead, nil).Body["data"].([]any)
	if len(evs) != 1 || evs[0].(map[string]any)["actor"] != "ana" || evs[0].(map[string]any)["type"] != "created" {
		t.Fatalf("%v", evs)
	}
	// with no X-Actor it says so rather than leaving it blank
	other := r.create(nil)
	evs = r.do("GET", incBase+"/"+other["id"].(string)+"/events", incUsRead, nil).Body["data"].([]any)
	if evs[0].(map[string]any)["actor"] != "api" {
		t.Fatalf("%v", evs)
	}
}

func TestCreateIsIdempotentWithADedupeKey(t *testing.T) {
	r := newIncRig(t)
	body := map[string]any{"title": "frontend errors", "source": "detected", "dedupe_key": "/k8s/shop/frontend|1800000000"}
	a := r.do("POST", incBase, incUsManage, body)
	b := r.do("POST", incBase, incUsManage, body)
	if a.Status != 201 || b.Status != 200 {
		t.Fatalf("%d then %d", a.Status, b.Status)
	}
	if a.Body["data"].(map[string]any)["id"] != b.Body["data"].(map[string]any)["id"] {
		t.Fatal("two incidents")
	}
	list := r.do("GET", incBase, incUsRead, nil).Body["data"].([]any)
	if len(list) != 1 {
		t.Fatalf("%d incidents", len(list))
	}
}

func TestCreateRefusals(t *testing.T) {
	r := newIncRig(t)
	for name, body := range map[string]any{
		"no title":         map[string]any{},
		"a blank title":    map[string]any{"title": "  "},
		"a bad severity":   map[string]any{"title": "x", "severity": "meh"},
		"a bad source":     map[string]any{"title": "x", "source": "robot"},
		"a bad start":      map[string]any{"title": "x", "started_at": "yesterday"},
		"a future start":   map[string]any{"title": "x", "started_at": "2999-01-01T00:00:00Z"},
		"a wrong type":     map[string]any{"title": 5},
		"an unknown field": map[string]any{"title": "x", "state": "resolved"},
		"an id of its own": map[string]any{"title": "x", "id": "abcd1234"},
	} {
		if got := r.do("POST", incBase, incUsManage, body); got.Status != 400 || got.Body["error"] == nil {
			t.Errorf("%s: %d %s", name, got.Status, got.Raw)
		}
	}
	if len(r.do("GET", incBase, incUsRead, nil).Body["data"].([]any)) != 0 {
		t.Fatal("something was created")
	}
}

func TestBodies(t *testing.T) {
	r := newIncRig(t)
	good := `{"title":"x"}`
	cases := []struct {
		name string
		body string
		hdr  []string
		want int
	}{
		{"fine", good, nil, 201},
		{"with a charset", good, []string{"Content-Type", "application/json; charset=utf-8"}, 201},
		{"no content type", good, []string{"Content-Type", "-"}, 415},
		{"plain text", good, []string{"Content-Type", "text/plain"}, 415},
		{"a form", good, []string{"Content-Type", "application/x-www-form-urlencoded"}, 415},
		{"json that is not an object", `[1]`, nil, 400},
		{"not json", `{"title":`, nil, 400},
		{"empty", ``, nil, 400},
		{"two objects", good + good, nil, 400},
		{"trailing words", good + ` extra`, nil, 400},
		{"too big", `{"title":"x","summary":"` + strings.Repeat("a", 70<<10) + `"}`, nil, 413},
		{"null", `null`, nil, 400},
	}
	for _, c := range cases {
		got := r.do("POST", incBase, incUsManage, c.body, c.hdr...)
		if got.Status != c.want {
			t.Errorf("%s: %d %.200s", c.name, got.Status, got.Raw)
		}
	}
}

func TestActorHeader(t *testing.T) {
	r := newIncRig(t)
	for _, bad := range []string{"a;b", "<script>", strings.Repeat("a", 65), "-lead", "a\tb"} {
		if got := r.do("POST", incBase, incUsManage, map[string]any{"title": "x"}, "X-Actor", bad); got.Status != 400 {
			t.Errorf("%q: %d", bad, got.Status)
		}
	}
	if got := r.do("POST", incBase, incUsManage, map[string]any{"title": "x"}, "X-Actor", "ana@example.com"); got.Status != 201 {
		t.Errorf("%d", got.Status)
	}
	// a read does not need one, and does not look at it
	if got := r.do("GET", incBase, incUsRead, nil, "X-Actor", "a;b"); got.Status != 200 {
		t.Errorf("%d", got.Status)
	}
}

func TestGetAndNotFound(t *testing.T) {
	r := newIncRig(t)
	inc := r.create(map[string]any{"title": "frontend errors"})
	got := r.do("GET", incBase+"/"+inc["id"].(string), incUsRead, nil)
	if got.Status != 200 || got.Body["data"].(map[string]any)["title"] != "frontend errors" {
		t.Fatalf("%d %s", got.Status, got.Raw)
	}
	for _, id := range []string{"nope0000", "ABCD1234", "short", "%2e%2e", "abcd1234%27", "a%20b"} {
		for _, p := range []string{"", "/events", "/evidence"} {
			if got := r.do("GET", incBase+"/"+id+p, incUsRead, nil); got.Status != 404 {
				t.Errorf("%q%s: %d %s", id, p, got.Status, got.Raw)
			}
		}
	}
	if got := r.do("POST", incBase+"/nope0000/transitions", incUsManage, map[string]any{"to": "acknowledged"}); got.Status != 404 {
		t.Errorf("%d", got.Status)
	}
}

func TestTheLifecycleOverHTTP(t *testing.T) {
	r := newIncRig(t)
	id := r.create(nil)["id"].(string)
	inc := func(g resp) map[string]any { return g.Body["data"].(map[string]any) }

	got := inc(r.step(id, map[string]any{"to": "acknowledged"}, 200))
	if got["state"] != "acknowledged" || got["acknowledged_at"] == nil || got["version"].(float64) != 2 {
		t.Fatalf("%v", got)
	}
	r.step(id, map[string]any{"to": "investigating", "note": "looking at redis"}, 200)
	if m := inc(r.step(id, map[string]any{"to": "mitigated"}, 200)); m["mitigated_at"] == nil {
		t.Fatal("no mitigated_at")
	}
	got = inc(r.step(id, map[string]any{"to": "resolved", "resolution": "fixed", "note": "network"}, 200))
	if got["state"] != "resolved" || got["resolution"] != "fixed" || got["resolved_at"] == nil {
		t.Fatalf("%v", got)
	}
	inc(r.step(id, map[string]any{"to": "closed"}, 200))
	got = inc(r.step(id, map[string]any{"to": "open", "note": "it is back"}, 200))
	if got["state"] != "open" || got["reopened"].(float64) != 1 || got["resolved_at"] != nil || got["closed_at"] != nil {
		t.Fatalf("%v", got)
	}
	evs := r.do("GET", incBase+"/"+id+"/events", incUsRead, nil).Body["data"].([]any)
	var types []string
	for _, e := range evs {
		types = append(types, e.(map[string]any)["type"].(string))
	}
	if strings.Join(types, ",") != "created,state_changed,state_changed,state_changed,state_changed,state_changed,state_changed" {
		t.Fatalf("%v", types)
	}
	last := evs[len(evs)-1].(map[string]any)["data"].(map[string]any)
	if last["from"] != "closed" || last["to"] != "open" || last["note"] != "it is back" {
		t.Fatalf("%v", last)
	}
}

func TestRefusedTransitionsSayWhatIsAllowed(t *testing.T) {
	r := newIncRig(t)
	id := r.create(nil)["id"].(string)
	got := r.step(id, map[string]any{"to": "closed"}, 409)
	allowed := got.Body["allowed"].([]any)
	if got.Body["from"] != "open" || len(allowed) != 4 || allowed[0] != "acknowledged" {
		t.Fatalf("%s", got.Raw)
	}
	r.step(id, map[string]any{"to": "resolved"}, 409) // no resolution
	r.step(id, map[string]any{"to": "open"}, 409)     // already open
	r.step(id, map[string]any{"to": "bogus"}, 409)
	r.step(id, map[string]any{"to": "acknowledged", "resolution": "fixed"}, 409)
	r.step(id, map[string]any{}, 400)                  // no target at all
	r.step(id, map[string]any{"state": "closed"}, 400) // the wrong name for it
	if v := r.do("GET", incBase+"/"+id, incUsRead, nil).Body["data"].(map[string]any)["version"].(float64); v != 1 {
		t.Fatalf("refused moves changed the incident: version %v", v)
	}
	r.step(id, map[string]any{"to": "resolved", "resolution": "fixed"}, 200)
	r.step(id, map[string]any{"to": "open"}, 409) // reopening needs a note
}

func TestVersions(t *testing.T) {
	r := newIncRig(t)
	id := r.create(nil)["id"].(string)
	// the version in the body
	got := r.step(id, map[string]any{"to": "acknowledged", "expected_version": 1}, 200)
	if got.Body["data"].(map[string]any)["version"].(float64) != 2 {
		t.Fatal(got.Raw)
	}
	stale := r.step(id, map[string]any{"to": "investigating", "expected_version": 1}, 409)
	if stale.Body["current_version"].(float64) != 2 {
		t.Fatalf("%s", stale.Raw)
	}
	// the version in If-Match, quoted or not
	if g := r.do("POST", incBase+"/"+id+"/comments", incUsManage, map[string]any{"text": "x"}, "If-Match", `"1"`); g.Status != 409 {
		t.Fatalf("%d %s", g.Status, g.Raw)
	}
	if g := r.do("POST", incBase+"/"+id+"/comments", incUsManage, map[string]any{"text": "x"}, "If-Match", "2"); g.Status != 201 {
		t.Fatalf("%d %s", g.Status, g.Raw)
	}
	// both, and they agree or they do not
	if g := r.do("POST", incBase+"/"+id+"/comments", incUsManage, map[string]any{"text": "y", "expected_version": 3}, "If-Match", "3"); g.Status != 201 {
		t.Fatalf("%d %s", g.Status, g.Raw)
	}
	if g := r.do("POST", incBase+"/"+id+"/comments", incUsManage, map[string]any{"text": "z", "expected_version": 4}, "If-Match", "3"); g.Status != 400 {
		t.Fatalf("%d %s", g.Status, g.Raw)
	}
	for _, bad := range []string{"abc", "0", "-1", "99999999999", `W/"1"`} {
		if g := r.do("POST", incBase+"/"+id+"/comments", incUsManage, map[string]any{"text": "x"}, "If-Match", bad); g.Status != 400 {
			t.Errorf("If-Match %q: %d", bad, g.Status)
		}
	}
	if g := r.step(id, map[string]any{"to": "investigating", "expected_version": 0}, 400); g.Body["error"] == nil {
		t.Fatal("expected_version 0 should be refused")
	}
	// a stale version on a change that was also invalid says it is stale first: the caller has to look again before anything else matters
	r.step(id, map[string]any{"to": "closed", "expected_version": 1}, 409)
}

func TestUpdateAndComment(t *testing.T) {
	r := newIncRig(t)
	id := r.create(nil)["id"].(string)
	got := r.do("PATCH", incBase+"/"+id, incUsManage, map[string]any{"title": "new", "severity": "info", "summary": "s", "assignee": "bo"})
	d := got.Body["data"].(map[string]any)
	if got.Status != 200 || d["title"] != "new" || d["severity"] != "info" || d["assignee"] != "bo" || d["version"].(float64) != 2 {
		t.Fatalf("%d %s", got.Status, got.Raw)
	}
	if g := r.do("PATCH", incBase+"/"+id, incUsManage, map[string]any{"assignee": ""}); g.Status != 200 || g.Body["data"].(map[string]any)["assignee"] != nil {
		t.Fatalf("clearing the assignee: %d %s", g.Status, g.Raw)
	}
	for name, body := range map[string]any{
		"nothing":             map[string]any{},
		"no change":           map[string]any{"title": "new"},
		"a state by the back": map[string]any{"state": "closed"},
		"a blank title":       map[string]any{"title": " "},
		"a bad severity":      map[string]any{"severity": "x"},
	} {
		if g := r.do("PATCH", incBase+"/"+id, incUsManage, body); g.Status != 400 {
			t.Errorf("%s: %d %s", name, g.Status, g.Raw)
		}
	}
	if g := r.do("POST", incBase+"/"+id+"/comments", incUsManage, map[string]any{"text": "checked the logs"}, "X-Actor", "bo"); g.Status != 201 {
		t.Fatalf("%d %s", g.Status, g.Raw)
	}
	for _, bad := range []any{map[string]any{}, map[string]any{"text": " "}, map[string]any{"text": strings.Repeat("a", 4001)}} {
		if g := r.do("POST", incBase+"/"+id+"/comments", incUsManage, bad); g.Status != 400 {
			t.Errorf("%d", g.Status)
		}
	}
	evs := r.do("GET", incBase+"/"+id+"/events?after=2", incUsRead, nil).Body["data"].([]any)
	last := evs[len(evs)-1].(map[string]any)
	if last["type"] != "commented" || last["actor"] != "bo" {
		t.Fatalf("%v", last)
	}
}

func TestEvidence(t *testing.T) {
	r := newIncRig(t)
	id := r.create(nil)["id"].(string)
	trace := map[string]any{"kind": "trace", "trace_id": strings.Repeat("a", 32), "note": "the failing one"}
	got := r.do("POST", incBase+"/"+id+"/evidence", incUsManage, trace, "X-Actor", "ana")
	if got.Status != 201 {
		t.Fatalf("%d %s", got.Status, got.Raw)
	}
	data := got.Body["data"].(map[string]any)
	ev := data["evidence"].(map[string]any)
	evID := ev["id"].(string)
	if !strings.HasPrefix(evID, "ev_") || ev["added_by"] != "ana" || ev["ref"].(map[string]any)["trace_id"] != strings.Repeat("a", 32) {
		t.Fatalf("%v", ev)
	}
	if len(data["incident"].(map[string]any)["evidence"].([]any)) != 1 {
		t.Fatal("the incident does not list it")
	}
	// the same again: 200, nothing new
	again := r.do("POST", incBase+"/"+id+"/evidence", incUsManage, trace)
	if again.Status != 200 || again.Body["data"].(map[string]any)["evidence"].(map[string]any)["id"] != evID {
		t.Fatalf("%d %s", again.Status, again.Raw)
	}
	if v := again.Body["data"].(map[string]any)["incident"].(map[string]any)["version"].(float64); v != 2 {
		t.Fatalf("version %v: the repeat was recorded", v)
	}
	// logs, profile, metric, application and url references
	for _, ref := range []map[string]any{
		{"kind": "logs", "service": "/k8s/shop/cart", "severity": "error", "from": "2026-01-01T10:00:00Z", "to": "2026-01-01T10:10:00Z"},
		{"kind": "profile", "service": "/k8s/shop/cart", "profile_type": "cpu:nanoseconds"},
		{"kind": "metric", "metric": "container_net_latency_seconds", "match": map[string]any{"container_id": "/k8s/shop/cart/x"}},
		{"kind": "application", "service": "/k8s/shop/frontend"},
		{"kind": "url", "url": "https://grafana.example.com/d/abc"},
	} {
		if g := r.do("POST", incBase+"/"+id+"/evidence", incUsManage, ref); g.Status != 201 {
			t.Errorf("%v: %d %s", ref, g.Status, g.Raw)
		}
	}
	list := r.do("GET", incBase+"/"+id+"/evidence", incUsRead, nil).Body["data"].([]any)
	if len(list) != 6 {
		t.Fatalf("%d pieces", len(list))
	}
	// a reference that is wrong is refused whole
	for name, ref := range map[string]any{
		"an unknown kind":         map[string]any{"kind": "screenshot"},
		"a javascript url":        map[string]any{"kind": "url", "url": "javascript:alert(1)"},
		"a trace without an id":   map[string]any{"kind": "trace"},
		"a field of another kind": map[string]any{"kind": "trace", "trace_id": strings.Repeat("a", 32), "service": "x"},
		"an unknown field":        map[string]any{"kind": "application", "service": "x", "html": "<b>"},
		"nothing":                 map[string]any{},
	} {
		if g := r.do("POST", incBase+"/"+id+"/evidence", incUsManage, ref); g.Status != 400 {
			t.Errorf("%s: %d %s", name, g.Status, g.Raw)
		}
	}
	// remove one, by id, with a reason, at a version
	v := r.do("GET", incBase+"/"+id, incUsRead, nil).Body["data"].(map[string]any)["version"].(float64)
	if g := r.do("DELETE", incBase+"/"+id+"/evidence/"+evID+"?reason=wrong+trace", incUsManage, nil, "If-Match", "1"); g.Status != 409 {
		t.Fatalf("a stale delete: %d %s", g.Status, g.Raw)
	}
	del := r.do("DELETE", incBase+"/"+id+"/evidence/"+evID+"?reason=wrong+trace", incUsManage, nil, "If-Match", strconvF(v))
	if del.Status != 200 || len(del.Body["data"].(map[string]any)["evidence"].([]any)) != 5 {
		t.Fatalf("%d %s", del.Status, del.Raw)
	}
	if g := r.do("DELETE", incBase+"/"+id+"/evidence/"+evID, incUsManage, nil); g.Status != 404 {
		t.Fatalf("removing it twice: %d", g.Status)
	}
	if g := r.do("DELETE", incBase+"/"+id+"/evidence/ev_unknown", incUsManage, nil); g.Status != 404 {
		t.Fatalf("%d", g.Status)
	}
	evs := r.do("GET", incBase+"/"+id+"/events?limit=500", incUsRead, nil).Body["data"].([]any)
	last := evs[len(evs)-1].(map[string]any)
	if last["type"] != "evidence_removed" || last["data"].(map[string]any)["reason"] != "wrong trace" {
		t.Fatalf("%v", last)
	}
}

func strconvF(f float64) string { return strings.TrimSuffix(strings.TrimSpace(jsonNumber(f)), ".0") }
func jsonNumber(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

func TestEventsPaging(t *testing.T) {
	r := newIncRig(t)
	id := r.create(nil)["id"].(string)
	for i := 0; i < 5; i++ {
		r.do("POST", incBase+"/"+id+"/comments", incUsManage, map[string]any{"text": "c"})
	}
	page := r.do("GET", incBase+"/"+id+"/events?after=2&limit=2", incUsRead, nil).Body["data"].([]any)
	if len(page) != 2 || page[0].(map[string]any)["seq"].(float64) != 3 {
		t.Fatalf("%v", page)
	}
	if empty := r.do("GET", incBase+"/"+id+"/events?after=99", incUsRead, nil); empty.Status != 200 || empty.Raw == "" || strings.Contains(empty.Raw, "null") {
		t.Fatalf("an empty page is a list: %d %s", empty.Status, empty.Raw)
	}
	for _, q := range []string{"after=-1", "after=x", "limit=0", "limit=501", "limit=x"} {
		if g := r.do("GET", incBase+"/"+id+"/events?"+q, incUsRead, nil); g.Status != 400 {
			t.Errorf("%s: %d", q, g.Status)
		}
	}
}

func TestListFilters(t *testing.T) {
	r := newIncRig(t)
	a := r.create(map[string]any{"title": "a", "app": "/k8s/shop/frontend", "severity": "critical"})["id"].(string)
	b := r.create(map[string]any{"title": "b", "app": "/k8s/shop/cart", "severity": "warning"})["id"].(string)
	r.create(map[string]any{"title": "c", "app": "/k8s/shop/frontend", "severity": "warning"})
	r.step(b, map[string]any{"to": "resolved", "resolution": "fixed"}, 200)
	_ = a
	titles := func(q string) string {
		g := r.do("GET", incBase+q, incUsRead, nil)
		if g.Status != 200 {
			t.Fatalf("%s: %d %s", q, g.Status, g.Raw)
		}
		var out []string
		for _, i := range g.Body["data"].([]any) {
			out = append(out, i.(map[string]any)["title"].(string))
		}
		return strings.Join(out, "")
	}
	if got := titles(""); len(got) != 3 {
		t.Errorf("%q", got)
	}
	if got := titles("?state=resolved"); got != "b" {
		t.Errorf("%q", got)
	}
	if got := titles("?state=open,resolved"); len(got) != 3 {
		t.Errorf("%q", got)
	}
	if got := titles("?app=/k8s/shop/frontend"); len(got) != 2 {
		t.Errorf("%q", got)
	}
	if got := titles("?severity=critical"); got != "a" {
		t.Errorf("%q", got)
	}
	if got := titles("?limit=1"); len(got) != 1 {
		t.Errorf("%q", got)
	}
	for _, q := range []string{"?state=nope", "?state=open,nope", "?severity=loud", "?limit=0", "?limit=201", "?limit=x"} {
		if g := r.do("GET", incBase+q, incUsRead, nil); g.Status != 400 {
			t.Errorf("%s: %d", q, g.Status)
		}
	}
	if g := r.do("GET", "/api/v1/projects/eu/incidents", incEuRead, nil); strings.Contains(g.Raw, "null") {
		t.Errorf("an empty list is [], not null: %s", g.Raw)
	}
}

func TestMethodsThatAreNotThere(t *testing.T) {
	r := newIncRig(t)
	id := r.create(nil)["id"].(string)
	for _, c := range [][2]string{{"PUT", incBase + "/" + id}, {"DELETE", incBase + "/" + id}, {"POST", incBase + "/" + id}, {"DELETE", incBase}, {"PATCH", incBase}, {"GET", incBase + "/" + id + "/transitions"}, {"GET", incBase + "/" + id + "/comments"}} {
		if g := r.do(c[0], c[1], incUsManage, map[string]any{}); g.Status != 405 && g.Status != 404 {
			t.Errorf("%s %s: %d", c[0], c[1], g.Status)
		}
	}
	// and nothing was deleted by one of them
	if g := r.do("GET", incBase+"/"+id, incUsRead, nil); g.Status != 200 {
		t.Fatal("the incident is gone")
	}
}

func TestAFailingDatabaseIsAPlainErrorWithNothingInternal(t *testing.T) {
	r := newIncRig(t)
	id := r.create(nil)["id"].(string)
	r.mem.SetFailing(true)
	for _, c := range []struct {
		method, path string
		body         any
		key          string
	}{
		{"GET", incBase, nil, incUsRead},
		{"GET", incBase + "/" + id, nil, incUsRead},
		{"POST", incBase + "/" + id + "/transitions", map[string]any{"to": "acknowledged"}, incUsManage},
		{"POST", incBase, map[string]any{"title": "x"}, incUsManage},
	} {
		g := r.do(c.method, c.path, c.key, c.body)
		if g.Status != 500 || g.Body["error"] != "request failed" || strings.Contains(g.Raw, "disk") {
			t.Errorf("%s %s: %d %s", c.method, c.path, g.Status, g.Raw)
		}
	}
	r.mem.SetFailing(false)
	if g := r.do("GET", incBase+"/"+id, incUsRead, nil); g.Status != 200 || g.Body["data"].(map[string]any)["version"].(float64) != 1 {
		t.Fatalf("%d %s", g.Status, g.Raw)
	}
}

func TestErrorsNeverEchoTheRequestAtLength(t *testing.T) {
	r := newIncRig(t)
	long := strings.Repeat("A", 5000)
	g := r.do("POST", incBase, incUsManage, `{"title":"x","severity":`+long)
	if g.Status != 400 || len(g.Raw) > 600 {
		t.Fatalf("%d, %d bytes", g.Status, len(g.Raw))
	}
}
