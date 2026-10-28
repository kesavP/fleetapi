// Package controller is the impure shell: it reads and writes clusters and
// delegates every decision to the pure domain and adapter packages.
package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/sojoner/kro-lab/platform-mvp/vm-binding-controller/adapter"
	"github.com/sojoner/kro-lab/platform-mvp/vm-binding-controller/config"
	"github.com/sojoner/kro-lab/platform-mvp/vm-binding-controller/domain"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
)

// Finalizer keeps a hub request alive until its VMs and Secret are gone from the spoke.
const Finalizer = "platform.example.com/vm-cleanup"

var vmiListGVK = schema.GroupVersionKind{Group: adapter.VMInstanceGVK.Group, Version: adapter.VMInstanceGVK.Version, Kind: "VirtualMachineInstanceList"}

var vmListGVK = schema.GroupVersionKind{Group: adapter.VirtualMachineGVK.Group, Version: adapter.VirtualMachineGVK.Version, Kind: "VirtualMachineList"}

// ClusterGetter is the part of the multicluster manager the controllers use.
type ClusterGetter interface {
	GetCluster(ctx context.Context, name string) (cluster.Cluster, error)
}

// RequestReconciler turns each hub RegionalVmSetRequest into VMs on the
// region's spoke. Hub reads of Secrets go through HubReader (uncached).
type RequestReconciler struct {
	Hub       client.Client
	HubReader client.Reader
	Clusters  ClusterGetter
	Config    config.Config
	// Describer fills the cluster block of the status; nil leaves it out.
	Describer ClusterDescriber
}

// SetupWithManager watches RegionalVmSetRequest on the hub only; spoke
// engagement is handled by the provider.
func SetupWithManager(mgr mcmanager.Manager, r *RequestReconciler) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(adapter.RegionalVmSetRequestGVK)
	return ctrl.NewControllerManagedBy(mgr.GetLocalManager()).
		Named("regionalvmsetrequest").
		For(obj).
		Complete(r)
}

func (r *RequestReconciler) Reconcile(ctx context.Context, key reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(adapter.RegionalVmSetRequestGVK)
	if err := r.Hub.Get(ctx, key.NamespacedName, obj); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	req, parseErr := adapter.RequestFromObject(obj, r.Config.DefaultNamespace)

	if !obj.GetDeletionTimestamp().IsZero() {
		return reconcile.Result{}, r.finalize(ctx, obj, req, parseErr)
	}
	if parseErr != nil {
		// Retrying cannot fix a malformed spec; the CRD schema should prevent it.
		return reconcile.Result{}, reconcile.TerminalError(parseErr)
	}

	if !controllerutil.ContainsFinalizer(obj, Finalizer) {
		patch := client.MergeFrom(obj.DeepCopy())
		controllerutil.AddFinalizer(obj, Finalizer)
		if err := r.Hub.Patch(ctx, obj, patch); err != nil {
			return reconcile.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
	}

	spoke, err := r.Clusters.GetCluster(ctx, req.Region)
	if errors.Is(err, multicluster.ErrClusterNotFound) {
		logger.Info("no VM-capable cluster for region", "region", req.Region)
		return reconcile.Result{RequeueAfter: r.Config.RequeueInterval},
			r.writeStatus(ctx, obj, domain.UnschedulableStatus(req.Region))
	}
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("getting cluster for region %s: %w", req.Region, err)
	}

	if err := r.syncSecret(ctx, spoke, req); err != nil {
		return reconcile.Result{}, err
	}
	if err := r.syncVMs(ctx, spoke, req); err != nil {
		return reconcile.Result{}, err
	}
	if err := syncStatus(ctx, r.Hub, r.Describer, spoke, obj, req); err != nil {
		return reconcile.Result{}, err
	}
	// Periodic resync repairs VMs deleted out-of-band.
	return reconcile.Result{RequeueAfter: r.Config.RequeueInterval}, nil
}

func (r *RequestReconciler) syncSecret(ctx context.Context, spoke cluster.Cluster, req domain.Request) error {
	hubSecret := &corev1.Secret{}
	if err := r.HubReader.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.CloudInitSecret}, hubSecret); err != nil {
		return fmt.Errorf("reading hub secret %s/%s: %w", req.Namespace, req.CloudInitSecret, err)
	}
	desired := adapter.SpokeSecret(hubSecret, req)

	current := &corev1.Secret{}
	err := spoke.GetAPIReader().Get(ctx, client.ObjectKeyFromObject(desired), current)
	switch {
	case apierrors.IsNotFound(err):
		return spoke.GetClient().Create(ctx, desired)
	case err != nil:
		return fmt.Errorf("reading spoke secret: %w", err)
	case equality.Semantic.DeepEqual(current.Data, desired.Data):
		return nil
	}
	current.Data = desired.Data
	return spoke.GetClient().Update(ctx, current)
}

func (r *RequestReconciler) syncVMs(ctx context.Context, spoke cluster.Cluster, req domain.Request) error {
	vms, err := listVMs(ctx, spoke.GetClient(), req)
	if err != nil {
		return err
	}
	existing, _ := adapter.VMStates(vms)
	plan := domain.PlanVMs(req, existing)

	for _, name := range plan.Create {
		if err := spoke.GetClient().Create(ctx, adapter.VirtualMachine(req, name, r.Config.RunStrategy)); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating VM %s: %w", name, err)
		}
	}
	for _, name := range plan.Delete {
		vm := &unstructured.Unstructured{}
		vm.SetGroupVersionKind(adapter.VirtualMachineGVK)
		vm.SetName(name)
		vm.SetNamespace(req.Tenant)
		if err := spoke.GetClient().Delete(ctx, vm); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("deleting VM %s: %w", name, err)
		}
	}
	return nil
}

