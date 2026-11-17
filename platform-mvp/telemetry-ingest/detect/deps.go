package detect

import (
	"fmt"
	"math"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/incident"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/store"
)

// Dependency failure: callers that cannot connect to something they depend on. An application's own requests are judged by the burn-rate
// detector, but a database that has stopped accepting connections is not an HTTP service, and the application in front of it may answer with
// a redirect to an error page, which is not a 5xx. What shows it is the callers' side: the node-agent counts every connect that failed
// (container_net_tcp_failed_connects_total, by the address dialled) and every one that worked.
//
// The address dialled is turned into the application behind it the way the UI's map does it: a pod's address is the application whose container
// listens there (container_net_tcp_listen_info), and a Service's address is learned from the connections that worked, which pair the address
// dialled with the pod reached. That table is learned from the whole look-back, so a Service that has been refusing for an hour is still known:
// it did connect before. An address nothing can be named for (a service outside the cluster) is left out rather than guessed at.
//
// A stretch is where, at each step, the connections of the last window failed in number (MinFailed) and in share (Share). A dependency that was
// deliberately scaled down fails its callers' connections just the same, and is reported: whoever is on call can tell from the incident (which says
// what the cluster collector reports, when there is one) that it was meant, and close it.

const (
	FailedConnectsMetric = "container_net_tcp_failed_connects_total"
	ConnectsMetric       = "container_net_tcp_successful_connects_total"
	ListenMetric         = "container_net_tcp_listen_info"
)

// DepConfig is when failing connections are an incident.
type DepConfig struct {
	WindowSeconds int     // the connections of this long are judged at each step
	MinFailed     float64 // failed connections a window must hold before its share means anything
	Share         float64 // the share of connection attempts that must have failed (0 to 1)
	CriticalShare float64 // at this share the incident is critical
	JoinSeconds   int     // two stretches closer than this are one incident
	MinSeconds    int     // a stretch shorter than this is noise
}

// DepDefaults are the UI's alert rule ("Connections failing"): 20% of attempts, and at least 5 failed, in 5 minutes; critical at 90%.
func DepDefaults() DepConfig {
	return DepConfig{WindowSeconds: 300, MinFailed: 5, Share: 0.2, CriticalShare: 0.9, JoinSeconds: 300, MinSeconds: 120}
}

// Validate says what is unusable in c, or nil.
func (c DepConfig) Validate() error {
	switch {
	case c.WindowSeconds <= 0:
		return fmt.Errorf("the window of failed connections must be positive")
	case !(c.MinFailed > 0):
		return fmt.Errorf("the number of failed connections must be positive")
	case !(c.Share > 0 && c.Share <= 1):
		return fmt.Errorf("the share of failed connections must be above 0 and at most 1, such as 0.2")
	case !(c.CriticalShare >= c.Share && c.CriticalShare <= 1):
		return fmt.Errorf("the critical share must be at least the share and at most 1")
	case c.JoinSeconds < 0 || c.MinSeconds < 0:
		return fmt.Errorf("join and minimum duration must not be negative")
	}
	return nil
}

// ---- which application an address belongs to ----

// currentWithin is how close to the freshest report of an IP two listeners must be to both count as current there: a pod IP that was reused has
// an old pod that stopped reporting and a new one that did not, and only the new one counts. (The UI's map uses the same two minutes.)
const currentWithin = 2 * time.Minute

type claim struct {
	w    Workload
	last int64 // the newest point of the series, in milliseconds
	tie  bool  // two applications claim it at that same moment
}

func vote(m map[string]*claim, key string, w Workload, last int64) {
	cur := m[key]
	switch {
	case cur == nil || last > cur.last:
		m[key] = &claim{w: w, last: last}
	case last == cur.last && cur.w.ID != w.ID:
		cur.tie = true
	}
}

type addrIndex struct {
	exact map[string]*claim            // "ip:port"
	byIP  map[string]map[string]*claim // pods only: ip -> application id -> claim
}

func isUnspecified(ip string) bool { return ip == "" || ip == "0.0.0.0" || ip == "::" }
func isLoopback(ip string) bool {
	return strings.HasPrefix(ip, "127.") || ip == "::1" || ip == "localhost"
}

