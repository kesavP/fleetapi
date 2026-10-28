// Package domain holds the pure rules of the VM service: which VMs a request
// wants, what to create or delete to get there, and what phase that implies.
// It imports nothing from Kubernetes; every function is deterministic.
package domain

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Label keys that tie a spoke VM back to the hub request that owns it.
const (
	LabelTenant           = "platform.example.com/tenant"
	LabelVMSet            = "platform.example.com/vmset"
	LabelRequest          = "platform.example.com/vmset-request"
	LabelRequestNamespace = "platform.example.com/vmset-request-namespace"
)

// Request is one RegionalVmSetRequest, reduced to what planning needs.
type Request struct {
	Name            string // hub object name, also the VM name prefix
	Namespace       string // hub namespace of the request
	Region          string
	Tenant          string // spoke namespace the VMs live in
	VMSet           string // name of the VmSet that produced this request
	Replicas        int
	CPU             int
	Memory          string
	Image           string
	CloudInitSecret string
}

type Phase string

const (
	PhasePending       Phase = "Pending"
	PhaseReady         Phase = "Ready"
	PhaseUnschedulable Phase = "Unschedulable"
)

// VMInstance is the runtime view of one VM: what the spoke reports once the
// VM is actually running (scheduling, node, address).
type VMInstance struct {
	Name  string
	Phase string // VirtualMachineInstance phase; Pending until one exists
	Node  string
	IP    string
	Pod   *PodInfo // the virt-launcher pod backing the VM, once it exists
}

// PodInfo is the hub-visible summary of a VM's virt-launcher pod.
type PodInfo struct {
	Name     string
	Phase    string
	IP       string
	Restarts int
}

// PodSummary totals a region's VM pods, for alerting. Per-VM detail lives in
// Status.VMs; these totals exist because metrics can only be derived from
// one array level of the hub status.
type PodSummary struct {
	Restarts   int // container restarts across the region's launcher pods
	NotRunning int // launcher pods whose phase is not Running
}

// ClusterInfo describes the spoke a region's VMs run on.
type ClusterInfo struct {
	Name                string
	DisplayName         string
	KubernetesVersion   string
	ControlPlaneHealthy string // ClusterProfile condition status: True, False or Unknown; empty when unreported
}

// Status is what the hub request reports for its region.
type Status struct {
	Region        string
	Phase         Phase
	ReadyReplicas int
	Cluster       *ClusterInfo
	Pods          *PodSummary // nil when the pods could not be read
	VMs           []VMInstance
	Message       string
}

// Plan is the set of VM changes that converge the spoke on the request.
type Plan struct {
	Create []string
	Delete []string
}

func VMName(request string, index int) string {
	return fmt.Sprintf("%s-%d", request, index)
}

func DesiredVMNames(r Request) []string {
	names := make([]string, 0, r.Replicas)
	for i := 0; i < r.Replicas; i++ {
		names = append(names, VMName(r.Name, i))
	}
	return names
}

// PlanVMs diffs the wanted VM names against the ones the request already
// owns. Deletions run highest index first so scaling down removes the
// newest VMs; names that do not parse as an index are surplus and go last.
func PlanVMs(r Request, existing []string) Plan {
	have := toSet(existing)
	want := toSet(DesiredVMNames(r))

	var plan Plan
	for _, name := range DesiredVMNames(r) {
		if !have[name] {
			plan.Create = append(plan.Create, name)
		}
	}
	for name := range have {
		if !want[name] {
			plan.Delete = append(plan.Delete, name)
		}
	}
	sort.Slice(plan.Delete, func(i, j int) bool {
		ii, iok := vmIndex(r.Name, plan.Delete[i])
		ji, jok := vmIndex(r.Name, plan.Delete[j])
		switch {
		case iok && jok:
			return ii > ji
		case iok != jok:
			return iok
		default:
			return plan.Delete[i] < plan.Delete[j]
		}
	})
	return plan
}

// TenantNamespace is the spoke namespace for a request: the tenant label,
// or fallback when it is absent or empty.
func TenantNamespace(labels map[string]string, fallback string) string {
	if tenant := labels[LabelTenant]; tenant != "" {
		return tenant
	}
	return fallback
}

// ReadyStatus reports Ready only when every desired VM is ready; ready names
// outside the desired set (leftovers from a scale-down) do not count.
func ReadyStatus(r Request, readyNames []string) Status {
	ready := toSet(readyNames)
	count := 0
	for _, name := range DesiredVMNames(r) {
		if ready[name] {
			count++
		}
	}
	phase := PhasePending
	if count == r.Replicas {
		phase = PhaseReady
	}
	return Status{Region: r.Region, Phase: phase, ReadyReplicas: count}
}

func UnschedulableStatus(region string) Status {
	return Status{
		Region:  region,
		Phase:   PhaseUnschedulable,
		Message: fmt.Sprintf("region %q does not offer VMs", region),
	}
}

func vmIndex(request, name string) (int, bool) {
	suffix, ok := strings.CutPrefix(name, request+"-")
	if !ok {
		return 0, false
	}
	i, err := strconv.Atoi(suffix)
	return i, err == nil
}

func toSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set
}
