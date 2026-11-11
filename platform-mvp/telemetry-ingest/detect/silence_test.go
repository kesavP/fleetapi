package detect

import (
	"encoding/json"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/incident"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/store"
)

func wl(kind, ns, name string, desired, ready int, cluster ...string) store.InventoryObject {
	c := "gke"
	if len(cluster) > 0 {
		c = cluster[0]
	}
	d, _ := json.Marshal(map[string]any{"desired": desired, "ready": ready})
	return store.InventoryObject{Cluster: c, Kind: kind, UID: kind + "-" + ns + "-" + name, Namespace: ns, Name: name, Data: d}
}

func podRow(ns, workload string, data map[string]any) store.InventoryObject {
	if _, ok := data["workload_kind"]; !ok {
		data["workload_kind"], data["workload_name"] = "deployment", workload
	}
	d, _ := json.Marshal(data)
	return store.InventoryObject{Cluster: "gke", Kind: "pod", UID: "p-" + ns + workload, Namespace: ns, Name: workload + "-abc-xyz", Data: d}
}

func TestAppIDOf(t *testing.T) {
	for _, c := range []struct{ kind, want string }{{"deployment", "/k8s/shop/web"}, {"statefulset", "/k8s/shop/web"}, {"daemonset", "/k8s/shop/web"}, {"cronjob", "/k8s-cronjob/shop/web"}} {
		if got := AppIDOf(c.kind, "shop", "web"); got != c.want {
			t.Errorf("%s: %s", c.kind, got)
		}
	}
}

func TestWantedFromAddsTheSameWorkloadOfTwoClustersAndSkipsWhatCannotBeRead(t *testing.T) {
	rows := []store.InventoryObject{
		wl("deployment", "shop", "web", 2, 2, "a"), wl("deployment", "shop", "web", 3, 1, "b"), wl("deployment", "shop", "api", 1, 0),
		{Cluster: "a", Kind: "deployment", Namespace: "shop", Name: "broken", Data: json.RawMessage(`not json`)},
		{Cluster: "a", Kind: "deployment", Namespace: "", Name: "nons", Data: json.RawMessage(`{}`)},
	}
	got := WantedFrom(rows)
	if len(got) != 2 || got[0].ID != "/k8s/shop/web" || got[0].Desired != 5 || got[0].Ready != 3 || got[0].Label != "shop/web" || got[1].ID != "/k8s/shop/api" {
		t.Fatalf("%+v", got)
	}
}

func series(container string, last time.Time) store.MetricSeries {
	return store.MetricSeries{Labels: map[string]string{"container_id": container}, Points: []store.Point{{T: last.Add(-time.Minute).UnixMilli(), V: 1}, {T: last.UnixMilli(), V: 1}}}
}

func TestTelemetryOf(t *testing.T) {
	end := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	web1, web2 := "/k8s/shop/web-5d6f8c7b9-xk2lp/app", "/k8s/shop/web-5d6f8c7b9-zz9bd/app"
	tel := TelemetryOf([]store.MetricSeries{
		series(web1, end.Add(-time.Minute)),    // sending
		series(web2, end.Add(-10*time.Minute)), // stopped
		series("/k8s/shop/old-5d6f8c7b9-xk2lp/app", end.Add(-30*time.Minute)),
		series("/nomad/x/y", end),                         // not an application this knows
		{Labels: map[string]string{"container_id": web1}}, // no points
	}, end)
	web := tel["/k8s/shop/web"]
	if web.Fresh != 1 || !web.Seen || !web.Last.Equal(end.Add(-time.Minute)) {
		t.Fatalf("web: %+v (one of two containers is sending; the newest reading is the fresh one's)", web)
	}
	if old := tel["/k8s/shop/old"]; old.Fresh != 0 || !old.Seen || !old.Last.Equal(end.Add(-30*time.Minute)) {
		t.Fatalf("old: %+v", old)
	}
	if len(tel) != 2 {
		t.Fatalf("%+v", tel)
	}
	// a reading exactly at the edge of "fresh" is fresh, one a moment before it is not
	edge := TelemetryOf([]store.MetricSeries{series(web1, end.Add(-freshWithin))}, end)["/k8s/shop/web"]
	late := TelemetryOf([]store.MetricSeries{series(web1, end.Add(-freshWithin-time.Millisecond))}, end)["/k8s/shop/web"]
	if edge.Fresh != 1 || late.Fresh != 0 {
		t.Fatalf("edge %+v, late %+v", edge, late)
	}
	if !AgentAlive(tel) || AgentAlive(map[string]Telemetry{"x": {Seen: true}}) || AgentAlive(nil) {
		t.Fatal("AgentAlive")
	}
}

