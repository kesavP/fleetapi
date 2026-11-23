package detect

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/incident"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/inventory"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/store"
)

// Silence: an application that has stopped sending data, found by putting what the cluster collector says a workload is SET to run beside what
// the node-agent still sends from it. This is the same judgement as the UI's "Silent" status (telemetry-ui/src/api/silence.js), narrowed to
// what is worth an incident:
//
//	not-ready  it is set to run N (N > 0), fewer are ready, and nothing is sending: a failure, whatever the node-agent saw
//	stopped    it is ready, it was sending earlier in the window, and nothing is now: the data stopped though the pods did not
//
// and NOT: scaled to zero (a decision, not a failure), removed from the cluster, a cron job (idle between runs), a workload that is ready and
// never sent anything (the node-agent may not cover it, and an incident for every such workload would be noise), and anything at all when no
// application is sending (that is the agent or the way to the hub, which is not an application's incident). Without an inventory nothing can be
// judged, because "stopped sending" is then the same as "was scaled down".

// RSSMetric is the memory gauge: one series per container, and a pod that stopped keeps its last reading, which is how a workload that is not
// sending is told from one that never existed.
const RSSMetric = "container_resources_memory_rss_bytes"

const (
	CauseNotReady = "not-ready"
	CauseStopped  = "stopped"
)

// silenceWindow is how far back the node-agent's series are looked at, and so how long ago a workload may have last sent for "it was sending
// earlier" to hold.
const silenceWindow = time.Hour

// freshWithin is how recent a series' last reading must be for the container to count as sending: a few steps of the agent's.
const freshWithin = 3 * time.Minute

// staleInventory is how long a cluster collector may be quiet before what it said is not trusted.
const staleInventory = 10 * time.Minute

// Wanted is what the cluster says a workload is set to run and how many are ready.
type Wanted struct {
	ID, Label, Namespace, Kind string
	Desired, Ready             int
}

// Telemetry is what the node-agent sends from an application: how many of its containers are sending, whether any was in the window, and
// when the newest reading was.
type Telemetry struct {
	Fresh int
	Seen  bool
	Last  time.Time
}

// Silent is an application that has stopped sending, and why.
type Silent struct {
	App, Label, Cause string
	Desired, Ready    int
	Reasons           []string  // why its pods are not ready, the commonest first
	LastData          time.Time // the newest reading of any of its containers; zero when it never sent in the window
}

// AppIDOf is the node-agent's name for a workload: "/k8s/<namespace>/<name>", "/k8s-cronjob/..." for a cron job.
func AppIDOf(kind, namespace, name string) string {
	if kind == inventory.CronJob {
		return "/k8s-cronjob/" + namespace + "/" + name
	}
	return "/k8s/" + namespace + "/" + name
}

type workloadData struct {
	Desired int `json:"desired"`
	Ready   int `json:"ready"`
}

// WantedFrom is the workloads of the inventory rows, one per application: the same workload in two clusters is one application here (as on the
// page), so their numbers are added. Rows whose data cannot be read are left out.
func WantedFrom(rows []store.InventoryObject) []Wanted {
	byID := map[string]*Wanted{}
	var order []string
	for _, r := range rows {
		if r.Namespace == "" || r.Name == "" {
			continue
		}
		var d workloadData
		if err := json.Unmarshal(r.Data, &d); err != nil {
			continue
		}
		id := AppIDOf(r.Kind, r.Namespace, r.Name)
		if w, ok := byID[id]; ok {
			w.Desired += d.Desired
			w.Ready += d.Ready
			continue
		}
		byID[id] = &Wanted{ID: id, Label: r.Namespace + "/" + r.Name, Namespace: r.Namespace, Kind: r.Kind, Desired: d.Desired, Ready: d.Ready}
		order = append(order, id)
	}
	out := make([]Wanted, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out
}

// TelemetryOf is what the memory gauge's series say about each application: they are laid out by workload (WorkloadOf), and a container is
// sending when its newest reading is within freshWithin of `end`.
func TelemetryOf(series []store.MetricSeries, end time.Time) map[string]Telemetry {
	out := map[string]Telemetry{}
	for _, s := range series {
		w, ok := WorkloadOf(s.Labels["container_id"])
		if !ok || len(s.Points) == 0 {
			continue
		}
		var last int64
		for _, p := range s.Points {
			last = max(last, p.T)
		}
		t := out[w.ID]
		t.Seen = true
		if lt := time.UnixMilli(last).UTC(); lt.After(t.Last) {
			t.Last = lt
		}
		if !time.UnixMilli(last).Before(end.Add(-freshWithin)) {
			t.Fresh++
		}
		out[w.ID] = t
	}
	return out
}

// AgentAlive reports whether any application is sending. When none is, the node-agent or the way to the hub is the trouble, and calling every
// application silent for it would be wrong.
func AgentAlive(tel map[string]Telemetry) bool {
	for _, t := range tel {
		if t.Fresh > 0 {
			return true
		}
	}
	return false
}

// JudgeSilence is the applications to open incidents for, by what the inventory wants and what the node-agent sends. exclude, when set, names
// the namespaces to leave out (the platform's own).
func JudgeSilence(ws []Wanted, tel map[string]Telemetry, exclude *regexp.Regexp) []Silent {
	var out []Silent
	for _, w := range ws {
		if w.Kind == inventory.CronJob || w.Desired <= 0 { // idle between runs; scaled down on purpose
			continue
		}
		if exclude != nil && exclude.MatchString(w.Namespace) {
			continue
		}
		t := tel[w.ID]
		if t.Fresh > 0 {
			continue
		}
		s := Silent{App: w.ID, Label: w.Label, Desired: w.Desired, Ready: w.Ready, LastData: t.Last}
		switch {
		case w.Ready < w.Desired:
			s.Cause = CauseNotReady
		case t.Seen:
			s.Cause = CauseStopped
		default:
			continue // ready, and never seen sending: not covered, perhaps; not an incident
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].App < out[j].App })
	return out
}

