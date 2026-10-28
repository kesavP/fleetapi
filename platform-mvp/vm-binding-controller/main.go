// vm-binding-controller places the hub's RegionalVmSetRequests onto the
// VM-capable spokes (ClusterProfiles matching --cluster-selector) as KubeVirt
// VirtualMachines, and reports their readiness back to the hub.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/sojoner/kro-lab/platform-mvp/vm-binding-controller/config"
	"github.com/sojoner/kro-lab/platform-mvp/vm-binding-controller/controller"
	provider "github.com/sojoner/kro-lab/providers/cluster-inventory-api"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
)

func main() {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrl.Log

	cfg, err := config.Parse(os.Args[1:])
	if err != nil {
		log.Error(err, "invalid configuration")
		os.Exit(1)
	}

	// VMs, requests and ClusterProfiles are handled as unstructured; only
	// Secrets are typed.
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		log.Error(err, "failed to build scheme")
		os.Exit(1)
	}

	hubConfig, err := clientcmd.BuildConfigFromFlags("", cfg.HubKubeconfig)
	if err != nil {
		log.Error(err, "failed to build hub config")
		os.Exit(1)
	}

	// The provider needs its own uncached client: mcmanager.New requires the
	// provider before the manager (and its cache) exists.
	hubClient, err := client.New(hubConfig, client.Options{Scheme: scheme})
	if err != nil {
		log.Error(err, "failed to build hub client for provider")
		os.Exit(1)
	}
	p := provider.New(scheme, hubClient, cfg.PollInterval)
	p.SetLabelSelector(cfg.ClusterSelector)

	mgr, err := mcmanager.New(hubConfig, p, ctrl.Options{Scheme: scheme})
	if err != nil {
		log.Error(err, "failed to create manager")
		os.Exit(1)
	}
	local := mgr.GetLocalManager()

	describer := &controller.ProfileDescriber{Hub: local.GetAPIReader(), Namespace: cfg.ProfileNamespace}
	requests := &controller.RequestReconciler{
		Hub:       local.GetClient(),
		HubReader: local.GetAPIReader(),
		Clusters:  mgr,
		Config:    cfg,
		Describer: describer,
	}
	if err := controller.SetupWithManager(mgr, requests); err != nil {
		log.Error(err, "failed to set up request controller")
		os.Exit(1)
	}
	status := &controller.StatusReconciler{Hub: local.GetClient(), Clusters: mgr, Config: cfg, Describer: describer}
	if err := controller.SetupStatusReconcilerWithManager(mgr, status); err != nil {
		log.Error(err, "failed to set up status controller")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// The provider discovers matching spokes and engages the manager with
	// them, which attaches the status controller's VM watch to each.
	go func() {
		if err := p.Run(ctx, mgr); err != nil {
			log.Error(err, "provider exited with error")
		}
	}()

	log.Info("starting vm-binding-controller", "selector", cfg.ClusterSelector)
	if err := local.Start(ctx); err != nil {
		log.Error(err, "manager exited with error")
		os.Exit(1)
	}
}
