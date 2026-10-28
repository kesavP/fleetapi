package controller

import (
	"context"
	"testing"

	widgetv1alpha1 "github.com/sojoner/kro-lab/platform-mvp/widget-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
)

// fakeSpokeCluster adapts a fake client.Client to cluster.Cluster for tests
// (StatusReconciler only ever calls GetClient()).
type fakeSpokeCluster struct {
	cluster.Cluster
	c client.Client
}

func (f *fakeSpokeCluster) GetClient() client.Client { return f.c }

// fakeLocalManager adapts a fake client.Client to manager.Manager, standing
// in for mcmanager.Manager.GetLocalManager() in tests.
type fakeLocalManager struct {
	manager.Manager
	c client.Client
}

func (f *fakeLocalManager) GetClient() client.Client { return f.c }

// fakeMCManager is a minimal mcmanager.Manager test double: GetCluster
// returns a seeded spoke cluster by name, GetLocalManager exposes the hub.
type fakeMCManager struct {
	mcmanager.Manager
	spoke map[string]cluster.Cluster
	hub   client.Client
}

func (f *fakeMCManager) GetCluster(ctx context.Context, name string) (cluster.Cluster, error) {
	cl, ok := f.spoke[name]
	if !ok {
		return nil, multicluster.ErrClusterNotFound
	}
	return cl, nil
}

func (f *fakeMCManager) GetLocalManager() manager.Manager {
	return &fakeLocalManager{c: f.hub}
}

func newWidget(name, namespace string, phase widgetv1alpha1.WidgetPhase, endpoint string) *widgetv1alpha1.Widget {
	return &widgetv1alpha1.Widget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Status: widgetv1alpha1.WidgetStatus{
			Phase:    phase,
			Endpoint: endpoint,
		},
	}
}

func TestStatusReconciler_PropagatesWidgetStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	corev1.AddToScheme(scheme)
	widgetv1alpha1.AddToScheme(scheme)

	widget := newWidget("test-widget-us", widgetNamespace, widgetv1alpha1.WidgetPhaseReady, "widget://default/test-widget-us")
	spokeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(widget).Build()

	hubObj := newRegionalWidgetRequest("test-widget-us", "us", "hello")
	// RegionalWidgetRequest has the status subresource enabled in its CRD, so
	// the fake client must model that too — otherwise a plain Update() (which
	// the real API server would silently ignore for .status) would appear to
	// work in this test but fail against a real cluster.
	statusSubresource := &unstructured.Unstructured{}
	statusSubresource.SetGroupVersionKind(RegionalWidgetRequestGVK)
	hubClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(hubObj).WithStatusSubresource(statusSubresource).Build()

	mgr := &fakeMCManager{
		spoke: map[string]cluster.Cluster{"us": &fakeSpokeCluster{c: spokeClient}},
		hub:   hubClient,
	}

	r := &StatusReconciler{Manager: mgr}

	req := mcreconcile.Request{
		Request: reconcile.Request{
			NamespacedName: client.ObjectKey{Name: "test-widget-us", Namespace: widgetNamespace},
		},
		ClusterName: "us",
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	updated := &unstructured.Unstructured{}
	updated.SetGroupVersionKind(RegionalWidgetRequestGVK)
	if err := hubClient.Get(context.Background(), client.ObjectKey{Name: "test-widget-us", Namespace: hubObj.GetNamespace()}, updated); err != nil {
		t.Fatalf("failed to get updated RegionalWidgetRequest: %v", err)
	}

	regions, found, err := unstructured.NestedSlice(updated.Object, "status", "regions")
	if err != nil || !found || len(regions) == 0 {
		t.Fatalf("expected status.regions to be populated, found=%v err=%v", found, err)
	}

	region, ok := regions[0].(map[string]interface{})
	if !ok {
		t.Fatalf("expected status.regions[0] to be a map, got %T", regions[0])
	}

	phase, _ := region["phase"].(string)
	endpoint, _ := region["endpoint"].(string)

	if phase != string(widgetv1alpha1.WidgetPhaseReady) {
		t.Errorf("expected phase Ready, got %q", phase)
	}
	if endpoint == "" {
		t.Error("expected non-empty endpoint")
	}
}
