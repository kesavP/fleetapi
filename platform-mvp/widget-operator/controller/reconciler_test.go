package controller

import (
	"context"
	"testing"
	"time"

	widgetv1alpha1 "github.com/sojoner/kro-lab/platform-mvp/widget-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const testReadyDelay = 2 * time.Second

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := widgetv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding widget scheme: %v", err)
	}
	return scheme
}

func newWidget(name string, createdAt time.Time) *widgetv1alpha1.Widget {
	return &widgetv1alpha1.Widget{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(createdAt),
		},
		Spec: widgetv1alpha1.WidgetSpec{Message: "hello"},
	}
}

func TestReconciler_RequeuesBeforeDelayElapsed(t *testing.T) {
	scheme := newScheme(t)
	widget := newWidget("fresh", time.Now())
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&widgetv1alpha1.Widget{}).WithObjects(widget).Build()

	r := &WidgetReconciler{Client: c, ReadyDelay: testReadyDelay}
	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(widget)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Fatalf("expected a positive RequeueAfter, got %v", res.RequeueAfter)
	}

	got := &widgetv1alpha1.Widget{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(widget), got); err != nil {
		t.Fatalf("getting widget: %v", err)
	}
	if got.Status.Phase != widgetv1alpha1.WidgetPhasePending {
		t.Errorf("expected phase Pending, got %q", got.Status.Phase)
	}
	if got.Status.Endpoint != "" {
		t.Errorf("expected no endpoint yet, got %q", got.Status.Endpoint)
	}
}

func TestReconciler_SetsReadyAfterDelayElapsed(t *testing.T) {
	scheme := newScheme(t)
	widget := newWidget("aged", time.Now().Add(-2*testReadyDelay))
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&widgetv1alpha1.Widget{}).WithObjects(widget).Build()

	r := &WidgetReconciler{Client: c, ReadyDelay: testReadyDelay}
	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(widget)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue once ready, got %v", res.RequeueAfter)
	}

	got := &widgetv1alpha1.Widget{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(widget), got); err != nil {
		t.Fatalf("getting widget: %v", err)
	}
	if got.Status.Phase != widgetv1alpha1.WidgetPhaseReady {
		t.Errorf("expected phase Ready, got %q", got.Status.Phase)
	}
	if got.Status.Endpoint == "" {
		t.Error("expected endpoint to be populated")
	}
}

func TestReconciler_NotFoundReturnsNoError(t *testing.T) {
	scheme := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	r := &WidgetReconciler{Client: c, ReadyDelay: testReadyDelay}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKey{Name: "missing", Namespace: "default"}})
	if err != nil {
		t.Fatalf("expected no error for missing widget, got %v", err)
	}
}
