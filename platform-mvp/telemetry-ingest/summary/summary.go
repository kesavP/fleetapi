// Package summary builds an incident's instant summary: where it stands and how long each stage took, and what changed or went wrong around the
// time its trouble began — rollouts of its workload, the Kubernetes events about it and, from fleet-node-agent's process lineage, unusual processes
// in its containers. It is computed from data the platform already holds, in milliseconds, with no model involved, so it is there the moment an
// incident is opened; the Claude analysis (package analysis) is the slower second opinion.
//
// Build is pure: the server gathers the Input (store/summary.go) and Build ranks and words it. Nothing here guesses a cause: a finding is
// something that happened near the start, scored by how often that kind of thing is the cause and how close to the start it happened.
package summary

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/incident"
)

// Lookback is how long before the start of the trouble the summary looks for changes; MaxSpan caps how much of a long incident it reads.
const (
	Lookback   = 30 * time.Minute
	After      = 5 * time.Minute
	MaxSpan    = 24 * time.Hour
	MaxFinding = 5
	MaxEntries = 60
	maxMessage = 160
)

// KubeEvent is a Kubernetes event (from the cluster collector), as it last stood.
type KubeEvent struct {
	Namespace, Kind, Name, Type, Reason, Message string
	Count                                        uint32
	First, Last                                  time.Time
}

// Rollout is a change to a workload's spec: its generation went up at At. Before and After are its container images.
type Rollout struct {
	Kind, Namespace, Name string
	At                    time.Time
	Generation            int64
	Before, After         []string
}

// Process lineage events, as fleet-node-agent exports them (log records with log.source=process). The names are the contract with the agent.
const (
	ProcessSource     = "process"
	ProcessExecNew    = "exec_new"      // a binary this container had not run before
	ProcessExitFailed = "exit_abnormal" // a process ended with a non-zero code or a signal
	ProcessOOMKill    = "oom_kill"      // the kernel's OOM killer ended it
	ProcessForkBurst  = "fork_burst"    // the container started far more processes than usual

	AttrEvent     = "process.event"
	AttrExe       = "process.executable.path"
	AttrParentExe = "process.parent.executable.path"
	AttrPid       = "process.pid"
	AttrParentPid = "process.parent_pid"
	AttrExitCode  = "process.exit.code"
	AttrSignal    = "process.exit.signal"
	AttrCount     = "process.count"
	AttrContainer = "container.id"
	AttrLogSource = "log.source"
)

// ProcessEvent is one process lineage event.
type ProcessEvent struct {
	At             time.Time
	Kind           string
	Container      string
	Exe, ParentExe string
	ExitCode       int
	Signal         string
	Count          int
}

// Source says whether a kind of data could be read for the summary, and if not, why.
type Source struct {
	Read bool
	Note string
}

type Input struct {
	Incident incident.Incident
	Now      time.Time

	Kube          []KubeEvent
	KubeSource    Source
	Rollouts      []Rollout
	RolloutSource Source
	Processes     []ProcessEvent
	ProcessSource Source
}

type Finding struct {
	At    time.Time `json:"at"`
	Kind  string    `json:"kind"` // rollout, kubernetes or process
	Text  string    `json:"text"`
	Score int       `json:"score"`
}

type Entry struct {
	At     time.Time `json:"at"`
	Source string    `json:"source"` // incident, rollout, kubernetes or process
	Text   string    `json:"text"`
}

type Summary struct {
	Headline       string            `json:"headline"`
	State          incident.State    `json:"state"`
	Severity       incident.Severity `json:"severity"`
	ElapsedSeconds int64             `json:"elapsed_seconds"` // from the start to the resolution, or to now
	Timing         incident.Timing   `json:"timing"`
	From           time.Time         `json:"from"` // the window looked at
	To             time.Time         `json:"to"`
	Findings       []Finding         `json:"findings"`
	Timeline       []Entry           `json:"timeline"`
	Gaps           []string          `json:"gaps"`
}

// Workload splits an application id (/k8s/<namespace>/<name>, /k8s-cronjob/<namespace>/<name>) into its namespace and name; ok is false for
// anything else (a host process, an empty app).
func Workload(app string) (namespace, name string, ok bool) {
	for _, prefix := range []string{"/k8s/", "/k8s-cronjob/"} {
		if rest, found := strings.CutPrefix(app, prefix); found {
			parts := strings.Split(rest, "/")
			if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
				return parts[0], parts[1], true
			}
		}
	}
	return "", "", false
}

