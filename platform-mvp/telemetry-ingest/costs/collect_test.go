package costs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/store"
)

type gen struct {
	labels map[string]string
	v      func(t int64) float64
}

func lab(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func steady(v float64) func(int64) float64 { return func(int64) float64 { return v } }

// fakeSource serves generated series for exactly the window a query asks for, and what the cluster collector says.
type fakeSource struct {
	mu        sync.Mutex
	series    map[string][]gen
	truncated map[string]bool
	err       map[string]error
	summary   []store.InventoryCluster
	rows      []store.InventoryObject
	invErr    error
	asked     []store.MetricQuery
	podLimit  int
}

func (f *fakeSource) QueryMetric(_ context.Context, q store.MetricQuery) (store.MetricResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, q)
	if err := f.err[q.Name]; err != nil {
		return store.MetricResult{}, err
	}
	res := store.MetricResult{Series: []store.MetricSeries{}, Truncated: f.truncated[q.Name]}
	for _, g := range f.series[q.Name] {
		s := store.MetricSeries{Labels: g.labels}
		for t := q.From.Unix(); t+int64(q.StepSec) <= q.To.Unix(); t += int64(q.StepSec) {
			s.Points = append(s.Points, store.Point{T: t * 1000, V: g.v(t)})
		}
		res.Series = append(res.Series, s)
	}
	return res, nil
}

func (f *fakeSource) InventorySummary(context.Context, string) ([]store.InventoryCluster, error) {
	return f.summary, f.invErr
}

func (f *fakeSource) ListInventory(_ context.Context, q store.InventoryQuery) ([]store.InventoryObject, error) {
	f.podLimit = q.Limit
	if f.invErr != nil {
		return nil, f.invErr
	}
	return f.rows, nil
}

func pod(node, ns, kind, workload string, phase string, containers ...map[string]any) store.InventoryObject {
	cs := []any{}
	for _, c := range containers {
		cs = append(cs, c)
	}
	d, _ := json.Marshal(map[string]any{"node": node, "phase": phase, "workload_kind": kind, "workload_name": workload, "containers": cs})
	return store.InventoryObject{Cluster: "aks", Kind: "pod", UID: node + ns + workload, Namespace: ns, Name: workload + "-abc-xyz", Data: d}
}

func req(cpuM int64, memBytes int64) map[string]any {
	return map[string]any{"name": "app", "requests": map[string]any{"cpu_m": cpuM, "memory_bytes": memBytes}}
}

const (
	m1 = "machine-one-0000"
	m2 = "machine-two-0000"
)

// two Azure nodes of 4 cores and 16 GiB, each running some of shop/web (a Deployment) and one of them mongo.
func rig(t *testing.T) (*Collector, *fakeSource, *time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	f := &fakeSource{series: map[string][]gen{}, truncated: map[string]bool{}, err: map[string]error{}}
	for i, m := range []string{m1, m2} {
		host := []string{"aks-n1", "aks-n2"}[i]
		f.series["node_cloud_info"] = append(f.series["node_cloud_info"], gen{lab("machine_id", m, "provider", "Azure", "region", "SouthIndia", "instance_type", "Standard_D4s_v6", "instance_life_cycle", "", "availability_zone", ""), steady(1)})
		f.series["node_info"] = append(f.series["node_info"], gen{lab("machine_id", m, "hostname", host), steady(1)})
		f.series["node_resources_cpu_logical_cores"] = append(f.series["node_resources_cpu_logical_cores"], gen{lab("machine_id", m), steady(4)})
		f.series["node_resources_memory_total_bytes"] = append(f.series["node_resources_memory_total_bytes"], gen{lab("machine_id", m), steady(16 * gib)})
		f.series["node_resources_memory_available_bytes"] = append(f.series["node_resources_memory_available_bytes"], gen{lab("machine_id", m), steady(12 * gib)})
		for mode, v := range map[string]float64{"idle": 3.0, "user": 0.6, "system": 0.2, "iowait": 0.1} {
			f.series["node_resources_cpu_usage_seconds_total"] = append(f.series["node_resources_cpu_usage_seconds_total"], gen{lab("machine_id", m, "mode", mode), steady(v)})
		}
	}
	web1 := "/k8s/shop/web-5d6f8c7b9-xk2lp/app"
	web2 := "/k8s/shop/web-5d6f8c7b9-zzzzz/app"
	f.series["container_resources_cpu_usage_seconds_total"] = []gen{
		{lab("container_id", web1, "machine_id", m1), steady(0.3)}, {lab("container_id", web2, "machine_id", m2), steady(0.2)},
		{lab("container_id", "/system.slice/kubelet.service", "machine_id", m1), steady(0.1)},
	}
	f.series["container_resources_memory_rss_bytes"] = []gen{
		{lab("container_id", web1, "machine_id", m1), steady(1 * gib)}, {lab("container_id", web2, "machine_id", m2), steady(1 * gib)},
	}
	f.summary = []store.InventoryCluster{{Cluster: "aks", LastReceived: now.Add(-30 * time.Second)}}
	f.rows = []store.InventoryObject{
		pod("aks-n1", "shop", "deployment", "web", "Running", req(500, int64(2*gib))),
		pod("aks-n2", "shop", "deployment", "web", "Running", req(500, int64(2*gib))),
	}
	c := &Collector{
		Source: f, Prices: &Prices{Overrides: map[string]Override{"Standard_D4s_v6": {Hourly: 0.20}}}, Opts: Options{VCPUGBRatio: 7.5},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }, MaxSeries: 5000,
	}
	return c, f, &now
}

