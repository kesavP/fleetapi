package controller

import (
	"context"
	"fmt"

	widgetv1alpha1 "github.com/sojoner/kro-lab/platform-mvp/widget-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ctrl "sigs.k8s.io/controller-runtime"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
)

var RegionalWidgetRequestGVK = schema.GroupVersionKind{
	Group:   "platform.example.com",
	Version: "v1alpha1",
	Kind:    "RegionalWidgetRequest",
}

// widgetNamespace is where both RegionalWidgetRequest (hub) and Widget
// (spoke) live for this MVP — a single fixed namespace, not per-tenant.
const widgetNamespace = "default"

// RegionalWidgetReconciler watches RegionalWidgetRequest on the hub only —
// no cross-cluster fan-out needed here — and creates the corresponding
// Widget on the target spoke, looked up via Manager.GetCluster.
type RegionalWidgetReconciler struct {
	HubClient client.Client
	Manager   mcmanager.Manager
}

// SetupWithManager registers the reconciler against the hub's local
// manager. Using the builder (rather than bare controller.New, as before)
// also wires the Watch — a bare controller.New with no Watch never receives
// any events.
func SetupWithManager(mgr mcmanager.Manager, r *RegionalWidgetReconciler) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(RegionalWidgetRequestGVK)
	return ctrl.NewControllerManagedBy(mgr.GetLocalManager()).
		Named("regionalwidgetrequest").
		For(obj).
		Complete(r)
}

func (r *RegionalWidgetReconciler) Reconcile(ctx context.Context, req reconcile.Request) (res reconcile.Result, retErr error) {
	logger := log.FromContext(ctx)

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(RegionalWidgetRequestGVK)
	err := r.HubClient.Get(ctx, req.NamespacedName, obj)
	if err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("getting regional widget request: %w", err)
	}

	region, found, err := unstructured.NestedString(obj.Object, "spec", "region")
	if err != nil || !found {
		return reconcile.Result{}, fmt.Errorf("regional widget request %s missing spec.region", req.Name)
	}

	tenantID := tenantNamespace(obj)

	defer func() {
		result := "success"
		if retErr != nil {
			result = "error"
		}
		ReconcileTotal.WithLabelValues(tenantID, region, result).Inc()
	}()

	spokeCluster, err := r.Manager.GetCluster(ctx, region)
	if err != nil {
		if err == multicluster.ErrClusterNotFound {
			logger.Info("region cluster not found", "region", region)
			return reconcile.Result{}, fmt.Errorf("unknown region %s: no cluster profile found", region)
		}
		return reconcile.Result{}, fmt.Errorf("getting spoke cluster for region %s: %w", region, err)
	}

	spokeClient := spokeCluster.GetClient()

	if err = r.ensureWidget(ctx, spokeClient, obj, tenantID); err != nil {
		return reconcile.Result{}, fmt.Errorf("ensuring Widget: %w", err)
	}

	return reconcile.Result{}, nil
}

func tenantNamespace(obj *unstructured.Unstructured) string {
	labels := obj.GetLabels()
	if labels != nil {
		if tenantID, ok := labels["platform.example.com/tenant"]; ok && tenantID != "" {
			return tenantID
		}
	}
	return widgetNamespace
}

func (r *RegionalWidgetReconciler) ensureWidget(ctx context.Context, spokeClient client.Client, obj *unstructured.Unstructured, ns string) error {
	name := obj.GetName()
	message, _, _ := unstructured.NestedString(obj.Object, "spec", "message")

	widget := &widgetv1alpha1.Widget{}
	err := spokeClient.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, widget)
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("getting Widget %s: %w", name, err)
	}

	widget = &widgetv1alpha1.Widget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: widgetv1alpha1.WidgetSpec{Message: message},
	}
	if err := spokeClient.Create(ctx, widget); err != nil {
		return fmt.Errorf("creating Widget %s: %w", name, err)
	}
	return nil
}
