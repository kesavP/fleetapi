package detect

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/incident"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/rules"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/store"
)

// Right-sizing: a container that was given far more than it uses. The node-agent sends what a container USES (memory, CPU, a volume's fill) and what
// it was GIVEN (its limit), and a container whose use stayed under a share of its limit for the whole window is over-provisioned: the room it was given
// is room nothing else can have, and is paid for.
//
// What "use" means is the part that decides whether a suggestion is safe. Use is read as the PEAK of each step of the window (a gauge's highest reading
// in the step; for CPU, the cores used on average over the step, as a counter can only say that), and a rule judges by a percentile of those peaks: the
// 95th means "the use that was exceeded only 5% of the time", and 100 the highest there was. A pod that is not watched for most of the window is not
// judged (it may simply be new), nor is a limit too small to matter. And a suggestion is only that: a rare large request, a start-up or a monthly batch
// is not in a window of days, so the incident says what was measured and for how long, and leaves the decision to a person.

// The metrics the node-agent sends, per resource: what is used, and what it was given.
const (
	MemoryUsageMetric = "container_resources_memory_rss_bytes"
	MemoryLimitMetric = "container_resources_memory_limit_bytes"
	CPUUsageMetric    = "container_resources_cpu_usage_seconds_total"
	CPULimitMetric    = "container_resources_cpu_limit_cores"
	DiskUsedMetric    = "container_resources_disk_used_bytes"
	DiskSizeMetric    = "container_resources_disk_size_bytes"
)

// Resource is what is measured.
type Resource string

const (
	Memory  Resource = "memory"
	CPU     Resource = "cpu"
	Storage Resource = "storage"
)

// Label is the resource as a person writes it.
func (r Resource) Label() string {
	if r == CPU {
		return "CPU"
	}
	return string(r)
}

// RuleName is the rule that governs the resource.
func (r Resource) RuleName() string { return "rightsize." + string(r) }

// Headroom is what a suggested limit leaves above the use it was worked out from: 30%.
const Headroom = 0.3

// minLimit is the smallest limit worth a suggestion: below it there is too little to give back.
var minLimit = map[Resource]float64{Memory: 32 << 20, CPU: 0.05, Storage: 100 << 20}

// tidy is what a suggestion is rounded up to.
var tidy = map[Resource]float64{Memory: 16 << 20, CPU: 0.05, Storage: 1 << 30}

// coverage is how much of the window a container must have been watched for to be judged: four fifths.
const coverageNum, coverageDen = 4, 5

// Use is one container (or volume) of one application, over the window.
type Use struct {
	App, Label string
	Part       string // the container's name, or the volume's
	Resource   Resource
	Limit      float64 // what it was given (the highest of its pods')
	// AtPercentile is the use at the rule's percentile, of the busiest pod's peak in each step; Peak is the highest there was. Both in the unit of Limit.
	AtPercentile, Peak float64
	Covered, Steps     int // steps with a reading, of the steps of the window
}

// Share is the use as a share of the limit, from 0.
func (u Use) Share() float64 {
	if u.Limit <= 0 {
		return 0
	}
	return u.AtPercentile / u.Limit
}

// Young says the container was watched for too little of the window to be judged.
func (u Use) Young() bool { return u.Covered*coverageDen < u.Steps*coverageNum }

// Suggested is a limit that would still leave Headroom above the use, rounded up to something tidy, and never more than it has.
func (u Use) Suggested() float64 {
	step := tidy[u.Resource]
	if step <= 0 {
		return u.Limit
	}
	s := math.Ceil(u.AtPercentile*(1+Headroom)/step-1e-9) * step
	return math.Min(s, u.Limit)
}

// Judge says whether a rule calls the use over-provisioned: it is on, the container was watched long enough, the limit is worth something, and the
// use stayed strictly below the rule's percent of it.
func Judge(u Use, s rules.Settings) bool {
	return s.Enabled && !u.Young() && u.Limit >= minLimit[u.Resource] && u.Share() < s.Percent/100
}

// RightsizeStep is the step, in seconds, a window of that many days is read at: a window ten times as long is read at a step ten times as long, so that
// the query stays about 288 points a series (up to two hours a step, at which a month is 360).
func RightsizeStep(days int) int { return min(max(days, 1)*300, 7200) }

