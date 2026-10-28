package adapter

import (
	"reflect"
	"testing"

	"github.com/sojoner/kro-lab/platform-mvp/vm-binding-controller/domain"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func requestObject(mutate func(map[string]interface{})) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"region":              "eu",
		"replicas":            int64(2),
		"cpu":                 int64(2),
		"memory":              "2Gi",
		"image":               "quay.io/kubevirt/cirros-container-disk-demo:v1.9.1",
		"cloudInitSecretName": "demo-cloudinit",
	}
	if mutate != nil {
		mutate(spec)
	}
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "platform.example.com/v1alpha1",
		"kind":       "RegionalVmSetRequest",
		"metadata": map[string]interface{}{
			"name":      "demo-eu",
			"namespace": "hub-ns",
			"labels": map[string]interface{}{
				domain.LabelTenant: "acme-corp",
				domain.LabelVMSet:  "demo",
			},
		},
		"spec": spec,
	}}
	return u
}

func TestRequestFromObject(t *testing.T) {
	got, err := RequestFromObject(requestObject(nil), "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := domain.Request{
		Name: "demo-eu", Namespace: "hub-ns", Region: "eu", Tenant: "acme-corp", VMSet: "demo",
		Replicas: 2, CPU: 2, Memory: "2Gi",
		Image:           "quay.io/kubevirt/cirros-container-disk-demo:v1.9.1",
		CloudInitSecret: "demo-cloudinit",
	}
	if got != want {
		t.Errorf("RequestFromObject =\n%+v\nwant\n%+v", got, want)
	}
}

func TestRequestFromObject_FallsBackToDefaultNamespace(t *testing.T) {
	u := requestObject(nil)
	u.SetLabels(nil)
	got, err := RequestFromObject(u, "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Tenant != "default" {
		t.Errorf("Tenant = %q, want default", got.Tenant)
	}
}

func TestRequestFromObject_RejectsIncompleteSpec(t *testing.T) {
	for _, field := range []string{"region", "replicas", "cpu", "memory", "image", "cloudInitSecretName"} {
		t.Run("missing "+field, func(t *testing.T) {
			u := requestObject(func(spec map[string]interface{}) { delete(spec, field) })
			if _, err := RequestFromObject(u, "default"); err == nil {
				t.Errorf("expected an error when spec.%s is missing", field)
			}
		})
	}
}

func testRequest() domain.Request {
	return domain.Request{
		Name: "demo-eu", Namespace: "hub-ns", Region: "eu", Tenant: "acme-corp", VMSet: "demo",
		Replicas: 2, CPU: 2, Memory: "2Gi",
		Image:           "quay.io/kubevirt/cirros-container-disk-demo:v1.9.1",
		CloudInitSecret: "demo-cloudinit",
	}
}

func TestVirtualMachine(t *testing.T) {
	vm := VirtualMachine(testRequest(), "demo-eu-1", "Always")

	if vm.GetKind() != "VirtualMachine" || vm.GetAPIVersion() != "kubevirt.io/v1" {
		t.Errorf("unexpected type %s %s", vm.GetAPIVersion(), vm.GetKind())
	}
	if vm.GetName() != "demo-eu-1" || vm.GetNamespace() != "acme-corp" {
		t.Errorf("unexpected identity %s/%s", vm.GetNamespace(), vm.GetName())
	}
	wantLabels := map[string]string{
		domain.LabelTenant:           "acme-corp",
		domain.LabelVMSet:            "demo",
		domain.LabelRequest:          "demo-eu",
		domain.LabelRequestNamespace: "hub-ns",
	}
	if !reflect.DeepEqual(vm.GetLabels(), wantLabels) {
		t.Errorf("labels = %v, want %v", vm.GetLabels(), wantLabels)
	}

	check := func(path []string, want interface{}) {
		t.Helper()
		got, found, err := unstructured.NestedFieldNoCopy(vm.Object, path...)
		if err != nil || !found || !reflect.DeepEqual(got, want) {
			t.Errorf("%v = %v (found=%v err=%v), want %v", path, got, found, err, want)
		}
	}
	check([]string{"spec", "runStrategy"}, "Always")
	check([]string{"spec", "template", "spec", "domain", "cpu", "cores"}, int64(2))
	check([]string{"spec", "template", "spec", "domain", "memory", "guest"}, "2Gi")
	check([]string{"spec", "template", "spec", "domain", "devices", "logSerialConsole"}, true)

	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	if len(volumes) != 2 {
		t.Fatalf("expected 2 volumes, got %d", len(volumes))
	}
	root := volumes[0].(map[string]interface{})
	if image, _, _ := unstructured.NestedString(root, "containerDisk", "image"); image != testRequest().Image {
		t.Errorf("root disk image = %q", image)
	}
	cloud := volumes[1].(map[string]interface{})
	if secret, _, _ := unstructured.NestedString(cloud, "cloudInitNoCloud", "secretRef", "name"); secret != "demo-cloudinit" {
		t.Errorf("cloud-init secret = %q", secret)
	}
}