// Window is the time the summary looks at: from Lookback before the start of the trouble to After past its resolution (or now), at most MaxSpan
// of the incident itself.
func Window(inc incident.Incident, now time.Time) (from, to time.Time) {
	start := startOf(inc)
	end := now
	if inc.ResolvedAt != nil {
		end = *inc.ResolvedAt
	}
	if end.Before(start) {
		end = start
	}
	if end.Sub(start) > MaxSpan {
		end = start.Add(MaxSpan)
	}
	return start.Add(-Lookback), end.Add(After)
}

func startOf(inc incident.Incident) time.Time {
	if inc.StartedAt.IsZero() {
		return inc.CreatedAt
	}
	return inc.StartedAt
}

// belongs says whether a Kubernetes object is the workload or one of its parts: the Deployment "api", its ReplicaSets "api-7d9f8" and pods
// "api-7d9f8-x2bq4", its HPA "api".
func belongs(object, workload string) bool {
	return object == workload || strings.HasPrefix(object, workload+"-")
}

// Build words the summary.
func Build(in Input) Summary {
	inc := in.Incident
	now := in.Now.UTC()
	start := startOf(inc)
	from, to := Window(inc, now)
	end := now
	if inc.ResolvedAt != nil {
		end = *inc.ResolvedAt
	}
	s := Summary{State: inc.State, Severity: inc.Severity, Timing: inc.Timing, From: from, To: to, Findings: []Finding{}, Timeline: []Entry{}, Gaps: []string{}}
	if e := int64(end.Sub(start) / time.Second); e > 0 {
		s.ElapsedSeconds = e
	}
	ns, name, isWorkload := Workload(inc.App)

	s.Timeline = append(s.Timeline, milestones(inc)...)
	var findings []Finding

	// rollouts
	switch {
	case !isWorkload:
	case !in.RolloutSource.Read:
		s.Gaps = append(s.Gaps, "Rollouts: "+orDefault(in.RolloutSource.Note, "could not be read"))
	default:
		for _, r := range in.Rollouts {
			if r.At.Before(from) || r.At.After(to) {
				continue
			}
			text := rolloutText(r, start)
			s.Timeline = append(s.Timeline, Entry{At: r.At, Source: "rollout", Text: text})
			findings = append(findings, Finding{At: r.At, Kind: "rollout", Text: text, Score: rolloutScore(r.At, start)})
		}
	}

	// Kubernetes events, grouped by reason
	switch {
	case !isWorkload:
	case !in.KubeSource.Read:
		s.Gaps = append(s.Gaps, "Kubernetes events: "+orDefault(in.KubeSource.Note, "could not be read"))
	default:
		groups := groupKube(in.Kube, ns, name, from, to)
		if len(groups) == 0 {
			s.Gaps = append(s.Gaps, fmt.Sprintf("Kubernetes events: none about %s/%s in the window (the cluster collector keeps only Warning events unless it runs with --events=all).", ns, name))
		}
		for _, g := range groups {
			s.Timeline = append(s.Timeline, Entry{At: g.first, Source: "kubernetes", Text: g.text()})
			if sc := kubeScore(g, start); sc > 0 {
				findings = append(findings, Finding{At: g.first, Kind: "kubernetes", Text: g.text(), Score: sc})
			}
		}
	}

	// process lineage
	switch {
	case !in.ProcessSource.Read:
		s.Gaps = append(s.Gaps, "Process lineage: "+orDefault(in.ProcessSource.Note, "could not be read"))
	case len(in.Processes) == 0:
		s.Gaps = append(s.Gaps, "Process lineage: nothing unusual was recorded for this application (it needs fleet-node-agent with --process-lineage).")
	default:
		for _, p := range in.Processes {
			if p.At.Before(from) || p.At.After(to) {
				continue
			}
			text := processText(p)
			s.Timeline = append(s.Timeline, Entry{At: p.At, Source: "process", Text: text})
			findings = append(findings, Finding{At: p.At, Kind: "process", Text: text, Score: processScore(p, start)})
		}
	}

	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Score != findings[j].Score {
			return findings[i].Score > findings[j].Score
		}
		return findings[i].At.Before(findings[j].At)
	})
	if len(findings) > MaxFinding {
		findings = findings[:MaxFinding]
	}
	s.Findings = append(s.Findings, findings...)

	sort.SliceStable(s.Timeline, func(i, j int) bool { return s.Timeline[i].At.Before(s.Timeline[j].At) })
	if len(s.Timeline) > MaxEntries {
		s.Timeline = s.Timeline[:MaxEntries]
	}
	s.Headline = headline(inc, s, isWorkload, ns, name)
	return s
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func milestones(inc incident.Incident) []Entry {
	out := []Entry{}
	start := startOf(inc)
	if !inc.StartedAt.IsZero() && inc.StartedAt.Before(inc.CreatedAt) {
		out = append(out, Entry{At: start, Source: "incident", Text: "The trouble began"})
	}
	how := "Opened by detection"
	if inc.Source == incident.Manual {
		how = "Opened by hand"
	}
	out = append(out, Entry{At: inc.CreatedAt, Source: "incident", Text: how})
	add := func(t *time.Time, text string) {
		if t != nil {
			out = append(out, Entry{At: *t, Source: "incident", Text: text})
		}
	}
	add(inc.AcknowledgedAt, "Taken on")
	add(inc.MitigatedAt, "Mitigated")
	if inc.ResolvedAt != nil {
		add(inc.ResolvedAt, "Resolved ("+strings.ReplaceAll(string(inc.Resolution), "_", " ")+")")
	}
	add(inc.ClosedAt, "Closed")
	return out
}

