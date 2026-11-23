package detect

import (
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/incident"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/rules"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/store"
)

const (
	gib = float64(1 << 30)
	mib = float64(1 << 20)
)

var (
	p95  = rules.Settings{Enabled: true, Percent: 50, Days: 7, Percentile: 95}
	p100 = rules.Settings{Enabled: true, Percent: 50, Days: 7, Percentile: 100}
)

func use(container string, f func(i int) float64) store.MetricSeries {
	return connSeries(lab("container_id", container), f)
}

// ratio: a step's use as a share of 1 GiB, for steps 0 to 59 (so the first is 1%, and the last 60%, at a scale of 0.01).
func ramp(scale float64) func(int) float64 {
	return func(i int) float64 { return float64(i+1) * scale * gib }
}

func useOf(t *testing.T, uses []Use, part string) Use {
	t.Helper()
	for _, u := range uses {
		if u.Part == part {
			return u
		}
	}
	t.Fatalf("no use for %q among %+v", part, uses)
	return Use{}
}

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b)) }

func TestAContainersUseIsJudgedByTheStepsItPeakedInAndAPercentileOfThem(t *testing.T) {
	usage := []store.MetricSeries{use(webPod, ramp(0.01))} // 1% in the first step, 60% in the last
	limit := []store.MetricSeries{use(webPod, always(gib))}
	cases := []struct {
		s    rules.Settings
		want float64
	}{
		{p95, 0.57}, // the 57th of 60 steps, lowest first: ceil(0.95 * 60)
		{rules.Settings{Enabled: true, Percent: 50, Days: 7, Percentile: 50}, 0.30},
		{p100, 0.60},
	}
	for _, c := range cases {
		uses := MeasureUse(Memory, usage, limit, depGrid, c.s, nil)
		if len(uses) != 1 {
			t.Fatalf("uses = %+v", uses)
		}
		u := uses[0]
		if u.App != webApp || u.Label != "shop/web" || u.Part != "app" || u.Resource != Memory {
			t.Fatalf("use = %+v", u)
		}
		if !near(u.Share(), c.want) || !near(u.Peak, 0.60*gib) || u.Limit != gib || !near(u.AtPercentile, c.want*gib) {
			t.Errorf("percentile %v: share %v peak %v limit %v at %v, want share %v", c.s.Percentile, u.Share(), u.Peak, u.Limit, u.AtPercentile, c.want)
		}
		if u.Covered != 60 || u.Steps != 60 {
			t.Errorf("covered %d of %d", u.Covered, u.Steps)
		}
	}
}

func TestTheBusiestPodOfAnApplicationSetsEachStep(t *testing.T) {
	quiet := use(webPod, always(0.1*gib))
	spike := use("/k8s/shop/web-5d6f8c7b9-zzzzz/app", func(i int) float64 {
		if i == 10 {
			return 0.5 * gib
		}
		return 0.1 * gib
	})
	limit := []store.MetricSeries{use(webPod, always(gib)), use("/k8s/shop/web-5d6f8c7b9-zzzzz/app", always(gib))}
	uses := MeasureUse(Memory, []store.MetricSeries{quiet, spike}, limit, depGrid, p100, nil)
	if len(uses) != 1 || !near(uses[0].Share(), 0.5) {
		t.Fatalf("uses = %+v: one pod spiking is the application spiking, at the highest", uses)
	}
	// at the 95th percentile one spike in sixty steps is not what the application usually needs
	uses = MeasureUse(Memory, []store.MetricSeries{quiet, spike}, limit, depGrid, p95, nil)
	if !near(uses[0].Share(), 0.1) {
		t.Fatalf("share at the 95th = %v", uses[0].Share())
	}
}

func TestTheContainersOfOnePodAreJudgedApart(t *testing.T) {
	usage := []store.MetricSeries{use("/k8s/shop/web-5d6f8c7b9-xk2lp/app", always(0.2*gib)), use("/k8s/shop/web-5d6f8c7b9-xk2lp/proxy", always(0.9*gib))}
	limit := []store.MetricSeries{use("/k8s/shop/web-5d6f8c7b9-xk2lp/app", always(gib)), use("/k8s/shop/web-5d6f8c7b9-xk2lp/proxy", always(gib))}
	uses := MeasureUse(Memory, usage, limit, depGrid, p95, nil)
	if len(uses) != 2 || !near(useOf(t, uses, "app").Share(), 0.2) || !near(useOf(t, uses, "proxy").Share(), 0.9) {
		t.Fatalf("uses = %+v", uses)
	}
}