func TestSpokeSecret(t *testing.T) {
	hub := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-cloudinit", Namespace: "hub-ns", ResourceVersion: "42", UID: "x"},
		Data:       map[string][]byte{"userdata": []byte("#!/bin/sh\n")},
	}
	got := SpokeSecret(hub, testRequest())

	if got.Name != "demo-cloudinit" || got.Namespace != "acme-corp" {
		t.Errorf("identity = %s/%s", got.Namespace, got.Name)
	}
	if !reflect.DeepEqual(got.Data, hub.Data) {
		t.Errorf("data not copied: %v", got.Data)
	}
	if got.ResourceVersion != "" || got.UID != "" {
		t.Error("hub identity fields must not leak onto the spoke copy")
	}
	if got.Labels[domain.LabelTenant] != "acme-corp" || got.Labels[domain.LabelRequest] != "demo-eu" {
		t.Errorf("labels = %v", got.Labels)
	}
}

func TestOwnerLabels(t *testing.T) {
	want := map[string]string{domain.LabelRequest: "demo-eu", domain.LabelRequestNamespace: "hub-ns"}
	if got := OwnerLabels(testRequest()); !reflect.DeepEqual(got, want) {
		t.Errorf("OwnerLabels = %v, want %v", got, want)
	}
}

func TestRequestKey(t *testing.T) {
	key, ok := RequestKey(map[string]string{domain.LabelRequest: "demo-eu", domain.LabelRequestNamespace: "hub-ns"})
	if !ok || key != (types.NamespacedName{Namespace: "hub-ns", Name: "demo-eu"}) {
		t.Errorf("RequestKey = %v ok=%v", key, ok)
	}
	if _, ok := RequestKey(map[string]string{domain.LabelRequest: "demo-eu"}); ok {
		t.Error("a VM without the namespace label is not ours")
	}
	if _, ok := RequestKey(nil); ok {
		t.Error("an unlabelled VM is not ours")
	}
}

