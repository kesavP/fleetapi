package model

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var testRules = Rules{
	Sizes:        []string{"M", "S"},
	DefaultSize:  "S",
	MaxReplicas:  3,
	SSHKeyFormat: regexp.MustCompile(`^(ssh-ed25519|ssh-rsa) [A-Za-z0-9+/=]+( [A-Za-z0-9@._-]+)?$`),
}

const goodKey = "ssh-ed25519 AAAAC3Nza me@host"

func hubVmSet(status interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{
			"name": "web", "namespace": "acme-corp",
			"labels": map[string]interface{}{LabelTenant: "acme-corp"},
		},
		"spec": map[string]interface{}{"size": "S", "replicas": int64(2), "regions": []interface{}{"eu"}},
	}}
	if status != nil {
		u.Object["status"] = map[string]interface{}{"regions": status}
	}
	return u
}

func TestSummarize_FlattensStatus(t *testing.T) {
	got := Summarize(hubVmSet([]interface{}{map[string]interface{}{
		"region": "eu", "phase": "Ready", "readyReplicas": int64(2),
		"cluster": map[string]interface{}{"name": "eu", "kubernetesVersion": "v1.35.0"},
		"vms": []interface{}{map[string]interface{}{
			"name": "web-eu-0", "phase": "Running", "node": "eu-worker", "ip": "10.244.1.7",
			"pod": map[string]interface{}{"name": "virt-launcher-x", "phase": "Running", "restarts": int64(1)},
		}},
	}}))

	if got.Name != "web" || got.Tenant != "acme-corp" || got.Size != "S" || got.Replicas != 2 || got.Phase != PhaseReady {
		t.Errorf("top level = %+v", got)
	}
	r := got.Regions[0]
	if r.Cluster == nil || r.Cluster.KubernetesVersion != "v1.35.0" {
		t.Errorf("cluster = %+v", r.Cluster)
	}
	if vm := r.VMs[0]; vm.Node != "eu-worker" || vm.Pod == nil || vm.Pod.Name != "virt-launcher-x" || vm.Pod.Restarts != 1 {
		t.Errorf("vm = %+v", vm)
	}
}

func TestSummarize_PhaseRollup(t *testing.T) {
	region := func(name, phase string) interface{} {
		return map[string]interface{}{"region": name, "phase": phase}
	}
	tests := map[string]struct {
		status interface{}
		want   Phase
	}{
		"no status yet":         {nil, PhasePending},
		"all ready":             {[]interface{}{region("eu", "Ready"), region("us", "Ready")}, PhaseReady},
		"one pending":           {[]interface{}{region("eu", "Ready"), region("us", "Pending")}, PhasePending},
		"any unschedulable win": {[]interface{}{region("eu", "Ready"), region("us", "Unschedulable")}, PhaseUnschedulable},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := Summarize(hubVmSet(tt.status)).Phase; got != tt.want {
				t.Errorf("phase = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestSummarize_RegionsAreSorted(t *testing.T) {
	got := Summarize(hubVmSet([]interface{}{
		map[string]interface{}{"region": "us", "phase": "Ready"},
		map[string]interface{}{"region": "eu", "phase": "Ready"},
	}))
	if got.Regions[0].Region != "eu" {
		t.Errorf("regions = %+v, want eu first", got.Regions)
	}
}

func TestBuild_RendersHubObject(t *testing.T) {
	two := 2
	u, err := Build("acme-corp", CreateRequest{
		Name: "web", Regions: []string{"eu", "eu", "us"}, Size: "M", Replicas: &two, SSHPublicKey: goodKey,
	}, testRules)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.GetNamespace() != "acme-corp" || u.GetLabels()[LabelTenant] != "acme-corp" {
		t.Errorf("tenant namespace/label = %s/%v", u.GetNamespace(), u.GetLabels())
	}
	if u.GetAPIVersion() != "platform.example.com/v1alpha1" || u.GetKind() != "VmSet" {
		t.Errorf("type = %s %s", u.GetAPIVersion(), u.GetKind())
	}
	regions, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "regions")
	if !reflect.DeepEqual(regions, []string{"eu", "us"}) {
		t.Errorf("regions = %v, want duplicates removed", regions)
	}
	if replicas, _, _ := unstructured.NestedInt64(u.Object, "spec", "replicas"); replicas != 2 {
		t.Errorf("replicas = %d", replicas)
	}
}

func TestBuild_Defaults(t *testing.T) {
	u, err := Build("acme-corp", CreateRequest{Name: "web", Regions: []string{"eu"}, SSHPublicKey: goodKey}, testRules)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if size, _, _ := unstructured.NestedString(u.Object, "spec", "size"); size != "S" {
		t.Errorf("size = %q, want default S", size)
	}
	if replicas, _, _ := unstructured.NestedInt64(u.Object, "spec", "replicas"); replicas != 1 {
		t.Errorf("replicas = %d, want default 1", replicas)
	}
}

func TestBuild_RejectsInvalidRequests(t *testing.T) {
	zero, four := 0, 4
	tests := map[string]struct {
		tenant string
		req    CreateRequest
		want   string
	}{
		"bad name":        {"t", CreateRequest{Name: "Web_1", Regions: []string{"eu"}, SSHPublicKey: goodKey}, "name"},
		"no regions":      {"t", CreateRequest{Name: "web", SSHPublicKey: goodKey}, "regions must list"},
		"bad region":      {"t", CreateRequest{Name: "web", Regions: []string{"EU!"}, SSHPublicKey: goodKey}, "region"},
		"unknown size":    {"t", CreateRequest{Name: "web", Regions: []string{"eu"}, Size: "XL", SSHPublicKey: goodKey}, "size"},
		"zero replicas":   {"t", CreateRequest{Name: "web", Regions: []string{"eu"}, Replicas: &zero, SSHPublicKey: goodKey}, "replicas"},
		"too many":        {"t", CreateRequest{Name: "web", Regions: []string{"eu"}, Replicas: &four, SSHPublicKey: goodKey}, "replicas"},
		"missing key":     {"t", CreateRequest{Name: "web", Regions: []string{"eu"}}, "sshPublicKey is required"},
		"shell injection": {"t", CreateRequest{Name: "web", Regions: []string{"eu"}, SSHPublicKey: "ssh-ed25519 A'; rm -rf /"}, "public key format"},
		"bad tenant":      {"Not A NS", CreateRequest{Name: "web", Regions: []string{"eu"}, SSHPublicKey: goodKey}, "tenant"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Build(tt.tenant, tt.req, testRules)
			ve, ok := err.(*ValidationError)
			if !ok {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
			if !strings.Contains(ve.Error(), tt.want) {
				t.Errorf("error %q should mention %q", ve.Error(), tt.want)
			}
		})
	}
}

func TestBuild_ReportsEveryProblemAtOnce(t *testing.T) {
	_, err := Build("t", CreateRequest{Name: "Bad", Size: "XL"}, testRules)
	ve := err.(*ValidationError)
	if len(ve.Problems) < 4 {
		t.Errorf("problems = %v, want name, regions, size and key reported together", ve.Problems)
	}
}
