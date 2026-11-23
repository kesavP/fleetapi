// Package costs works out what the infrastructure costs and where the money is wasted.
//
// A node has a price (a month of it). That price is split between its CPU and its memory by what each is worth (a core against a GiB: see Options), and
// each part is then divided by what it was used for:
//
//	used            what the workloads on the node actually used
//	over-provisioned what was requested for workloads and not used
//	idle            what nobody requested: capacity the node has, and pays for, that no pod has been promised
//
// so used + over-provisioned + idle is the node's price. Without requests (they come from the cluster collector) there is no promise to speak of, and idle
// is simply what was not used. An application is costed the same way across the nodes it runs on: its usage, and what it requested (its allocation).
// Everything is per month at the rate of the window that was looked at, and in dollars (list prices: see price.go).
package costs

import (
	"math"
	"sort"
	"strings"
	"time"
)

// Resource is one resource of a node or an application: cores for CPU and bytes for memory. Requests is nil when what pods request is not known.
type Resource struct {
	Capacity float64  `json:"capacity,omitempty"`
	Usage    float64  `json:"usage"`
	Requests *float64 `json:"requests,omitempty"`
}

// Node is a node, what it costs and what the money is for.
type Node struct {
	Name             string   `json:"name"`
	MachineID        string   `json:"machine_id"`
	Provider         string   `json:"provider"`
	Region           string   `json:"region"`
	Zone             string   `json:"zone,omitempty"`
	InstanceType     string   `json:"instance_type"`
	Lifecycle        string   `json:"lifecycle"`
	LifecycleAssumed bool     `json:"lifecycle_assumed"`
	CPU              Resource `json:"cpu"`
	Memory           Resource `json:"memory"`
	CPUShare         float64  `json:"cpu_share"` // the part of the price that is CPU
	PriceMonthly     *float64 `json:"price_monthly"`
	PriceSource      string   `json:"price_source,omitempty"`

	UsedMonthly            float64 `json:"used_monthly"`
	OverprovisionedMonthly float64 `json:"overprovisioned_monthly"`
	IdleMonthly            float64 `json:"idle_monthly"`
}

// App is a Kubernetes workload and what it costs. Allocation and over-provisioning are nil when requests are not known.
type App struct {
	App       string   `json:"app"`
	Label     string   `json:"label"`
	Namespace string   `json:"namespace"`
	CPU       Resource `json:"cpu"`
	Memory    Resource `json:"memory"`

	UsageMonthly            float64  `json:"usage_monthly"`
	AllocationMonthly       *float64 `json:"allocation_monthly"`
	OverprovisioningMonthly *float64 `json:"overprovisioning_monthly"`
}

// Totals are the sums of the nodes that have a price, and of the applications.
type Totals struct {
	PriceMonthly            float64  `json:"price_monthly"`
	UsedMonthly             float64  `json:"used_monthly"`
	OverprovisionedMonthly  float64  `json:"overprovisioned_monthly"`
	IdleMonthly             float64  `json:"idle_monthly"`
	UnattributedMonthly     float64  `json:"unattributed_monthly"` // used on the nodes by what is not a Kubernetes workload (the system) or not seen
	NodesWithoutPrice       int      `json:"nodes_without_price"`
	AppUsageMonthly         float64  `json:"app_usage_monthly"`
	AppAllocationMonthly    *float64 `json:"app_allocation_monthly"`
	AppOverprovisionMonthly *float64 `json:"app_overprovisioning_monthly"`
}

// FindingItem is one line of a finding: a node or an application and what it comes to.
type FindingItem struct {
	Name    string  `json:"name"`
	Monthly float64 `json:"monthly"`
}

// Finding is one thing the analysis says. Kind is "saving" (money that could be saved), "info" or "warning" (something that limits what can be said).
type Finding struct {
	ID            string        `json:"id"`
	Kind          string        `json:"kind"`
	Title         string        `json:"title"`
	Detail        string        `json:"detail"`
	MonthlySaving *float64      `json:"monthly_saving"`
	Items         []FindingItem `json:"items"`
}

// Report is what the page shows.
type Report struct {
	Currency      string    `json:"currency"`
	WindowSeconds int       `json:"window_seconds"`
	GeneratedAt   time.Time `json:"generated_at"`
	HasRequests   bool      `json:"has_requests"`
	Nodes         []Node    `json:"nodes"`
	Applications  []App     `json:"applications"`
	Totals        Totals    `json:"totals"`
	Analysis      []Finding `json:"analysis"`
	Warnings      []string  `json:"warnings"`
}

