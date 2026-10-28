// Package model is the gateway's public vocabulary: the JSON a client sees
// for a VmSet, and the rules for turning a create request into a hub object.
// It does no I/O; every function is deterministic.
package model

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// LabelTenant ties a hub object to the tenant that owns it.
const LabelTenant = "platform.example.com/tenant"

// Group and Version of the Kro-generated VmSet API. The plural is a gateway
// flag because Kro derives it.
const (
	Group   = "platform.example.com"
	Version = "v1alpha1"
	Kind    = "VmSet"
)

type Phase string

const (
	PhasePending       Phase = "Pending"
	PhaseReady         Phase = "Ready"
	PhaseUnschedulable Phase = "Unschedulable"
)

type Pod struct {
	Name     string `json:"name"`
	Phase    string `json:"phase"`
	PodIP    string `json:"podIP,omitempty"`
	Restarts int    `json:"restarts"`
}

type VM struct {
	Name  string `json:"name"`
	Phase string `json:"phase"`
	Node  string `json:"node,omitempty"`
	IP    string `json:"ip,omitempty"`
	Pod   *Pod   `json:"pod,omitempty"`
}

type Cluster struct {
	Name                string `json:"name"`
	DisplayName         string `json:"displayName,omitempty"`
	KubernetesVersion   string `json:"kubernetesVersion,omitempty"`
	ControlPlaneHealthy string `json:"controlPlaneHealthy,omitempty"`
}

type Region struct {
	Region        string   `json:"region"`
	Phase         string   `json:"phase"`
	ReadyReplicas int      `json:"readyReplicas"`
	Message       string   `json:"message,omitempty"`
	Cluster       *Cluster `json:"cluster,omitempty"`
	VMs           []VM     `json:"vms,omitempty"`
}

// VmSet is what the gateway returns for one VmSet.
type VmSet struct {
	Name     string   `json:"name"`
	Tenant   string   `json:"tenant"`
	Size     string   `json:"size,omitempty"`
	Replicas int      `json:"replicas"`
	Phase    Phase    `json:"phase"`
	Regions  []Region `json:"regions"`
}

// Summarize flattens a hub VmSet object into the public model. Fields the
// hub has not filled in yet (status arrives after the controllers report)
// are left empty rather than treated as errors.
func Summarize(u *unstructured.Unstructured) VmSet {
	size, _, _ := unstructured.NestedString(u.Object, "spec", "size")
	out := VmSet{
		Name:     u.GetName(),
		Tenant:   u.GetLabels()[LabelTenant],
		Size:     size,
		Replicas: int(number(u.Object, "spec", "replicas")),
		Regions:  []Region{},
	}
	if raw, found, _ := unstructured.NestedFieldNoCopy(u.Object, "status", "regions"); found {
		// The status is controller-written JSON; round-tripping it through the
		// typed model keeps the decoding rules in the struct tags.
		if b, err := json.Marshal(raw); err == nil {
			_ = json.Unmarshal(b, &out.Regions)
		}
		sort.SliceStable(out.Regions, func(i, j int) bool { return out.Regions[i].Region < out.Regions[j].Region })
	}
	out.Phase = rollup(out.Regions)
	return out
}

// rollup is Ready only when every region is Ready, Unschedulable as soon as
// any region cannot be placed, and Pending otherwise (including no status yet).
func rollup(regions []Region) Phase {
	if len(regions) == 0 {
		return PhasePending
	}
	ready := 0
	for _, r := range regions {
		switch Phase(r.Phase) {
		case PhaseUnschedulable:
			return PhaseUnschedulable
		case PhaseReady:
			ready++
		}
	}
	if ready == len(regions) {
		return PhaseReady
	}
	return PhasePending
}

func number(obj map[string]interface{}, fields ...string) int64 {
	v, _, _ := unstructured.NestedFieldNoCopy(obj, fields...)
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

// CreateRequest is the body of POST /v1/tenants/{tenant}/vmsets.
type CreateRequest struct {
	Name         string   `json:"name"`
	Regions      []string `json:"regions"`
	Size         string   `json:"size"`
	Replicas     *int     `json:"replicas"`
	SSHPublicKey string   `json:"sshPublicKey"`
}

// Rules are the limits a create request must satisfy before it reaches the
// hub. They mirror the VmSet schema so callers get a clear 400 here instead
// of an admission error from Kubernetes.
type Rules struct {
	Sizes        []string
	DefaultSize  string
	MaxReplicas  int
	SSHKeyFormat *regexp.Regexp
}

var (
	nameFormat   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$`)
	regionFormat = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
)

// ValidTenant reports whether s can be a tenant, which is also a namespace name.
func ValidTenant(s string) bool {
	return len(s) <= 63 && regionFormat.MatchString(s)
}

// ValidationError lists everything wrong with a request, so a client can fix
// it in one round trip.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string { return strings.Join(e.Problems, "; ") }

// Build validates the request and renders the hub VmSet for the tenant. The
// tenant label and namespace always come from the path, never the body.
func Build(tenant string, req CreateRequest, rules Rules) (*unstructured.Unstructured, error) {
	var problems []string
	if !ValidTenant(tenant) {
		problems = append(problems, fmt.Sprintf("tenant %q is not a valid namespace name", tenant))
	}
	if !nameFormat.MatchString(req.Name) {
		problems = append(problems, "name must be a lowercase DNS label of at most 40 characters")
	}

	regions := dedupe(req.Regions)
	if len(regions) == 0 {
		problems = append(problems, "regions must list at least one region")
	}
	for _, r := range regions {
		if !regionFormat.MatchString(r) {
			problems = append(problems, fmt.Sprintf("region %q is not a valid region name", r))
		}
	}

	size := req.Size
	if size == "" {
		size = rules.DefaultSize
	}
	if !contains(rules.Sizes, size) {
		problems = append(problems, fmt.Sprintf("size must be one of %s", strings.Join(rules.Sizes, ", ")))
	}

	replicas := 1
	if req.Replicas != nil {
		replicas = *req.Replicas
	}
	if replicas < 1 || replicas > rules.MaxReplicas {
		problems = append(problems, fmt.Sprintf("replicas must be between 1 and %d", rules.MaxReplicas))
	}

	if req.SSHPublicKey == "" {
		problems = append(problems, "sshPublicKey is required")
	} else if rules.SSHKeyFormat != nil && !rules.SSHKeyFormat.MatchString(req.SSHPublicKey) {
		problems = append(problems, "sshPublicKey is not an accepted public key format")
	}

	if len(problems) > 0 {
		return nil, &ValidationError{Problems: problems}
	}

	regionList := make([]interface{}, len(regions))
	for i, r := range regions {
		regionList[i] = r
	}
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":      req.Name,
			"namespace": tenant,
			"labels":    map[string]interface{}{LabelTenant: tenant},
		},
		"spec": map[string]interface{}{
			"regions":      regionList,
			"size":         size,
			"replicas":     int64(replicas),
			"sshPublicKey": req.SSHPublicKey,
		},
	}}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: Group, Version: Version, Kind: Kind})
	return u, nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