// splitAddr is the lower-cased host and the port of "ip:port" or "[ip]:port".
func splitAddr(a string) (string, string, bool) {
	h, p, err := net.SplitHostPort(a)
	if err != nil || p == "" {
		return "", "", false
	}
	return strings.ToLower(h), p, true
}

func lastOf(s store.MetricSeries) int64 {
	var last int64
	for _, p := range s.Points {
		last = max(last, p.T)
	}
	return last
}

func newAddrIndex(listen []store.MetricSeries) *addrIndex {
	ix := &addrIndex{exact: map[string]*claim{}, byIP: map[string]map[string]*claim{}}
	for _, s := range listen {
		w, ok := WorkloadOf(s.Labels["container_id"])
		ip, port, good := splitAddr(s.Labels["listen_addr"])
		if !ok || !good || isUnspecified(ip) || isLoopback(ip) || len(s.Points) == 0 {
			continue
		}
		last := lastOf(s)
		vote(ix.exact, ip+":"+port, w, last)
		if w.Host {
			continue
		}
		if ix.byIP[ip] == nil {
			ix.byIP[ip] = map[string]*claim{}
		}
		vote(ix.byIP[ip], w.ID, w, last)
	}
	return ix
}

// resolve is the application the address belongs to: the exact "ip:port" when one application listens there, else (for a pod, whose IP is its
// own) the one application current on that IP. An address that two applications claim at once is nobody's.
func (ix *addrIndex) resolve(addr string) (Workload, bool) {
	ip, port, ok := splitAddr(addr)
	if !ok || isUnspecified(ip) || isLoopback(ip) {
		return Workload{}, false
	}
	if c := ix.exact[ip+":"+port]; c != nil {
		return c.w, !c.tie
	}
	apps := ix.byIP[ip]
	var freshest int64
	for _, c := range apps {
		freshest = max(freshest, c.last)
	}
	var only *claim
	for _, c := range apps {
		if c.last < freshest-currentWithin.Milliseconds() {
			continue
		}
		if only != nil {
			return Workload{}, false
		}
		only = c
	}
	if only == nil {
		return Workload{}, false
	}
	return only.w, true
}

// learnServices is the application each Service address leads to, from the connections that worked: each pairs the address dialled with the
// pod reached. The address with the most connections to an application names it.
func learnServices(ok []store.MetricSeries, ix *addrIndex) map[string]Workload {
	type tally struct {
		w      Workload
		weight float64
	}
	votes := map[string]map[string]*tally{}
	for _, s := range ok {
		dest, actual := s.Labels["destination"], s.Labels["actual_destination"]
		if dest == "" || actual == "" || dest == actual {
			continue
		}
		if dip, _, good := splitAddr(dest); !good || isLoopback(dip) {
			continue
		}
		w, found := ix.resolve(actual)
		if !found {
			continue
		}
		var rate float64
		for _, p := range s.Points {
			rate += max(p.V, 0)
		}
		if votes[dest] == nil {
			votes[dest] = map[string]*tally{}
		}
		t := votes[dest][w.ID]
		if t == nil {
			t = &tally{w: w}
			votes[dest][w.ID] = t
		}
		t.weight += rate + 1e-9 // a series that exists is a vote even at a rate that rounds to nothing
	}
	out := map[string]Workload{}
	for dest, by := range votes {
		var best *tally
		for _, t := range by {
			if best == nil || t.weight > best.weight || (t.weight == best.weight && t.w.ID < best.w.ID) {
				best = t
			}
		}
		out[dest] = best.w
	}
	return out
}

// ---- what failed, laid out on the grid ----

// Dep is an application that others connect to, and how those connections went at each step (per second).
type Dep struct {
	ID, Label    string
	Failed, Ok   []float64
	CallerFailed map[string][]float64 // application id of a caller -> its failed connections per second
}

// CallerCount is a caller and how many of its connections failed.
type CallerCount struct {
	ID, Label string
	Failed    float64
}

// labelOfApp is "shop/cart" for a Kubernetes application, the last part of the path for a process on a host.
func labelOfApp(id string) string {
	if rest, ok := strings.CutPrefix(id, "/k8s/"); ok {
		return rest
	}
	if rest, ok := strings.CutPrefix(id, "/k8s-cronjob/"); ok {
		return rest
	}
	if w, ok := hostWorkload(id); ok {
		return w.Label
	}
	return id
}