// NodeIn is a node as it was measured: its capacity, what it used on average over the window, and its price per hour (nil when there is none).
type NodeIn struct {
	Name, MachineID, Provider, Region, Zone, InstanceType string
	Lifecycle                                             string
	LifecycleAssumed                                      bool
	CPUCores, MemBytes, CPUUsed, MemUsed                  float64
	HourlyPrice                                           *float64
	PriceSource                                           string
}

// WorkloadUse is what a workload used on a node, on average over the window.
type WorkloadUse struct {
	MachineID, App, Label string
	CPU, Mem              float64
}

// WorkloadReq is what a workload's pods request on a node (by the node's name).
type WorkloadReq struct {
	Node, App, Label string
	CPU, Mem         float64
}

// Inputs are what Compute works from.
type Inputs struct {
	Nodes         []NodeIn
	Usage         []WorkloadUse
	Requests      []WorkloadReq
	HasRequests   bool // whether requests are known at all: false when the cluster collector is not reporting, and then Requests is ignored
	WindowSeconds int
	Now           time.Time
}

// Options are how money is split and how the analysis judges.
type Options struct {
	// VCPUGBRatio is how many GiB of memory cost as much as one core: 7.5 is about what the clouds charge (a core is worth 7 to 8 GiB). It sets what
	// share of a node's price is its CPU and what share is its memory.
	VCPUGBRatio float64
}

const (
	hoursPerMonth = 730
	// consolidationHeadroom is what a node is kept free of when asking how few nodes would do.
	consolidationHeadroom = 0.2
	minNodes              = 2
)

func (o Options) ratio() float64 {
	if o.VCPUGBRatio > 0 {
		return o.VCPUGBRatio
	}
	return 7.5
}

func clamp01(v float64) float64 { return math.Min(1, math.Max(0, v)) }

func frac(v, capacity float64) float64 {
	if capacity <= 0 {
		return 0
	}
	return clamp01(v / capacity)
}

// cpuShare is the part of a node's price that is its CPU, from what a core and a GiB are worth against each other.
func cpuShare(cores, mem, ratio float64) float64 {
	if cores <= 0 || mem <= 0 {
		return 0.5
	}
	c, m := cores, mem/float64(1<<30)/ratio
	return c / (c + m)
}

func isWorkload(app string) bool {
	return strings.HasPrefix(app, "/k8s/") || strings.HasPrefix(app, "/k8s-cronjob/")
}

func namespaceOf(app string) string {
	for _, p := range []string{"/k8s/", "/k8s-cronjob/"} {
		if rest, ok := strings.CutPrefix(app, p); ok {
			ns, _, _ := strings.Cut(rest, "/")
			return ns
		}
	}
	return ""
}

type pair struct{ node, app string }

type pairUse struct{ useCPU, useMem, reqCPU, reqMem float64 }

