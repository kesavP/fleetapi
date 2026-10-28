package controller

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/vm-binding-controller/adapter"
	"github.com/sojoner/kro-lab/platform-mvp/vm-binding-controller/config"
	"github.com/sojoner/kro-lab/platform-mvp/vm-binding-controller/domain"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
)

const (
	hubNS      = "hub-ns"
	tenantNS   = "acme-corp"
	testRegion = "eu"
)

var testConfig = config.Config{
	RunStrategy:      "Always",
	RequeueInterval:  30 * time.Second,
	DefaultNamespace: "default",
}

type fakeSpoke struct {
	cluster.Cluster
	c client.Client
}

func (f *fakeSpoke) GetClient() client.Client    { return f.c }
func (f *fakeSpoke) GetAPIReader() client.Reader { return f.c }

type fakeClusters map[string]cluster.Cluster

func (f fakeClusters) GetCluster(_ context.Context, name string) (cluster.Cluster, error) {
	if cl, ok := f[name]; ok {
		return cl, nil
	}
	return nil, multicluster.ErrClusterNotFound
}

func requestObj(name string, replicas int64) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": hubNS,
			"labels":    map[string]interface{}{domain.LabelTenant: tenantNS, domain.LabelVMSet: "demo"},
		},
		"spec": map[string]interface{}{
			"region": testRegion, "replicas": replicas, "cpu": int64(1), "memory": "1Gi",
			"image": "img:1", "cloudInitSecretName": "demo-cloudinit",
		},
	}}
	u.SetGroupVersionKind(adapter.RegionalVmSetRequestGVK)
	return u
}

func hubSecret(data string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-cloudinit", Namespace: hubNS},
		Data:       map[string][]byte{"userdata": []byte(data)},
	}
}

func newClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(requestObj("x", 1)).Build()
}

type env struct {
	hub, spoke client.Client
	r          *RequestReconciler
}

// newEnv wires a reconciler to fake hub/spoke clients. engaged=false leaves
// the region out of the cluster set, as when no spoke offers VMs there.
func newEnv(hubObjs []client.Object, spokeObjs []client.Object, engaged bool) *env {
	hub, spoke := newClient(hubObjs...), newClient(spokeObjs...)
	clusters := fakeClusters{}
	if engaged {
		clusters[testRegion] = &fakeSpoke{c: spoke}
	}
	return &env{hub: hub, spoke: spoke, r: &RequestReconciler{
		Hub: hub, HubReader: hub, Clusters: clusters, Config: testConfig,
	}}
}

func vmList() *unstructured.UnstructuredList {
	l := &unstructured.UnstructuredList{}
	l.SetGroupVersionKind(schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineList"})
	return l
}

func vmNames(t *testing.T, c client.Client, ns string) []string {
	t.Helper()
	l := vmList()
	if err := c.List(context.Background(), l, client.InNamespace(ns)); err != nil {
		t.Fatalf("listing VMs: %v", err)
	}
	var names []string
	for _, vm := range l.Items {
		names = append(names, vm.GetName())
	}
	sort.Strings(names)
	return names
}

func existingVM(r domain.Request, name string, ready bool) *unstructured.Unstructured {
	vm := adapter.VirtualMachine(r, name, "Always")
	if ready {
		_ = unstructured.SetNestedField(vm.Object, true, "status", "ready")
	}
	return vm
}

func hubRequest(t *testing.T, c client.Client, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(adapter.RegionalVmSetRequestGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: hubNS, Name: name}, u); err != nil {
		t.Fatalf("getting request %s: %v", name, err)
	}
	return u
}

func regionStatus(t *testing.T, u *unstructured.Unstructured) (phase string, ready int64) {
	t.Helper()
	regions, found, _ := unstructured.NestedSlice(u.Object, "status", "regions")
	if !found || len(regions) != 1 {
		t.Fatalf("expected one status.regions entry, got %v", regions)
	}
	entry := regions[0].(map[string]interface{})
	phase, _ = entry["phase"].(string)
	switch n := entry["readyReplicas"].(type) {
	case int64:
		ready = n
	case float64:
		ready = int64(n)
	}
	return phase, ready
}
