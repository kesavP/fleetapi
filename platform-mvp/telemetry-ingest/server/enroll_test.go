package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/enroll"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/project"
)

const (
	enrUsManage = "us-manage-key-000001"
	enrEuManage = "eu-manage-key-000001"
)

type enrollRig struct {
	srv *httptest.Server
	reg *project.Registry
	m   *enroll.Manager
	st  *enroll.MemStore
	log *bytes.Buffer
	lim *RateLimiter
}

func newEnrollRig(t *testing.T) *enrollRig {
	t.Helper()
	path := t.TempDir() + "/p.json"
	cfg := `{"projects":[
	 {"name":"us","ingestKeys":["` + usIngest + `"],"readKeys":["` + usRead + `"],"manageKeys":["` + enrUsManage + `"]},
	 {"name":"eu","ingestKeys":["eu-ingest-key-0000001"],"readKeys":["` + euRead + `"],"manageKeys":["` + enrEuManage + `"]}]}`
	if err := writeFile(path, cfg); err != nil {
		t.Fatal(err)
	}
	reg, err := project.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	st := enroll.NewMemStore()
	m := enroll.NewManager(st)
	m.OnKeys = func() {
		keys, _ := m.Keys(context.Background())
		issued := map[[32]byte]string{}
		for _, k := range keys {
			issued[k.Hash] = k.Project
		}
		reg.SetIssuedIngest(issued)
	}
	buf := &bytes.Buffer{}
	lim := &RateLimiter{PerAddress: 1000, Global: 10000, Window: time.Minute}
	mux := http.NewServeMux()
	(&Enroll{Projects: reg, Manager: m, Log: slog.New(slog.NewTextHandler(buf, nil)), Count: NewCounters(), Limiter: lim}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &enrollRig{srv: srv, reg: reg, m: m, st: st, log: buf, lim: lim}
}

// call makes a request; body is a string or a value to encode; headers pair up, "-" removing one.
func (g *enrollRig) call(t *testing.T, method, path, key string, body any, headers ...string) (int, string, http.Header) {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, g.srv.URL+path, rd)
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	if body != nil {
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
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw), res.Header
}

const enrollBase = "/api/v1/projects/us/enrollments"

func (g *enrollRig) make(t *testing.T, cluster string) (id, token string) {
	t.Helper()
	code, body, _ := g.call(t, "POST", enrollBase, enrUsManage, map[string]any{"cluster": cluster})
	if code != 201 {
		t.Fatalf("%d %s", code, body)
	}
	var out struct {
		Data struct{ ID, Token string }
	}
	_ = json.Unmarshal([]byte(body), &out)
	return out.Data.ID, out.Data.Token
}

func TestCreatingNeedsAManageKeyOfThatProject(t *testing.T) {
	g := newEnrollRig(t)
	for name, c := range map[string]struct {
		key  string
		path string
		want int
	}{
		"no key":                {"", enrollBase, 401},
		"a read key":            {usRead, enrollBase, 403},
		"an ingest key":         {usIngest, enrollBase, 403},
		"another project's key": {enrEuManage, enrollBase, 403},
		"an unknown key":        {"nope-nope-nope-nope-0", enrollBase, 403},
		"a project that is not": {enrUsManage, "/api/v1/projects/nosuch/enrollments", 403},
	} {
		if code, _, _ := g.call(t, "POST", c.path, c.key, map[string]any{"cluster": "gke"}); code != c.want {
			t.Errorf("%s: %d", name, code)
		}
	}
	if len(g.st.Events()) != 0 {
		t.Fatal("something was stored for a refused request")
	}
}