func TestWithoutALimitThereIsNothingToCompareWith(t *testing.T) {
	usage := []store.MetricSeries{use(webPod, always(0.2*gib))}
	if uses := MeasureUse(Memory, usage, nil, depGrid, p95, nil); len(uses) != 0 {
		t.Fatalf("no limit series: %+v", uses)
	}
	if uses := MeasureUse(Memory, usage, []store.MetricSeries{use(webPod, always(0))}, depGrid, p95, nil); len(uses) != 0 {
		t.Fatalf("a limit of zero is no limit: %+v", uses)
	}
	if uses := MeasureUse(Memory, usage, []store.MetricSeries{use(apiPod, always(gib))}, depGrid, p95, nil); len(uses) != 0 {
		t.Fatalf("another container's limit: %+v", uses)
	}
}

func TestALimitThatWasReportedOnceHoldsForTheStepsThatHaveNone(t *testing.T) {
	limit := []store.MetricSeries{serRange(lab("container_id", webPod), 0, 1, always(gib))}
	uses := MeasureUse(Memory, []store.MetricSeries{use(webPod, always(0.2*gib))}, limit, depGrid, p95, nil)
	if len(uses) != 1 || !near(uses[0].Share(), 0.2) || uses[0].Covered != 60 {
		t.Fatalf("uses = %+v", uses)
	}
}

func TestAContainerWatchedForPartOfTheWindowSaysSoAndIsNotJudged(t *testing.T) {
	half := serRange(lab("container_id", webPod), 30, 60, always(0.1*gib))
	uses := MeasureUse(Memory, []store.MetricSeries{half}, []store.MetricSeries{use(webPod, always(gib))}, depGrid, p95, nil)
	if len(uses) != 1 || uses[0].Covered != 30 || !uses[0].Young() {
		t.Fatalf("uses = %+v", uses)
	}
	if Judge(uses[0], p95) {
		t.Fatal("a container watched for half the window was judged over-provisioned")
	}
	if full := MeasureUse(Memory, []store.MetricSeries{use(webPod, always(0.1*gib))}, []store.MetricSeries{use(webPod, always(gib))}, depGrid, p95, nil); full[0].Young() {
		t.Fatal("a container watched all along is young")
	}
	// the share is four fifths: 48 of 60 steps is enough, 47 is not
	for covered, young := range map[int]bool{48: false, 47: true} {
		u := MeasureUse(Memory, []store.MetricSeries{serRange(lab("container_id", webPod), 60-covered, 60, always(0.1*gib))}, []store.MetricSeries{use(webPod, always(gib))}, depGrid, p95, nil)[0]
		if u.Young() != young {
			t.Errorf("%d of 60 steps: young = %v", covered, u.Young())
		}
	}
}

func TestStorageIsJudgedPerVolumeAndOnlyForVolumes(t *testing.T) {
	vol := func(container, volume string, f func(int) float64) store.MetricSeries {
		return connSeries(lab("container_id", container, "volume", volume, "mount_point", "/data", "device", "sda"), f)
	}
	usage := []store.MetricSeries{
		vol(mongoPod, "data", always(10*gib)), vol(mongoPod2, "data", always(12*gib)), vol(mongoPod, "logs", always(1*gib)),
		connSeries(lab("container_id", mongoPod, "mount_point", "/", "device", "overlay"), always(5*gib)), // the container's own filesystem: no volume
	}
	size := []store.MetricSeries{
		vol(mongoPod, "data", always(100*gib)), vol(mongoPod2, "data", always(100*gib)), vol(mongoPod, "logs", always(10*gib)),
		connSeries(lab("container_id", mongoPod, "mount_point", "/", "device", "overlay"), always(200*gib)),
	}
	uses := MeasureUse(Storage, usage, size, depGrid, p100, nil)
	if len(uses) != 2 {
		t.Fatalf("uses = %+v: the two volumes, and not the container's own filesystem", uses)
	}
	data, logs := useOf(t, uses, "data"), useOf(t, uses, "logs")
	if data.App != mongoApp || !near(data.Share(), 0.12) || data.Limit != 100*gib || !near(logs.Share(), 0.1) {
		t.Fatalf("data %+v logs %+v: the volume of two pods is judged at the fullest", data, logs)
	}
}

