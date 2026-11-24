package costs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/detect"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/inventory"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/store"
)

// Source is what a report is read from: the same calls the read API is made of, and what the cluster collector says.
type Source interface {
	QueryMetric(ctx context.Context, q store.MetricQuery) (store.MetricResult, error)
	InventorySummary(ctx context.Context, project string) ([]store.InventoryCluster, error)
	ListInventory(ctx context.Context, q store.InventoryQuery) ([]store.InventoryObject, error)
}

// The node-agent's metrics a report is made of. Every one of them carries the node's machine_id, which is how a container is placed on a node.
const (
	metricCloudInfo   = "node_cloud_info"
	metricNodeInfo    = "node_info"
	metricCores       = "node_resources_cpu_logical_cores"
	metricMemTotal    = "node_resources_memory_total_bytes"
	metricMemAvail    = "node_resources_memory_available_bytes"
	metricNodeCPU     = "node_resources_cpu_usage_seconds_total"
	metricContainerCP = "container_resources_cpu_usage_seconds_total"
	metricContainerMe = "container_resources_memory_rss_bytes"
)

const (
	minWindow      = 60
	maxWindow      = 31 * 24 * 3600
	maxPods        = 5000
	staleInventory = 10 * time.Minute
)

// ErrWindow is a window that is shorter than a minute or longer than 31 days.
var ErrWindow = errors.New("the window must be from 60 seconds to 31 days")

