package server

import (
	"bytes"
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

	"github.com/sojoner/kro-lab/platform-mvp/alert-router/dedupe"
	"github.com/sojoner/kro-lab/platform-mvp/alert-router/model"
)

const token = "s3cret-token"

type recordingSink struct {
	name string
	err  error
	mu   sync.Mutex
	got  []model.Alert
}

func (r *recordingSink) Name() string { return r.name }
func (r *recordingSink) Send(_ context.Context, a []model.Alert) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.got = append(r.got, a...)
	return nil
}
func (r *recordingSink) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.got) }

type harness struct {
	srv   *httptest.Server
	logs  *bytes.Buffer
	main  *recordingSink
	extra *recordingSink
}

func newHarness(t *testing.T, tokenFn func() (string, error)) *harness {
	t.Helper()
	h := &harness{logs: &bytes.Buffer{}, main: &recordingSink{name: "log"}, extra: &recordingSink{name: "agent"}}
	if tokenFn == nil {
		tokenFn = func() (string, error) { return token, nil }
	}
	s := &Server{
		Token:       tokenFn,
		Sinks:       []Registration{{Sink: h.main, Required: true}, {Sink: h.extra, Required: false}},
		Deduper:     dedupe.New(10 * time.Minute),
		SinkTimeout: time.Second,
		Log:         slog.New(slog.NewTextHandler(h.logs, nil)),
	}
	h.srv = httptest.NewServer(s.Handler())
	t.Cleanup(h.srv.Close)
	return h
}

func payload(fingerprints ...string) string {
	var alerts []string
	for _, fp := range fingerprints {
		alerts = append(alerts, `{"status":"firing","fingerprint":"`+fp+`","startsAt":"2026-10-28T10:00:00Z",`+
			`"labels":{"alertname":"VmSetNotReady","severity":"warning","tenant":"acme-corp","vmset":"web","region":"us"},"annotations":{}}`)
	}
	return `{"version":"4","status":"firing","alerts":[` + strings.Join(alerts, ",") + `]}`
}

func (h *harness) post(t *testing.T, body, auth string) (*http.Response, response) {
	t.Helper()
	req, _ := http.NewRequest("POST", h.srv.URL+"/alerts", strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out response
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &out)
	return resp, out
}

func TestRejectsMissingOrWrongToken(t *testing.T) {
	h := newHarness(t, nil)
	for name, auth := range map[string]string{"missing": "", "wrong": "Bearer nope", "wrong scheme": "Basic " + token} {
		resp, _ := h.post(t, payload("a"), auth)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, resp.StatusCode)
		}
	}
	if h.main.count() != 0 {
		t.Error("an unauthenticated request must not reach any sink")
	}
}

func TestFailsClosedWhenTokenIsUnreadable(t *testing.T) {
	for name, fn := range map[string]func() (string, error){
		"read error": func() (string, error) { return "", errors.New("no such file") },
		"empty":      func() (string, error) { return "", nil },
	} {
		h := newHarness(t, fn)
		resp, _ := h.post(t, payload("a"), "Bearer anything")
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503", name, resp.StatusCode)
		}
		if h.main.count() != 0 {
			t.Errorf("%s: nothing may be accepted without a token", name)
		}
	}
}

func TestDeliversToEverySinkAndCounts(t *testing.T) {
	h := newHarness(t, nil)
	resp, out := h.post(t, payload("a", "b"), "Bearer "+token)
	if resp.StatusCode != http.StatusOK || out != (response{Received: 2, Delivered: 2}) {
		t.Fatalf("status = %d, response = %+v", resp.StatusCode, out)
	}
	if h.main.count() != 2 || h.extra.count() != 2 {
		t.Errorf("sinks received %d and %d alerts, want 2 each", h.main.count(), h.extra.count())
	}
}

func TestDropsUnchangedRepeatsWithinTheInterval(t *testing.T) {
	h := newHarness(t, nil)
	h.post(t, payload("a"), "Bearer "+token)
	resp, out := h.post(t, payload("a", "b"), "Bearer "+token)
	if resp.StatusCode != http.StatusOK || out != (response{Received: 2, Deduped: 1, Delivered: 1}) {
		t.Errorf("status = %d, response = %+v; want a deduped and b delivered", resp.StatusCode, out)
	}
	if h.main.count() != 2 {
		t.Errorf("sink saw %d alerts in total, want a once and b once", h.main.count())
	}
}

func TestRequiredSinkFailureAsksForRetryAndTheRetrySucceeds(t *testing.T) {
	h := newHarness(t, nil)
	h.main.err = errors.New("disk full")
	resp, _ := h.post(t, payload("a"), "Bearer "+token)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 so Alertmanager retries", resp.StatusCode)
	}

	h.main.err = nil
	resp, out := h.post(t, payload("a"), "Bearer "+token)
	if resp.StatusCode != http.StatusOK || out.Delivered != 1 || out.Deduped != 0 {
		t.Errorf("retry: status = %d, response = %+v; the failed alert must not be dropped as a repeat", resp.StatusCode, out)
	}
}

func TestBestEffortSinkFailureDoesNotTriggerARetry(t *testing.T) {
	h := newHarness(t, nil)
	h.extra.err = errors.New("agent down")
	resp, out := h.post(t, payload("a"), "Bearer "+token)
	if resp.StatusCode != http.StatusOK || out.Delivered != 1 {
		t.Errorf("status = %d, response = %+v; an optional sink failing must not fail the request", resp.StatusCode, out)
	}
	if h.main.count() != 1 {
		t.Error("the required sink should still have received the alert")
	}
	if !strings.Contains(h.logs.String(), "sink failed") {
		t.Error("the failure should be logged")
	}
}

func TestRejectsBadPayloads(t *testing.T) {
	h := newHarness(t, nil)
	if resp, _ := h.post(t, `{"version":"3"}`, "Bearer "+token); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad payload: status = %d, want 400", resp.StatusCode)
	}
	huge := `{"version":"4","alerts":[],"pad":"` + strings.Repeat("x", maxBodyBytes) + `"}`
	if resp, _ := h.post(t, huge, "Bearer "+token); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized payload: status = %d, want 413", resp.StatusCode)
	}
}

func TestMetricsAndLogsNeverExposeTheToken(t *testing.T) {
	h := newHarness(t, nil)
	h.post(t, payload("a"), "Bearer "+token)
	h.post(t, payload("a"), "Bearer "+token) // deduped
	h.extra.err = errors.New("agent down")
	h.post(t, payload("z"), "Bearer "+token)

	resp, err := http.Get(h.srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	m := string(b)
	for _, want := range []string{
		"alert_router_alerts_received_total 3",
		"alert_router_alerts_deduped_total 1",
		"alert_router_alerts_delivered_total 2",
		`alert_router_sink_errors_total{sink="agent"} 1`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics missing %q:\n%s", want, m)
		}
	}
	if strings.Contains(h.logs.String(), token) {
		t.Errorf("access log leaked the token:\n%s", h.logs)
	}
}

func TestHealthzNeedsNoToken(t *testing.T) {
	h := newHarness(t, nil)
	resp, err := http.Get(h.srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}
