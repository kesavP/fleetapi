package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/analysis"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/incident"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/project"
)

// fakeAnalyst stands for Claude: it records what it was asked about and answers as the test says.
type fakeAnalyst struct {
	mu    sync.Mutex
	asked []incident.Incident
	out   string
	err   error
	gate  chan struct{} // when set, a call waits for it to be closed
	enter chan struct{} // when set, a call says it has begun
}

func (f *fakeAnalyst) Analyse(ctx context.Context, inc incident.Incident) (string, error) {
	f.mu.Lock()
	f.asked = append(f.asked, inc)
	gate, enter, out, err := f.gate, f.enter, f.out, f.err
	f.mu.Unlock()
	if enter != nil {
		enter <- struct{}{}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return out, err
}

func (f *fakeAnalyst) calls() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.asked) }

type anRig struct {
	*incRig
	now *time.Time
	mu  sync.Mutex
}

func newAnRig(t *testing.T, a Analyst) *anRig {
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
	start := time.Now()
	rg := &anRig{now: &start}
	mux := http.NewServeMux()
	(&Incidents{
		Projects: reg, Manager: incident.NewManager(mem, log), Log: log, Analyst: a,
		now: func() time.Time { rg.mu.Lock(); defer rg.mu.Unlock(); return *rg.now },
	}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	rg.incRig = &incRig{t: t, srv: srv, mem: mem}
	return rg
}

func (r *anRig) advance(d time.Duration) { r.mu.Lock(); defer r.mu.Unlock(); *r.now = r.now.Add(d) }

func (r *anRig) analyse(id, key string, headers ...string) resp {
	r.t.Helper()
	return r.do("POST", incBase+"/"+id+"/analysis", key, nil, headers...)
}

func events(t *testing.T, r *incRig, id string) []map[string]any {
	t.Helper()
	got := r.do("GET", incBase+"/"+id+"/events", incUsRead, nil)
	var out []map[string]any
	for _, e := range got.Body["data"].([]any) {
		out = append(out, e.(map[string]any))
	}
	return out
}

func TestAnAnalysisIsKeptAsACommentByClaude(t *testing.T) {
	f := &fakeAnalyst{out: "Analysis by Claude (m)...\n\nLikely cause: Mongo is scaled to zero."}
	r := newAnRig(t, f)
	inc := r.create(map[string]any{"title": "db/mongo: connections failing", "app": "/k8s/db/mongo", "severity": "critical", "summary": "all failed"})
	id := inc["id"].(string)

	got := r.analyse(id, incUsManage, "X-Actor", "sam")
	if got.Status != 201 {
		t.Fatalf("%d %s", got.Status, got.Raw)
	}
	if f.calls() != 1 || f.asked[0].ID != id || f.asked[0].Title != "db/mongo: connections failing" || f.asked[0].Summary != "all failed" {
		t.Fatalf("Claude was asked about %+v", f.asked)
	}
	evs := events(t, r.incRig, id)
	last := evs[len(evs)-1]
	if last["type"] != "commented" || last["actor"] != analysis.Actor {
		t.Fatalf("the last event = %v: the comment is Claude's, not whoever asked", last)
	}
	if d, _ := last["data"].(map[string]any); d["text"] != f.out {
		t.Fatalf("comment = %v", last["data"])
	}
	data := got.Body["data"].(map[string]any)
	if data["id"] != id || data["version"].(float64) != inc["version"].(float64)+1 {
		t.Fatalf("the answer is the incident with the comment on it: %v", data)
	}
}

func TestOnlyAManageKeyOfTheProjectMayAskForAnAnalysis(t *testing.T) {
	f := &fakeAnalyst{out: "x"}
	r := newAnRig(t, f)
	id := r.create(nil)["id"].(string)
	for name, c := range map[string]struct {
		key  string
		want int
	}{"no key": {"", 401}, "a read key": {incUsRead, 403}, "an ingest key": {incUsIngest, 403}, "another project's manage key": {incEuManage, 403}} {
		if got := r.analyse(id, c.key); got.Status != c.want {
			t.Errorf("%s: %d %s", name, got.Status, got.Raw)
		}
	}
	if f.calls() != 0 {
		t.Fatalf("Claude was asked %d times without a manage key: it costs money", f.calls())
	}
}

func TestWithoutAnAnalystTheServiceSaysItIsNotSetUp(t *testing.T) {
	r := newAnRig(t, nil)
	id := r.create(nil)["id"].(string)
	got := r.analyse(id, incUsManage)
	if got.Status != 501 || !strings.Contains(got.Raw, "not set up") {
		t.Fatalf("%d %s", got.Status, got.Raw)
	}
}

func TestAnIncidentThatDoesNotExistIsNotFoundAndCostsNothing(t *testing.T) {
	f := &fakeAnalyst{out: "x"}
	r := newAnRig(t, f)
	if got := r.analyse("abcdefgh", incUsManage); got.Status != 404 {
		t.Fatalf("%d %s", got.Status, got.Raw)
	}
	if f.calls() != 0 {
		t.Fatal("Claude was asked about an incident that does not exist")
	}
}

func TestAskingAgainTooSoonIsRefusedAndLaterIsAllowed(t *testing.T) {
	f := &fakeAnalyst{out: "x"}
	r := newAnRig(t, f)
	id := r.create(nil)["id"].(string)
	if got := r.analyse(id, incUsManage); got.Status != 201 {
		t.Fatalf("first: %d %s", got.Status, got.Raw)
	}
	got := r.analyse(id, incUsManage)
	if got.Status != 429 || got.Header.Get("Retry-After") == "" {
		t.Fatalf("again at once: %d %v %s", got.Status, got.Header, got.Raw)
	}
	if f.calls() != 1 {
		t.Fatalf("Claude was asked %d times", f.calls())
	}
	r.advance(analysisEvery + time.Second)
	if got := r.analyse(id, incUsManage); got.Status != 201 {
		t.Fatalf("after the wait: %d %s", got.Status, got.Raw)
	}
	// another incident is not held up by this one
	other := r.create(map[string]any{"title": "another"})["id"].(string)
	if got := r.analyse(other, incUsManage); got.Status != 201 {
		t.Fatalf("another incident: %d %s", got.Status, got.Raw)
	}
}

func TestAFailureSaysWhatKindAndLeavesNoCommentAndNoWait(t *testing.T) {
	cases := map[string]struct {
		err    error
		status int
		say    string
	}{
		"the key was refused":  {analysis.ErrKeyRefused, 502, "refused the key"},
		"rate limited":         {analysis.ErrBusy, 429, "rate limiting"},
		"unavailable":          {fmt.Errorf("%w (status 503)", analysis.ErrUnavailable), 502, "could not be reached"},
		"the model declined":   {analysis.ErrDeclined, 502, "declined"},
		"an answer cut off":    {analysis.ErrCutOff, 502, "cut off"},
		"something unexpected": {errors.New("secret internal detail sk-ant-LEAK"), 502, "the analysis failed"},
	}
	for name, c := range cases {
		f := &fakeAnalyst{err: c.err}
		r := newAnRig(t, f)
		id := r.create(nil)["id"].(string)
		got := r.analyse(id, incUsManage)
		if got.Status != c.status || !strings.Contains(got.Raw, c.say) {
			t.Errorf("%s: %d %s", name, got.Status, got.Raw)
		}
		if strings.Contains(got.Raw, "sk-ant-LEAK") || strings.Contains(got.Raw, "internal detail") {
			t.Errorf("%s: the answer repeats what went wrong inside: %s", name, got.Raw)
		}
		if c.status == 429 && got.Header.Get("Retry-After") == "" {
			t.Errorf("%s: no Retry-After", name)
		}
		if evs := events(t, r.incRig, id); len(evs) != 1 {
			t.Errorf("%s: %d events: a failed analysis must not leave a comment", name, len(evs))
		}
		// a failure is not an analysis: asking again at once is allowed
		f.mu.Lock()
		f.err, f.out = nil, "now it works"
		f.mu.Unlock()
		if again := r.analyse(id, incUsManage); again.Status != 201 {
			t.Errorf("%s: asking again after a failure: %d %s", name, again.Status, again.Raw)
		}
	}
}

func TestNoMoreAnalysesRunAtOnceThanTheLimit(t *testing.T) {
	f := &fakeAnalyst{out: "x", gate: make(chan struct{}), enter: make(chan struct{}, 16)}
	r := newAnRig(t, f)
	ids := make([]string, maxAnalyses+1)
	for i := range ids {
		ids[i] = r.create(map[string]any{"title": fmt.Sprintf("incident %d", i)})["id"].(string)
	}
	var wg sync.WaitGroup
	statuses := make([]int, maxAnalyses)
	for i := 0; i < maxAnalyses; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = r.analyse(ids[i], incUsManage).Status
		}(i)
	}
	for i := 0; i < maxAnalyses; i++ {
		<-f.enter // all of them are running
	}
	over := r.analyse(ids[maxAnalyses], incUsManage)
	if over.Status != 429 || over.Header.Get("Retry-After") == "" {
		t.Fatalf("one over the limit: %d %s", over.Status, over.Raw)
	}
	close(f.gate)
	wg.Wait()
	for i, s := range statuses {
		if s != 201 {
			t.Errorf("analysis %d: %d", i, s)
		}
	}
	if got := r.analyse(ids[maxAnalyses], incUsManage); got.Status != 201 {
		t.Fatalf("once the others are done: %d %s", got.Status, got.Raw)
	}
}