func TestCPUIsJudgedAsCoresInUseAgainstTheLimitInCores(t *testing.T) {
	uses := MeasureUse(CPU, []store.MetricSeries{use(webPod, always(0.1))}, []store.MetricSeries{use(webPod, always(1))}, depGrid, p95, nil)
	if len(uses) != 1 || uses[0].Resource != CPU || !near(uses[0].Share(), 0.1) || uses[0].Limit != 1 {
		t.Fatalf("uses = %+v", uses)
	}
}

func TestNamespacesLeftOutAreNotMeasured(t *testing.T) {
	kube := "/k8s/kube-system/coredns-5d78c9869d-bcdfg/coredns"
	usage := []store.MetricSeries{use(kube, always(0.1*gib)), use(webPod, always(0.1*gib))}
	limit := []store.MetricSeries{use(kube, always(gib)), use(webPod, always(gib))}
	uses := MeasureUse(Memory, usage, limit, depGrid, p95, regexp.MustCompile(`^kube-`))
	if len(uses) != 1 || uses[0].App != webApp {
		t.Fatalf("uses = %+v", uses)
	}
}

func TestAUsageWithNoApplicationOrNoReadingsIsLeftOut(t *testing.T) {
	usage := []store.MetricSeries{use("/", always(0.1*gib)), {Labels: lab("container_id", webPod)}}
	limit := []store.MetricSeries{use("/", always(gib)), use(webPod, always(gib))}
	if uses := MeasureUse(Memory, usage, limit, depGrid, p95, nil); len(uses) != 0 {
		t.Fatalf("uses = %+v", uses)
	}
	if uses := MeasureUse(Memory, usage, limit, Grid{}, p95, nil); len(uses) != 0 {
		t.Fatalf("an empty grid: %+v", uses)
	}
}

func TestAReadingThatIsNotANumberIsNotAReading(t *testing.T) {
	usage := []store.MetricSeries{use(webPod, func(i int) float64 {
		if i < 30 {
			return math.NaN()
		}
		return 0.1 * gib
	})}
	uses := MeasureUse(Memory, usage, []store.MetricSeries{use(webPod, always(gib))}, depGrid, p95, nil)
	if len(uses) != 1 || uses[0].Covered != 30 || !near(uses[0].Share(), 0.1) {
		t.Fatalf("uses = %+v", uses)
	}
}

// ---- when it is over-provisioned ----

func over(res Resource, limit, share float64) Use {
	return Use{App: webApp, Label: "shop/web", Part: "app", Resource: res, Limit: limit, AtPercentile: share * limit, Peak: share * limit, Covered: 60, Steps: 60}
}

func TestOverProvisionedMeansStrictlyBelowThePercentOfTheLimit(t *testing.T) {
	if !Judge(over(Memory, gib, 0.49), p95) {
		t.Error("49% is below 50%")
	}
	if Judge(over(Memory, gib, 0.50), p95) {
		t.Error("exactly 50% is not below 50%")
	}
	if Judge(over(Memory, gib, 0.80), p95) {
		t.Error("80% is busy")
	}
	off := p95
	off.Enabled = false
	if Judge(over(Memory, gib, 0.1), off) {
		t.Error("a rule that is switched off judged something")
	}
	tight := p95
	tight.Percent = 20
	if Judge(over(Memory, gib, 0.3), tight) || !Judge(over(Memory, gib, 0.1), tight) {
		t.Error("the percent of the rule is not the one used")
	}
}