func TestTheReportCombinesWhatTheNodesAndTheContainersSentWithWhatPodsRequest(t *testing.T) {
	c, _, _ := rig(t)
	r, err := c.Report(bg, "us", 3600)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Nodes) != 2 || !r.HasRequests || r.WindowSeconds != 3600 {
		t.Fatalf("report = %+v", r)
	}
	n := nodeByName(t, r, "aks-n1")
	if n.Provider != "Azure" || n.Region != "SouthIndia" || n.InstanceType != "Standard_D4s_v6" || n.CPU.Capacity != 4 || !near(n.Memory.Capacity, 16*gib) {
		t.Fatalf("node = %+v", n)
	}
	if !near(n.CPU.Usage, 0.8) || !near(n.Memory.Usage, 4*gib) {
		t.Fatalf("usage cpu %v memory %v: every mode but idle and iowait, and what is not available", n.CPU.Usage, n.Memory.Usage)
	}
	if n.PriceMonthly == nil || !near(*n.PriceMonthly, 146) || n.PriceSource != "override" {
		t.Fatalf("price = %v %q", n.PriceMonthly, n.PriceSource)
	}
	if n.CPU.Requests == nil || !near(*n.CPU.Requests, 0.5) || !near(*n.Memory.Requests, 2*gib) {
		t.Fatalf("requests = %+v %+v", n.CPU, n.Memory)
	}
	a := appByID(t, r, "/k8s/shop/web")
	if !near(a.CPU.Usage, 0.5) || !near(a.Memory.Usage, 2*gib) || !near(*a.CPU.Requests, 1) || !near(*a.Memory.Requests, 4*gib) {
		t.Fatalf("app = %+v", a)
	}
	if len(r.Applications) != 1 {
		t.Fatalf("applications = %+v: the kubelet is a host service, not an application", r.Applications)
	}
	if r.Totals.UnattributedMonthly <= 0 {
		t.Fatal("what the nodes used beyond the application is not counted")
	}
}

func TestTheWindowIsTheOneAskedForAndTheStepKeepsTheQueriesTheSameSize(t *testing.T) {
	c, f, now := rig(t)
	if _, err := c.Report(bg, "us", 24*3600); err != nil {
		t.Fatal(err)
	}
	for _, q := range f.asked {
		if q.To.Sub(q.From) != 24*time.Hour || q.StepSec != 300 || q.Project != "us" || q.MaxSeries != 5000 {
			t.Fatalf("query = %+v", q)
		}
		if !q.To.Equal(time.Unix(now.Unix()/300*300, 0)) {
			t.Fatalf("a window ends on a step: %v", q.To)
		}
		counter := q.Name == "node_resources_cpu_usage_seconds_total" || q.Name == "container_resources_cpu_usage_seconds_total"
		if q.Rate != counter {
			t.Errorf("%s: rate = %v: counters are read as rates and gauges are not", q.Name, q.Rate)
		}
	}
	f.asked = nil
	if _, err := c.Report(bg, "us", 3600); err != nil {
		t.Fatal(err)
	}
	if f.asked[0].StepSec != 60 {
		t.Fatalf("an hour is read at 60 seconds, not %d", f.asked[0].StepSec)
	}
}

func TestAWindowThatIsTooShortOrTooLongIsRefused(t *testing.T) {
	c, f, _ := rig(t)
	for _, w := range []int{0, -5, 59, 31*24*3600 + 1} {
		if _, err := c.Report(bg, "us", w); !errors.Is(err, ErrWindow) {
			t.Errorf("%d seconds: %v", w, err)
		}
	}
	if len(f.asked) != 0 {
		t.Fatal("it queried before it checked the window")
	}
}

