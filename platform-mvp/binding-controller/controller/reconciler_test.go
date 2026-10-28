package controller

import (
	"context"
	"testing"

	widgetv1alpha1 "github.com/sojoner/kro-lab/platform-mvp/widget-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// ── Existing tests (backward compat) ────────────────────────────

func TestReconciler_CreatesSpokeWidget(t *testing.T) {
	scheme := runtime.NewScheme()
	corev1.AddToScheme(scheme)
	widgetv1alpha1.AddToScheme(scheme)

	hubClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	spokeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&widgetv1alpha1.Widget{}).Build()

	mgr := &fakeMCManager{
		spoke: map[string]cluster.Cluster{"us": &fakeSpokeCluster{c: spokeClient}},
		hub:   hubClient,
	}

	r := &RegionalWidgetReconciler{
		HubClient: hubClient,
		Manager:   mgr,
	}

	obj := newRegionalWidgetRequest("test-widget-us", "us", "hello")
	err := hubClient.Create(context.Background(), obj)
	if err != nil {
		t.Fatalf("failed to create test object: %v", err)
	}

	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(obj)}
	_, err = r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	widget := &widgetv1alpha1.Widget{}
	err = spokeClient.Get(context.Background(), client.ObjectKey{
		Name:      "test-widget-us",
		Namespace: widgetNamespace,
	}, widget)
	if err != nil {
		t.Fatalf("expected Widget to exist in %s: %v", widgetNamespace, err)
	}
	if widget.Spec.Message != "hello" {
		t.Errorf("expected widget message %q, got %q", "hello", widget.Spec.Message)
	}
}

func TestReconciler_UnknownRegionReturnsError(t *testing.T) {
	scheme := runtime.NewScheme()
	corev1.AddToScheme(scheme)
	widgetv1alpha1.AddToScheme(scheme)

	hubClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	mgr := &fakeMCManager{
		spoke: map[string]cluster.Cluster{},
		hub:   hubClient,
	}

	r := &RegionalWidgetReconciler{
		HubClient: hubClient,
		Manager:   mgr,
	}

	obj := newRegionalWidgetRequest("test-widget-unknown", "unknown", "hello")
	err := hubClient.Create(context.Background(), obj)
	if err != nil {
		t.Fatalf("failed to create test object: %v", err)
	}

	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(obj)}
	_, err = r.Reconcile(context.Background(), req)
	if err == nil {
		t.Error("expected error for unknown region")
	}
}

// ── Tenant-aware tests ──────────────────────────────────────────

func TestReconciler_TenantWidgetCreatedInTenantNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	corev1.AddToScheme(scheme)
	widgetv1alpha1.AddToScheme(scheme)

	hubClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	spokeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&widgetv1alpha1.Widget{}).Build()

	mgr := &fakeMCManager{
		spoke: map[string]cluster.Cluster{"us": &fakeSpokeCluster{c: spokeClient}},
		hub:   hubClient,
	}

	r := &RegionalWidgetReconciler{
		HubClient: hubClient,
		Manager:   mgr,
	}

	obj := newRegionalWidgetRequestWithTenant("acme-widget-us", "us", "hello", "acme-corp")
	err := hubClient.Create(context.Background(), obj)
	if err != nil {
		t.Fatalf("failed to create test object: %v", err)
	}

	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(obj)}
	_, err = r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	widget := &widgetv1alpha1.Widget{}
	err = spokeClient.Get(context.Background(), client.ObjectKey{
		Name:      "acme-widget-us",
		Namespace: "acme-corp",
	}, widget)
	if err != nil {
		t.Fatalf("expected Widget in tenant namespace acme-corp: %v", err)
	}
	if widget.Spec.Message != "hello" {
		t.Errorf("expected message %q, got %q", "hello", widget.Spec.Message)
	}
}

func TestReconciler_NoTenantFallsBackToDefaultNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	corev1.AddToScheme(scheme)
	widgetv1alpha1.AddToScheme(scheme)

	hubClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	spokeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&widgetv1alpha1.Widget{}).Build()

	mgr := &fakeMCManager{
		spoke: map[string]cluster.Cluster{"us": &fakeSpokeCluster{c: spokeClient}},
		hub:   hubClient,
	}

	r := &RegionalWidgetReconciler{
		HubClient: hubClient,
		Manager:   mgr,
	}

	// No tenant field → should fall back to widgetNamespace
	obj := newRegionalWidgetRequest("no-tenant-us", "us", "hello")
	err := hubClient.Create(context.Background(), obj)
	if err != nil {
		t.Fatalf("failed to create test object: %v", err)
	}

	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(obj)}
	_, err = r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	widget := &widgetv1alpha1.Widget{}
	err = spokeClient.Get(context.Background(), client.ObjectKey{
		Name:      "no-tenant-us",
		Namespace: widgetNamespace,
	}, widget)
	if err != nil {
		t.Fatalf("expected Widget in default namespace: %v", err)
	}
}

// ── Helpers ─────────────────────────────────────────────────────

func newRegionalWidgetRequest(name, region, message string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(RegionalWidgetRequestGVK)
	obj.SetName(name)
	obj.SetNamespace(widgetNamespace)
	obj.Object["spec"] = map[string]interface{}{
		"region":  region,
		"message": message,
	}
	return obj
}

func newRegionalWidgetRequestWithTenant(name, region, message, tenantID string) *unstructured.Unstructured {
	obj := newRegionalWidgetRequest(name, region, message)
	obj.SetLabels(map[string]string{"platform.example.com/tenant": tenantID})
	return obj
}
