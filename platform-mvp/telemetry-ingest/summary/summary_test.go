package summary

import (
	"strings"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/incident"
)

var start = time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

func at(min int) time.Time { return start.Add(time.Duration(min) * time.Minute) }

func openIncident() incident.Incident {
	inc := incident.Incident{
		ID: "abcd1234", Title: "checkout errors", App: "/k8s/shop/checkout", Severity: incident.Critical, State: incident.Open,
		Source: incident.Detected, StartedAt: start, CreatedAt: at(2),
	}
	inc.Timing = incident.TimingOf(inc)
	return inc
}

func read() Source { return Source{Read: true} }

func TestWorkload(t *testing.T) {
	for app, want := range map[string][2]string{
		"/k8s/shop/checkout": {"shop", "checkout"}, "/k8s-cronjob/ops/backup": {"ops", "backup"},
	} {
		ns, name, ok := Workload(app)
		if !ok || ns != want[0] || name != want[1] {
			t.Errorf("Workload(%q) = %q %q %v", app, ns, name, ok)
		}
	}
	for _, app := range []string{"", "/system.slice/nginx.service", "/k8s/shop", "/k8s/shop/checkout/extra", "/k8s//x"} {
		if _, _, ok := Workload(app); ok {
			t.Errorf("Workload(%q) should not be a workload", app)
		}
	}
}

func TestWindow(t *testing.T) {
	inc := openIncident()
	from, to := Window(inc, at(60))
	if !from.Equal(at(-30)) || !to.Equal(at(65)) {
		t.Fatalf("open: %v %v", from, to)
	}
	r := at(20)
	inc.ResolvedAt = &r
	if _, to := Window(inc, at(600)); !to.Equal(at(25)) {
		t.Fatalf("resolved: to %v", to)
	}
	inc.ResolvedAt = nil
	if _, to := Window(inc, start.Add(72*time.Hour)); !to.Equal(start.Add(MaxSpan + After)) {
		t.Fatalf("long: to %v", to)
	}
}

func TestARolloutBeforeTheStartLeads(t *testing.T) {
	s := Build(Input{
		Incident: openIncident(), Now: at(42),
		Rollouts:      []Rollout{{Kind: "deployment", Namespace: "shop", Name: "checkout", At: at(-4), Generation: 7, Before: []string{"shop/checkout:1.4"}, After: []string{"shop/checkout:1.5"}}},
		RolloutSource: read(),
		Kube: []KubeEvent{
			{Namespace: "shop", Kind: "Pod", Name: "checkout-7d9f8-x2bq4", Type: "Warning", Reason: "BackOff", Message: "Back-off restarting failed\ncontainer", Count: 5, First: at(1), Last: at(30)},
			{Namespace: "shop", Kind: "Pod", Name: "checkout-7d9f8-k8m2n", Type: "Warning", Reason: "BackOff", Message: "Back-off restarting failed container", Count: 7, First: at(2), Last: at(30)},
			{Namespace: "shop", Kind: "Pod", Name: "cart-5c6b7-k8m2n", Type: "Warning", Reason: "OOMKilling", Count: 1, First: at(1), Last: at(1)},  // another workload
			{Namespace: "web", Kind: "Pod", Name: "checkout-1-a", Type: "Warning", Reason: "Unhealthy", Count: 1, First: at(1), Last: at(1)},        // another namespace
			{Namespace: "shop", Kind: "Pod", Name: "checkout-7d9f8-x2bq4", Type: "Normal", Reason: "Pulled", Count: 1, First: at(-3), Last: at(-3)}, // context only
		},
		KubeSource:    read(),
		ProcessSource: read(),
	})
	if len(s.Findings) != 2 || s.Findings[0].Kind != "rollout" || s.Findings[1].Kind != "kubernetes" {
		t.Fatalf("findings %+v", s.Findings)
	}
	if want := "deployment shop/checkout rolled out 4 min before the trouble began (image shop/checkout:1.4 → shop/checkout:1.5)"; s.Findings[0].Text != want {
		t.Fatalf("rollout text %q", s.Findings[0].Text)
	}
	if want := "BackOff ×12 on pod checkout-7d9f8-k8m2n, pod checkout-7d9f8-x2bq4: Back-off restarting failed container"; s.Findings[1].Text != want {
		t.Fatalf("kube text %q", s.Findings[1].Text)
	}
	if !strings.HasPrefix(s.Headline, "Critical: checkout errors (shop/checkout) — open for 42 min, not yet taken on. Most likely related: deployment shop/checkout rolled out") {
		t.Fatalf("headline %q", s.Headline)
	}
	if s.ElapsedSeconds != 42*60 {
		t.Fatalf("elapsed %d", s.ElapsedSeconds)
	}
	// the timeline is ordered, has the Normal event as context, and the milestones
	var texts []string
	for i, e := range s.Timeline {
		if i > 0 && e.At.Before(s.Timeline[i-1].At) {
			t.Fatal("timeline out of order")
		}
		texts = append(texts, e.Source+":"+e.Text)
	}
	joined := strings.Join(texts, "\n")
	for _, want := range []string{"incident:The trouble began", "incident:Opened by detection", "kubernetes:Pulled ×1", "rollout:deployment shop/checkout"} {
		if !strings.Contains(joined, want) {
			t.Errorf("timeline lacks %q:\n%s", want, joined)
		}
	}
	if len(s.Gaps) != 1 || !strings.Contains(s.Gaps[0], "Process lineage: nothing unusual") {
		t.Fatalf("gaps %v", s.Gaps)
	}
}

