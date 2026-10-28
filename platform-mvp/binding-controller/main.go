package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/binding-controller/controller"
	widgetv1alpha1 "github.com/sojoner/kro-lab/platform-mvp/widget-operator/api/v1alpha1"
	provider "github.com/sojoner/kro-lab/providers/cluster-inventory-api"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
)

// clusterProfilePollInterval is how often the cluster-inventory-api
// provider re-lists ClusterProfile objects on the hub to discover newly
// registered spoke clusters.
const clusterProfilePollInterval = 10 * time.Second

// runnableProvider is a multicluster.Provider that also discovers and
// engages clusters when run. There's no formal ProviderRunnable interface
// in this multicluster-runtime version — each concrete provider exposes its
// own Run(ctx, mcmanager.Manager) method by convention.
type runnableProvider interface {
	multicluster.Provider
	Run(context.Context, mcmanager.Manager) error
}

func main() {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrl.Log

	scheme := runtime.NewScheme()
	corev1.AddToScheme(scheme)
	if err := widgetv1alpha1.AddToScheme(scheme); err != nil {
		ctrl.Log.Error(err, "failed to add widget scheme")
		os.Exit(1)
	}

	hubKubeconfig := flag.String("hub-kubeconfig", "", "Path to hub cluster kubeconfig")
	spokeKubeconfig := flag.String("spoke-kubeconfig", "", "Path to spoke cluster kubeconfig (static fallback)")
	flag.Parse()

	hubConfig, err := clientcmd.BuildConfigFromFlags("", *hubKubeconfig)
	if err != nil {
		log.Error(err, "failed to build hub config")
		os.Exit(1)
	}

	var p runnableProvider
	if *spokeKubeconfig != "" {
		spokeConfig, err := clientcmd.BuildConfigFromFlags("", *spokeKubeconfig)
		if err != nil {
			log.Error(err, "failed to build spoke config")
			os.Exit(1)
		}
		p = &staticProvider{name: "us", config: spokeConfig, scheme: scheme}
	} else {
		// Provider needs its own client to list ClusterProfile objects on
		// the hub — mcmanager.New requires the provider up front, before
		// the manager (and its cached client) exists.
		hubClient, err := client.New(hubConfig, client.Options{Scheme: scheme})
		if err != nil {
			log.Error(err, "failed to build hub client for provider")
			os.Exit(1)
		}
		p = provider.New(scheme, hubClient, clusterProfilePollInterval)
	}

	mgr, err := mcmanager.New(hubConfig, p, ctrl.Options{Scheme: scheme})
	if err != nil {
		log.Error(err, "failed to create manager")
		os.Exit(1)
	}

	regionalWidgetReconciler := &controller.RegionalWidgetReconciler{
		HubClient: mgr.GetLocalManager().GetClient(),
		Manager:   mgr,
	}
	if err := controller.SetupWithManager(mgr, regionalWidgetReconciler); err != nil {
		log.Error(err, "failed to setup regionalwidgetrequest controller")
		os.Exit(1)
	}

	statusReconciler := &controller.StatusReconciler{Manager: mgr}
	if err := controller.SetupStatusReconcilerWithManager(mgr, statusReconciler); err != nil {
		log.Error(err, "failed to setup status controller")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// The provider discovers spoke clusters and engages mgr with them —
	// this is what makes StatusReconciler's cross-cluster watch actually
	// attach to newly discovered clusters.
	go func() {
		if err := p.Run(ctx, mgr); err != nil {
			log.Error(err, "provider exited with error")
		}
	}()

	log.Info("starting binding controller")
	if err := mgr.GetLocalManager().Start(ctx); err != nil {
		log.Error(err, "manager exited with error")
		os.Exit(1)
	}
}

// staticProvider engages a single, statically-configured spoke cluster —
// used for local runs without ClusterProfile/fleet registration set up.
type staticProvider struct {
	name   string
	config *rest.Config
	scheme *runtime.Scheme

	mu sync.Mutex
	cl cluster.Cluster
}

func (p *staticProvider) Get(_ context.Context, clusterName string) (cluster.Cluster, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if clusterName != p.name || p.cl == nil {
		return nil, multicluster.ErrClusterNotFound
	}
	return p.cl, nil
}

func (p *staticProvider) IndexField(ctx context.Context, obj client.Object, field string, extractValue client.IndexerFunc) error {
	p.mu.Lock()
	cl := p.cl
	p.mu.Unlock()
	if cl == nil {
		return nil
	}
	return cl.GetFieldIndexer().IndexField(ctx, obj, field, extractValue)
}

// Run builds and starts the static cluster, then engages mgr with it —
// required for cross-cluster controllers (built via mcbuilder) to actually
// attach a watch to this cluster.
func (p *staticProvider) Run(ctx context.Context, mgr mcmanager.Manager) error {
	cl, err := cluster.New(p.config, func(o *cluster.Options) {
		o.Scheme = p.scheme
	})
	if err != nil {
		return err
	}

	p.mu.Lock()
	p.cl = cl
	p.mu.Unlock()

	go func() { _ = cl.Start(ctx) }()

	if err := mgr.Engage(ctx, p.name, cl); err != nil {
		return err
	}

	<-ctx.Done()
	return nil
}