func TestTheAverageOfTheWindowIsUsedNotTheLastReading(t *testing.T) {
	c, f, now := rig(t)
	end := now.Unix()
	f.series["node_resources_memory_available_bytes"][0].v = func(t int64) float64 {
		if t < end-1800 {
			return 14 * gib // two gib used for the first half hour
		}
		return 10 * gib // six gib for the second
	}
	r, err := c.Report(bg, "us", 3600)
	if err != nil {
		t.Fatal(err)
	}
	if got := nodeByName(t, r, "aks-n1").Memory.Usage; !near(got, 4*gib) {
		t.Fatalf("usage = %v GiB, want the average of 2 and 6", got/gib)
	}
}

func TestMemoryThatIsMoreAvailableThanThereIsIsNotNegativeUse(t *testing.T) {
	c, f, _ := rig(t)
	f.series["node_resources_memory_available_bytes"][0].v = steady(20 * gib)
	r, _ := c.Report(bg, "us", 3600)
	if got := nodeByName(t, r, "aks-n1").Memory.Usage; got != 0 {
		t.Fatalf("usage = %v", got)
	}
}

func TestWithoutTheClusterCollectorTheRequestsAreUnknownAndSaidSo(t *testing.T) {
	c, f, _ := rig(t)
	f.summary = nil
	r, err := c.Report(bg, "us", 3600)
	if err != nil || r.HasRequests || nodeByName(t, r, "aks-n1").CPU.Requests != nil {
		t.Fatalf("%+v %v", r.HasRequests, err)
	}
	if f.podLimit != 0 {
		t.Fatal("pods were read with no inventory to read them from")
	}
	if !hasFinding(r, "requests-unknown") {
		t.Fatal("it does not say why there is no allocation")
	}
}

func TestACollectorThatHasGoneQuietIsNotTrustedAndOneThatCannotBeReadIsNotFatal(t *testing.T) {
	c, f, now := rig(t)
	f.summary = []store.InventoryCluster{{Cluster: "aks", LastReceived: now.Add(-time.Hour)}}
	if r, _ := c.Report(bg, "us", 3600); r.HasRequests {
		t.Fatal("requests from a collector that has been silent for an hour were used")
	}
	c2, f2, _ := rig(t)
	f2.invErr = errors.New("clickhouse busy")
	r, err := c2.Report(bg, "us", 3600)
	if err != nil || r.HasRequests {
		t.Fatalf("an inventory that could not be read stopped the report: %v", err)
	}
	if !strings.Contains(strings.Join(r.Warnings, " "), "inventory") {
		t.Fatalf("warnings = %v", r.Warnings)
	}
}

func TestOnlyPodsThatHoldACapacityAreCounted(t *testing.T) {
	c, f, _ := rig(t)
	f.rows = append(f.rows,
		pod("aks-n1", "shop", "deployment", "done", "Succeeded", req(4000, int64(8*gib))),
		pod("aks-n1", "shop", "deployment", "failed", "Failed", req(4000, int64(8*gib))),
		pod("", "shop", "deployment", "pending", "Pending", req(4000, int64(8*gib))),
		pod("aks-n9", "shop", "deployment", "elsewhere", "Running", req(4000, int64(8*gib))), // a node that sent no metrics
	)
	r, _ := c.Report(bg, "us", 3600)
	n := nodeByName(t, r, "aks-n1")
	if !near(*n.CPU.Requests, 0.5) {
		t.Fatalf("requests = %v: a pod that finished, failed, has no node or is on another node does not hold this one", *n.CPU.Requests)
	}
}

func TestAPodWithNoOwnerIsAnApplicationNamedAfterItself(t *testing.T) {
	c, f, _ := rig(t)
	p := pod("aks-n1", "tools", "pod", "debug", "Running", req(1000, int64(1*gib)))
	p.Name = "debug"
	f.rows = append(f.rows, p)
	r, _ := c.Report(bg, "us", 3600)
	a := appByID(t, r, "/k8s/tools/debug")
	if !near(*a.CPU.Requests, 1) {
		t.Fatalf("app = %+v", a)
	}
}