// Duration words a number of seconds as people say it: 45 s, 12 min, 3 h 20 min, 2 d 4 h.
func Duration(sec int64) string {
	switch {
	case sec < 60:
		return fmt.Sprintf("%d s", sec)
	case sec < 3600:
		return fmt.Sprintf("%d min", sec/60)
	case sec < 86400:
		if m := (sec % 3600) / 60; m > 0 {
			return fmt.Sprintf("%d h %d min", sec/3600, m)
		}
		return fmt.Sprintf("%d h", sec/3600)
	default:
		if h := (sec % 86400) / 3600; h > 0 {
			return fmt.Sprintf("%d d %d h", sec/86400, h)
		}
		return fmt.Sprintf("%d d", sec/86400)
	}
}

func relative(at, start time.Time) string {
	d := start.Sub(at)
	switch {
	case d >= time.Minute:
		return Duration(int64(d/time.Second)) + " before the trouble began"
	case d <= -time.Minute:
		return Duration(int64(-d/time.Second)) + " after the trouble began"
	default:
		return "as the trouble began"
	}
}

func rolloutText(r Rollout, start time.Time) string {
	what := fmt.Sprintf("%s %s/%s rolled out %s", r.Kind, r.Namespace, r.Name, relative(r.At, start))
	if changed := imageChange(r.Before, r.After); changed != "" {
		return what + " (" + changed + ")"
	}
	return what + fmt.Sprintf(" (spec change, generation %d)", r.Generation)
}

func imageChange(before, after []string) string {
	b := map[string]bool{}
	for _, x := range before {
		b[x] = true
	}
	var added []string
	for _, x := range after {
		if !b[x] {
			added = append(added, x)
		}
	}
	if len(added) == 0 {
		return ""
	}
	if len(before) == 1 && len(after) == 1 {
		return "image " + before[0] + " → " + after[0]
	}
	return "new image " + strings.Join(added, ", ")
}

// rolloutScore: a rollout shortly before the trouble is the most common cause there is; one during it is likely the response to it.
func rolloutScore(at, start time.Time) int {
	d := start.Sub(at)
	switch {
	case d >= 0 && d <= Lookback:
		sc := 100 - int(d/time.Minute)*2
		if sc < 50 {
			sc = 50
		}
		return sc
	case d < 0:
		return 25
	}
	return 10
}

type kubeGroup struct {
	reason, typ, message string
	count                uint32
	first, last          time.Time
	objects              []string
}

func (g kubeGroup) text() string {
	objs := g.objects
	more := ""
	if len(objs) > 2 {
		more = fmt.Sprintf(" and %d more", len(objs)-2)
		objs = objs[:2]
	}
	return fmt.Sprintf("%s ×%d on %s%s: %s", g.reason, g.count, strings.Join(objs, ", "), more, g.message)
}

func groupKube(events []KubeEvent, ns, name string, from, to time.Time) []kubeGroup {
	by := map[string]*kubeGroup{}
	var order []string
	for _, e := range events {
		if e.Namespace != ns || !belongs(e.Name, name) || e.Last.Before(from) || e.First.After(to) {
			continue
		}
		key := e.Type + "|" + e.Reason
		g := by[key]
		if g == nil {
			g = &kubeGroup{reason: orDefault(e.Reason, "Event"), typ: e.Type, first: e.First, last: e.Last}
			by[key] = g
			order = append(order, key)
		}
		count := e.Count
		if count == 0 {
			count = 1
		}
		g.count += count
		if e.First.Before(g.first) || g.message == "" {
			g.message = truncate(oneLine(e.Message), maxMessage)
		}
		if e.First.Before(g.first) {
			g.first = e.First
		}
		if e.Last.After(g.last) {
			g.last = e.Last
		}
		obj := strings.ToLower(e.Kind) + " " + e.Name
		if !contains(g.objects, obj) {
			g.objects = append(g.objects, obj)
		}
	}
	out := make([]kubeGroup, 0, len(order))
	for _, k := range order {
		sort.Strings(by[k].objects)
		out = append(out, *by[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].first.Before(out[j].first) })
	return out
}

