package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/sojoner/kro-lab/platform-mvp/vm-binding-controller/adapter"
	"github.com/sojoner/kro-lab/platform-mvp/vm-binding-controller/domain"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
)

func clusterProfile() *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": testRegion, "namespace": "default"},
		"spec":     map[string]interface{}{"displayName": "EU spoke"},
	}}
	u.SetGroupVersionKind(clusterProfileGVK)
	return u
}

func runningVMI(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": name, "namespace": tenantNS},
		"status": map[string]interface{}{
			"phase": "Running", "nodeName": "eu-worker",
			"interfaces": []interface{}{map[string]interface{}{"ipAddress": "10.244.1.7"}},
		},
	}}
	u.SetGroupVersionKind(adapter.VMInstanceGVK)
	return u
}

func launcher(vm string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "virt-launcher-" + vm, Namespace: tenantNS,
			Labels: map[string]string{adapter.LabelVMName: vm},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.1.7"},
	}
}

func fixedVersion(v string) func(cluster.Cluster) (string, error) {
	return func(cluster.Cluster) (string, error) { return v, nil }
}

func TestProfileDescriber(t *testing.T) {
	hub := newClient()
	if err := hub.Create(context.Background(), clusterProfile()); err != nil {
		t.Fatal(err)
	}
	d := &ProfileDescriber{Hub: hub, Namespace: "default", Version: fixedVersion("v1.35.0")}

	got, err := d.Describe(context.Background(), testRegion, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := domain.ClusterInfo{Name: testRegion, DisplayName: "EU spoke", KubernetesVersion: "v1.35.0"}
	if got != want {
		t.Errorf("Describe = %+v, want %+v", got, want)
	}

	missing := &ProfileDescriber{Hub: newClient(), Namespace: "default", Version: fixedVersion("v1.35.0")}
	if _, err := missing.Describe(context.Background(), testRegion, nil); err == nil {
		t.Error("a missing ClusterProfile must be an error so the caller can degrade explicitly")
	}
}

type fakeDescriber struct {
	info domain.ClusterInfo
	err  error
}

func (f fakeDescriber) Describe(context.Context, string, cluster.Cluster) (domain.ClusterInfo, error) {
	return f.info, f.err
}

func TestSyncStatus_ReportsClusterVMInstanceAndPod(t *testing.T) {
	r := domainRequest(t, 1)
	e := newEnv([]client.Object{requestObj("demo-eu", 1)},
		[]client.Object{existingVM(r, "demo-eu-0", true), runningVMI("demo-eu-0"), launcher("demo-eu-0")}, true)
	sr := statusEnv(e)
	sr.Describer = fakeDescriber{info: domain.ClusterInfo{Name: testRegion, KubernetesVersion: "v1.35.0"}}
	if _, err := sr.Reconcile(context.Background(), vmEvent("demo-eu-0")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	regions, _, _ := unstructured.NestedSlice(hubRequest(t, e.hub, "demo-eu").Object, "status", "regions")
	entry := regions[0].(map[string]interface{})
	if v, _, _ := unstructured.NestedString(entry, "cluster", "kubernetesVersion"); v != "v1.35.0" {
		t.Errorf("cluster version = %q, want v1.35.0 (entry %v)", v, entry)
	}
	vm := entry["vms"].([]interface{})[0].(map[string]interface{})
	if vm["node"] != "eu-worker" || vm["ip"] != "10.244.1.7" {
		t.Errorf("VM details = %v", vm)
	}
	if name, _, _ := unstructured.NestedString(vm, "pod", "name"); name != "virt-launcher-demo-eu-0" {
		t.Errorf("pod name = %q, want virt-launcher-demo-eu-0 (vm %v)", name, vm)
	}
}

func TestSyncStatus_DescriberFailureStillReportsVMState(t *testing.T) {
	r := domainRequest(t, 1)
	e := newEnv([]client.Object{requestObj("demo-eu", 1)}, []client.Object{existingVM(r, "demo-eu-0", true)}, true)
	sr := statusEnv(e)
	sr.Describer = fakeDescriber{err: errors.New("profile missing")}

	if _, err := sr.Reconcile(context.Background(), vmEvent("demo-eu-0")); err != nil {
		t.Fatalf("a failed cluster lookup must not fail the sync: %v", err)
	}
	u := hubRequest(t, e.hub, "demo-eu")
	if phase, ready := regionStatus(t, u); phase != "Ready" || ready != 1 {
		t.Errorf("status = %s/%d, want Ready/1", phase, ready)
	}
	regions, _, _ := unstructured.NestedSlice(u.Object, "status", "regions")
	if _, has := regions[0].(map[string]interface{})["cluster"]; has {
		t.Error("cluster block should be omitted when the lookup fails")
	}
}

func restartedLauncher(vm string, restarts int32, phase corev1.PodPhase) *corev1.Pod {
	p := launcher(vm)
	p.Status.Phase = phase
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: restarts}}
	return p
}

func entryOf(t *testing.T, e *env) map[string]interface{} {
	t.Helper()
	regions, _, _ := unstructured.NestedSlice(hubRequest(t, e.hub, "demo-eu").Object, "status", "regions")
	return regions[0].(map[string]interface{})
}

func TestSyncStatus_ReportsPodTotals(t *testing.T) {
	r := domainRequest(t, 2)
	e := newEnv([]client.Object{requestObj("demo-eu", 2)}, []client.Object{
		existingVM(r, "demo-eu-0", true), existingVM(r, "demo-eu-1", true),
		restartedLauncher("demo-eu-0", 3, corev1.PodRunning),
		restartedLauncher("demo-eu-1", 2, corev1.PodFailed),
		// A pod of another VmSet in the same tenant namespace must not be counted.
		restartedLauncher("someone-elses-vm", 9, corev1.PodFailed),
	}, true)

	if _, err := statusEnv(e).Reconcile(context.Background(), vmEvent("demo-eu-0")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	entry := entryOf(t, e)
	if entry["podRestarts"] != int64(5) || entry["podsNotRunning"] != int64(1) {
		t.Errorf("totals = restarts %v, notRunning %v, want 5 and 1", entry["podRestarts"], entry["podsNotRunning"])
	}
}

func TestSyncStatus_UnreadablePodsLeaveTotalsOut(t *testing.T) {
	r := domainRequest(t, 1)
	e := newEnv([]client.Object{requestObj("demo-eu", 1)}, nil, true)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	spoke := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingVM(r, "demo-eu-0", true)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, isPods := list.(*corev1.PodList); isPods {
					return errors.New("pods is forbidden")
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()
	e.r.Clusters = fakeClusters{testRegion: &fakeSpoke{c: spoke}}

	if _, err := statusEnv(e).Reconcile(context.Background(), vmEvent("demo-eu-0")); err != nil {
		t.Fatalf("a failed pod read must not fail the sync: %v", err)
	}
	if phase, ready := regionStatus(t, hubRequest(t, e.hub, "demo-eu")); phase != "Ready" || ready != 1 {
		t.Errorf("status = %s/%d, want Ready/1: VM state is still reported", phase, ready)
	}
	entry := entryOf(t, e)
	if _, ok := entry["podRestarts"]; ok {
		t.Errorf("unreadable pods must not be reported as zero, got %v", entry)
	}
}
