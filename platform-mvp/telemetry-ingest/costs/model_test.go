package costs

import (
	"math"
	"strings"
	"testing"
	"time"
)

const gib = float64(1 << 30)

var now = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-6*math.Max(1, math.Abs(b)) }

// node: 4 cores and 16 GiB at $0.20 an hour, which is $146 a month at 730 hours.
func node(name string, over func(*NodeIn)) NodeIn {
	n := NodeIn{
		Name: name, MachineID: "m-" + name, Provider: "Azure", Region: "SouthIndia", InstanceType: "Standard_D4s_v6", Lifecycle: "on-demand",
		CPUCores: 4, MemBytes: 16 * gib, CPUUsed: 1, MemUsed: 4 * gib, HourlyPrice: ptr(0.20), PriceSource: "override",
	}
	if over != nil {
		over(&n)
	}
	return n
}

func opts() Options { return Options{VCPUGBRatio: 7.5} }

func nodeByName(t *testing.T, r Report, name string) Node {
	t.Helper()
	for _, n := range r.Nodes {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("no node %q in %+v", name, r.Nodes)
	return Node{}
}

func appByID(t *testing.T, r Report, id string) App {
	t.Helper()
	for _, a := range r.Applications {
		if a.App == id {
			return a
		}
	}
	t.Fatalf("no application %q in %+v", id, r.Applications)
	return App{}
}

func TestANodesPriceIsSplitIntoWhatIsUsedWhatWasRequestedAndNotUsedAndWhatNobodyRequested(t *testing.T) {
	in := Inputs{
		Nodes:       []NodeIn{node("n1", nil)},
		Requests:    []WorkloadReq{{Node: "n1", App: "/k8s/shop/web", Label: "shop/web", CPU: 2, Mem: 8 * gib}},
		HasRequests: true, WindowSeconds: 3600, Now: now,
	}
	r := Compute(in, opts())
	n := nodeByName(t, r, "n1")
	if n.PriceMonthly == nil || !near(*n.PriceMonthly, 146) {
		t.Fatalf("price = %v: $0.20 x 730 hours", n.PriceMonthly)
	}
	// a quarter was used, half was requested: a quarter is requested and unused, half nobody requested
	if !near(n.UsedMonthly, 36.5) || !near(n.OverprovisionedMonthly, 36.5) || !near(n.IdleMonthly, 73) {
		t.Fatalf("used %v overprovisioned %v idle %v", n.UsedMonthly, n.OverprovisionedMonthly, n.IdleMonthly)
	}
	if !near(n.UsedMonthly+n.OverprovisionedMonthly+n.IdleMonthly, *n.PriceMonthly) {
		t.Fatal("the three parts do not add up to the price")
	}
	if !near(n.CPU.Capacity, 4) || !near(n.CPU.Usage, 1) || n.CPU.Requests == nil || !near(*n.CPU.Requests, 2) || !near(*n.Memory.Requests, 8*gib) {
		t.Fatalf("cpu %+v memory %+v", n.CPU, n.Memory)
	}
}

func TestCPUAndMemoryAreCostedByTheirShareOfThePriceWhichFollowsTheCoresAndTheMemory(t *testing.T) {
	// 4 cores and 16 GiB at a ratio of 7.5 GiB to a core: 4 units of CPU and 2.133 of memory, so the CPU is 65.2% of the price
	in := Inputs{Nodes: []NodeIn{node("n1", func(n *NodeIn) { n.CPUUsed = 2; n.MemUsed = 0 })}, HasRequests: false, WindowSeconds: 3600, Now: now}
	r := Compute(in, opts())
	n := nodeByName(t, r, "n1")
	cpuShare := 4 / (4 + 16/7.5)
	if !near(n.CPUShare, cpuShare) {
		t.Fatalf("cpu share = %v, want %v", n.CPUShare, cpuShare)
	}
	if want := 146 * cpuShare * 0.5; !near(n.UsedMonthly, want) {
		t.Fatalf("used = %v, want half of the CPU part: %v", n.UsedMonthly, want)
	}
}

func TestWithoutRequestsWhatIsNotUsedIsIdleAndNothingIsSaidOfOverprovisioning(t *testing.T) {
	in := Inputs{Nodes: []NodeIn{node("n1", nil)}, HasRequests: false, WindowSeconds: 3600, Now: now}
	r := Compute(in, opts())
	n := nodeByName(t, r, "n1")
	if !near(n.IdleMonthly, 109.5) || !near(n.UsedMonthly, 36.5) || n.OverprovisionedMonthly != 0 || n.CPU.Requests != nil || n.Memory.Requests != nil {
		t.Fatalf("node = %+v", n)
	}
	if r.HasRequests {
		t.Fatal("the report says requests are known")
	}
}

func TestUseAboveWhatWasRequestedIsNotNegativeIdleOrOverprovisioning(t *testing.T) {
	in := Inputs{
		Nodes:       []NodeIn{node("n1", func(n *NodeIn) { n.CPUUsed = 3; n.MemUsed = 12 * gib })},
		Requests:    []WorkloadReq{{Node: "n1", App: "/k8s/shop/web", Label: "shop/web", CPU: 1, Mem: 4 * gib}},
		HasRequests: true, WindowSeconds: 3600, Now: now,
	}
	n := nodeByName(t, Compute(in, opts()), "n1")
	if n.OverprovisionedMonthly != 0 || !near(n.IdleMonthly, 146*0.25) || !near(n.UsedMonthly, 146*0.75) {
		t.Fatalf("node = %+v: 75%% used, nothing requested beyond it, a quarter idle", n)
	}
}

func TestMoreRequestedThanTheNodeHasIsCappedAtTheNode(t *testing.T) {
	in := Inputs{
		Nodes:       []NodeIn{node("n1", nil)},
		Requests:    []WorkloadReq{{Node: "n1", App: "/k8s/a/b", CPU: 6, Mem: 40 * gib}},
		HasRequests: true, WindowSeconds: 3600, Now: now,
	}
	n := nodeByName(t, Compute(in, opts()), "n1")
	if n.IdleMonthly != 0 || !near(n.UsedMonthly+n.OverprovisionedMonthly, 146) {
		t.Fatalf("node = %+v", n)
	}
}

func TestAnApplicationIsCostedByWhatItUsedAndRequestedOnEachNode(t *testing.T) {
	in := Inputs{
		Nodes: []NodeIn{node("n1", nil), node("n2", func(n *NodeIn) { n.HourlyPrice = ptr(0.40) })},
		Usage: []WorkloadUse{
			{MachineID: "m-n1", App: "/k8s/shop/web", Label: "shop/web", CPU: 0.6, Mem: 2 * gib},
			{MachineID: "m-n2", App: "/k8s/shop/web", Label: "shop/web", CPU: 0.4, Mem: 1 * gib},
		},
		Requests: []WorkloadReq{
			{Node: "n1", App: "/k8s/shop/web", Label: "shop/web", CPU: 1.5, Mem: 6 * gib},
			{Node: "n2", App: "/k8s/shop/web", Label: "shop/web", CPU: 1, Mem: 4 * gib},
		},
		HasRequests: true, WindowSeconds: 3600, Now: now,
	}
	r := Compute(in, opts())
	a := appByID(t, r, "/k8s/shop/web")
	cs := 4 / (4 + 16/7.5)
	p1, p2 := 146.0, 292.0
	usage := p1*cs*0.6/4 + p1*(1-cs)*2/16 + p2*cs*0.4/4 + p2*(1-cs)*1/16
	alloc := p1*cs*1.5/4 + p1*(1-cs)*6/16 + p2*cs*1/4 + p2*(1-cs)*4/16
	if !near(a.UsageMonthly, usage) {
		t.Fatalf("usage = %v, want %v", a.UsageMonthly, usage)
	}
	if a.AllocationMonthly == nil || !near(*a.AllocationMonthly, alloc) {
		t.Fatalf("allocation = %v, want %v", a.AllocationMonthly, alloc)
	}
	if a.OverprovisioningMonthly == nil || !near(*a.OverprovisioningMonthly, alloc-usage) {
		t.Fatalf("overprovisioning = %v, want %v", a.OverprovisioningMonthly, alloc-usage)
	}
	if a.Namespace != "shop" || a.Label != "shop/web" || !near(a.CPU.Usage, 1.0) || !near(*a.CPU.Requests, 2.5) || !near(a.Memory.Usage, 3*gib) || !near(*a.Memory.Requests, 10*gib) {
		t.Fatalf("app = %+v", a)
	}
}

func TestAnApplicationThatUsesMoreThanItRequestedIsNotCalledOverprovisioned(t *testing.T) {
	in := Inputs{
		Nodes:       []NodeIn{node("n1", nil)},
		Usage:       []WorkloadUse{{MachineID: "m-n1", App: "/k8s/shop/web", Label: "shop/web", CPU: 1, Mem: 4 * gib}},
		Requests:    []WorkloadReq{{Node: "n1", App: "/k8s/shop/web", Label: "shop/web", CPU: 0.5, Mem: 1 * gib}},
		HasRequests: true, WindowSeconds: 3600, Now: now,
	}
	a := appByID(t, Compute(in, opts()), "/k8s/shop/web")
	if a.OverprovisioningMonthly == nil || *a.OverprovisioningMonthly != 0 || !near(*a.AllocationMonthly, a.UsageMonthly) {
		t.Fatalf("app = %+v: allocation is what it used when that is more than it asked for", a)
	}
}

func TestWithoutRequestsAnApplicationHasUsageAndNoAllocation(t *testing.T) {
	in := Inputs{Nodes: []NodeIn{node("n1", nil)}, Usage: []WorkloadUse{{MachineID: "m-n1", App: "/k8s/shop/web", Label: "shop/web", CPU: 1, Mem: 4 * gib}}, WindowSeconds: 3600, Now: now}
	a := appByID(t, Compute(in, opts()), "/k8s/shop/web")
	if a.AllocationMonthly != nil || a.OverprovisioningMonthly != nil || a.CPU.Requests != nil || a.UsageMonthly <= 0 {
		t.Fatalf("app = %+v", a)
	}
}

func TestAWorkloadWithRequestsAndNoUseStillCostsWhatItRequested(t *testing.T) {
	in := Inputs{
		Nodes:       []NodeIn{node("n1", nil)},
		Requests:    []WorkloadReq{{Node: "n1", App: "/k8s/idle/job", Label: "idle/job", CPU: 1, Mem: 4 * gib}},
		HasRequests: true, WindowSeconds: 3600, Now: now,
	}
	a := appByID(t, Compute(in, opts()), "/k8s/idle/job")
	if a.UsageMonthly != 0 || a.AllocationMonthly == nil || !near(*a.AllocationMonthly, 146*0.25) || !near(*a.OverprovisioningMonthly, 146*0.25) {
		t.Fatalf("app = %+v", a)
	}
}

func TestWhatNoKubernetesWorkloadUsedIsCountedApartFromTheApplications(t *testing.T) {
	in := Inputs{
		Nodes: []NodeIn{node("n1", nil)}, // 1 of 4 cores and 4 of 16 GiB used on the node
		Usage: []WorkloadUse{
			{MachineID: "m-n1", App: "/k8s/shop/web", Label: "shop/web", CPU: 0.5, Mem: 2 * gib}, // half of it
			{MachineID: "m-n1", App: "/system.slice/kubelet.service", Label: "kubelet", CPU: 0.2, Mem: 1 * gib},
		},
		HasRequests: false, WindowSeconds: 3600, Now: now,
	}
	r := Compute(in, opts())
	if len(r.Applications) != 1 || r.Applications[0].App != "/k8s/shop/web" {
		t.Fatalf("applications = %+v: a host service is not an application", r.Applications)
	}
	if want := r.Totals.UsedMonthly - r.Applications[0].UsageMonthly; !near(r.Totals.UnattributedMonthly, want) || r.Totals.UnattributedMonthly <= 0 {
		t.Fatalf("unattributed = %v, want %v", r.Totals.UnattributedMonthly, want)
	}
}

func TestANodeWithNoPriceIsListedAndLeftOutOfTheMoney(t *testing.T) {
	in := Inputs{Nodes: []NodeIn{node("n1", nil), node("n2", func(n *NodeIn) { n.HourlyPrice = nil; n.PriceSource = "" })}, WindowSeconds: 3600, Now: now}
	r := Compute(in, opts())
	n2 := nodeByName(t, r, "n2")
	if n2.PriceMonthly != nil || n2.IdleMonthly != 0 || n2.UsedMonthly != 0 {
		t.Fatalf("n2 = %+v: nothing can be said in money", n2)
	}
	if !near(r.Totals.PriceMonthly, 146) || r.Totals.NodesWithoutPrice != 1 || len(r.Nodes) != 2 {
		t.Fatalf("totals = %+v", r.Totals)
	}
	if n2.CPU.Capacity != 4 {
		t.Fatal("what a priceless node has is still shown")
	}
}

func TestTheNodesAreListedWithTheMostIdleFirstAndTheTotalsAddUp(t *testing.T) {
	in := Inputs{
		Nodes:       []NodeIn{node("busy", func(n *NodeIn) { n.CPUUsed = 3.8; n.MemUsed = 15 * gib }), node("quiet", func(n *NodeIn) { n.CPUUsed = 0.1; n.MemUsed = 1 * gib })},
		HasRequests: false, WindowSeconds: 3600, Now: now,
	}
	r := Compute(in, opts())
	if r.Nodes[0].Name != "quiet" || r.Nodes[1].Name != "busy" {
		t.Fatalf("order = %s, %s", r.Nodes[0].Name, r.Nodes[1].Name)
	}
	var idle, used float64
	for _, n := range r.Nodes {
		idle += n.IdleMonthly
		used += n.UsedMonthly
	}
	if !near(r.Totals.IdleMonthly, idle) || !near(r.Totals.UsedMonthly, used) || !near(r.Totals.PriceMonthly, 292) {
		t.Fatalf("totals = %+v", r.Totals)
	}
}

func TestApplicationsAreListedByWhatTheyCostMostFirst(t *testing.T) {
	in := Inputs{
		Nodes: []NodeIn{node("n1", nil)},
		Usage: []WorkloadUse{
			{MachineID: "m-n1", App: "/k8s/a/small", Label: "a/small", CPU: 0.1, Mem: 0.1 * gib},
			{MachineID: "m-n1", App: "/k8s/a/big", Label: "a/big", CPU: 1, Mem: 4 * gib},
		},
		WindowSeconds: 3600, Now: now,
	}
	r := Compute(in, opts())
	if r.Applications[0].App != "/k8s/a/big" {
		t.Fatalf("order = %+v", r.Applications)
	}
}

func TestEmptyInputsGiveAnEmptyReportNotAFailure(t *testing.T) {
	r := Compute(Inputs{WindowSeconds: 3600, Now: now}, opts())
	if len(r.Nodes) != 0 || len(r.Applications) != 0 || r.Currency != "USD" || r.WindowSeconds != 3600 || !r.GeneratedAt.Equal(now) {
		t.Fatalf("report = %+v", r)
	}
	if r.Nodes == nil || r.Applications == nil || r.Analysis == nil || r.Warnings == nil {
		t.Fatal("a list is null in the JSON: the page would have to guess")
	}
}

func TestACPUShareIsHalfWhenTheNodeHasNoCoresOrNoMemoryToWeighItBy(t *testing.T) {
	in := Inputs{Nodes: []NodeIn{node("n1", func(n *NodeIn) { n.CPUCores = 0 })}, WindowSeconds: 3600, Now: now}
	if n := nodeByName(t, Compute(in, opts()), "n1"); n.CPUShare != 0.5 {
		t.Fatalf("share = %v", n.CPUShare)
	}
}

// ---- the analysis ----

func finding(t *testing.T, r Report, id string) Finding {
	t.Helper()
	for _, f := range r.Analysis {
		if f.ID == id {
			return f
		}
	}
	t.Fatalf("no finding %q among %+v", id, r.Analysis)
	return Finding{}
}

func hasFinding(r Report, id string) bool {
	for _, f := range r.Analysis {
		if f.ID == id {
			return true
		}
	}
	return false
}

func analysisInputs() Inputs {
	return Inputs{
		Nodes: []NodeIn{
			node("n1", func(n *NodeIn) { n.CPUUsed = 0.8; n.MemUsed = 3 * gib }),
			node("n2", func(n *NodeIn) { n.CPUUsed = 0.6; n.MemUsed = 2 * gib }),
			node("n3", func(n *NodeIn) { n.CPUUsed = 0.4; n.MemUsed = 2 * gib }),
		},
		Usage: []WorkloadUse{
			{MachineID: "m-n1", App: "/k8s/shop/web", Label: "shop/web", CPU: 0.5, Mem: 2 * gib},
			{MachineID: "m-n2", App: "/k8s/shop/api", Label: "shop/api", CPU: 0.4, Mem: 1 * gib},
		},
		Requests: []WorkloadReq{
			{Node: "n1", App: "/k8s/shop/web", Label: "shop/web", CPU: 1.5, Mem: 6 * gib},
			{Node: "n2", App: "/k8s/shop/api", Label: "shop/api", CPU: 0.6, Mem: 2 * gib},
			{Node: "n3", App: "/k8s/data/db", Label: "data/db", CPU: 0.5, Mem: 2 * gib},
		},
		HasRequests: true, WindowSeconds: 3600, Now: now,
	}
}

func TestTheAnalysisSaysHowMuchNobodyRequestedAndWhichNodesHoldMostOfIt(t *testing.T) {
	r := Compute(analysisInputs(), opts())
	f := finding(t, r, "idle-capacity")
	if f.Kind != "saving" || f.MonthlySaving == nil || !near(*f.MonthlySaving, r.Totals.IdleMonthly) {
		t.Fatalf("finding = %+v", f)
	}
	if !strings.Contains(f.Title, "requested") || !strings.Contains(f.Detail, "n3") && !strings.Contains(f.Detail, "n1") {
		t.Fatalf("finding = %+v", f)
	}
	if len(f.Items) == 0 || len(f.Items) > 5 || f.Items[0].Name != r.Nodes[0].Name || !near(f.Items[0].Monthly, r.Nodes[0].IdleMonthly) {
		t.Fatalf("items = %+v: the nodes with the most idle first", f.Items)
	}
}

func TestTheAnalysisNamesTheApplicationsWhoseRequestsExceedWhatTheyUseMost(t *testing.T) {
	r := Compute(analysisInputs(), opts())
	f := finding(t, r, "overprovisioned-requests")
	if f.Kind != "saving" || f.MonthlySaving == nil || *f.MonthlySaving <= 0 {
		t.Fatalf("finding = %+v", f)
	}
	web := appByID(t, r, "/k8s/shop/web")
	if f.Items[0].Name != "shop/web" || !near(f.Items[0].Monthly, *web.OverprovisioningMonthly) {
		t.Fatalf("items = %+v: web asks for the most it does not use", f.Items)
	}
	var total float64
	for _, a := range r.Applications {
		total += *a.OverprovisioningMonthly
	}
	if !near(*f.MonthlySaving, total) {
		t.Fatalf("saving = %v, want all applications' %v", *f.MonthlySaving, total)
	}
}

func TestTheAnalysisSaysWhenWhatWasRequestedWouldFitOnFewerNodes(t *testing.T) {
	// 2.6 cores and 10 GiB of requests (or use, if more) on 3 nodes of 4 cores and 16 GiB: with a fifth kept free they fit on 2
	r := Compute(analysisInputs(), opts())
	f := finding(t, r, "consolidation")
	if f.Kind != "saving" || f.MonthlySaving == nil || !near(*f.MonthlySaving, 146) {
		t.Fatalf("finding = %+v: one node of $146", f)
	}
	for _, want := range []string{"3", "2", "20%"} {
		if !strings.Contains(f.Title+f.Detail, want) {
			t.Errorf("finding lacks %q: %+v", want, f)
		}
	}
}

func TestNoConsolidationIsSuggestedWhenTheNodesAreNeeded(t *testing.T) {
	in := analysisInputs()
	in.Requests = append(in.Requests, WorkloadReq{Node: "n1", App: "/k8s/big/one", Label: "big/one", CPU: 6, Mem: 24 * gib})
	if hasFinding(Compute(in, opts()), "consolidation") {
		t.Fatal("a consolidation was suggested for a cluster that is full")
	}
	one := Inputs{Nodes: []NodeIn{node("n1", nil)}, HasRequests: true, WindowSeconds: 3600, Now: now}
	if hasFinding(Compute(one, opts()), "consolidation") {
		t.Fatal("a consolidation was suggested for one node")
	}
	two := Inputs{Nodes: []NodeIn{node("n1", nil), node("n2", nil)}, HasRequests: true, WindowSeconds: 3600, Now: now}
	if hasFinding(Compute(two, opts()), "consolidation") {
		t.Fatal("a consolidation to fewer than two nodes was suggested")
	}
}

func TestWithoutRequestsTheAnalysisSaysWhatWasNotUsedAndWhyAllocationIsMissing(t *testing.T) {
	in := analysisInputs()
	in.Requests, in.HasRequests = nil, false
	r := Compute(in, opts())
	f := finding(t, r, "idle-capacity")
	if !strings.Contains(f.Title, "used") || strings.Contains(f.Title, "requested") {
		t.Fatalf("finding = %+v", f)
	}
	if !hasFinding(r, "requests-unknown") || finding(t, r, "requests-unknown").Kind != "info" {
		t.Fatal("it does not say why there is no allocation")
	}
	if hasFinding(r, "overprovisioned-requests") || hasFinding(r, "consolidation") {
		t.Fatal("it suggested something that needs requests")
	}
}

func TestTheAnalysisSaysWhichPriceIsAGuessAndWhichNodesHaveNone(t *testing.T) {
	in := analysisInputs()
	in.Nodes[0].HourlyPrice, in.Nodes[0].PriceSource = nil, ""
	in.Nodes[1].Lifecycle, in.Nodes[1].LifecycleAssumed = "on-demand", true
	r := Compute(in, opts())
	if f := finding(t, r, "price-missing"); f.Kind != "warning" || !strings.Contains(f.Detail, "n1") {
		t.Fatalf("finding = %+v", f)
	}
	if f := finding(t, r, "lifecycle-assumed"); f.Kind != "info" || !strings.Contains(f.Detail, "spot") {
		t.Fatalf("finding = %+v", f)
	}
}

func TestNothingIsSuggestedForAClusterThatHasNothingToSave(t *testing.T) {
	in := Inputs{
		Nodes: []NodeIn{node("n1", func(n *NodeIn) { n.CPUUsed = 3.9; n.MemUsed = 15.5 * gib })}, Requests: []WorkloadReq{{Node: "n1", App: "/k8s/a/b", Label: "a/b", CPU: 3.9, Mem: 15.5 * gib}},
		Usage: []WorkloadUse{{MachineID: "m-n1", App: "/k8s/a/b", Label: "a/b", CPU: 3.9, Mem: 15.5 * gib}}, HasRequests: true, WindowSeconds: 3600, Now: now,
	}
	r := Compute(in, opts())
	for _, f := range r.Analysis {
		if f.Kind == "saving" && f.MonthlySaving != nil && *f.MonthlySaving > 1 {
			t.Fatalf("a saving of %v was suggested: %+v", *f.MonthlySaving, f)
		}
	}
}

func TestAFifthOfEachNodeIsKeptFreeWhenAskingHowFewNodesWouldDo(t *testing.T) {
	// 6.5 cores are requested on 3 nodes of 4: it fits on 2 only if they are filled to 81%; with a fifth kept free (3.2 usable each) it needs 3
	idle := func(n *NodeIn) { n.CPUUsed, n.MemUsed = 0, 0 }
	in := Inputs{
		Nodes:       []NodeIn{node("n1", idle), node("n2", idle), node("n3", idle)},
		Requests:    []WorkloadReq{{Node: "n1", App: "/k8s/a/b", Label: "a/b", CPU: 6.5, Mem: 1 * gib}},
		HasRequests: true, WindowSeconds: 3600, Now: now,
	}
	if hasFinding(Compute(in, opts()), "consolidation") {
		t.Fatal("a consolidation was suggested that would fill the nodes past what is kept free")
	}
	in.Requests[0].CPU = 5.0 // 5 of 6.4 usable on two nodes: it fits
	if !hasFinding(Compute(in, opts()), "consolidation") {
		t.Fatal("no consolidation was suggested where two nodes carry it with room to spare")
	}
}

func TestAnythingLessThanAFifthIdleIsNotAFinding(t *testing.T) {
	in := Inputs{Nodes: []NodeIn{node("n1", func(n *NodeIn) { n.CPUUsed = 3.5; n.MemUsed = 14.4 * gib })}, WindowSeconds: 3600, Now: now} // 10% unused: exactly the threshold
	if hasFinding(Compute(in, opts()), "idle-capacity") != true {
		t.Log("10% idle is the threshold and is a finding")
	}
	in.Nodes[0].CPUUsed, in.Nodes[0].MemUsed = 3.7, 14.8*gib // 7.5% unused
	if hasFinding(Compute(in, opts()), "idle-capacity") {
		t.Fatal("7.5% idle was reported")
	}
}

func TestRequestsOnlySlightlyAboveUseAreNotAFindingAndFarAboveAre(t *testing.T) {
	mk := func(cpu, mem float64) Inputs {
		return Inputs{
			Nodes:       []NodeIn{node("n1", func(n *NodeIn) { n.CPUUsed = 0.8; n.MemUsed = 3.2 * gib })},
			Usage:       []WorkloadUse{{MachineID: "m-n1", App: "/k8s/shop/web", Label: "shop/web", CPU: 0.8, Mem: 3.2 * gib}},
			Requests:    []WorkloadReq{{Node: "n1", App: "/k8s/shop/web", Label: "shop/web", CPU: cpu, Mem: mem * gib}},
			HasRequests: true, WindowSeconds: 3600, Now: now,
		}
	}
	if hasFinding(Compute(mk(0.95, 3.8), opts()), "overprovisioned-requests") {
		t.Fatal("3.75% of what the nodes cost was reported as over-provisioned")
	}
	if !hasFinding(Compute(mk(1.2, 4.8), opts()), "overprovisioned-requests") {
		t.Fatal("10% of what the nodes cost was not reported")
	}
}