// partOf is the last part of a container id: the container's name.
func partOf(containerID string) string {
	if i := strings.LastIndex(containerID, "/"); i >= 0 {
		return containerID[i+1:]
	}
	return containerID
}

// key says which pod a series belongs to, and which container or volume of its application it is: false when it has none (a container's own
// filesystem, which is not a volume, or something that is no application).
func (r Resource) key(labels map[string]string) (pod string, w Workload, part string, ok bool) {
	id := labels["container_id"]
	w, found := WorkloadOf(id)
	if !found {
		return "", Workload{}, "", false
	}
	if r == Storage {
		vol := labels["volume"]
		if vol == "" {
			return "", Workload{}, "", false
		}
		return id + "|" + vol + "|" + labels["mount_point"], w, vol, true
	}
	return id, w, partOf(id), true
}

// place is the series' readings by step of the grid: NaN where a step has none.
func place(s store.MetricSeries, g Grid) []float64 {
	out := make([]float64, g.N)
	for i := range out {
		out[i] = math.NaN()
	}
	for _, p := range s.Points {
		i := int(math.Floor((float64(p.T)/1000 - float64(g.Start)) / float64(g.Step)))
		if i >= 0 && i < g.N && !math.IsNaN(p.V) {
			out[i] = p.V
		}
	}
	return out
}

// MeasureUse is the use of every container (or volume) over the grid: the busiest pod's use of its limit at each step, and the percentile of those.
// `usage` and `limit` are the two metrics of the resource (CPU's usage as a rate: cores). Nothing is returned for a container with no limit, as there
// is nothing to compare with; exclude, when set, names the namespaces to leave out.
func MeasureUse(res Resource, usage, limit []store.MetricSeries, g Grid, s rules.Settings, exclude *regexp.Regexp) []Use {
	if g.N <= 0 || g.Step <= 0 {
		return nil
	}
	limits := map[string][]float64{}
	for _, l := range limit {
		if pod, _, _, ok := res.key(l.Labels); ok {
			limits[pod] = place(l, g)
		}
	}
	type acc struct {
		w     Workload
		part  string
		ratio []float64 // the busiest pod's share at each step; NaN where none was read
		limit float64
	}
	by := map[string]*acc{}
	for _, u := range usage {
		pod, w, part, ok := res.key(u.Labels)
		if !ok || len(u.Points) == 0 {
			continue
		}
		if exclude != nil && !w.Host && exclude.MatchString(namespaceOf(w.ID)) {
			continue
		}
		lim, have := limits[pod]
		if !have {
			continue
		}
		a := by[w.ID+"|"+part]
		if a == nil {
			a = &acc{w: w, part: part, ratio: make([]float64, g.N)}
			for i := range a.ratio {
				a.ratio[i] = math.NaN()
			}
			by[w.ID+"|"+part] = a
		}
		use := place(u, g)
		known := math.NaN() // a limit holds until a new one is reported
		for i := 0; i < g.N; i++ {
			if !math.IsNaN(lim[i]) {
				known = lim[i]
			}
			if math.IsNaN(use[i]) || math.IsNaN(known) || known <= 0 {
				continue
			}
			r := use[i] / known
			if math.IsNaN(a.ratio[i]) || r > a.ratio[i] {
				a.ratio[i] = r
			}
			a.limit = math.Max(a.limit, known)
		}
	}

	var out []Use
	for _, a := range by {
		var seen []float64
		for _, r := range a.ratio {
			if !math.IsNaN(r) {
				seen = append(seen, r)
			}
		}
		if len(seen) == 0 || a.limit <= 0 {
			continue
		}
		sort.Float64s(seen)
		k := int(math.Ceil(s.Percentile*float64(len(seen))/100 - 1e-9)) // nearest rank
		k = min(max(k, 1), len(seen))
		out = append(out, Use{
			App: a.w.ID, Label: a.w.Label, Part: a.part, Resource: res, Limit: a.limit,
			AtPercentile: seen[k-1] * a.limit, Peak: seen[len(seen)-1] * a.limit, Covered: len(seen), Steps: g.N,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].App != out[j].App {
			return out[i].App < out[j].App
		}
		return out[i].Part < out[j].Part
	})
	return out
}

// ---- words ----

// bytesText is a size as a person reads it: "1.5 GiB", "640 MiB".
func bytesText(b float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for b >= 1024 && i < len(units)-1 {
		b /= 1024
		i++
	}
	text := strconv.FormatFloat(math.Round(b*10)/10, 'f', -1, 64)
	return text + " " + units[i]
}

