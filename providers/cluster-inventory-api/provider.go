package clusterinventoryapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"

	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
)

const (
	ClusterProfileAPIVersion = "multicluster.x-k8s.io/v1alpha1"
	ClusterProfileKind       = "ClusterProfile"

	KubeconfigSecretSuffix = "-kubeconfig"

	DefaultTokenDir = "/var/run/secrets/tokens"
)

var ClusterProfileGVK = schema.FromAPIVersionAndKind(ClusterProfileAPIVersion, ClusterProfileKind)

var _ multicluster.Provider = (*Provider)(nil)

type indexRegistration struct {
	obj          client.Object
	field        string
	extractValue client.IndexerFunc
}

// Provider discovers spoke clusters from ClusterProfile objects on the hub
// and engages the multicluster-runtime manager with a cluster.Cluster for
// each one it finds.
//
// In v2, cluster authentication uses projected ServiceAccount tokens
// (BearerTokenFile) mounted from the binding-controller pod instead of
// static tokens from the kubeconfig Secret. The kubeconfig Secret is still
// read for server address and CA certificate, but the auth section is
// overridden with the projected token file path.
//
// Token files follow the convention: {tokenDir}/{clusterName}-token
// (e.g., /var/run/secrets/tokens/us-token).
type Provider struct {
	scheme       *runtime.Scheme
	hubClient    client.Client
	pollInterval time.Duration
	newCluster   func(cfg *rest.Config, scheme *runtime.Scheme) (cluster.Cluster, error)
	tokenDir     string

	// selector, when non-empty, restricts discovery to ClusterProfiles whose
	// labels match. Set before Run; not safe to change while running.
	selector map[string]string

	mu        sync.RWMutex
	clusters  map[string]cluster.Cluster
	cancelFns map[string]context.CancelFunc
	indexes   []indexRegistration

	// clusterKeys tracks server+CA identity so the provider can detect
	// when a spoke is re-created with different endpoints (e.g., a kind
	// cluster was destroyed and re-created) and re-engage it.
	clusterKeys map[string]string
}

func New(scheme *runtime.Scheme, hubClient client.Client, pollInterval time.Duration) *Provider {
	return NewWithTokenDir(scheme, hubClient, pollInterval, DefaultTokenDir)
}

func NewWithTokenDir(scheme *runtime.Scheme, hubClient client.Client, pollInterval time.Duration, tokenDir string) *Provider {
	return &Provider{
		scheme:       scheme,
		hubClient:    hubClient,
		pollInterval: pollInterval,
		newCluster:   newRealCluster,
		tokenDir:     tokenDir,
		clusters:     map[string]cluster.Cluster{},
		cancelFns:    map[string]context.CancelFunc{},
		clusterKeys:  map[string]string{},
	}
}

// SetClusterFactory overrides the cluster creation function. Used for
// testing where real cluster.New (which creates a client-go client) can't
// connect to a real API server.
func (p *Provider) SetClusterFactory(fn func(cfg *rest.Config, scheme *runtime.Scheme) (cluster.Cluster, error)) {
	p.newCluster = fn
}

// SetLabelSelector restricts discovery to ClusterProfiles carrying all the
// given labels, so a controller engages only the clusters it serves. A
// ClusterProfile that stops matching is disengaged on the next poll. A nil or
// empty selector engages every ClusterProfile. Call before Run.
func (p *Provider) SetLabelSelector(selector map[string]string) {
	p.selector = selector
}

func newRealCluster(cfg *rest.Config, scheme *runtime.Scheme) (cluster.Cluster, error) {
	return cluster.New(cfg, func(o *cluster.Options) {
		o.Scheme = scheme
	})
}

func (p *Provider) Get(_ context.Context, clusterName string) (cluster.Cluster, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if cl, ok := p.clusters[clusterName]; ok {
		return cl, nil
	}
	return nil, multicluster.ErrClusterNotFound
}

func (p *Provider) IndexField(ctx context.Context, obj client.Object, field string, extractValue client.IndexerFunc) error {
	p.mu.Lock()
	p.indexes = append(p.indexes, indexRegistration{obj: obj, field: field, extractValue: extractValue})
	clusters := make([]cluster.Cluster, 0, len(p.clusters))
	for _, cl := range p.clusters {
		clusters = append(clusters, cl)
	}
	p.mu.Unlock()

	for _, cl := range clusters {
		if err := cl.GetFieldIndexer().IndexField(ctx, obj, field, extractValue); err != nil {
			return err
		}
	}
	return nil
}

func (p *Provider) Run(ctx context.Context, mgr mcmanager.Manager) error {
	p.discover(ctx, mgr)

	ticker := time.NewTicker(p.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			p.discover(ctx, mgr)
		}
	}
}

