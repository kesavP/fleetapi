package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/summary"
)

type fakeSources struct {
	kubeErr   error
	asked     []string
	rolloutAt time.Time
}

func (f *fakeSources) SummaryKubeEvents(_ context.Context, project, namespace string, from, to time.Time) ([]summary.KubeEvent, error) {
	f.asked = append(f.asked, "kube "+project+" "+namespace)
	return nil, f.kubeErr
}

func (f *fakeSources) SummaryRollouts(_ context.Context, project, namespace, name string, from, to time.Time) ([]summary.Rollout, error) {
	f.asked = append(f.asked, "rollouts "+project+" "+namespace+"/"+name)
	return []summary.Rollout{{Kind: "deployment", Namespace: namespace, Name: name, At: f.rolloutAt, Generation: 3, Before: []string{"a:1"}, After: []string{"a:2"}}}, nil
}

func (f *fakeSources) SummaryProcesses(_ context.Context, project, service string, from, to time.Time) ([]summary.ProcessEvent, error) {
	f.asked = append(f.asked, "processes "+project+" "+service)
	return nil, nil
}

func TestIncidentSummary(t *testing.T) {
	src := &fakeSources{kubeErr: errors.New("clickhouse down")}
	r := newIncRig(t, func(h *Incidents) { h.Summaries = src })
	src.rolloutAt = time.Now().UTC().Add(-3 * time.Minute)
	inc := r.create(map[string]any{"app": "/k8s/shop/checkout", "severity": "critical"})
	id := inc["id"].(string)

	got := r.do("GET", incBase+"/"+id+"/summary", incUsRead, nil)
	if got.Status != 200 {
		t.Fatalf("%d %s", got.Status, got.Raw)
	}
	data := got.Body["data"].(map[string]any)
	if h := data["headline"].(string); !strings.Contains(h, "Most likely related: deployment shop/checkout rolled out") {
		t.Fatalf("headline %q", h)
	}
	gaps := data["gaps"].([]any)
	if len(gaps) != 2 || gaps[0] != "Kubernetes events: could not be read just now" {
		t.Fatalf("a failing source is a gap, not an error: %v", gaps)
	}
	want := "kube us shop|rollouts us shop/checkout|processes us /k8s/shop/checkout"
	if strings.Join(src.asked, "|") != want {
		t.Fatalf("asked %v", src.asked)
	}
	if strings.Contains(got.Raw, "clickhouse") {
		t.Fatal("an internal error must not reach the caller")
	}

	if got := r.do("GET", incBase+"/"+id+"/summary", incEuRead, nil); got.Status != 403 {
		t.Fatalf("another project's key: %d", got.Status)
	}
	if got := r.do("GET", incBase+"/nosuch01/summary", incUsRead, nil); got.Status != 404 {
		t.Fatalf("missing incident: %d %s", got.Status, got.Raw)
	}
}

func TestIncidentSummaryWithoutSources(t *testing.T) {
	r := newIncRig(t)
	id := r.create(map[string]any{"app": "/k8s/shop/checkout"})["id"].(string)
	got := r.do("GET", incBase+"/"+id+"/summary", incUsRead, nil)
	if got.Status != 200 || !strings.Contains(got.Raw, "not available on this service") {
		t.Fatalf("%d %s", got.Status, got.Raw)
	}
}

func TestIncidentStats(t *testing.T) {
	r := newIncRig(t)
	id := r.create(map[string]any{"severity": "warning"})["id"].(string)
	r.step(id, map[string]any{"to": "resolved", "resolution": "fixed"}, 200)
	got := r.do("GET", incBase+"/stats?days=30", incUsRead, nil)
	if got.Status != 200 {
		t.Fatalf("%d %s", got.Status, got.Raw)
	}
	data := got.Body["data"].(map[string]any)
	if data["opened"].(float64) != 1 || data["resolved"].(float64) != 1 || data["mttr"].(map[string]any)["count"].(float64) != 1 {
		t.Fatalf("%s", got.Raw)
	}
	for _, q := range []string{"?days=0", "?days=91", "?days=x"} {
		if got := r.do("GET", incBase+"/stats"+q, incUsRead, nil); got.Status != 400 {
			t.Errorf("%s: %d", q, got.Status)
		}
	}
	if got := r.do("GET", incBase+"/stats", "", nil); got.Status != 401 {
		t.Fatalf("no key: %d", got.Status)
	}
}