// coresText is CPU as a person reads it: "250m" below a core, "1 core", "1.5 cores".
func coresText(c float64) string {
	if c < 1 {
		return strconv.FormatFloat(math.Round(c*1000), 'f', -1, 64) + "m"
	}
	text := strconv.FormatFloat(math.Round(c*100)/100, 'f', -1, 64)
	if text == "1" {
		return "1 core"
	}
	return text + " cores"
}

func amountText(r Resource, v float64) string {
	if r == CPU {
		return coresText(v)
	}
	return bytesText(v)
}

func percentText(share float64) string {
	return strconv.FormatFloat(math.Round(share*100), 'f', -1, 64) + "%"
}

func ordinal(n int) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return strconv.Itoa(n) + suffix
}

// RightsizeKey names an over-provisioned application so that finding it twice in a day opens it once: "rightsize:", the resource, the application
// and the day. The prefix is what tells it from the other kinds of incident (see familyOf), and the resource is in it so that memory and CPU are two.
func RightsizeKey(res Resource, app string, day time.Time) string {
	h := sha256.Sum256([]byte(app))
	return "rightsize:" + string(res) + ":" + hex.EncodeToString(h[:6]) + ":" + strconv.FormatInt(day.UTC().Unix()/86400, 10)
}

// listed is how many containers an incident names; the rest are counted.
const listed = 5

// DescribeRightsize is the incident an over-provisioned application becomes, for one resource: what was measured, over how long, and a limit to try.
// It is information (severity info), never urgent. `step` is the step the window was read at, in seconds.
func DescribeRightsize(app, label string, res Resource, uses []Use, s rules.Settings, step int, now time.Time) incident.CreateInput {
	sorted := append([]Use(nil), uses...)
	sort.Slice(sorted, func(i, j int) bool {
		if a, b := sorted[i].Share(), sorted[j].Share(); a != b {
			return a < b
		}
		return sorted[i].Part < sorted[j].Part
	})
	judged := "the " + ordinal(int(s.Percentile)) + " percentile of"
	if s.Percentile >= 100 {
		judged = "the highest use, taken from"
	}
	noun, given, thing := "container", "limit", "limit"
	if res == Storage {
		noun, given, thing = "volume", "size", "size"
	}
	what := "use of its " + res.Label() + " " + thing
	if res == Storage {
		what = "use of its volumes"
	}
	lines := []string{fmt.Sprintf("Opened automatically. Over the last %d days, %s's %s stayed under %s (%s the peak of each %d minutes).",
		s.Days, label, what, strconv.FormatFloat(s.Percent, 'f', -1, 64)+"%", judged, step/60)}
	for i, u := range sorted {
		if i == listed {
			lines = append(lines, fmt.Sprintf("and %d more.", len(sorted)-listed))
			break
		}
		at := "use at the " + ordinal(int(s.Percentile)) + " percentile"
		if s.Percentile >= 100 {
			at = "highest use"
		}
		line := fmt.Sprintf("- %s %s: %s %s; %s %s (%s)", noun, u.Part, given, amountText(res, u.Limit), at, percentText(u.Share()), amountText(res, u.AtPercentile))
		if s.Percentile < 100 {
			line += fmt.Sprintf(", highest %s (%s)", percentText(u.Peak/u.Limit), amountText(res, u.Peak))
		}
		line += fmt.Sprintf(". A %s of about %s would still leave %d%% headroom.", thing, amountText(res, u.Suggested()), int(Headroom*100))
		lines = append(lines, line)
	}
	if res == Storage {
		lines = append(lines, "Note: a volume usually cannot be made smaller, so this is for the next one. A suggestion from usage alone: check what the volume is for (growth, snapshots, a database's headroom) before choosing a size.")
	} else {
		lines = append(lines, "This is a suggestion from usage alone: before lowering a limit, check for rare large requests, start-up and batch peaks that the window did not cover.")
	}
	return incident.CreateInput{
		Title:     fmt.Sprintf("%s: %s over-provisioned", label, res.Label()),
		App:       app,
		Severity:  incident.Info,
		Source:    incident.Detected,
		DedupeKey: RightsizeKey(res, app, now),
		Summary:   strings.Join(lines, "\n"),
		StartedAt: now,
	}
}