func TestJudgeSilence(t *testing.T) {
	last := time.Date(2026, 11, 10, 11, 40, 0, 0, time.UTC)
	w := func(kind, ns, name string, desired, ready int) Wanted {
		return Wanted{ID: AppIDOf(kind, ns, name), Label: ns + "/" + name, Namespace: ns, Kind: kind, Desired: desired, Ready: ready}
	}
	sending := Telemetry{Fresh: 1, Seen: true, Last: last}
	stale := Telemetry{Seen: true, Last: last}
	exclude := regexp.MustCompile(`^kube-`)
	ws := []Wanted{
		w("deployment", "shop", "sending", 2, 2),
		w("deployment", "shop", "zero", 0, 0),
		w("deployment", "shop", "notready", 3, 0),
		w("deployment", "shop", "partial", 3, 1),
		w("deployment", "shop", "stopped", 2, 2),
		w("deployment", "shop", "uncovered", 2, 2),
		w("cronjob", "shop", "cleanup", 1, 0),
		w("daemonset", "kube-system", "agent", 3, 0),
		w("statefulset", "data", "db", 1, 1),
	}
	tel := map[string]Telemetry{
		"/k8s/shop/sending": sending, "/k8s/shop/zero": stale, "/k8s/shop/notready": stale, "/k8s/shop/stopped": stale,
		"/k8s/shop/partial": stale, "/k8s-cronjob/shop/cleanup": stale, "/k8s/data/db": sending,
	}
	got := JudgeSilence(ws, tel, exclude)
	want := map[string]string{"/k8s/shop/notready": CauseNotReady, "/k8s/shop/partial": CauseNotReady, "/k8s/shop/stopped": CauseStopped}
	if len(got) != len(want) {
		t.Fatalf("%+v", got)
	}
	for _, s := range got {
		if want[s.App] != s.Cause {
			t.Errorf("%s: %s, want %s", s.App, s.Cause, want[s.App])
		}
	}
	if got[0].App != "/k8s/shop/notready" && got[0].App != "/k8s/shop/partial" && got[0].App != "/k8s/shop/stopped" || got[0].App > got[1].App {
		t.Errorf("not sorted: %+v", got)
	}
	// what is left out, and why
	for _, id := range []string{"/k8s/shop/sending", "/k8s/shop/zero", "/k8s/shop/uncovered", "/k8s-cronjob/shop/cleanup", "/k8s/kube-system/agent", "/k8s/data/db"} {
		for _, s := range got {
			if s.App == id {
				t.Errorf("%s was judged silent", id)
			}
		}
	}
	// with no exclusion the platform's own workloads count
	if all := JudgeSilence(ws, tel, nil); len(all) != 4 {
		t.Errorf("without an exclusion: %d", len(all))
	}
	// the last reading is carried, for when it began
	for _, s := range got {
		if s.App == "/k8s/shop/stopped" && !s.LastData.Equal(last) {
			t.Errorf("last data %v", s.LastData)
		}
	}
}

func TestPodReasons(t *testing.T) {
	crash := map[string]any{"phase": "Running", "ready": false, "containers": []any{map[string]any{"state": "waiting", "reason": "CrashLoopBackOff", "last_reason": "OOMKilled"}}}
	cp := func() map[string]any {
		m := map[string]any{}
		for k, v := range crash {
			m[k] = v
		}
		return m
	}
	got := PodReasons([]store.InventoryObject{
		podRow("shop", "pay", cp()), podRow("shop", "pay", cp()), podRow("shop", "pay", map[string]any{"phase": "Pending", "reason": "Unschedulable", "ready": false}),
		podRow("shop", "pay", map[string]any{"ready": true, "phase": "Running"}),
		podRow("shop", "pay", map[string]any{"phase": "Succeeded", "ready": false}),
		podRow("shop", "web", map[string]any{"reason": "Evicted", "ready": false}),
		podRow("shop", "solo", map[string]any{"ready": false, "workload_kind": "pod", "workload_name": "solo"}),
		podRow("shop", "cron", map[string]any{"ready": false, "reason": "Error", "workload_kind": "cronjob", "workload_name": "cleanup"}),
		{Kind: "pod", Namespace: "shop", Data: json.RawMessage(`nope`)},
	})
	if r := got["/k8s/shop/pay"]; len(r) != 2 || r[0] != "CrashLoopBackOff (OOMKilled) ×2" || r[1] != "Unschedulable" {
		t.Fatalf("pay: %v", r)
	}
	if got["/k8s/shop/web"][0] != "Evicted" || got["/k8s-cronjob/shop/cleanup"][0] != "Error" || len(got["/k8s/shop/solo"]) != 0 {
		t.Fatalf("%v", got)
	}
	// at most three
	var many []store.InventoryObject
	for _, r := range []string{"a", "b", "c", "d"} {
		many = append(many, podRow("shop", "x", map[string]any{"reason": r, "ready": false}))
	}
	if r := PodReasons(many)["/k8s/shop/x"]; len(r) != 3 {
		t.Fatalf("%v", r)
	}
}

func TestPodReasonWords(t *testing.T) {
	c := func(state, reason, last string) podData {
		return podData{Containers: []podContainer{{state, reason, last}}}
	}
	cases := map[string]podData{
		"Evicted":                      {Reason: "Evicted", Phase: "Failed"},
		"ImagePullBackOff":             c("waiting", "ImagePullBackOff", ""),
		"CrashLoopBackOff":             c("waiting", "CrashLoopBackOff", ""),
		"CrashLoopBackOff (OOMKilled)": c("waiting", "CrashLoopBackOff", "OOMKilled"),
		"Error":                        c("terminated", "Error", ""),
		"Pending":                      {Phase: "Pending"},
		"not ready":                    {Phase: "Running"},
	}
	for want, d := range cases {
		if got := podReason(d); got != want {
			t.Errorf("%q: %q", want, got)
		}
	}
}