func TestALimitTooSmallToMatterIsLeftAlone(t *testing.T) {
	for name, u := range map[string]Use{
		"31 MiB of memory": over(Memory, 31*mib, 0.1),
		"40m of CPU":       over(CPU, 0.04, 0.1),
		"99 MiB of volume": over(Storage, 99*mib, 0.1),
	} {
		if Judge(u, p95) {
			t.Errorf("%s was judged", name)
		}
	}
	for name, u := range map[string]Use{
		"32 MiB of memory":  over(Memory, 32*mib, 0.1),
		"50m of CPU":        over(CPU, 0.05, 0.1),
		"100 MiB of volume": over(Storage, 100*mib, 0.1),
	} {
		if !Judge(u, p95) {
			t.Errorf("%s was not judged", name)
		}
	}
}

func TestTheSuggestionLeavesHeadroomAndIsRoundedUpToSomethingTidy(t *testing.T) {
	cases := []struct {
		u    Use
		want float64
	}{
		{over(Memory, gib, 640.0/1024), 832 * mib}, // 640 MiB x 1.3 = 832 MiB, a multiple of 16 MiB already
		{over(Memory, gib, 0.3), 400 * mib},        // 307.2 x 1.3 = 399.4, up to 400
		{over(CPU, 1, 0.1), 0.15},                  // 0.13 up to the next 50m
		{over(Storage, 100*gib, 0.12), 16 * gib},   // 12 x 1.3 = 15.6, up to the next GiB
	}
	for _, c := range cases {
		if got := c.u.Suggested(); !near(got, c.want) {
			t.Errorf("%s: suggested %v, want %v", c.u.Resource, got, c.want)
		}
	}
	// a suggestion is never more than what it has
	if u := over(Memory, gib, 0.9); u.Suggested() > gib {
		t.Errorf("suggested %v for a limit of %v", u.Suggested(), gib)
	}
}

func TestTheStepGrowsWithTheWindowSoThatTheQueryStaysTheSameSize(t *testing.T) {
	for days, want := range map[int]int{1: 300, 7: 2100, 14: 4200, 30: 7200} {
		if got := RightsizeStep(days); got != want {
			t.Errorf("%d days: step %d, want %d", days, got, want)
		}
	}
	for days := 1; days <= rules.MaxDays; days++ {
		if s := RightsizeStep(days); s%300 != 0 || s < 300 || s > 7200 {
			t.Errorf("%d days: step %d", days, s)
		}
	}
}

func TestWhatIsSaidAboutNumbersIsReadable(t *testing.T) {
	for in, want := range map[float64]string{1 << 30: "1 GiB", 640 << 20: "640 MiB", 1536 << 20: "1.5 GiB", 0: "0 B", 100 << 30: "100 GiB", 512: "512 B"} {
		if got := bytesText(in); got != want {
			t.Errorf("bytesText(%v) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[float64]string{0.25: "250m", 1: "1 core", 1.5: "1.5 cores", 0.05: "50m", 2: "2 cores"} {
		if got := coresText(in); got != want {
			t.Errorf("coresText(%v) = %q, want %q", in, got, want)
		}
	}
}

// ---- the incident ----

func lowUses(res Resource, limit float64) []Use {
	return []Use{
		{App: webApp, Label: "shop/web", Part: "app", Resource: res, Limit: limit, AtPercentile: 0.29 * limit, Peak: 0.30 * limit, Covered: 60, Steps: 60},
		{App: webApp, Label: "shop/web", Part: "proxy", Resource: res, Limit: limit, AtPercentile: 0.10 * limit, Peak: 0.12 * limit, Covered: 60, Steps: 60},
	}
}

func TestTheIncidentSaysWhatWasMeasuredAndWhatToTry(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 7, 0, 0, time.UTC)
	in := DescribeRightsize(webApp, "shop/web", Memory, lowUses(Memory, gib), p95, 2100, now)
	if in.Title != "shop/web: memory over-provisioned" || in.App != webApp || in.Source != incident.Detected || in.Severity != incident.Info {
		t.Fatalf("incident = %+v: it is information, and never urgent", in)
	}
	if !in.StartedAt.Equal(now) {
		t.Fatalf("started = %v", in.StartedAt)
	}
	for _, want := range []string{
		"Opened automatically", "last 7 days", "memory", "50%", "95th percentile", "35 minutes",
		"container app: limit 1 GiB", "29%", "highest 30%", "about 400 MiB", "30% headroom",
		"container proxy:", "check for rare large requests",
	} {
		if !strings.Contains(in.Summary, want) {
			t.Errorf("summary lacks %q:\n%s", want, in.Summary)
		}
	}
	if !strings.HasPrefix(in.DedupeKey, "rightsize:memory:") {
		t.Fatalf("dedupe key = %q: the resource is in it so that memory and CPU are two incidents", in.DedupeKey)
	}
}

func TestEachResourceSaysItInItsOwnWords(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 7, 0, 0, time.UTC)
	cpu := DescribeRightsize(webApp, "shop/web", CPU, lowUses(CPU, 2), p95, 2100, now)
	for _, want := range []string{"shop/web: CPU over-provisioned", "limit 2 cores", "about"} {
		if !strings.Contains(cpu.Title+cpu.Summary, want) {
			t.Errorf("cpu lacks %q:\n%s\n%s", want, cpu.Title, cpu.Summary)
		}
	}
	vol := lowUses(Storage, 100*gib)
	vol[0].Part, vol[1].Part = "data", "logs"
	st := DescribeRightsize(webApp, "shop/web", Storage, vol, p100, 2100, now)
	for _, want := range []string{"shop/web: storage over-provisioned", "volume data: size 100 GiB", "a volume usually cannot be made smaller", "highest use"} {
		if !strings.Contains(st.Title+st.Summary, want) {
			t.Errorf("storage lacks %q:\n%s\n%s", want, st.Title, st.Summary)
		}
	}
	if !strings.HasPrefix(st.DedupeKey, "rightsize:storage:") || !strings.HasPrefix(cpu.DedupeKey, "rightsize:cpu:") {
		t.Fatalf("keys = %q %q", st.DedupeKey, cpu.DedupeKey)
	}
}