func TestStatusObject(t *testing.T) {
	got := StatusObject(domain.Status{Region: "eu", Phase: domain.PhaseReady, ReadyReplicas: 2})
	want := []interface{}{map[string]interface{}{"region": "eu", "phase": "Ready", "readyReplicas": int64(2)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("StatusObject = %v, want %v", got, want)
	}

	withMsg := StatusObject(domain.UnschedulableStatus("us"))
	entry := withMsg[0].(map[string]interface{})
	if entry["message"] == "" || entry["phase"] != "Unschedulable" {
		t.Errorf("unschedulable entry = %v", entry)
	}
}

func vmWithReady(name string, ready *bool) unstructured.Unstructured {
	u := unstructured.Unstructured{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": name}}}
	if ready != nil {
		u.Object["status"] = map[string]interface{}{"ready": *ready}
	}
	return u
}

func TestVMStates(t *testing.T) {
	yes, no := true, false
	all, ready := VMStates([]unstructured.Unstructured{
		vmWithReady("demo-eu-0", &yes),
		vmWithReady("demo-eu-1", &no),
		vmWithReady("demo-eu-2", nil),
	})
	if !reflect.DeepEqual(all, []string{"demo-eu-0", "demo-eu-1", "demo-eu-2"}) {
		t.Errorf("all = %v", all)
	}
	if !reflect.DeepEqual(ready, []string{"demo-eu-0"}) {
		t.Errorf("ready = %v", ready)
	}
}

func TestStatusChanged(t *testing.T) {
	s := domain.Status{Region: "eu", Phase: domain.PhaseReady, ReadyReplicas: 2}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
	if !StatusChanged(obj, s) {
		t.Error("an object with no status must count as changed")
	}
	// Decoded JSON numbers may arrive as float64; equal values must not look changed.
	obj.Object["status"] = map[string]interface{}{"regions": []interface{}{
		map[string]interface{}{"region": "eu", "phase": "Ready", "readyReplicas": float64(2)},
	}}
	if StatusChanged(obj, s) {
		t.Error("identical status must not count as changed")
	}
	if !StatusChanged(obj, domain.Status{Region: "eu", Phase: domain.PhasePending, ReadyReplicas: 1}) {
		t.Error("different status must count as changed")
	}
}

func TestSetStatus_ToleratesNilAndMissingStatus(t *testing.T) {
	s := domain.Status{Region: "eu", Phase: domain.PhasePending}
	for name, obj := range map[string]map[string]interface{}{
		"missing status": {},
		"nil status":     {"status": nil},
		"empty status":   {"status": map[string]interface{}{}},
	} {
		t.Run(name, func(t *testing.T) {
			u := &unstructured.Unstructured{Object: obj}
			if err := SetStatus(u, s); err != nil {
				t.Fatalf("SetStatus: %v", err)
			}
			if StatusChanged(u, s) {
				t.Error("status was not written")
			}
		})
	}
}

func TestVMInstances(t *testing.T) {
	vm := func(name string) unstructured.Unstructured {
		u := unstructured.Unstructured{Object: map[string]interface{}{}}
		u.SetName(name)
		return u
	}
	vmi := vm("web-1")
	vmi.Object["status"] = map[string]interface{}{
		"phase":      "Running",
		"nodeName":   "eu-worker",
		"interfaces": []interface{}{map[string]interface{}{"ipAddress": "10.244.1.7"}},
	}

	got := VMInstances([]unstructured.Unstructured{vm("web-1"), vm("web-0")}, []unstructured.Unstructured{vmi}, nil)
	want := []domain.VMInstance{
		{Name: "web-0", Phase: "Pending"},
		{Name: "web-1", Phase: "Running", Node: "eu-worker", IP: "10.244.1.7"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("VMInstances = %+v, want %+v", got, want)
	}
}

func launcherPod(name, vm string, phase corev1.PodPhase, restarts int32) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{LabelVMName: vm}},
		Status: corev1.PodStatus{
			Phase: phase, PodIP: "10.244.1.7",
			ContainerStatuses: []corev1.ContainerStatus{{RestartCount: restarts}, {RestartCount: 1}},
		},
	}
}

func TestVMInstancesJoinsLauncherPod(t *testing.T) {
	vm := unstructured.Unstructured{Object: map[string]interface{}{}}
	vm.SetName("web-0")
	other := unstructured.Unstructured{Object: map[string]interface{}{}}
	other.SetName("web-1")

	// During a migration two pods carry the label; the running one wins.
	pods := []corev1.Pod{
		launcherPod("virt-launcher-web-0-a", "web-0", corev1.PodSucceeded, 0),
		launcherPod("virt-launcher-web-0-b", "web-0", corev1.PodRunning, 2),
	}
	got := VMInstances([]unstructured.Unstructured{vm, other}, nil, pods)

	wantPod := &domain.PodInfo{Name: "virt-launcher-web-0-b", Phase: "Running", IP: "10.244.1.7", Restarts: 3}
	if !reflect.DeepEqual(got[0].Pod, wantPod) {
		t.Errorf("web-0 pod = %+v, want %+v", got[0].Pod, wantPod)
	}
	if got[1].Pod != nil {
		t.Errorf("web-1 has no launcher pod, got %+v", got[1].Pod)
	}
}

