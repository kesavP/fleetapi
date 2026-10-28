package domain

import (
	"reflect"
	"testing"
)

func req(replicas int) Request {
	return Request{Name: "demo-eu", Namespace: "hub-ns", Region: "eu", Tenant: "acme-corp", Replicas: replicas}
}

func TestVMName(t *testing.T) {
	if got := VMName("demo-eu", 3); got != "demo-eu-3" {
		t.Errorf("VMName = %q, want demo-eu-3", got)
	}
}

func TestDesiredVMNames(t *testing.T) {
	got := DesiredVMNames(req(3))
	want := []string{"demo-eu-0", "demo-eu-1", "demo-eu-2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DesiredVMNames = %v, want %v", got, want)
	}
}

func TestPlanVMs(t *testing.T) {
	tests := []struct {
		name     string
		replicas int
		existing []string
		create   []string
		del      []string
	}{
		{"creates all when none exist", 2, nil, []string{"demo-eu-0", "demo-eu-1"}, nil},
		{"creates only the missing ones", 3, []string{"demo-eu-0", "demo-eu-2"}, []string{"demo-eu-1"}, nil},
		{"is idempotent when converged", 2, []string{"demo-eu-1", "demo-eu-0"}, nil, nil},
		{"scale down deletes highest index first", 1, []string{"demo-eu-0", "demo-eu-1", "demo-eu-2"}, nil, []string{"demo-eu-2", "demo-eu-1"}},
		{"index order is numeric, not lexical", 1, []string{"demo-eu-0", "demo-eu-2", "demo-eu-10"}, nil, []string{"demo-eu-10", "demo-eu-2"}},
		{"unrecognised owned names are surplus and go last", 1, []string{"demo-eu-0", "demo-eu-1", "stray"}, nil, []string{"demo-eu-1", "stray"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PlanVMs(req(tt.replicas), tt.existing)
			if !reflect.DeepEqual(got.Create, tt.create) {
				t.Errorf("Create = %v, want %v", got.Create, tt.create)
			}
			if !reflect.DeepEqual(got.Delete, tt.del) {
				t.Errorf("Delete = %v, want %v", got.Delete, tt.del)
			}
		})
	}
}

func TestTenantNamespace(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"nil labels fall back", nil, "default"},
		{"missing label falls back", map[string]string{"x": "y"}, "default"},
		{"empty label falls back", map[string]string{LabelTenant: ""}, "default"},
		{"label wins", map[string]string{LabelTenant: "acme-corp"}, "acme-corp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TenantNamespace(tt.labels, "default"); got != tt.want {
				t.Errorf("TenantNamespace = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadyStatus(t *testing.T) {
	tests := []struct {
		name     string
		replicas int
		ready    []string
		phase    Phase
		count    int
	}{
		{"nothing ready is pending", 2, nil, PhasePending, 0},
		{"partially ready is pending", 2, []string{"demo-eu-0"}, PhasePending, 1},
		{"all ready is ready", 2, []string{"demo-eu-0", "demo-eu-1"}, PhaseReady, 2},
		{"ready VMs beyond desired are not counted", 1, []string{"demo-eu-0", "demo-eu-5"}, PhaseReady, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ReadyStatus(req(tt.replicas), tt.ready)
			if got.Phase != tt.phase || got.ReadyReplicas != tt.count || got.Region != "eu" {
				t.Errorf("ReadyStatus = %+v, want phase %s ready %d region eu", got, tt.phase, tt.count)
			}
		})
	}
}

func TestUnschedulableStatus(t *testing.T) {
	got := UnschedulableStatus("us")
	if got.Phase != PhaseUnschedulable || got.Region != "us" || got.ReadyReplicas != 0 || got.Message == "" {
		t.Errorf("UnschedulableStatus = %+v", got)
	}
}