func TestDescribeSilence(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	since := now.Add(-20 * time.Minute)
	nr := DescribeSilence(Silent{App: "/k8s/shop/pay", Label: "shop/pay", Cause: CauseNotReady, Desired: 3, Ready: 0, Reasons: []string{"CrashLoopBackOff (OOMKilled) ×3"}, LastData: since}, since, now)
	if nr.Severity != incident.Critical || nr.Title != "shop/pay: not ready (0 of 3)" || nr.Source != incident.Detected || !nr.StartedAt.Equal(since) || nr.App != "/k8s/shop/pay" {
		t.Fatalf("%+v", nr)
	}
	for _, want := range []string{"Opened automatically", "silent since 11:40 UTC (20m0s)", "set to run 3 and 0 are ready", "CrashLoopBackOff (OOMKilled) ×3", "last reading from it was at 11:40 UTC", "not a scale-down"} {
		if !strings.Contains(nr.Summary, want) {
			t.Errorf("summary lacks %q:\n%s", want, nr.Summary)
		}
	}
	if part := DescribeSilence(Silent{Label: "shop/pay", Cause: CauseNotReady, Desired: 3, Ready: 1}, since, now); part.Severity != incident.Warning {
		t.Fatalf("a partly ready workload is a warning: %v", part.Severity)
	}
	st := DescribeSilence(Silent{App: "/k8s/shop/ad", Label: "shop/ad", Cause: CauseStopped, Desired: 2, Ready: 2}, since, now)
	if st.Severity != incident.Warning || st.Title != "shop/ad: ready but sending no data" || !strings.Contains(st.Summary, "It has sent nothing in the last hour") || !strings.Contains(st.Summary, "All 2 of its pods are ready") {
		t.Fatalf("%+v", st)
	}
}

func TestSilenceKey(t *testing.T) {
	a := time.Unix(1_700_000_000, 0)
	k := SilenceKey("/k8s/shop/web", a)
	if !strings.HasPrefix(k, "silence:") || k != SilenceKey("/k8s/shop/web", a) || k == SilenceKey("/k8s/shop/web", a.Add(time.Minute)) || k == SilenceKey("/k8s/shop/api", a) {
		t.Fatal(k)
	}
	if strings.Contains(k, "detect:") || k == DedupeKey("/k8s/shop/web", a) {
		t.Fatal("a silence key is not a burn key")
	}
	m := incident.NewManager(incident.NewMemStore(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, _, err := m.Create(bg, "gke", Actor, incident.CreateInput{Title: "t", DedupeKey: SilenceKey("/system.slice/we ird~name.service", a), Source: incident.Detected}); err != nil {
		t.Fatalf("the manager refuses the key: %v", err)
	}
	if familyOf(incident.Incident{DedupeKey: k}) != familySilence || familyOf(incident.Incident{DedupeKey: DedupeKey("x", a)}) != familyBurn || familyOf(incident.Incident{}) != familyBurn {
		t.Fatal("familyOf")
	}
}

// What an active incident of one kind covers: the same kind, and not the other.
func TestAnIncidentOfOneKindDoesNotCoverTheOther(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	burn := incident.Incident{App: "a", Source: incident.Detected, State: incident.Open, StartedAt: now.Add(-time.Hour), DedupeKey: DedupeKey("a", now.Add(-time.Hour))}
	silence := incident.Incident{App: "a", Source: incident.Detected, State: incident.Open, StartedAt: now.Add(-time.Hour), DedupeKey: SilenceKey("a", now.Add(-time.Hour))}
	legacy := incident.Incident{App: "a", Source: incident.Detected, State: incident.Open, StartedAt: now.Add(-time.Hour)} // no key: a burn incident
	f := func(family string) []Found {
		return []Found{{App: "a", Family: family, Breach: Breach{Start: now.Add(-10 * time.Minute), End: now, Open: true}}}
	}
	c := Defaults()
	for name, tc := range map[string]struct {
		have   incident.Incident
		family string
		kept   bool
	}{
		"a burn incident does not cover a silence":    {burn, familySilence, true},
		"a silence incident does not cover a burn":    {silence, familyBurn, true},
		"a silence incident covers a silence":         {silence, familySilence, false},
		"a burn incident covers a burn":               {burn, familyBurn, false},
		"a keyless incident is a burn one (covers)":   {legacy, familyBurn, false},
		"a keyless incident does not cover a silence": {legacy, familySilence, true},
	} {
		got := Fresh(f(tc.family), []incident.Incident{tc.have}, now, c, 30*time.Minute)
		if (len(got) == 1) != tc.kept {
			t.Errorf("%s: kept %v", name, len(got) == 1)
		}
	}
}
