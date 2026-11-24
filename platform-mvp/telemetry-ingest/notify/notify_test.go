package notify

import (
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
	"unicode/utf8"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/incident"
)

var (
	bg  = context.Background()
	now = time.Date(2026, 11, 17, 14, 30, 0, 0, time.UTC)
)

// amStub is Alertmanager as far as the notifier knows it: it keeps what was posted to /api/v2/alerts.
type amStub struct {
	srv    *httptest.Server
	mu     sync.Mutex
	posts  [][]map[string]any
	paths  []string
	ctype  []string
	status int
}

func newStub(t *testing.T) *amStub {
	t.Helper()
	s := &amStub{status: http.StatusOK}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body []map[string]any
		_ = json.Unmarshal(raw, &body)
		s.mu.Lock()
		s.posts = append(s.posts, body)
		s.paths = append(s.paths, r.Method+" "+r.URL.Path)
		s.ctype = append(s.ctype, r.Header.Get("Content-Type"))
		st := s.status
		s.mu.Unlock()
		w.WriteHeader(st)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *amStub) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.posts) }

func (s *amStub) only(t *testing.T) map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.posts) != 1 || len(s.posts[0]) != 1 {
		t.Fatalf("posts = %+v", s.posts)
	}
	return s.posts[0][0]
}

func newNotifier(s *amStub) *Alertmanager {
	return &Alertmanager{URL: s.srv.URL, MinSeverity: incident.Warning, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }}
}

func inc(over func(*incident.Incident)) incident.Incident {
	i := incident.Incident{
		ID: "vcxxe33p", Title: "db/mongo: connections failing", App: "/k8s/db/mongo", Severity: incident.Critical, State: incident.Open, Source: incident.Detected,
		DedupeKey: "deps:abc123:1794806580", Summary: "Opened automatically. Connections to db/mongo have been failing.\n1800 of 1800 connection attempts failed (100%).",
		StartedAt: now.Add(-12 * time.Minute), CreatedAt: now,
	}
	if over != nil {
		over(&i)
	}
	return i
}

func labels(a map[string]any) map[string]any      { return a["labels"].(map[string]any) }
func annotations(a map[string]any) map[string]any { return a["annotations"].(map[string]any) }

func TestAnOpenedIncidentIsOneAlertWithWhatAPersonNeedsToKnow(t *testing.T) {
	s := newStub(t)
	n := newNotifier(s)
	n.UIURL = "http://ui.example/"
	n.Opened(bg, "us", inc(nil))
	a := s.only(t)
	want := map[string]string{"alertname": "IncidentOpened", "source": "incident", "severity": "critical", "project": "us", "app": "/k8s/db/mongo", "incident": "vcxxe33p", "kind": "dependency"}
	for k, v := range want {
		if labels(a)[k] != v {
			t.Errorf("label %s = %v, want %v", k, labels(a)[k], v)
		}
	}
	if annotations(a)["summary"] != "db/mongo: connections failing" || !strings.Contains(annotations(a)["description"].(string), "1800 of 1800") {
		t.Fatalf("annotations = %v", annotations(a))
	}
	if a["generatorURL"] != "http://ui.example/recorded/vcxxe33p" {
		t.Fatalf("generatorURL = %v: a trailing slash on the address must not double", a["generatorURL"])
	}
	if s.paths[0] != "POST /api/v2/alerts" || s.ctype[0] != "application/json" {
		t.Fatalf("%v %v", s.paths, s.ctype)
	}
}

func TestTheTimesAreWhenItBeganAndAnHourAheadSoItFiresOnceAndExpires(t *testing.T) {
	s := newStub(t)
	newNotifier(s).Opened(bg, "us", inc(nil))
	a := s.only(t)
	if a["startsAt"] != now.Add(-12*time.Minute).Format(time.RFC3339) {
		t.Fatalf("startsAt = %v", a["startsAt"])
	}
	if a["endsAt"] != now.Add(time.Hour).Format(time.RFC3339) {
		t.Fatalf("endsAt = %v: nothing here resolves an incident, so it expires an hour on", a["endsAt"])
	}
}