// Collector reads a project's metrics and inventory, prices its nodes, and makes the report.
type Collector struct {
	Source    Source
	Prices    *Prices
	Opts      Options
	Log       *slog.Logger
	Now       func() time.Time
	MaxSeries int
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// costStep is the step a window is read at, in whole minutes: about 288 points a series however long the window is.
func costStep(window int) int { return max(60, int(math.Ceil(float64(window)/288/60))*60) }

// mean is the average of a series' points, and false when it has none.
func mean(s store.MetricSeries) (float64, bool) {
	var sum float64
	var n int
	for _, p := range s.Points {
		if !math.IsNaN(p.V) {
			sum += p.V
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

// useKey is a workload on a node.
type useKey struct{ machine, app string }

type podData struct {
	Node         string `json:"node"`
	Phase        string `json:"phase"`
	WorkloadKind string `json:"workload_kind"`
	WorkloadName string `json:"workload_name"`
	Containers   []struct {
		Requests struct {
			CPUm float64 `json:"cpu_m"`
			Mem  float64 `json:"memory_bytes"`
		} `json:"requests"`
	} `json:"containers"`
}

// Report is the cost report of the project over the last `window` seconds.
func (c *Collector) Report(ctx context.Context, project string, window int) (Report, error) {
	if window < minWindow || window > maxWindow {
		return Report{}, ErrWindow
	}
	step := costStep(window)
	end := c.now().Unix() / int64(step) * int64(step)
	var warnings []string
	query := func(name string, rate bool) (store.MetricResult, error) {
		res, err := c.Source.QueryMetric(ctx, store.MetricQuery{
			Project: project, Name: name, From: time.Unix(end-int64(window), 0), To: time.Unix(end, 0), StepSec: step, Rate: rate, MaxSeries: c.MaxSeries, MaxPoints: 2_000_000,
		})
		if err != nil {
			return res, fmt.Errorf("reading %s: %w", name, err)
		}
		if res.Truncated {
			warnings = append(warnings, name+" returned more series than one read allows, so some of it is missing and the costs are low")
		}
		return res, nil
	}
	read := map[string]store.MetricResult{}
	for _, m := range []struct {
		name string
		rate bool
	}{
		{metricCloudInfo, false}, {metricNodeInfo, false}, {metricCores, false}, {metricMemTotal, false}, {metricMemAvail, false},
		{metricNodeCPU, true}, {metricContainerCP, true}, {metricContainerMe, false},
	} {
		res, err := query(m.name, m.rate)
		if err != nil {
			return Report{}, err
		}
		read[m.name] = res
	}

	// the nodes: a machine whose cores and memory are known
	type acc struct {
		in       NodeIn
		haveCPU  bool
		haveMem  bool
		memAvail float64
		haveAvl  bool
	}
	nodes := map[string]*acc{}
	var order []string
	node := func(machine string) *acc {
		a := nodes[machine]
		if a == nil {
			a = &acc{in: NodeIn{MachineID: machine}}
			nodes[machine] = a
			order = append(order, machine)
		}
		return a
	}
	for _, s := range read[metricCores].Series {
		if v, ok := mean(s); ok && s.Labels["machine_id"] != "" {
			a := node(s.Labels["machine_id"])
			a.in.CPUCores, a.haveCPU = v, true
		}
	}
	for _, s := range read[metricMemTotal].Series {
		if v, ok := mean(s); ok && s.Labels["machine_id"] != "" {
			a := node(s.Labels["machine_id"])
			a.in.MemBytes, a.haveMem = v, true
		}
	}
	for _, s := range read[metricMemAvail].Series {
		if v, ok := mean(s); ok && nodes[s.Labels["machine_id"]] != nil {
			a := nodes[s.Labels["machine_id"]]
			a.memAvail, a.haveAvl = v, true
		}
	}
	for _, s := range read[metricNodeCPU].Series {
		mode := s.Labels["mode"]
		if mode == "idle" || mode == "iowait" || nodes[s.Labels["machine_id"]] == nil {
			continue
		}
		if v, ok := mean(s); ok {
			nodes[s.Labels["machine_id"]].in.CPUUsed += v
		}
	}
	for _, s := range read[metricNodeInfo].Series {
		if a := nodes[s.Labels["machine_id"]]; a != nil {
			a.in.Name = s.Labels["hostname"]
		}
	}
	for _, s := range read[metricCloudInfo].Series {
		a := nodes[s.Labels["machine_id"]]
		if a == nil {
			continue
		}
		a.in.Provider, a.in.Region, a.in.Zone, a.in.InstanceType = s.Labels["provider"], s.Labels["region"], s.Labels["availability_zone"], s.Labels["instance_type"]
		switch lc := strings.ToLower(s.Labels["instance_life_cycle"]); {
		case lc == "":
			a.in.Lifecycle, a.in.LifecycleAssumed = "on-demand", true
		case strings.Contains(lc, "spot") || strings.Contains(lc, "preempt"):
			a.in.Lifecycle = "spot"
		default:
			a.in.Lifecycle = lc
		}
	}

	in := Inputs{WindowSeconds: window, Now: c.now()}
	warned := map[string]bool{}
	byMachine := map[string]string{}
	for _, m := range order {
		a := nodes[m]
		if !a.haveCPU || !a.haveMem {
			continue // a node whose size is not known cannot be costed
		}
		if a.in.Name == "" {
			a.in.Name = m
			if len(m) > 12 {
				a.in.Name = m[:12]
			}
		}
		if a.haveAvl {
			a.in.MemUsed = math.Max(0, a.in.MemBytes-a.memAvail)
		}
		if c.Prices != nil && a.in.InstanceType != "" {
			price, source, err := c.Prices.Hourly(ctx, a.in.Provider, a.in.Region, a.in.InstanceType, a.in.Lifecycle)
			if err != nil {
				if k := a.in.InstanceType + "|" + a.in.Region; !warned[k] {
					warned[k] = true
					warnings = append(warnings, fmt.Sprintf("No price for %s in %s (%s): %v", a.in.InstanceType, a.in.Region, a.in.Provider, err))
				}
			}
			a.in.HourlyPrice, a.in.PriceSource = price, source
		}
		byMachine[m] = a.in.Name
		in.Nodes = append(in.Nodes, a.in)
	}

	// what the workloads used, by node
	use := map[useKey]*WorkloadUse{}
	for _, s := range read[metricContainerCP].Series {
		c.addUse(use, s, true)
	}
	for _, s := range read[metricContainerMe].Series {
		c.addUse(use, s, false)
	}
	for _, u := range use {
		in.Usage = append(in.Usage, *u)
	}

	// what pods request, from the cluster collector
	in.Requests, in.HasRequests = c.requests(ctx, project, &warnings)

	r := Compute(in, c.Opts)
	r.Warnings = append(r.Warnings, warnings...)
	return r, nil
}

func (c *Collector) addUse(use map[useKey]*WorkloadUse, s store.MetricSeries, cpu bool) {
	machine := s.Labels["machine_id"]
	w, ok := detect.WorkloadOf(s.Labels["container_id"])
	if !ok || machine == "" {
		return
	}
	v, ok := mean(s)
	if !ok {
		return
	}
	k := useKey{machine, w.ID}
	u := use[k]
	if u == nil {
		u = &WorkloadUse{MachineID: machine, App: w.ID, Label: w.Label}
		use[k] = u
	}
	if cpu {
		u.CPU += v
	} else {
		u.Mem += v
	}
}

// requests is what running pods request, by node and workload, when the cluster collector is current; false when it is not (or cannot be read).
func (c *Collector) requests(ctx context.Context, project string, warnings *[]string) ([]WorkloadReq, bool) {
	clusters, err := c.Source.InventorySummary(ctx, project)
	if err != nil {
		*warnings = append(*warnings, "The inventory could not be read, so what pods request is not known: "+err.Error())
		return nil, false
	}
	fresh := map[string]bool{}
	for _, cl := range clusters {
		if c.now().Sub(cl.LastReceived) <= staleInventory {
			fresh[cl.Cluster] = true
		}
	}
	if len(fresh) == 0 {
		return nil, false
	}
	rows, err := c.Source.ListInventory(ctx, store.InventoryQuery{Project: project, Kinds: []string{inventory.Pod}, Limit: maxPods})
	if err != nil {
		*warnings = append(*warnings, "The inventory could not be read, so what pods request is not known: "+err.Error())
		return nil, false
	}
	if len(rows) >= maxPods {
		*warnings = append(*warnings, fmt.Sprintf("The inventory holds %d pods or more, so some are not counted and requests are low", maxPods))
	}
	out := []WorkloadReq{}
	for _, o := range rows {
		if !fresh[o.Cluster] {
			continue
		}
		var d podData
		if json.Unmarshal(o.Data, &d) != nil || d.Node == "" || d.Phase == "Succeeded" || d.Phase == "Failed" {
			continue
		}
		app := "/k8s/" + o.Namespace + "/" + o.Name
		label := o.Namespace + "/" + o.Name
		if k := d.WorkloadKind; k != "" && k != "pod" && d.WorkloadName != "" {
			app = detect.AppIDOf(k, o.Namespace, d.WorkloadName)
			label = o.Namespace + "/" + d.WorkloadName
		}
		var cpu, mem float64
		for _, ct := range d.Containers {
			cpu += ct.Requests.CPUm / 1000
			mem += ct.Requests.Mem
		}
		out = append(out, WorkloadReq{Node: d.Node, App: app, Label: label, CPU: cpu, Mem: mem})
	}
	return out, true
}