func TestCreatingGivesATokenOnceAndListingNeverDoes(t *testing.T) {
	g := newEnrollRig(t)
	code, body, h := g.call(t, "POST", enrollBase, enrUsManage, map[string]any{"cluster": "gke", "ttl_minutes": 30}, "X-Actor", "ana@example.com")
	if code != 201 || !strings.Contains(body, `"token":"enr_`) || !strings.Contains(body, `"status":"pending"`) || !strings.Contains(body, `"cluster":"gke"`) || !strings.Contains(body, `"created_by":"ana@example.com"`) {
		t.Fatalf("%d %s", code, body)
	}
	if h.Get("Cache-Control") != "no-store" {
		t.Fatalf("a token must not be cached: %q", h.Get("Cache-Control"))
	}
	var out struct{ Data struct{ Token string } }
	_ = json.Unmarshal([]byte(body), &out)
	for _, key := range []string{usRead, enrUsManage} {
		code, list, _ := g.call(t, "GET", enrollBase, key, nil)
		if code != 200 || !strings.Contains(list, `"cluster":"gke"`) || strings.Contains(list, out.Data.Token) || strings.Contains(list, `"token"`) {
			t.Fatalf("%d %s", code, list)
		}
	}
	// the lifetime asked for
	var e struct {
		Data struct {
			CreatedAt time.Time `json:"created_at"`
			ExpiresAt time.Time `json:"expires_at"`
		}
	}
	_ = json.Unmarshal([]byte(body), &e)
	if d := e.Data.ExpiresAt.Sub(e.Data.CreatedAt); d != 30*time.Minute {
		t.Fatalf("lifetime %v", d)
	}
}

