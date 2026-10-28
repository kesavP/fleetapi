package controller

import (
	"context"
	"fmt"
	"time"

	widgetv1alpha1 "github.com/sojoner/kro-lab/platform-mvp/widget-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// WidgetReconciler flips a Widget's status from Pending to Ready once
// ReadyDelay has elapsed since creation — a stand-in for whatever async
// provisioning a real downstream integration would perform.
type WidgetReconciler struct {
	Client     client.Client
	ReadyDelay time.Duration
}

func SetupWithManager(mgr ctrl.Manager, r *WidgetReconciler) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("widget").
		For(&widgetv1alpha1.Widget{}).
		Complete(r)
}

func (r *WidgetReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	widget := &widgetv1alpha1.Widget{}
	if err := r.Client.Get(ctx, req.NamespacedName, widget); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("getting widget: %w", err)
	}

	if widget.Status.Phase == widgetv1alpha1.WidgetPhaseReady {
		return reconcile.Result{}, nil
	}

	if remaining := time.Until(widget.CreationTimestamp.Add(r.ReadyDelay)); remaining > 0 {
		if widget.Status.Phase != widgetv1alpha1.WidgetPhasePending {
			widget.Status.Phase = widgetv1alpha1.WidgetPhasePending
			if err := r.Client.Status().Update(ctx, widget); err != nil {
				return reconcile.Result{}, fmt.Errorf("updating widget status to Pending: %w", err)
			}
		}
		return reconcile.Result{RequeueAfter: remaining}, nil
	}

	widget.Status.Phase = widgetv1alpha1.WidgetPhaseReady
	widget.Status.Endpoint = fmt.Sprintf("widget://%s/%s", widget.Namespace, widget.Name)
	if err := r.Client.Status().Update(ctx, widget); err != nil {
		return reconcile.Result{}, fmt.Errorf("updating widget status to Ready: %w", err)
	}
	return reconcile.Result{}, nil
}
