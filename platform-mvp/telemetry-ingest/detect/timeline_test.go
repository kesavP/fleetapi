package detect

import (
	"testing"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/store"
)

const (
	podA = "/k8s/shop/web-5d6f8c7b9-xk2lp/app"
	podB = "/k8s/shop/web-5d6f8c7b9-zz9bd/app"
	// one second into step i
	t0ms = int64(1_700_000_000) * 1000
)

func at0(i int) int64 { return t0ms + int64(i)*60_000 + 1000 }

func ser(labels map[string]string, vals map[int]float64) store.MetricSeries {
	s := store.MetricSeries{Labels: labels}
	for i := 0; i < 20; i++ {
		if v, ok := vals[i]; ok {
			s.Points = append(s.Points, store.Point{T: at0(i), V: v})
		}
	}
	return s
}

func lab(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func find(t *testing.T, tl Timeline, id string) App {
	t.Helper()
	for _, a := range tl.Apps {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("no application %q in %+v", id, tl.Apps)
	return App{}
}

func TestTimelineSumsPodsAndCountsFailures(t *testing.T) {
	g := Grid{Start: 1_700_000_000, Step: 60, N: 10}
	reqs := []store.MetricSeries{
		ser(lab("container_id", podA, "status", "200"), map[int]float64{0: 4, 1: 4}),
		ser(lab("container_id", podB, "status", "200"), map[int]float64{0: 3, 1: 3}), // another pod of the same application
		ser(lab("container_id", podA, "status", "500"), map[int]float64{1: 2}),
		ser(lab("container_id", podA, "status", "grpc:INTERNAL"), map[int]float64{1: 1}),
		ser(lab("container_id", podA, "status", "404"), map[int]float64{1: 5}), // the caller's fault: a request, not a failure
		ser(lab("container_id", "/nomad/x/y", "status", "500"), map[int]float64{0: 100}),
	}
	tl := BuildTimeline(reqs, nil, g, 0.5)
	if len(tl.Apps) != 1 {
		t.Fatalf("apps = %+v, want only shop/web", tl.Apps)
	}
	a := find(t, tl, "/k8s/shop/web")
	if a.Label != "shop/web" {
		t.Fatalf("label = %q", a.Label)
	}
	if a.Total[0] != 7 || a.Total[1] != 15 {
		t.Fatalf("total = %v", a.Total)
	}
	if a.Errors[0] != 0 || a.Errors[1] != 3 {
		t.Fatalf("errors = %v, want 2 from the 500 and 1 from the gRPC failure", a.Errors)
	}
	if a.Slow != nil || tl.Bound != 0 {
		t.Fatalf("no histogram: slow = %v, bound = %v", a.Slow, tl.Bound)
	}
}

func TestTimelineIgnoresPointsOutsideTheGridAndNonPositiveOnes(t *testing.T) {
	g := Grid{Start: 1_700_000_000, Step: 60, N: 3}
	s := store.MetricSeries{Labels: lab("container_id", podA, "status", "200"), Points: []store.Point{
		{T: t0ms - 1000, V: 9}, {T: at0(0), V: 1}, {T: at0(2), V: 2}, {T: at0(3), V: 9}, {T: at0(1), V: 0}, {T: at0(1), V: -4},
	}}
	a := find(t, BuildTimeline([]store.MetricSeries{s}, nil, g, 0.5), "/k8s/shop/web")
	if a.Total[0] != 1 || a.Total[1] != 0 || a.Total[2] != 2 {
		t.Fatalf("total = %v", a.Total)
	}
}

func buckets(le string, container string, vals map[int]float64) store.MetricSeries {
	return ser(lab("container_id", container, "le", le), vals)
}

func TestTimelineSlowIsWhatTheNextBoundDidNotCatch(t *testing.T) {
	g := Grid{Start: 1_700_000_000, Step: 60, N: 5}
	reqs := []store.MetricSeries{ser(lab("container_id", podA, "status", "200"), map[int]float64{0: 10, 1: 10})}
	bs := []store.MetricSeries{
		buckets("0.1", podA, map[int]float64{0: 5, 1: 2}),
		buckets("0.5", podA, map[int]float64{0: 9, 1: 4}),
		buckets("1", podA, map[int]float64{0: 10, 1: 8}),
		buckets("+Inf", podA, map[int]float64{0: 10, 1: 10}),
	}
	tl := BuildTimeline(reqs, bs, g, 0.5)
	if tl.Bound != 0.5 {
		t.Fatalf("bound = %v", tl.Bound)
	}
	a := find(t, tl, "/k8s/shop/web")
	if a.Slow[0] != 1 || a.Slow[1] != 6 {
		t.Fatalf("slow = %v, want total minus the 0.5 s bucket", a.Slow)
	}
	// 0.3 s is between two bounds: judged by the larger, and that is said
	if tl := BuildTimeline(reqs, bs, g, 0.3); tl.Bound != 0.5 {
		t.Fatalf("bound for 0.3 = %v, want 0.5", tl.Bound)
	}
	// 5 s is above every finite bound: nothing can be said
	tl = BuildTimeline(reqs, bs, g, 5)
	if tl.Bound != 0 || find(t, tl, "/k8s/shop/web").Slow != nil {
		t.Fatalf("above every bound: bound %v, slow %v", tl.Bound, find(t, tl, "/k8s/shop/web").Slow)
	}
}

func TestTimelineSlowNeverGoesNegative(t *testing.T) {
	// bucket rates are summed on their own and can exceed the counter's for a step
	g := Grid{Start: 1_700_000_000, Step: 60, N: 2}
	reqs := []store.MetricSeries{ser(lab("container_id", podA, "status", "200"), map[int]float64{0: 5})}
	bs := []store.MetricSeries{buckets("0.5", podA, map[int]float64{0: 6})}
	a := find(t, BuildTimeline(reqs, bs, g, 0.5), "/k8s/shop/web")
	if a.Slow[0] != 0 {
		t.Fatalf("slow = %v", a.Slow)
	}
}

func TestTimelineAppsAreSortedAndFiniteBoundsOnly(t *testing.T) {
	g := Grid{Start: 1_700_000_000, Step: 60, N: 2}
	reqs := []store.MetricSeries{
		ser(lab("container_id", "/k8s/shop/zeta-0/c", "status", "200"), map[int]float64{0: 1}),
		ser(lab("container_id", "/k8s/shop/alpha-0/c", "status", "200"), map[int]float64{0: 1}),
	}
	bs := []store.MetricSeries{buckets("+Inf", "/k8s/shop/alpha-0/c", map[int]float64{0: 1}), buckets("junk", "/k8s/shop/alpha-0/c", map[int]float64{0: 1})}
	tl := BuildTimeline(reqs, bs, g, 0.5)
	if len(tl.Apps) != 2 || tl.Apps[0].ID != "/k8s/shop/alpha" || tl.Apps[1].ID != "/k8s/shop/zeta" {
		t.Fatalf("apps = %+v", tl.Apps)
	}
	if tl.Bound != 0 {
		t.Fatalf("an +Inf or unreadable bound is no bound: %v", tl.Bound)
	}
}