func (p *Provider) discover(ctx context.Context, mgr mcmanager.Manager) {
	cps := &unstructured.UnstructuredList{}
	cps.SetGroupVersionKind(ClusterProfileGVK)
	if err := p.hubClient.List(ctx, cps, client.MatchingLabels(p.selector)); err != nil {
		return
	}

	seen := make(map[string]bool, len(cps.Items))

	for _, cp := range cps.Items {
		name := cp.GetName()
		seen[name] = true

		clusterKey, err := p.readClusterKey(ctx, &cp)
		if err != nil {
			p.mu.RLock()
			_, engaged := p.clusters[name]
			p.mu.RUnlock()
			if engaged {
				continue
			}
			clusterKey = ""
		}

		p.mu.RLock()
		_, engaged := p.clusters[name]
		prevKey := p.clusterKeys[name]
		p.mu.RUnlock()

		if engaged && clusterKey == prevKey {
			continue
		}

		if engaged {
			p.disengageCluster(name)
		}

		cl, err := p.clusterFromClusterProfile(ctx, &cp)
		if err != nil {
			continue
		}

		clusterCtx, cancel := context.WithCancel(ctx)
		go func() { _ = cl.Start(clusterCtx) }()

		p.mu.Lock()
		p.clusters[name] = cl
		p.cancelFns[name] = cancel
		p.clusterKeys[name] = clusterKey
		indexes := make([]indexRegistration, len(p.indexes))
		copy(indexes, p.indexes)
		p.mu.Unlock()

		for _, idx := range indexes {
			_ = cl.GetFieldIndexer().IndexField(ctx, idx.obj, idx.field, idx.extractValue)
		}

		if err := mgr.Engage(ctx, name, cl); err != nil {
			p.disengageCluster(name)
		}
	}

	p.mu.RLock()
	engagedNames := make([]string, 0, len(p.clusters))
	for name := range p.clusters {
		engagedNames = append(engagedNames, name)
	}
	p.mu.RUnlock()

	for _, name := range engagedNames {
		if !seen[name] {
			p.disengageCluster(name)
		}
	}
}

func (p *Provider) disengageCluster(name string) {
	p.mu.Lock()
	_, ok := p.clusters[name]
	if !ok {
		p.mu.Unlock()
		return
	}
	delete(p.clusters, name)
	delete(p.clusterKeys, name)
	if cancel, exists := p.cancelFns[name]; exists {
		cancel()
		delete(p.cancelFns, name)
	}
	p.mu.Unlock()
}

// readClusterKey computes a stable identity hash from the kubeconfig
// Secret's server and CA fields. This is used to detect when a spoke
// cluster's endpoint changes (e.g., after a kind cluster recreation).
func (p *Provider) readClusterKey(ctx context.Context, cp *unstructured.Unstructured) (string, error) {
	secretName := cp.GetName() + KubeconfigSecretSuffix
	secretNamespace := cp.GetNamespace()

	secret := &corev1.Secret{}
	if err := p.hubClient.Get(ctx, types.NamespacedName{
		Name:      secretName,
		Namespace: secretNamespace,
	}, secret); err != nil {
		return "", err
	}

	kubeconfigBytes, ok := secret.Data["value"]
	if !ok {
		return "", fmt.Errorf("kubeconfig secret %s/%s missing 'value' key", secretNamespace, secretName)
	}

	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigBytes)
	if err != nil {
		return "", err
	}

	h := sha256.New()
	h.Write([]byte(cfg.Host))
	h.Write(cfg.TLSClientConfig.CAData)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// clusterFromClusterProfile builds a cluster.Cluster for the given
// ClusterProfile. It reads the kubeconfig Secret to extract server address
// and CA certificate, then overrides the auth mechanism with a projected
// ServiceAccount token file (BearerTokenFile) sourced from the
// binding-controller pod's projected volume.
func (p *Provider) clusterFromClusterProfile(ctx context.Context, cp *unstructured.Unstructured) (cluster.Cluster, error) {
	secretName := cp.GetName() + KubeconfigSecretSuffix
	secretNamespace := cp.GetNamespace()

	secret := &corev1.Secret{}
	if err := p.hubClient.Get(ctx, types.NamespacedName{
		Name:      secretName,
		Namespace: secretNamespace,
	}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, multicluster.ErrClusterNotFound
		}
		return nil, fmt.Errorf("getting kubeconfig secret %s/%s: %w", secretNamespace, secretName, err)
	}

	kubeconfigBytes, ok := secret.Data["value"]
	if !ok {
		return nil, fmt.Errorf("kubeconfig secret %s/%s missing 'value' key", secretNamespace, secretName)
	}

	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigBytes)
	if err != nil {
		return nil, fmt.Errorf("parsing kubeconfig for cluster %s: %w", cp.GetName(), err)
	}

	tokenFile := fmt.Sprintf("%s/%s-token", p.tokenDir, cp.GetName())
	cfg.BearerToken = ""
	cfg.BearerTokenFile = tokenFile
	cfg.Username = ""
	cfg.Password = ""
	cfg.AuthProvider = nil
	cfg.ExecProvider = nil
	cfg.TLSClientConfig.CertData = nil
	cfg.TLSClientConfig.KeyData = nil

	cl, err := p.newCluster(cfg, p.scheme)
	if err != nil {
		return nil, fmt.Errorf("creating cluster for %s: %w", cp.GetName(), err)
	}
	return cl, nil
}