func namespaceOf(id string) string {
	for _, p := range []string{"/k8s/", "/k8s-cronjob/"} {
		if rest, ok := strings.CutPrefix(id, p); ok {
			ns, _, _ := strings.Cut(rest, "/")
			return ns
		}
	}
	return ""
}

// BuildDeps lays the connections of the grid out by the application that was dialled. Only applications with a failed connection are returned.
// exclude, when set, names the namespaces whose applications are not judged (the platform's own).
func BuildDeps(failed, ok, listen []store.MetricSeries, g Grid, exclude *regexp.Regexp) []Dep {
	if g.N <= 0 || g.Step <= 0 {
		return nil
	}
	ix := newAddrIndex(listen)
	services := learnServices(ok, ix)
	deps := map[string]*Dep{}
	zeros := func() []float64 { return make([]float64, g.N) }
	get := func(w Workload) *Dep {
		d := deps[w.ID]
		if d == nil {
			d = &Dep{ID: w.ID, Label: w.Label, Failed: zeros(), Ok: zeros(), CallerFailed: map[string][]float64{}}
			deps[w.ID] = d
		}
		return d
	}
	add := func(dst []float64, s store.MetricSeries) {
		for _, p := range s.Points {
			i := int(math.Floor((float64(p.T)/1000 - float64(g.Start)) / float64(g.Step)))
			if i >= 0 && i < g.N && p.V > 0 {
				dst[i] += p.V
			}
		}
	}
	judged := func(callee, caller Workload) bool {
		if callee.ID == caller.ID {
			return false // an application connecting to itself (its replicas to each other) is not a dependency
		}
		return exclude == nil || callee.Host || !exclude.MatchString(namespaceOf(callee.ID))
	}
	reach := func(addr string) (Workload, bool) {
		if w, found := ix.resolve(addr); found {
			return w, true
		}
		w, found := services[addr]
		return w, found
	}

	for _, s := range ok {
		caller, cok := WorkloadOf(s.Labels["container_id"])
		if !cok {
			continue
		}
		callee, found := Workload{}, false
		if actual := s.Labels["actual_destination"]; actual != "" {
			callee, found = reach(actual)
		}
		if !found {
			callee, found = reach(s.Labels["destination"])
		}
		if found && judged(callee, caller) {
			add(get(callee).Ok, s)
		}
	}
	for _, s := range failed {
		caller, cok := WorkloadOf(s.Labels["container_id"])
		if !cok {
			continue
		}
		callee, found := reach(s.Labels["destination"])
		if !found || !judged(callee, caller) {
			continue
		}
		d := get(callee)
		add(d.Failed, s)
		per := d.CallerFailed[caller.ID]
		if per == nil {
			per = zeros()
			d.CallerFailed[caller.ID] = per
		}
		add(per, s)
	}

	var out []Dep
	for _, d := range deps {
		if sumAll(d.Failed) > 0 {
			out = append(out, *d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func sumAll(a []float64) float64 {
	var s float64
	for _, v := range a {
		s += v
	}
	return s
}

// CallersIn is the callers whose connections failed between two steps (inclusive), the busiest first.
func (d Dep) CallersIn(first, last, step int) []CallerCount {
	var out []CallerCount
	for id, per := range d.CallerFailed {
		if n := sum(per, first, last) * float64(step); n > 0 {
			out = append(out, CallerCount{ID: id, Label: labelOfApp(id), Failed: n})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Failed != out[j].Failed {
			return out[i].Failed > out[j].Failed
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ---- stretches ----

// DepBreach is a stretch in which the connections to a dependency failed: the first and last step that had a failed connection, and what was
// counted across it.
type DepBreach struct {
	Breach
	Failed, Attempts float64 // connections across the breach
	PeakShare        float64 // the highest share of a window's attempts that failed
}

func windowShare(d Dep, i, steps, step int) (failed, attempts float64) {
	failed = sum(d.Failed, i-steps+1, i) * float64(step)
	return failed, failed + sum(d.Ok, i-steps+1, i)*float64(step)
}

// FindDepBreaches is the stretches of one dependency over the grid, oldest first.
func FindDepBreaches(d Dep, g Grid, c DepConfig) []DepBreach {
	if g.N <= 0 || g.Step <= 0 || len(d.Failed) < g.N || len(d.Ok) < g.N {
		return nil
	}
	win := max(roundSteps(c.WindowSeconds, g.Step), 1)
	join := roundSteps(c.JoinSeconds, g.Step)
	share := func(i int) float64 {
		f, a := windowShare(d, i, win, g.Step)
		if f < c.MinFailed || a <= 0 {
			return 0
		}
		return f / a
	}
	hot := make([]bool, g.N)
	for i := range hot {
		hot[i] = share(i) >= c.Share
	}
	var out []DepBreach
	for _, s := range stretches(hot, join) {
		first, last := s[0], s[1]
		// the window that judged it reaches past where the failures began and ended: the stretch is the steps that had a failed connect
		for first-1 >= max(s[0]-win, 0) && d.Failed[first-1] > 0 {
			first--
		}
		for first < last && d.Failed[first] <= 0 {
			first++
		}
		for last > first && d.Failed[last] <= 0 {
			last--
		}
		if (last-first+1)*g.Step < c.MinSeconds {
			continue
		}
		b := DepBreach{Breach: Breach{
			First: first, Last: last,
			Start: time.Unix(g.Start+int64(first*g.Step), 0).UTC(),
			End:   time.Unix(g.Start+int64((last+1)*g.Step), 0).UTC(),
			Open:  last >= g.N-1,
		}}
		b.Failed = sum(d.Failed, first, last) * float64(g.Step)
		b.Attempts = b.Failed + sum(d.Ok, first, last)*float64(g.Step)
		for i := first; i <= last; i++ {
			b.PeakShare = max(b.PeakShare, share(i))
		}
		out = append(out, b)
	}
	return out
}

// ---- the incident ----

// DepKey names a breach so that finding it twice opens it once: "deps:", the application and when it began. The prefix is also what tells it from
// the other kinds of incident the detector opens (see familyOf).
func DepKey(app string, start time.Time) string {
	return "deps:" + DedupeKey(app, start)[len("detect:"):]
}

// DescribeDep is the incident a breach becomes. note, when it is not empty, is put after what was measured: what else is known of the dependency.
func DescribeDep(d Dep, b DepBreach, c DepConfig, g Grid, note string) incident.CreateInput {
	sev := incident.Warning
	if b.PeakShare >= c.CriticalShare {
		sev = incident.Critical
	}
	span := b.End.Sub(b.Start).Round(time.Minute)
	state := "ended " + b.End.Format("15:04") + " UTC"
	if b.Open {
		state = "still going"
	}
	callers := d.CallersIn(b.First, b.Last, g.Step)
	var named []string
	for i, cc := range callers {
		if i == 3 {
			break
		}
		named = append(named, fmt.Sprintf("%s (%s)", cc.Label, trim(math.Round(cc.Failed))))
	}
	who := strings.Join(named, ", ")
	if len(callers) > 3 {
		who += fmt.Sprintf(" and %d more", len(callers)-3)
	}
	plural := "s"
	if len(callers) == 1 {
		plural = ""
	}
	pct := 0.0
	if b.Attempts > 0 {
		pct = 100 * b.Failed / b.Attempts
	}
	lines := []string{
		fmt.Sprintf("Opened automatically. Connections to %s have been failing from %s UTC (%s, %s).", d.Label, b.Start.Format("15:04"), span, state),
		fmt.Sprintf("%s of %s connection attempts failed (%s%%) from %d caller%s: %s.", trim(math.Round(b.Failed)), trim(math.Round(b.Attempts)), trim(math.Round(pct)), len(callers), plural, who),
	}
	if note != "" {
		lines = append(lines, note)
	}
	return incident.CreateInput{
		Title:     fmt.Sprintf("%s: connections failing", d.Label),
		App:       d.ID,
		Severity:  sev,
		Source:    incident.Detected,
		DedupeKey: DepKey(d.ID, b.Start),
		Summary:   strings.Join(lines, "\n"),
		StartedAt: b.Start,
	}
}