func TestOnlyTheSeveritiesAskedForAreSent(t *testing.T) {
	cases := []struct {
		min  incident.Severity
		sev  incident.Severity
		sent bool
	}{
		{incident.Warning, incident.Critical, true},
		{incident.Warning, incident.Warning, true},
		{incident.Warning, incident.Info, false}, // right-sizing and the like: information, and never worth a message
		{incident.Critical, incident.Warning, false},
		{incident.Critical, incident.Critical, true},
		{incident.Info, incident.Info, true},
		{"", incident.Info, false}, // not set: the default, which is warning
		{"", incident.Warning, true},
	}
	for _, c := range cases {
		s := newStub(t)
		n := newNotifier(s)
		n.MinSeverity = c.min
		n.Opened(bg, "us", inc(func(i *incident.Incident) { i.Severity = c.sev }))
		if got := s.count() == 1; got != c.sent {
			t.Errorf("min %q, severity %s: sent = %v, want %v", c.min, c.sev, got, c.sent)
		}
	}
}

func TestAnIncidentAPersonOpenedIsNotAnnouncedByTheDetector(t *testing.T) {
	s := newStub(t)
	newNotifier(s).Opened(bg, "us", inc(func(i *incident.Incident) { i.Source = incident.Manual }))
	if s.count() != 0 {
		t.Fatal("a manual incident was announced")
	}
}

func TestTheKindSaysWhichDetectorOpenedIt(t *testing.T) {
	for key, want := range map[string]string{"detect:abc:1": "burn", "silence:abc:1": "silence", "deps:abc:1": "dependency", "rightsize:memory:abc:1": "rightsize", "": "burn", "other": "burn"} {
		s := newStub(t)
		newNotifier(s).Opened(bg, "us", inc(func(i *incident.Incident) { i.DedupeKey = key }))
		if got := labels(s.only(t))["kind"]; got != want {
			t.Errorf("key %q: kind %v, want %v", key, got, want)
		}
	}
}

func TestALongSummaryIsCutAtACharacterAndKeepsItsBeginning(t *testing.T) {
	s := newStub(t)
	long := strings.Repeat("é", 3000)
	newNotifier(s).Opened(bg, "us", inc(func(i *incident.Incident) { i.Summary = long }))
	d := annotations(s.only(t))["description"].(string)
	if n := utf8.RuneCountInString(d); n > 800 || !strings.HasSuffix(d, "…") || !utf8.ValidString(d) {
		t.Fatalf("description is %d characters: %q", n, d[:20])
	}
}

func TestTheLinkIsLeftOutWhenThereIsNoUsableAddress(t *testing.T) {
	for _, ui := range []string{"", "ftp://ui.example", "javascript:alert(1)", "ui.example", "http://", "http://ui example"} {
		s := newStub(t)
		n := newNotifier(s)
		n.UIURL = ui
		n.Opened(bg, "us", inc(nil))
		if g, has := s.only(t)["generatorURL"]; has {
			t.Errorf("ui address %q gave a link: %v", ui, g)
		}
	}
	s := newStub(t)
	n := newNotifier(s)
	n.UIURL = "https://ui.example:8443/base/"
	n.Opened(bg, "us", inc(nil))
	if g := s.only(t)["generatorURL"]; g != "https://ui.example:8443/base/recorded/vcxxe33p" {
		t.Fatalf("link = %v", g)
	}
}

func TestAnAlertmanagerThatFailsIsLoggedAndNeverStopsAnything(t *testing.T) {
	s := newStub(t)
	s.status = 500
	newNotifier(s).Opened(bg, "us", inc(nil)) // must return
	dead := newStub(t)
	n := newNotifier(dead)
	dead.srv.Close()
	n.Opened(bg, "us", inc(nil))
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(2 * time.Second) }))
	defer slow.Close()
	n2 := newNotifier(s)
	n2.URL = slow.URL
	n2.Timeout = 100 * time.Millisecond
	start := time.Now()
	n2.Opened(bg, "us", inc(nil))
	if time.Since(start) > time.Second {
		t.Fatalf("a slow Alertmanager held it for %v", time.Since(start))
	}
	var log strings.Builder
	n3 := newNotifier(s)
	n3.Log = slog.New(slog.NewTextHandler(&log, nil))
	n3.Opened(bg, "us", inc(nil))
	if !strings.Contains(log.String(), "500") {
		t.Fatalf("the failure was not logged: %q", log.String())
	}
}

