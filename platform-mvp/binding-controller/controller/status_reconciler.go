package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
)

// WidgetGVK identifies the spoke-side Widget resource. Watched as
// unstructured (rather than the typed widget-operator API) to match the
// proven-working pattern for cross-cluster watches in this controller — see
// RegionalWidgetRequestGVK below and the equivalent hub-side watch in
// reconciler.go.
var WidgetGVK = schema.GroupVersionKind{
	Group:   "platform.example.com",
	Version: "v1alpha1",
	Kind:    "Widget",
}

// StatusReconciler watches Widget across engaged spoke clusters and
// propagates its status back to the matching RegionalWidgetRequest on the
// hub. This is the cross-cluster fan-out half of the binding controller —
// RegionalWidgetReconciler's hub-only watch creates the spoke resource,
// this reconciler watches spoke state and reports it back.
type StatusReconciler struct {
	Manager mcmanager.Manager
}

// SetupStatusReconcilerWithManager registers the status reconciler to watch
// Widget on every cluster mgr's provider engages.
func SetupStatusReconcilerWithManager(mgr mcmanager.Manager, r *StatusReconciler) error {
	widget := &unstructured.Unstructured{}
	widget.SetGroupVersionKind(WidgetGVK)
	return mcbuilder.ControllerManagedBy(mgr).
		Named("regionalwidgetrequest-status").
		For(widget).
		Complete(r)
}

func (r *StatusReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (reconcile.Result, error) {
	spokeCluster, err := r.Manager.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("getting spoke cluster %s: %w", req.ClusterName, err)
	}
	spokeClient := spokeCluster.GetClient()

	widget := &unstructured.Unstructured{}
	widget.SetGroupVersionKind(WidgetGVK)
	if err := spokeClient.Get(ctx, req.NamespacedName, widget); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("getting Widget: %w", err)
	}

	phase, _, _ := unstructured.NestedString(widget.Object, "status", "phase")
	endpoint, _, _ := unstructured.NestedString(widget.Object, "status", "endpoint")

	hubClient := r.Manager.GetLocalManager().GetClient()
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(RegionalWidgetRequestGVK)
	hubKey := types.NamespacedName{Name: widget.GetName(), Namespace: widgetNamespace}
	if err := hubClient.Get(ctx, hubKey, latest); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("getting RegionalWidgetRequest on hub: %w", err)
	}

	regionStatus := []interface{}{map[string]interface{}{
		"region":   req.ClusterName,
		"phase":    phase,
		"endpoint": endpoint,
	}}
	if err := unstructured.SetNestedSlice(latest.Object, regionStatus, "status", "regions"); err != nil {
		return reconcile.Result{}, fmt.Errorf("setting status.regions: %w", err)
	}
	// RegionalWidgetRequest has the status subresource enabled, so a plain
	// Update() silently ignores .status changes — must go through Status().
	if err := hubClient.Status().Update(ctx, latest); err != nil {
		return reconcile.Result{}, fmt.Errorf("updating status on hub: %w", err)
	}

	return reconcile.Result{}, nil
}