func TestOnlyTheFirstFewContainersAreListed(t *testing.T) {
	var many []Use
	for i, name := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		many = append(many, Use{App: webApp, Label: "shop/web", Part: name, Resource: Memory, Limit: gib, AtPercentile: float64(i+1) * 0.01 * gib, Peak: gib * 0.1, Covered: 60, Steps: 60})
	}
	in := DescribeRightsize(webApp, "shop/web", Memory, many, p95, 2100, time.Now().UTC())
	if strings.Count(in.Summary, "- container ") != 5 || !strings.Contains(in.Summary, "and 2 more") {
		t.Fatalf("summary:\n%s", in.Summary)
	}
}

func TestTheKeyIsTheSameForADayAndDiffersByResourceApplicationAndDay(t *testing.T) {
	d1 := time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC)
	d1late := time.Date(2026, 11, 17, 23, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC)
	if RightsizeKey(Memory, webApp, d1) != RightsizeKey(Memory, webApp, d1late) {
		t.Fatal("two looks in one day are two keys")
	}
	for name, other := range map[string]string{
		"another day": RightsizeKey(Memory, webApp, d2), "another resource": RightsizeKey(CPU, webApp, d1), "another application": RightsizeKey(Memory, apiApp, d1),
	} {
		if other == RightsizeKey(Memory, webApp, d1) {
			t.Errorf("%s shares a key", name)
		}
	}
	if RightsizeKey(Memory, webApp, d1) == DepKey(webApp, d1) || RightsizeKey(Memory, webApp, d1) == SilenceKey(webApp, d1) || RightsizeKey(Memory, webApp, d1) == DedupeKey(webApp, d1) {
		t.Fatal("shares a key with another kind of incident")
	}
}

func TestAPodThatReportedNoUseDoesNotSetTheLimit(t *testing.T) {
	pod2 := "/k8s/shop/web-5d6f8c7b9-zzzzz/app"
	usage := []store.MetricSeries{use(webPod, always(0.2*gib)), use(pod2, always(math.NaN()))}
	limit := []store.MetricSeries{use(webPod, always(gib)), use(pod2, always(2*gib))}
	uses := MeasureUse(Memory, usage, limit, depGrid, p95, nil)
	if len(uses) != 1 || uses[0].Limit != gib || !near(uses[0].Share(), 0.2) {
		t.Fatalf("uses = %+v: a pod with no readings of use says nothing about the limit", uses)
	}
}