func TestNothingIsSentWithoutAnAddress(t *testing.T) {
	n := &Alertmanager{MinSeverity: incident.Warning, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	n.Opened(bg, "us", inc(nil)) // must not panic or try
}

// ---- wrapping the detector's incidents ----

type fakeInner struct {
	created bool
	err     error
	made    []incident.CreateInput
}

func (f *fakeInner) Create(_ context.Context, project, actor string, in incident.CreateInput) (incident.Incident, bool, error) {
	f.made = append(f.made, in)
	return incident.Incident{ID: "abcd1234", Title: in.Title, App: in.App, Severity: in.Severity, Source: in.Source, DedupeKey: in.DedupeKey, Summary: in.Summary, StartedAt: in.StartedAt}, f.created, f.err
}
func (f *fakeInner) List(context.Context, string, incident.Filter) ([]incident.Incident, int, error) {
	return []incident.Incident{{ID: "listed"}}, 0, nil
}
func (f *fakeInner) AddEvidence(context.Context, string, string, string, *uint32, incident.EvidenceRef) (incident.Incident, incident.Evidence, bool, error) {
	return incident.Incident{ID: "evidenced"}, incident.Evidence{}, true, nil
}

func TestANewIncidentIsAnnouncedAndOneThatAlreadyExistedIsNot(t *testing.T) {
	s := newStub(t)
	in := &fakeInner{created: true}
	w := Wrap(in, newNotifier(s))
	got, created, err := w.Create(bg, "us", "detector", incident.CreateInput{Title: "db/mongo: connections failing", App: "/k8s/db/mongo", Severity: incident.Critical, Source: incident.Detected, StartedAt: now})
	if err != nil || !created || got.ID != "abcd1234" {
		t.Fatalf("the incident was not passed through: %+v %v %v", got, created, err)
	}
	w.Wait()
	if labels(s.only(t))["incident"] != "abcd1234" {
		t.Fatalf("posted = %+v", s.posts)
	}

	again := newStub(t)
	in2 := &fakeInner{created: false} // the same incident found again: the dedupe key had one
	w2 := Wrap(in2, newNotifier(again))
	w2.Create(bg, "us", "detector", incident.CreateInput{Title: "t", Severity: incident.Critical, Source: incident.Detected, StartedAt: now})
	w2.Wait()
	if again.count() != 0 {
		t.Fatal("an incident that already existed was announced again")
	}
}

func TestAFailedCreateIsNotAnnouncedAndTheErrorIsPassedOn(t *testing.T) {
	s := newStub(t)
	boom := errors.New("disk full")
	w := Wrap(&fakeInner{created: true, err: boom}, newNotifier(s))
	if _, _, err := w.Create(bg, "us", "detector", incident.CreateInput{Title: "t", Severity: incident.Critical, Source: incident.Detected, StartedAt: now}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	w.Wait()
	if s.count() != 0 {
		t.Fatal("an incident that was not created was announced")
	}
}

func TestEverythingElseGoesThroughUntouched(t *testing.T) {
	w := Wrap(&fakeInner{}, newNotifier(newStub(t)))
	if got, _, err := w.List(bg, "us", incident.Filter{}); err != nil || len(got) != 1 || got[0].ID != "listed" {
		t.Fatalf("List: %+v %v", got, err)
	}
	if got, _, _, err := w.AddEvidence(bg, "us", "x", "detector", nil, incident.EvidenceRef{}); err != nil || got.ID != "evidenced" {
		t.Fatalf("AddEvidence: %+v %v", got, err)
	}
}

func TestCreatingDoesNotWaitForAlertmanager(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer slow.Close()
	defer close(release)
	n := newNotifier(newStub(t))
	n.URL = slow.URL
	n.Timeout = 5 * time.Second
	w := Wrap(&fakeInner{created: true}, n)
	start := time.Now()
	w.Create(bg, "us", "detector", incident.CreateInput{Title: "t", Severity: incident.Critical, Source: incident.Detected, StartedAt: now})
	if time.Since(start) > time.Second {
		t.Fatalf("Create waited %v for Alertmanager: the detector would stall behind it", time.Since(start))
	}
}