// Compute costs the nodes and the applications, and says where the money is wasted.
func Compute(in Inputs, o Options) Report {
	r := Report{
		Currency: "USD", WindowSeconds: in.WindowSeconds, GeneratedAt: in.Now, HasRequests: in.HasRequests,
		Nodes: []Node{}, Applications: []App{}, Analysis: []Finding{}, Warnings: []string{},
	}
	byMachine := map[string]int{}
	byName := map[string]int{}
	for i, n := range in.Nodes {
		byMachine[n.MachineID] = i
		byName[n.Name] = i
	}

	reqCPU := make([]float64, len(in.Nodes))
	reqMem := make([]float64, len(in.Nodes))
	pairs := map[pair]*pairUse{}
	apps := map[string]*App{}
	get := func(id, label string) *App {
		a := apps[id]
		if a == nil {
			a = &App{App: id, Label: label, Namespace: namespaceOf(id)}
			apps[id] = a
		}
		if a.Label == "" {
			a.Label = label
		}
		return a
	}
	pairOf := func(i int, app string) *pairUse {
		k := pair{in.Nodes[i].Name, app}
		p := pairs[k]
		if p == nil {
			p = &pairUse{}
			pairs[k] = p
		}
		return p
	}
	for _, u := range in.Usage {
		i, ok := byMachine[u.MachineID]
		if !ok || !isWorkload(u.App) {
			continue
		}
		a := get(u.App, u.Label)
		a.CPU.Usage += u.CPU
		a.Memory.Usage += u.Mem
		p := pairOf(i, u.App)
		p.useCPU += u.CPU
		p.useMem += u.Mem
	}
	if in.HasRequests {
		for _, q := range in.Requests {
			i, ok := byName[q.Node]
			if !ok || !isWorkload(q.App) {
				continue
			}
			reqCPU[i] += q.CPU
			reqMem[i] += q.Mem
			a := get(q.App, q.Label)
			a.CPU.Requests = ptr(deref(a.CPU.Requests) + q.CPU)
			a.Memory.Requests = ptr(deref(a.Memory.Requests) + q.Mem)
			p := pairOf(i, q.App)
			p.reqCPU += q.CPU
			p.reqMem += q.Mem
		}
	}

	// the nodes
	type parts struct{ cpuP, memP, capCPU, capMem float64 }
	pr := make(map[string]parts, len(in.Nodes))
	for i, n := range in.Nodes {
		out := Node{
			Name: n.Name, MachineID: n.MachineID, Provider: n.Provider, Region: n.Region, Zone: n.Zone, InstanceType: n.InstanceType,
			Lifecycle: n.Lifecycle, LifecycleAssumed: n.LifecycleAssumed, PriceSource: n.PriceSource,
			CPU:      Resource{Capacity: n.CPUCores, Usage: n.CPUUsed},
			Memory:   Resource{Capacity: n.MemBytes, Usage: n.MemUsed},
			CPUShare: cpuShare(n.CPUCores, n.MemBytes, o.ratio()),
		}
		if in.HasRequests {
			out.CPU.Requests, out.Memory.Requests = ptr(reqCPU[i]), ptr(reqMem[i])
		}
		if n.HourlyPrice != nil {
			price := *n.HourlyPrice * hoursPerMonth
			out.PriceMonthly = &price
			cpuP, memP := price*out.CPUShare, price*(1-out.CPUShare)
			uCPU, uMem := frac(n.CPUUsed, n.CPUCores), frac(n.MemUsed, n.MemBytes)
			aCPU, aMem := uCPU, uMem
			if in.HasRequests {
				aCPU, aMem = frac(math.Max(reqCPU[i], n.CPUUsed), n.CPUCores), frac(math.Max(reqMem[i], n.MemUsed), n.MemBytes)
			}
			out.UsedMonthly = cpuP*uCPU + memP*uMem
			allocated := cpuP*aCPU + memP*aMem
			out.OverprovisionedMonthly = math.Max(0, allocated-out.UsedMonthly)
			out.IdleMonthly = math.Max(0, price-allocated)
			pr[n.Name] = parts{cpuP, memP, n.CPUCores, n.MemBytes}
			r.Totals.PriceMonthly += price
			r.Totals.UsedMonthly += out.UsedMonthly
			r.Totals.OverprovisionedMonthly += out.OverprovisionedMonthly
			r.Totals.IdleMonthly += out.IdleMonthly
		} else {
			r.Totals.NodesWithoutPrice++
		}
		r.Nodes = append(r.Nodes, out)
	}
	sort.SliceStable(r.Nodes, func(i, j int) bool {
		if r.Nodes[i].IdleMonthly != r.Nodes[j].IdleMonthly {
			return r.Nodes[i].IdleMonthly > r.Nodes[j].IdleMonthly
		}
		return r.Nodes[i].Name < r.Nodes[j].Name
	})

	// the applications, across the nodes they run on
	usage := map[string]float64{}
	alloc := map[string]float64{}
	for k, p := range pairs {
		pt, ok := pr[k.node]
		if !ok {
			continue // a node with no price: nothing to say in money
		}
		usage[k.app] += (pt.cpuP*frac(p.useCPU, pt.capCPU) + pt.memP*frac(p.useMem, pt.capMem))
		if in.HasRequests {
			alloc[k.app] += pt.cpuP*frac(math.Max(p.reqCPU, p.useCPU), pt.capCPU) + pt.memP*frac(math.Max(p.reqMem, p.useMem), pt.capMem)
		}
	}
	var appAlloc, appOver float64
	for id, a := range apps {
		a.UsageMonthly = usage[id]
		if in.HasRequests {
			a.AllocationMonthly = ptr(alloc[id])
			a.OverprovisioningMonthly = ptr(math.Max(0, alloc[id]-usage[id]))
			appAlloc += alloc[id]
			appOver += *a.OverprovisioningMonthly
		}
		r.Totals.AppUsageMonthly += a.UsageMonthly
		r.Applications = append(r.Applications, *a)
	}
	if in.HasRequests {
		r.Totals.AppAllocationMonthly, r.Totals.AppOverprovisionMonthly = ptr(appAlloc), ptr(appOver)
	}
	r.Totals.UnattributedMonthly = math.Max(0, r.Totals.UsedMonthly-r.Totals.AppUsageMonthly)
	sort.SliceStable(r.Applications, func(i, j int) bool {
		if r.Applications[i].UsageMonthly != r.Applications[j].UsageMonthly {
			return r.Applications[i].UsageMonthly > r.Applications[j].UsageMonthly
		}
		return r.Applications[i].Label < r.Applications[j].Label
	})

	r.Analysis = analyse(r, in)
	return r
}

func ptr(v float64) *float64 { return &v }

func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