func TestEnrollmentCreateRefusals(t *testing.T) {
	g := newEnrollRig(t)
	for name, c := range map[string]struct {
		body any
		want int
		frag string
	}{
		"no cluster":       {map[string]any{}, 400, "cluster"},
		"a bad cluster":    {map[string]any{"cluster": "My Cluster"}, 400, "cluster"},
		"an unknown field": {`{"cluster":"gke","admin":true}`, 400, "not valid"},
		"not JSON":         {`{nope`, 400, "not valid"},
		"empty":            {``, 400, "empty"},
		"a negative ttl":   {map[string]any{"cluster": "gke", "ttl_minutes": -1}, 400, "ttl_minutes"},
		"a ttl too long":   {map[string]any{"cluster": "gke", "ttl_minutes": 100000}, 400, "ttl_minutes"},
		"something after":  {`{"cluster":"gke"} {"cluster":"x"}`, 400, "after"},
	} {
		code, body, _ := g.call(t, "POST", enrollBase, enrUsManage, c.body)
		if name == "empty" { // an empty string body is sent as no body at all by call(): send it as one
			req, _ := http.NewRequest("POST", g.srv.URL+enrollBase, strings.NewReader(""))
			req.Header.Set("X-API-Key", enrUsManage)
			req.Header.Set("Content-Type", "application/json")
			res, _ := http.DefaultClient.Do(req)
			raw, _ := io.ReadAll(res.Body)
			res.Body.Close()
			code, body = res.StatusCode, string(raw)
		}
		if code != c.want || !strings.Contains(body, c.frag) {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	if code, _, _ := g.call(t, "POST", enrollBase, enrUsManage, map[string]any{"cluster": "gke"}, "Content-Type", "text/plain"); code != 415 {
		t.Errorf("a wrong content type: %d", code)
	}
	if code, _, _ := g.call(t, "POST", enrollBase, enrUsManage, map[string]any{"cluster": "gke"}, "X-Actor", "a;b"); code != 400 {
		t.Errorf("a bad actor: %d", code)
	}
	if code, _, _ := g.call(t, "POST", enrollBase, enrUsManage, `{"cluster":"`+strings.Repeat("a", 70<<10)+`"}`); code != 413 {
		t.Errorf("a body too large: %d", code)
	}
	if n := len(g.st.Events()); n != 0 {
		t.Fatalf("%d events stored for refused requests", n)
	}
	for i := 0; i < enroll.MaxPending; i++ {
		g.make(t, "c")
	}
	if code, body, _ := g.call(t, "POST", enrollBase, enrUsManage, map[string]any{"cluster": "c"}); code != 429 || !strings.Contains(body, "unredeemed") {
		t.Errorf("past the limit: %d %s", code, body)
	}
}

func TestRevoking(t *testing.T) {
	g := newEnrollRig(t)
	id, tok := g.make(t, "gke")
	for name, c := range map[string]struct {
		key  string
		path string
		want int
	}{
		"no key":          {"", enrollBase + "/" + id, 401},
		"a read key":      {usRead, enrollBase + "/" + id, 403},
		"another project": {enrEuManage, enrollBase + "/" + id, 403},
		"an unknown id":   {enrUsManage, enrollBase + "/000000000000", 404},
		"not an id":       {enrUsManage, enrollBase + "/..%2Fx", 404},
	} {
		if code, _, _ := g.call(t, "DELETE", c.path, c.key, nil); code != c.want {
			t.Errorf("%s: %d", name, code)
		}
	}
	// still good after the refusals
	if list, _ := g.m.List(context.Background(), "us"); list[0].Status != enroll.Pending {
		t.Fatalf("%+v", list[0])
	}
	code, body, _ := g.call(t, "DELETE", enrollBase+"/"+id, enrUsManage, nil, "X-Actor", "ana")
	if code != 200 || !strings.Contains(body, `"status":"revoked"`) || !strings.Contains(body, `"revoked_by":"ana"`) {
		t.Fatalf("%d %s", code, body)
	}
	if code, _, _ := g.call(t, "POST", "/v1/enroll", "", map[string]any{"token": tok}); code != 401 {
		t.Fatalf("a revoked token redeemed: %d", code)
	}
}

func redeemBody(tok string) map[string]any { return map[string]any{"token": tok} }

func TestRedeemingNeedsNoAPIKeyAndGivesAKeyThatWorksForIngestOnly(t *testing.T) {
	g := newEnrollRig(t)
	_, tok := g.make(t, "gke")
	code, body, h := g.call(t, "POST", "/v1/enroll", "", redeemBody(tok))
	if code != 200 || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %s %v", code, body, h)
	}
	var out struct {
		Data struct {
			Project, Cluster, Enrollment string
			IngestKey                    string `json:"ingest_key"`
		}
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Data.Project != "us" || out.Data.Cluster != "gke" || !strings.HasPrefix(out.Data.IngestKey, "ing_") || out.Data.Enrollment == "" {
		t.Fatalf("%s %v", body, err)
	}
	key := out.Data.IngestKey
	if p, ok := g.reg.IngestProject(key); !ok || p != "us" {
		t.Fatalf("the issued key is not accepted for ingest: %q %v", p, ok)
	}
	if _, ok := g.reg.ReadProject(key); ok {
		t.Fatal("an ingest key reads")
	}
	if _, ok := g.reg.ManageProject(key); ok {
		t.Fatal("an ingest key manages")
	}
	// the token is spent
	if code, _, _ := g.call(t, "POST", "/v1/enroll", "", redeemBody(tok)); code != 401 {
		t.Fatalf("a second redemption: %d", code)
	}
	// the list says so; revoking the enrollment cuts the key off
	list, _ := g.m.List(context.Background(), "us")
	if list[0].Status != enroll.Redeemed {
		t.Fatalf("%+v", list[0])
	}
	if code, _, _ := g.call(t, "DELETE", enrollBase+"/"+out.Data.Enrollment, enrUsManage, nil); code != 200 {
		t.Fatal(code)
	}
	if _, ok := g.reg.IngestProject(key); ok {
		t.Fatal("a revoked key is still accepted")
	}
	// the key the file holds was never touched
	if p, ok := g.reg.IngestProject(usIngest); !ok || p != "us" {
		t.Fatal("the file's own ingest key stopped working")
	}
}

func TestRedeemingAsAManifest(t *testing.T) {
	g := newEnrollRig(t)
	_, tok := g.make(t, "gke")
	code, body, h := g.call(t, "POST", "/v1/enroll?format=secret", "", redeemBody(tok))
	if code != 200 || !strings.HasPrefix(h.Get("Content-Type"), "application/x-yaml") {
		t.Fatalf("%d %v", code, h)
	}
	for _, want := range []string{"kind: Namespace", "name: telemetry-agent", "kind: Secret", "name: hub-telemetry-credentials", "namespace: telemetry-agent", "stringData:", `  apiKey: "ing_`, `enrolled-cluster: "gke"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the manifest lacks %q:\n%s", want, body)
		}
	}
	if strings.Count(body, "---") != 1 {
		t.Fatalf("a namespace and a secret are two documents:\n%s", body)
	}
	_, tok = g.make(t, "aks")
	_, body, _ = g.call(t, "POST", "/v1/enroll?format=secret", "", map[string]any{"token": tok, "namespace": "monitoring", "secret_name": "agent-key", "key_name": "api.key"})
	for _, want := range []string{"name: monitoring", "name: agent-key", `  api.key: "ing_`} {
		if !strings.Contains(body, want) {
			t.Errorf("custom names: %q missing:\n%s", want, body)
		}
	}
}

// A request that is wrong in a way that can be told before the token is looked at does not spend it.
func TestARequestThatIsWrongDoesNotSpendTheToken(t *testing.T) {
	g := newEnrollRig(t)
	_, tok := g.make(t, "gke")
	for name, body := range map[string]map[string]any{
		"a namespace with a newline":   {"token": tok, "namespace": "a\nkind: Secret"},
		"a namespace in capitals":      {"token": tok, "namespace": "Prod"},
		"a secret name with a slash":   {"token": tok, "secret_name": "a/b"},
		"a key name that is a path":    {"token": tok, "key_name": ".."},
		"a key name with a space":      {"token": tok, "key_name": "api key"},
		"a key name with YAML in it":   {"token": tok, "key_name": "k: v"},
		"a namespace that is too long": {"token": tok, "namespace": strings.Repeat("a", 64)},
		"a secret name with a quote":   {"token": tok, "secret_name": `a"b`},
	} {
		if code, _, _ := g.call(t, "POST", "/v1/enroll?format=secret", "", body); code != 400 {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code, _, _ := g.call(t, "POST", "/v1/enroll?format=xml", "", redeemBody(tok)); code != 400 {
		t.Errorf("an unknown format: %d", code)
	}
	if code, _, _ := g.call(t, "POST", "/v1/enroll", "", redeemBody(tok)); code != 200 {
		t.Fatalf("the token was spent by requests that were refused: %d", code)
	}
}

func TestEveryBadTokenGetsTheSameAnswer(t *testing.T) {
	g := newEnrollRig(t)
	_, used := g.make(t, "gke")
	g.call(t, "POST", "/v1/enroll", "", redeemBody(used))
	revokedID, revoked := g.make(t, "gke")
	g.call(t, "DELETE", enrollBase+"/"+revokedID, enrUsManage, nil)
	var first string
	for name, tok := range map[string]string{
		"used": used, "revoked": revoked, "unknown": "enr_" + strings.Repeat("A", 43), "garbage": "hello", "empty": "", "a key": usIngest,
	} {
		code, body, _ := g.call(t, "POST", "/v1/enroll", "", redeemBody(tok))
		if code != 401 {
			t.Errorf("%s: %d", name, code)
		}
		if first == "" {
			first = body
		} else if body != first {
			t.Errorf("%s: the answer differs, which tells the caller which it was:\n%s\n%s", name, first, body)
		}
	}
}

func TestRedeemRequestShape(t *testing.T) {
	g := newEnrollRig(t)
	_, tok := g.make(t, "gke")
	for name, c := range map[string]struct {
		body    any
		headers []string
		want    int
	}{
		"not JSON":        {`{nope`, nil, 400},
		"a field unknown": {`{"token":"x","admin":true}`, nil, 400},
		"a wrong type":    {redeemBody(tok), []string{"Content-Type", "text/plain"}, 415},
		"too large":       {`{"token":"` + strings.Repeat("x", 5<<10) + `"}`, nil, 413},
	} {
		if code, _, _ := g.call(t, "POST", "/v1/enroll", "", c.body, c.headers...); code != c.want {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code, _, _ := g.call(t, "GET", "/v1/enroll", "", nil); code != 405 {
		t.Errorf("a GET: %d", code)
	}
	// an API key that is sent is not what authenticates this, and does not matter
	if code, _, _ := g.call(t, "POST", "/v1/enroll", "wrong-key-wrong-key-00", redeemBody(tok)); code != 200 {
		t.Errorf("with an unrelated API key header: %d", code)
	}
}

func TestGuessingIsLimited(t *testing.T) {
	g := newEnrollRig(t)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	g.lim.Now = func() time.Time { return now }
	g.lim.PerAddress, g.lim.Global = 5, 8
	bad := redeemBody("enr_" + strings.Repeat("A", 43))
	for i := 0; i < 5; i++ {
		if code, _, _ := g.call(t, "POST", "/v1/enroll", "", bad); code != 401 {
			t.Fatalf("attempt %d: %d", i, code)
		}
	}
	code, _, h := g.call(t, "POST", "/v1/enroll", "", bad)
	if code != 429 || h.Get("Retry-After") == "" {
		t.Fatalf("past the limit: %d %v", code, h)
	}
	// a good token is limited as well: the limit comes before anything is looked at
	_, tok := g.make(t, "gke")
	if code, _, _ := g.call(t, "POST", "/v1/enroll", "", redeemBody(tok)); code != 429 {
		t.Fatalf("a good token past the limit: %d", code)
	}
	// the window passes
	now = now.Add(time.Minute + time.Second)
	if code, _, _ := g.call(t, "POST", "/v1/enroll", "", redeemBody(tok)); code != 200 {
		t.Fatalf("after the window: %d", code)
	}
}

func TestTheRateLimiter(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	l := &RateLimiter{PerAddress: 2, Global: 3, Window: time.Minute, Now: func() time.Time { return now }}
	for i, want := range []bool{true, true, false} {
		if ok, _ := l.Allow("a"); ok != want {
			t.Fatalf("a #%d: %v", i, ok)
		}
	}
	if ok, _ := l.Allow("b"); !ok { // the fourth of all is over the global limit of three
		// 3 attempts so far (a,a,a) count; b makes the fourth
		t.Log("b is within its own limit but the global one is reached")
	} else {
		t.Fatal("the global limit was not applied")
	}
	if _, wait := l.Allow("c"); wait <= 0 || wait > time.Minute {
		t.Fatalf("wait %v", wait)
	}
	now = now.Add(time.Minute)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("a new window starts clean")
	}
}

func TestNeitherTheTokenNorTheKeyIsLogged(t *testing.T) {
	g := newEnrollRig(t)
	_, tok := g.make(t, "gke")
	refused := "enr_" + strings.Repeat("B", 43)
	g.call(t, "POST", "/v1/enroll", "", redeemBody(refused))
	_, body, _ := g.call(t, "POST", "/v1/enroll", "", redeemBody(tok))
	key := strings.Split(strings.Split(body, `"ingest_key":"`)[1], `"`)[0]
	logs := g.log.String()
	for _, secret := range []string{tok, strings.TrimPrefix(tok, "enr_"), refused, strings.Repeat("B", 43), key, strings.TrimPrefix(key, "ing_"), enrUsManage} {
		if strings.Contains(logs, secret) {
			t.Fatalf("a secret is in the log:\n%s", logs)
		}
	}
	if !strings.Contains(logs, "a cluster enrolled") || !strings.Contains(logs, "an enrollment token was refused") {
		t.Fatalf("what happened is not logged:\n%s", logs)
	}
}

func TestAStorageFailureIsAPlainErrorAndNotAnInvalidToken(t *testing.T) {
	g := newEnrollRig(t)
	_, tok := g.make(t, "gke")
	g.st.SetFailing(true)
	code, body, _ := g.call(t, "POST", "/v1/enroll", "", redeemBody(tok))
	if code != 500 || strings.Contains(body, "disk") {
		t.Fatalf("%d %s", code, body)
	}
	g.st.SetFailing(false)
	if code, _, _ := g.call(t, "POST", "/v1/enroll", "", redeemBody(tok)); code != 200 {
		t.Fatalf("the token was spent by a failure: %d", code)
	}
}