type podData struct {
	Phase        string         `json:"phase"`
	Reason       string         `json:"reason"`
	Ready        bool           `json:"ready"`
	WorkloadKind string         `json:"workload_kind"`
	WorkloadName string         `json:"workload_name"`
	Containers   []podContainer `json:"containers"`
}

type podContainer struct {
	State      string `json:"state"`
	Reason     string `json:"reason"`
	LastReason string `json:"last_reason"`
}

// podReason is why a pod is not ready, in a word: its own reason (Unschedulable, Evicted), or what its containers wait for or died of.
func podReason(d podData) string {
	if d.Reason != "" {
		return d.Reason
	}
	for _, c := range d.Containers {
		switch {
		case c.State == "waiting" && c.Reason != "":
			if c.Reason == "CrashLoopBackOff" && c.LastReason != "" {
				return "CrashLoopBackOff (" + c.LastReason + ")"
			}
			return c.Reason
		case c.State == "terminated" && c.Reason != "":
			return c.Reason
		}
	}
	if d.Phase != "" && d.Phase != "Running" {
		return d.Phase
	}
	return "not ready"
}

// PodReasons is, for each application, why its pods that are not ready are not: the commonest first, at most three, as "CrashLoopBackOff ×2".
func PodReasons(rows []store.InventoryObject) map[string][]string {
	per := map[string]map[string]int{}
	for _, r := range rows {
		var d podData
		if json.Unmarshal(r.Data, &d) != nil || d.Ready || d.Phase == "Succeeded" || d.WorkloadKind == "" || d.WorkloadName == "" || d.WorkloadKind == "pod" {
			continue
		}
		id := AppIDOf(d.WorkloadKind, r.Namespace, d.WorkloadName)
		if per[id] == nil {
			per[id] = map[string]int{}
		}
		per[id][podReason(d)]++
	}
	out := map[string][]string{}
	for id, m := range per {
		type kv struct {
			k string
			n int
		}
		var list []kv
		for k, n := range m {
			list = append(list, kv{k, n})
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].n != list[j].n {
				return list[i].n > list[j].n
			}
			return list[i].k < list[j].k
		})
		for i, e := range list {
			if i == 3 {
				break
			}
			if e.n > 1 {
				out[id] = append(out[id], fmt.Sprintf("%s ×%d", e.k, e.n))
			} else {
				out[id] = append(out[id], e.k)
			}
		}
	}
	return out
}

// SilenceKey names a silence so that finding it twice opens it once: "silence:", the application and when it began. The application id is
// hashed, as it may hold characters a key may not. The prefix is also what tells a silence incident from a burn-rate one (see familyOf).
func SilenceKey(app string, since time.Time) string {
	return "silence:" + DedupeKey(app, since)[len("detect:"):]
}

// DescribeSilence is the incident a silent application becomes.
func DescribeSilence(s Silent, since, now time.Time) incident.CreateInput {
	sev := incident.Warning
	var title, what string
	switch s.Cause {
	case CauseNotReady:
		title = fmt.Sprintf("%s: not ready (%d of %d)", s.Label, s.Ready, s.Desired)
		what = fmt.Sprintf("It is set to run %d and %d are ready, and nothing is being sent from it.", s.Desired, s.Ready)
		if s.Ready == 0 {
			sev = incident.Critical
		}
		if len(s.Reasons) > 0 {
			what += " Its pods that are not ready: " + strings.Join(s.Reasons, ", ") + "."
		}
	default:
		title = fmt.Sprintf("%s: ready but sending no data", s.Label)
		what = fmt.Sprintf("All %d of its pods are ready, and nothing is being sent from them any more.", s.Ready)
	}
	var last string
	if !s.LastData.IsZero() {
		last = fmt.Sprintf(" The last reading from it was at %s UTC.", s.LastData.Format("15:04"))
	} else {
		last = " It has sent nothing in the last hour."
	}
	head := fmt.Sprintf("Opened automatically. %s has been silent since %s UTC (%s).", s.Label, since.Format("15:04"), now.Sub(since).Round(time.Minute))
	return incident.CreateInput{
		Title:     title,
		App:       s.App,
		Severity:  sev,
		Source:    incident.Detected,
		DedupeKey: SilenceKey(s.App, since),
		Summary:   head + "\n" + what + last + "\nIt is not set to run no replicas, so this is not a scale-down.",
		StartedAt: since,
	}
}