func TestClusterInfo(t *testing.T) {
	profile := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"displayName": "EU spoke"},
		"status": map[string]interface{}{"conditions": []interface{}{
			map[string]interface{}{"type": "Other", "status": "False"},
			map[string]interface{}{"type": "ControlPlaneHealthy", "status": "True"},
		}},
	}}
	got := ClusterInfo("eu", profile, "v1.35.0")
	want := domain.ClusterInfo{Name: "eu", DisplayName: "EU spoke", KubernetesVersion: "v1.35.0", ControlPlaneHealthy: "True"}
	if got != want {
		t.Errorf("ClusterInfo = %+v, want %+v", got, want)
	}
	if bare := ClusterInfo("eu", nil, "v1.35.0"); bare.DisplayName != "" || bare.ControlPlaneHealthy != "" {
		t.Errorf("without a profile only name and version are known, got %+v", bare)
	}
}

func TestStatusObjectClusterAndPod(t *testing.T) {
	got := StatusObject(domain.Status{
		Region: "eu", Phase: domain.PhaseReady, ReadyReplicas: 1,
		Cluster: &domain.ClusterInfo{Name: "eu", KubernetesVersion: "v1.35.0"},
		VMs:     []domain.VMInstance{{Name: "web-0", Phase: "Running", Pod: &domain.PodInfo{Name: "p", Phase: "Running", Restarts: 0}}},
	})
	entry := got[0].(map[string]interface{})
	cl := entry["cluster"].(map[string]interface{})
	if cl["name"] != "eu" || cl["kubernetesVersion"] != "v1.35.0" {
		t.Errorf("cluster = %v", cl)
	}
	if _, ok := cl["displayName"]; ok {
		t.Errorf("empty displayName should be omitted, got %v", cl)
	}
	pod := entry["vms"].([]interface{})[0].(map[string]interface{})["pod"].(map[string]interface{})
	if pod["name"] != "p" || pod["restarts"] != int64(0) {
		t.Errorf("pod = %v", pod)
	}
}

func TestSummarizePods(t *testing.T) {
	got := SummarizePods([]domain.VMInstance{
		{Name: "a", Pod: &domain.PodInfo{Phase: "Running", Restarts: 3}},
		{Name: "b", Pod: &domain.PodInfo{Phase: "Failed", Restarts: 2}},
		{Name: "c", Pod: &domain.PodInfo{Phase: "Pending"}},
		{Name: "d"}, // no pod yet: neither restarts nor a not-running pod
	})
	want := domain.PodSummary{Restarts: 5, NotRunning: 2}
	if got != want {
		t.Errorf("SummarizePods = %+v, want %+v", got, want)
	}
}

func TestStatusObjectPodTotals(t *testing.T) {
	with := StatusObject(domain.Status{Region: "eu", Pods: &domain.PodSummary{Restarts: 4, NotRunning: 0}})[0].(map[string]interface{})
	if with["podRestarts"] != int64(4) || with["podsNotRunning"] != int64(0) {
		t.Errorf("totals = %v, want podRestarts 4 and podsNotRunning 0 (zero is reported, not omitted)", with)
	}
	without := StatusObject(domain.Status{Region: "eu"})[0].(map[string]interface{})
	if _, ok := without["podRestarts"]; ok {
		t.Error("unread pods must leave the totals out: absent means unknown, zero would mean healthy")
	}
}