func TestProcessLineageFindings(t *testing.T) {
	s := Build(Input{
		Incident: openIncident(), Now: at(10),
		RolloutSource: read(), KubeSource: read(), ProcessSource: read(),
		Processes: []ProcessEvent{
			{At: at(-2), Kind: ProcessExecNew, Container: "/k8s/shop/checkout-7d9f8-x2bq4/app", Exe: "/usr/bin/curl", ParentExe: "/bin/sh"},
			{At: at(1), Kind: ProcessOOMKill, Container: "/k8s/shop/checkout-7d9f8-x2bq4/app", Exe: "/usr/bin/java"},
			{At: at(3), Kind: ProcessExitFailed, Exe: "/app/worker", ExitCode: 0, Signal: "SIGKILL"},
			{At: at(-120), Kind: ProcessExecNew, Exe: "/old"}, // outside the window
		},
	})
	if len(s.Findings) != 3 {
		t.Fatalf("%+v", s.Findings)
	}
	if s.Findings[0].Text != "/usr/bin/java was killed by the OOM killer in /k8s/shop/checkout-7d9f8-x2bq4/app" {
		t.Fatalf("top %q", s.Findings[0].Text)
	}
	if s.Findings[1].Text != "/app/worker exited with signal SIGKILL" {
		t.Fatalf("second %q", s.Findings[1].Text)
	}
	if !strings.Contains(s.Findings[2].Text, "/usr/bin/curl ran for the first time") || !strings.Contains(s.Findings[2].Text, "started by /bin/sh") {
		t.Fatalf("third %q", s.Findings[2].Text)
	}
	if len(s.Gaps) != 1 || !strings.Contains(s.Gaps[0], "Kubernetes events: none about shop/checkout") {
		t.Fatalf("gaps %v", s.Gaps)
	}
}

func TestResolvedAndNothingFound(t *testing.T) {
	inc := openIncident()
	r := at(35)
	inc.State, inc.Resolution, inc.ResolvedAt = incident.Resolved, incident.NotReproducing, &r
	s := Build(Input{Incident: inc, Now: at(500), RolloutSource: read(), KubeSource: Source{Note: "the query timed out"}, ProcessSource: Source{}})
	if s.Headline != "Critical: checkout errors (shop/checkout) — resolved after 35 min (not reproducible). No rollout, Kubernetes warning or unusual process was found around the start." {
		t.Fatalf("headline %q", s.Headline)
	}
	if len(s.Gaps) != 2 || s.Gaps[0] != "Kubernetes events: the query timed out" || s.Gaps[1] != "Process lineage: could not be read" {
		t.Fatalf("gaps %v", s.Gaps)
	}
	if s.Findings == nil || s.Timeline == nil {
		t.Fatal("empty lists, not null, in the JSON")
	}
}

func TestAHostIncidentSkipsWorkloadSources(t *testing.T) {
	inc := openIncident()
	inc.App, inc.Severity = "", ""
	s := Build(Input{Incident: inc, Now: at(5), ProcessSource: read()})
	if !strings.HasPrefix(s.Headline, "Incident: checkout errors — open for 5 min") || strings.Contains(s.Headline, "No rollout") {
		t.Fatalf("headline %q", s.Headline)
	}
	for _, g := range s.Gaps {
		if strings.HasPrefix(g, "Rollouts") || strings.HasPrefix(g, "Kubernetes") {
			t.Fatalf("no workload, so no workload gaps: %v", s.Gaps)
		}
	}
}

func TestARolloutDuringTheIncidentRanksLow(t *testing.T) {
	if sc := rolloutScore(at(10), start); sc >= 50 {
		t.Fatalf("a rollout after the start is likely the fix: %d", sc)
	}
	if rolloutScore(at(-1), start) <= rolloutScore(at(-25), start) {
		t.Fatal("a closer rollout scores higher")
	}
}

func TestDuration(t *testing.T) {
	for sec, want := range map[int64]string{0: "0 s", 59: "59 s", 60: "1 min", 3599: "59 min", 3600: "1 h", 3660: "1 h 1 min", 86400: "1 d", 90000: "1 d 1 h"} {
		if got := Duration(sec); got != want {
			t.Errorf("Duration(%d) = %q, want %q", sec, got, want)
		}
	}
}

func TestKubeGroupTextCapsObjects(t *testing.T) {
	g := kubeGroup{reason: "Unhealthy", count: 9, objects: []string{"pod a", "pod b", "pod c", "pod d"}, message: "probe failed"}
	if g.text() != "Unhealthy ×9 on pod a, pod b and 2 more: probe failed" {
		t.Fatalf("%q", g.text())
	}
}
