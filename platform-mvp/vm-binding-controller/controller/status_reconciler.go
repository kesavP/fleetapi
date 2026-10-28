package controller

import (
	"context"
	"fmt"

	"github.com/sojoner/kro-lab/platform-mvp/vm-binding-controller/adapter"
	"github.com/sojoner/kro-lab/platform-mvp/vm-binding-controller/config"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
)

// StatusReconciler is the cross-cluster half: it watches VirtualMachine on
// every engaged spoke and reflects readiness onto the owning hub request.
type StatusReconciler struct {
	Hub      client.Client
	Clusters ClusterGetter
	Config   config.Config
	// Describer fills the cluster block of the status; nil leaves it out.
	Describer ClusterDescriber
}

// SetupStatusReconcilerWithManager registers the watch on every cluster the
// manager's provider engages.
func SetupStatusReconcilerWithManager(mgr mcmanager.Manager, r *StatusReconciler) error {
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(adapter.VirtualMachineGVK)
	return mcbuilder.ControllerManagedBy(mgr).
		Named("regionalvmsetrequest-status").
		For(vm).
		Complete(r)
}

func (r *StatusReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)

	spoke, err := r.Clusters.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("getting spoke cluster %s: %w", req.ClusterName, err)
	}

	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(adapter.VirtualMachineGVK)
	if err := spoke.GetClient().Get(ctx, req.NamespacedName, vm); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	key, ok := adapter.RequestKey(vm.GetLabels())
	if !ok {
		return reconcile.Result{}, nil // not a platform VM
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(adapter.RegionalVmSetRequestGVK)
	if err := r.Hub.Get(ctx, key, obj); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	domainReq, err := adapter.RequestFromObject(obj, r.Config.DefaultNamespace)
	if err != nil {
		logger.Info("ignoring VM event for a malformed request", "request", key, "error", err.Error())
		return reconcile.Result{}, nil
	}
	if domainReq.Region != req.ClusterName {
		return reconcile.Result{}, nil // VM is not in the request's region
	}
	return reconcile.Result{}, syncStatus(ctx, r.Hub, r.Describer, spoke, obj, domainReq)
}