// finalize removes the request's VMs and Secret from the spoke, then releases
// the finalizer. Requests that cannot be placed or parsed are released at once.
func (r *RequestReconciler) finalize(ctx context.Context, obj *unstructured.Unstructured, req domain.Request, parseErr error) error {
	if !controllerutil.ContainsFinalizer(obj, Finalizer) {
		return nil
	}
	if parseErr == nil {
		spoke, err := r.Clusters.GetCluster(ctx, req.Region)
		switch {
		case err == nil:
			if err := r.cleanSpoke(ctx, spoke, req); err != nil {
				return err
			}
		case !errors.Is(err, multicluster.ErrClusterNotFound):
			return fmt.Errorf("getting cluster for region %s: %w", req.Region, err)
		}
	}
	patch := client.MergeFrom(obj.DeepCopy())
	controllerutil.RemoveFinalizer(obj, Finalizer)
	return client.IgnoreNotFound(r.Hub.Patch(ctx, obj, patch))
}

func (r *RequestReconciler) cleanSpoke(ctx context.Context, spoke cluster.Cluster, req domain.Request) error {
	vms, err := listVMs(ctx, spoke.GetClient(), req)
	if err != nil {
		return err
	}
	for i := range vms {
		if err := spoke.GetClient().Delete(ctx, &vms[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("deleting VM %s: %w", vms[i].GetName(), err)
		}
	}
	secret := &corev1.Secret{}
	secret.Name, secret.Namespace = req.CloudInitSecret, req.Tenant
	if err := spoke.GetClient().Delete(ctx, secret); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("deleting spoke secret: %w", err)
	}
	return nil
}

func (r *RequestReconciler) writeStatus(ctx context.Context, obj *unstructured.Unstructured, s domain.Status) error {
	return patchStatus(ctx, r.Hub, obj, s)
}

func listVMs(ctx context.Context, c client.Client, req domain.Request) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(vmListGVK)
	if err := c.List(ctx, list, client.InNamespace(req.Tenant), client.MatchingLabels(adapter.OwnerLabels(req))); err != nil {
		return nil, fmt.Errorf("listing VMs for %s: %w", req.Name, err)
	}
	return list.Items, nil
}

// syncStatus recomputes the request's region status from the spoke's VMs and
// writes it to the hub only when it changed. VM state is required; the
// instance, pod and cluster details are best effort, so a failed read leaves
// them out and logs instead of hiding whether the VMs are ready.
func syncStatus(ctx context.Context, hub client.Client, d ClusterDescriber, spoke cluster.Cluster, obj *unstructured.Unstructured, req domain.Request) error {
	logger := log.FromContext(ctx)
	vms, err := listVMs(ctx, spoke.GetClient(), req)
	if err != nil {
		return err
	}
	_, ready := adapter.VMStates(vms)
	s := domain.ReadyStatus(req, ready)

	vmis, err := listVMInstances(ctx, spoke.GetAPIReader(), req)
	if err != nil {
		logger.Info("skipping VM instance details", "request", req.Name, "error", err.Error())
	}
	pods, podsErr := listLauncherPods(ctx, spoke.GetAPIReader(), req)
	if podsErr != nil {
		logger.Info("skipping pod details", "request", req.Name, "error", podsErr.Error())
	}
	s.VMs = adapter.VMInstances(vms, vmis, pods)
	if podsErr == nil {
		// Only report totals we actually read: absent means unknown, whereas
		// zero would say the pods are healthy.
		totals := adapter.SummarizePods(s.VMs)
		s.Pods = &totals
	}

	if d != nil {
		info, err := d.Describe(ctx, req.Region, spoke)
		if err != nil {
			logger.Info("skipping cluster details", "region", req.Region, "error", err.Error())
		} else {
			s.Cluster = &info
		}
	}
	return patchStatus(ctx, hub, obj, s)
}

// listVMInstances and listLauncherPods read through the spoke's uncached API
// reader, scoped to the tenant namespace. A cached client would start
// cluster-wide informers and need cluster-wide read access.
// Instances do not inherit the VM's labels, so callers match them by name.
func listVMInstances(ctx context.Context, r client.Reader, req domain.Request) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(vmiListGVK)
	if err := r.List(ctx, list, client.InNamespace(req.Tenant)); err != nil {
		return nil, fmt.Errorf("listing VM instances for %s: %w", req.Name, err)
	}
	return list.Items, nil
}

// listLauncherPods lists the tenant namespace's pods that KubeVirt labelled
// with the VM they run.
func listLauncherPods(ctx context.Context, r client.Reader, req domain.Request) ([]corev1.Pod, error) {
	list := &corev1.PodList{}
	if err := r.List(ctx, list, client.InNamespace(req.Tenant), client.HasLabels{adapter.LabelVMName}); err != nil {
		return nil, fmt.Errorf("listing launcher pods for %s: %w", req.Name, err)
	}
	return list.Items, nil
}

func patchStatus(ctx context.Context, hub client.Client, obj *unstructured.Unstructured, s domain.Status) error {
	if !adapter.StatusChanged(obj, s) {
		return nil
	}
	patch := client.MergeFrom(obj.DeepCopy())
	if err := adapter.SetStatus(obj, s); err != nil {
		return fmt.Errorf("setting status.regions: %w", err)
	}
	if err := hub.Status().Patch(ctx, obj, patch); err != nil {
		return fmt.Errorf("patching request status: %w", err)
	}
	return nil
}