func TestTheRequestsOfAllTheContainersOfAPodAreAdded(t *testing.T) {
	c, f, _ := rig(t)
	f.rows = []store.InventoryObject{pod("aks-n1", "shop", "deployment", "web", "Running", req(500, int64(1*gib)), req(250, int64(512<<20)), map[string]any{"name": "nothing"})}
	r, _ := c.Report(bg, "us", 3600)
	a := appByID(t, r, "/k8s/shop/web")
	if !near(*a.CPU.Requests, 0.75) || !near(*a.Memory.Requests, 1.5*gib) {
		t.Fatalf("app = %+v", a)
	}
}

func TestSeveralNodesWithTheSameTypePayForItAndAPriceThatCannotBeFoundIsWarnedAboutOnce(t *testing.T) {
	c, f, now := rig(t)
	bad := newAzStub(t)
	bad.status = 503
	c.Prices = &Prices{Azure: newAzure(bad, now)}
	r, err := c.Report(bg, "us", 3600)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range r.Nodes {
		if n.PriceMonthly != nil {
			t.Fatalf("%s has a price from a service that failed", n.Name)
		}
	}
	if r.Totals.NodesWithoutPrice != 2 || !hasFinding(r, "price-missing") {
		t.Fatalf("totals = %+v", r.Totals)
	}
	var n int
	for _, w := range r.Warnings {
		if strings.Contains(w, "Standard_D4s_v6") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the price lookup of one type was warned about %d times: %v", n, r.Warnings)
	}
	_ = f
}

func TestALifecycleThatIsNotSaidIsAssumedOnDemandAndASpotNodeIsPricedAsSpot(t *testing.T) {
	c, f, _ := rig(t)
	c.Prices = &Prices{Overrides: map[string]Override{"Standard_D4s_v6": {Hourly: 0.20, SpotHourly: 0.06}}}
	f.series["node_cloud_info"][1].labels["instance_life_cycle"] = "spot"
	r, _ := c.Report(bg, "us", 3600)
	n1, n2 := nodeByName(t, r, "aks-n1"), nodeByName(t, r, "aks-n2")
	if n1.Lifecycle != "on-demand" || !n1.LifecycleAssumed || !near(*n1.PriceMonthly, 146) {
		t.Fatalf("n1 = %+v", n1)
	}
	if n2.Lifecycle != "spot" || n2.LifecycleAssumed || !near(*n2.PriceMonthly, 0.06*730) {
		t.Fatalf("n2 = %+v", n2)
	}
	if !hasFinding(r, "lifecycle-assumed") {
		t.Fatal("the guess is not said")
	}
}

func TestAResultThatWasCutShortIsSaidSoAndAFailureToReadIsAnError(t *testing.T) {
	c, f, _ := rig(t)
	f.truncated["container_resources_cpu_usage_seconds_total"] = true
	r, err := c.Report(bg, "us", 3600)
	if err != nil || !strings.Contains(strings.Join(r.Warnings, " "), "container_resources_cpu_usage_seconds_total") {
		t.Fatalf("warnings = %v err %v", r.Warnings, err)
	}
	c2, f2, _ := rig(t)
	f2.err["node_resources_memory_total_bytes"] = errors.New("clickhouse busy")
	if _, err := c2.Report(bg, "us", 3600); err == nil || !strings.Contains(err.Error(), "node_resources_memory_total_bytes") {
		t.Fatalf("err = %v", err)
	}
}

func TestANodeThatSentNoCapacityIsLeftOut(t *testing.T) {
	c, f, _ := rig(t)
	f.series["node_resources_cpu_logical_cores"] = f.series["node_resources_cpu_logical_cores"][:1]
	r, _ := c.Report(bg, "us", 3600)
	if len(r.Nodes) != 1 || r.Nodes[0].Name != "aks-n1" {
		t.Fatalf("nodes = %+v: a node whose size is not known cannot be costed", r.Nodes)
	}
}

func TestANodeWithoutAHostnameIsNamedByItsMachine(t *testing.T) {
	c, f, _ := rig(t)
	f.series["node_info"] = f.series["node_info"][:1]
	r, _ := c.Report(bg, "us", 3600)
	var found bool
	for _, n := range r.Nodes {
		if n.MachineID == m2 && n.Name == m2[:12] {
			found = true
		}
	}
	if !found {
		t.Fatalf("nodes = %+v", r.Nodes)
	}
}

func TestNothingSentIsAnEmptyReport(t *testing.T) {
	c, f, _ := rig(t)
	f.series = map[string][]gen{}
	r, err := c.Report(bg, "us", 3600)
	if err != nil || len(r.Nodes) != 0 || len(r.Applications) != 0 {
		t.Fatalf("%+v %v", r, err)
	}
}