// how often each event reason is the cause, roughly; a Normal event is context, not a finding, unless it says a container was killed
var kubeReasons = map[string]int{
	"OOMKilling": 90, "OOMKilled": 90, "Evicted": 85, "BackOff": 80, "CrashLoopBackOff": 80, "Failed": 70, "ErrImagePull": 75,
	"ImagePullBackOff": 75, "FailedScheduling": 70, "FailedMount": 65, "FailedAttachVolume": 65, "Unhealthy": 60, "NodeNotReady": 70,
	"FailedCreatePodSandBox": 60, "FailedKillPod": 50, "Preempted": 60, "Killing": 30,
}

func kubeScore(g kubeGroup, start time.Time) int {
	sc, known := kubeReasons[g.reason]
	if !known {
		if g.typ != "Warning" {
			return 0
		}
		sc = 40
	}
	if d := start.Sub(g.first); d >= -After && d <= Lookback {
		sc += 10
	}
	return sc
}

func processText(p ProcessEvent) string {
	where := ""
	if p.Container != "" {
		where = " in " + p.Container
	}
	exe := orDefault(p.Exe, "a process")
	switch p.Kind {
	case ProcessOOMKill:
		return fmt.Sprintf("%s was killed by the OOM killer%s", exe, where)
	case ProcessExitFailed:
		how := fmt.Sprintf("code %d", p.ExitCode)
		if p.Signal != "" {
			how = "signal " + p.Signal
		}
		return fmt.Sprintf("%s exited with %s%s", exe, how, where)
	case ProcessExecNew:
		by := ""
		if p.ParentExe != "" {
			by = ", started by " + p.ParentExe
		}
		return fmt.Sprintf("%s ran for the first time%s%s", exe, where, by)
	case ProcessForkBurst:
		return fmt.Sprintf("%d processes started within a minute%s", p.Count, where)
	}
	return fmt.Sprintf("%s: %s%s", p.Kind, exe, where)
}

var shells = []string{"/sh", "/bash", "/ash", "/dash", "/zsh"}

func processScore(p ProcessEvent, start time.Time) int {
	var sc int
	switch p.Kind {
	case ProcessOOMKill:
		sc = 95
	case ProcessExitFailed:
		sc = 65
	case ProcessForkBurst:
		sc = 60
	case ProcessExecNew:
		sc = 45
		for _, s := range shells {
			if strings.HasSuffix(p.ParentExe, s) {
				sc = 60 // something was run by hand or by a script, not by the application
				break
			}
		}
	default:
		sc = 20
	}
	if d := start.Sub(p.At); d >= -After && d <= Lookback {
		sc += 5
	}
	return sc
}

func headline(inc incident.Incident, s Summary, isWorkload bool, ns, name string) string {
	subject := inc.Title
	if isWorkload {
		subject = fmt.Sprintf("%s (%s/%s)", inc.Title, ns, name)
	}
	sev := "Incident"
	if inc.Severity != "" {
		sev = strings.ToUpper(string(inc.Severity[:1])) + string(inc.Severity[1:])
	}
	var state string
	switch inc.State {
	case incident.Resolved, incident.Closed:
		state = fmt.Sprintf("resolved after %s (%s)", Duration(s.ElapsedSeconds), strings.ReplaceAll(string(inc.Resolution), "_", " "))
	case incident.Open:
		state = fmt.Sprintf("open for %s, not yet taken on", Duration(s.ElapsedSeconds))
	case incident.Mitigated:
		state = fmt.Sprintf("mitigated, %s since it began", Duration(s.ElapsedSeconds))
	default:
		state = fmt.Sprintf("%s, %s since it began", inc.State, Duration(s.ElapsedSeconds))
	}
	line := fmt.Sprintf("%s: %s — %s.", sev, subject, state)
	switch {
	case len(s.Findings) > 0 && s.Findings[0].Score >= 50:
		line += " Most likely related: " + s.Findings[0].Text + "."
	case len(s.Findings) > 0:
		line += " Nearest signal: " + s.Findings[0].Text + "."
	case isWorkload:
		line += " No rollout, Kubernetes warning or unusual process was found around the start."
	}
	return line
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
