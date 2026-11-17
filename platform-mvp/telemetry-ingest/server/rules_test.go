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

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/project"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/rules"
)

const rulesBase = "/api/v1/projects/us/rules"

type rulesRig struct {
	t   *testing.T
	srv *httptest.Server
	mem *rules.MemStore
}

func newRulesRig(t *testing.T) *rulesRig {
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
	mem := rules.NewMemStore()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	(&Rules{Projects: reg, Manager: rules.NewManager(mem, nil), Log: log}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &rulesRig{t: t, srv: srv, mem: mem}
}

func (r *rulesRig) do(method, path, key string, body any, headers ...string) resp {
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

func goodRule() map[string]any {
	return map[string]any{"enabled": true, "percent": 40, "days": 14, "percentile": 99}
}

func (r *rulesRig) list(key string) []map[string]any {
	r.t.Helper()
	got := r.do("GET", rulesBase, key, nil)
	if got.Status != 200 {
		r.t.Fatalf("list: %d %s", got.Status, got.Raw)
	}
	var out []map[string]any
	for _, x := range got.Body["data"].([]any) {
		out = append(out, x.(map[string]any))
	}
	return out
}

func TestTheRulesAreListedWithTheirDefaultsWhenNothingIsSaved(t *testing.T) {
	r := newRulesRig(t)
	got := r.list(incUsRead)
	if len(got) != 3 || got[0]["name"] != "rightsize.memory" || got[1]["name"] != "rightsize.cpu" || got[2]["name"] != "rightsize.storage" {
		t.Fatalf("rules = %v", got)
	}
	m := got[0]
	if m["enabled"] != true || m["percent"] != float64(50) || m["days"] != float64(7) || m["percentile"] != float64(95) || m["version"] != float64(0) || m["saved"] != false {
		t.Fatalf("memory = %v", m)
	}
	if _, has := m["updated_at"]; has {
		t.Fatalf("a rule nothing is saved for has no time: %v", m)
	}
	if d, _ := m["default"].(map[string]any); d["percent"] != float64(50) {
		t.Fatalf("default = %v", m["default"])
	}
	if got[2]["enabled"] != false {
		t.Fatalf("storage is off by default: %v", got[2])
	}
	// a manage key reads too
	if len(r.list(incUsManage)) != 3 {
		t.Fatal("a manage key could not read")
	}
}

func TestAManageKeyChangesARuleAndEveryoneThenSeesIt(t *testing.T) {
	r := newRulesRig(t)
	got := r.do("PUT", rulesBase+"/rightsize.memory", incUsManage, goodRule(), "X-Actor", "sam")
	if got.Status != 200 {
		t.Fatalf("%d %s", got.Status, got.Raw)
	}
	d := got.Body["data"].(map[string]any)
	if d["name"] != "rightsize.memory" || d["percent"] != float64(40) || d["days"] != float64(14) || d["percentile"] != float64(99) || d["saved"] != true || d["version"] != float64(1) || d["updated_by"] != "sam" || d["updated_at"] == nil {
		t.Fatalf("put = %v", d)
	}
	for _, key := range []string{incUsRead, incUsManage} {
		if l := r.list(key)[0]; l["percent"] != float64(40) || l["version"] != float64(1) || l["updated_by"] != "sam" {
			t.Fatalf("list = %v", l)
		}
	}
	if l := r.list(incUsRead)[1]; l["saved"] != false || l["percent"] != float64(50) {
		t.Fatalf("saving memory changed cpu: %v", l)
	}
	// without a name the change is recorded as the API's
	got = r.do("PUT", rulesBase+"/rightsize.cpu", incUsManage, goodRule())
	if got.Body["data"].(map[string]any)["updated_by"] != "api" {
		t.Fatalf("put = %v", got.Body)
	}
	// another project's rules are its own
	if eu := r.do("GET", "/api/v1/projects/eu/rules", incEuRead, nil); eu.Status != 200 || strings.Contains(eu.Raw, `"saved":true`) {
		t.Fatalf("eu = %d %s", eu.Status, eu.Raw)
	}
}

func TestKeysDecideWhoMayReadAndWhoMayChange(t *testing.T) {
	r := newRulesRig(t)
	type c struct {
		name, method, path, key string
		body                    any
		want                    int
	}
	for _, k := range []c{
		{"no key reads", "GET", rulesBase, "", nil, 401},
		{"no key writes", "PUT", rulesBase + "/rightsize.memory", "", goodRule(), 401},
		{"a read key reads", "GET", rulesBase, incUsRead, nil, 200},
		{"an ingest key does not read", "GET", rulesBase, incUsIngest, nil, 403},
		{"another project's key does not read", "GET", rulesBase, incEuRead, nil, 403},
		{"a read key does not write", "PUT", rulesBase + "/rightsize.memory", incUsRead, goodRule(), 403},
		{"an ingest key does not write", "PUT", rulesBase + "/rightsize.memory", incUsIngest, goodRule(), 403},
		{"another project's manage key does not write", "PUT", rulesBase + "/rightsize.memory", incEuManage, goodRule(), 403},
		{"a manage key writes", "PUT", rulesBase + "/rightsize.memory", incUsManage, goodRule(), 200},
	} {
		if got := r.do(k.method, k.path, k.key, k.body); got.Status != k.want {
			t.Errorf("%s: %d %s", k.name, got.Status, got.Raw)
		}
	}
	if n := r.mem.Count(); n != 1 {
		t.Fatalf("%d versions were kept: only the manage key's save should have been", n)
	}
	for _, other := range []string{"POST", "PATCH", "DELETE"} {
		if got := r.do(other, rulesBase+"/rightsize.memory", incUsManage, goodRule()); got.Status != 405 && got.Status != 404 {
			t.Errorf("%s was answered %d: a rule is read and put, nothing else", other, got.Status)
		}
	}
}

func TestASettingThatIsWrongIsRefusedWithWhatIsWrongAndNothingIsSaved(t *testing.T) {
	r := newRulesRig(t)
	with := func(k string, v any) map[string]any {
		b := goodRule()
		if v == nil {
			delete(b, k)
		} else {
			b[k] = v
		}
		return b
	}
	for name, c := range map[string]struct {
		body any
		say  string
	}{
		"a percent of zero":        {with("percent", 0), "percent"},
		"a percent over 100":       {with("percent", 101), "percent"},
		"days of zero":             {with("days", 0), "days"},
		"too many days":            {with("days", 31), "days"},
		"a percentile under 50":    {with("percentile", 10), "percentile"},
		"no percent":               {with("percent", nil), "percent is required"},
		"no days":                  {with("days", nil), "days is required"},
		"no percentile":            {with("percentile", nil), "percentile is required"},
		"no enabled":               {with("enabled", nil), "enabled is required"},
		"a percent that is text":   {with("percent", "forty"), "not valid"},
		"days that are not whole":  {with("days", 7.5), "not valid"},
		"a setting it has not got": {with("colour", "red"), "not valid"},
		"nothing":                  {"", "empty"},
		"JSON that is not":         {"{", "not valid"},
	} {
		got := r.do("PUT", rulesBase+"/rightsize.memory", incUsManage, c.body)
		if got.Status != 400 || !strings.Contains(got.Raw, c.say) {
			t.Errorf("%s: %d %s", name, got.Status, got.Raw)
		}
	}
	if got := r.do("PUT", rulesBase+"/rightsize.memory", incUsManage, goodRule(), "Content-Type", "text/plain"); got.Status != 415 {
		t.Errorf("a body that is not JSON: %d", got.Status)
	}
	if got := r.do("PUT", rulesBase+"/rightsize.memory", incUsManage, goodRule(), "X-Actor", "a;b"); got.Status != 400 {
		t.Errorf("an actor with a character that is not allowed: %d", got.Status)
	}
	if n := r.mem.Count(); n != 0 {
		t.Fatalf("%d versions were kept by changes that were refused", n)
	}
}

func TestARuleThatDoesNotExistIsNotFound(t *testing.T) {
	r := newRulesRig(t)
	for _, name := range []string{"rightsize.network", "rightsize", "rightsize.memory.x", "RIGHTSIZE.MEMORY"} {
		if got := r.do("PUT", rulesBase+"/"+name, incUsManage, goodRule()); got.Status != 404 {
			t.Errorf("%q: %d %s", name, got.Status, got.Raw)
		}
	}
}

func TestAChangeBasedOnAnOldReadIsRefusedWithTheCurrentVersion(t *testing.T) {
	r := newRulesRig(t)
	r.do("PUT", rulesBase+"/rightsize.memory", incUsManage, goodRule())
	stale := goodRule()
	stale["expected_version"] = 0
	got := r.do("PUT", rulesBase+"/rightsize.memory", incUsManage, stale)
	if got.Status != 409 || got.Body["current_version"] != float64(1) {
		t.Fatalf("%d %s", got.Status, got.Raw)
	}
	ok := goodRule()
	ok["expected_version"] = 1
	if got := r.do("PUT", rulesBase+"/rightsize.memory", incUsManage, ok); got.Status != 200 || got.Body["data"].(map[string]any)["version"] != float64(2) {
		t.Fatalf("a change based on the current version: %d %s", got.Status, got.Raw)
	}
	// the If-Match header says the same
	if got := r.do("PUT", rulesBase+"/rightsize.memory", incUsManage, goodRule(), "If-Match", `"1"`); got.Status != 409 {
		t.Fatalf("If-Match with an old version: %d %s", got.Status, got.Raw)
	}
	if got := r.do("PUT", rulesBase+"/rightsize.memory", incUsManage, goodRule(), "If-Match", `"2"`); got.Status != 200 {
		t.Fatalf("If-Match with the current version: %d %s", got.Status, got.Raw)
	}
}

func TestAStoreThatFailsIsOneSentenceAndNeverTheInside(t *testing.T) {
	r := newRulesRig(t)
	r.mem.SetFailing(true)
	for _, c := range []struct{ method, path, key string }{{"GET", rulesBase, incUsRead}, {"PUT", rulesBase + "/rightsize.memory", incUsManage}} {
		got := r.do(c.method, c.path, c.key, map[string]any{"enabled": true, "percent": 40, "days": 14, "percentile": 99})
		if got.Status != 500 || strings.Contains(got.Raw, "disk full") {
			t.Errorf("%s: %d %s", c.method, got.Status, got.Raw)
		}
	}
}

func TestEveryAnswerSaysNotToKeepIt(t *testing.T) {
	r := newRulesRig(t)
	if got := r.do("GET", rulesBase, incUsRead, nil); got.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", got.Header.Get("Cache-Control"))
	}
}
