package clusterinventoryapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/cluster"

	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
)

type fakeCluster struct {
	cluster.Cluster
	indexer *fakeFieldIndexer
	started chan struct{}
	stopped chan struct{}
}

func (f *fakeCluster) GetFieldIndexer() client.FieldIndexer { return f.indexer }
func (f *fakeCluster) Start(ctx context.Context) error {
	close(f.started)
	<-ctx.Done()
	close(f.stopped)
	return nil
}

type fakeFieldIndexer struct {
	calls int
}

func (f *fakeFieldIndexer) IndexField(ctx context.Context, obj client.Object, field string, extractValue client.IndexerFunc) error {
	f.calls++
	extractValue(obj)
	return nil
}

type fakeClusterFactory struct {
	clusters []*fakeCluster
}

func (f *fakeClusterFactory) newCluster(cfg *rest.Config, scheme *runtime.Scheme) (cluster.Cluster, error) {
	fc := &fakeCluster{indexer: &fakeFieldIndexer{}, started: make(chan struct{}), stopped: make(chan struct{})}
	f.clusters = append(f.clusters, fc)
	return fc, nil
}

const testKubeconfigA = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: us
contexts:
- context:
    cluster: us
    user: admin
  name: kind-us
current-context: kind-us
users:
- name: admin
  user:
    token: token-a`

// testKubeconfigServerChanged has a different server URL to test
// server/endpoint change detection (v2 cluster key is host+CA, not token).
const testKubeconfigServerChanged = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://192.168.1.100:6443
  name: us
contexts:
- context:
    cluster: us
    user: admin
  name: kind-us
current-context: kind-us
users:
- name: admin
  user:
    token: token-a`

func TestProvider_KubeconfigUnchanged_SkipsReengage(t *testing.T) {
	cp := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": ClusterProfileAPIVersion, "kind": ClusterProfileKind, "metadata": map[string]interface{}{"name": "us", "namespace": "default"}, "spec": map[string]interface{}{"clusterManager": map[string]interface{}{"name": "us-fleet-manager"}}}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "us" + KubeconfigSecretSuffix, Namespace: "default"}, Data: map[string][]byte{"value": []byte(testKubeconfigA)}}

	scheme := runtime.NewScheme()
	corev1.AddToScheme(scheme)
	hubClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp, secret).Build()

	factory := &fakeClusterFactory{}
	p := New(scheme, hubClient, 10*time.Millisecond)
	p.newCluster = factory.newCluster

	aware := &recordingAwareInternal{engaged: make(chan string, 5)}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() { _ = p.Run(ctx, &fakeManagerInternal{aware: aware}) }()

	select {
	case name := <-aware.engaged:
		if name != "us" {
			t.Fatalf("expected us, got %s", name)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for first engagement")
	}

	select {
	case <-aware.engaged:
		t.Fatal("unexpected re-engagement: server+CA should not have changed")
	case <-time.After(200 * time.Millisecond):
	}

	if len(factory.clusters) != 1 {
		t.Errorf("expected 1 cluster created, got %d", len(factory.clusters))
	}
}

// TestProvider_ServerChange_ReengagesCluster verifies that when the
// kubeconfig Secret's server URL changes (which alters the clusterKey
// hash), the provider disengages the old cluster and engages a new one.
// In v2, token-only changes do NOT trigger re-engagement because the
// clusterKey is computed from Host + CAData, not the full kubeconfig
// (tokens are managed by Kubelet via projected volumes).
func TestProvider_ServerChange_ReengagesCluster(t *testing.T) {
	cp := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": ClusterProfileAPIVersion, "kind": ClusterProfileKind, "metadata": map[string]interface{}{"name": "us", "namespace": "default"}, "spec": map[string]interface{}{"clusterManager": map[string]interface{}{"name": "us-fleet-manager"}}}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "us" + KubeconfigSecretSuffix, Namespace: "default"}, Data: map[string][]byte{"value": []byte(testKubeconfigA)}}

	scheme := runtime.NewScheme()
	corev1.AddToScheme(scheme)
	hubClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp, secret).Build()

	factory := &fakeClusterFactory{}
	p := New(scheme, hubClient, 10*time.Millisecond)
	p.newCluster = factory.newCluster

	aware := &recordingAwareInternal{engaged: make(chan string, 5)}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() { _ = p.Run(ctx, &fakeManagerInternal{aware: aware}) }()

	select {
	case <-aware.engaged:
	case <-ctx.Done():
		t.Fatal("timed out waiting for first engagement")
	}

	if len(factory.clusters) != 1 {
		t.Fatalf("expected 1 cluster after first engage, got %d", len(factory.clusters))
	}
	oldCluster := factory.clusters[0]

	// Update secret with a kubeconfig that has a *different server URL*.
	// This triggers re-engagement in v2 (clusterKey change).
	updatedSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "us" + KubeconfigSecretSuffix, Namespace: "default"}, Data: map[string][]byte{"value": []byte(testKubeconfigServerChanged)}}
	if err := hubClient.Update(ctx, updatedSecret); err != nil {
		t.Fatalf("failed to update secret: %v", err)
	}

	select {
	case <-aware.engaged:
	case <-ctx.Done():
		t.Fatal("timed out waiting for re-engagement after server URL change")
	}

	if len(factory.clusters) != 2 {
		t.Errorf("expected 2 clusters created (old + new), got %d", len(factory.clusters))
	}

	select {
	case <-oldCluster.stopped:
	case <-time.After(500 * time.Millisecond):
		t.Error("old cluster was not stopped after server URL change")
	}
}

func TestProvider_ClusterProfileDeleted_RemovesCluster(t *testing.T) {
	cp := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": ClusterProfileAPIVersion, "kind": ClusterProfileKind, "metadata": map[string]interface{}{"name": "us", "namespace": "default"}, "spec": map[string]interface{}{"clusterManager": map[string]interface{}{"name": "us-fleet-manager"}}}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "us" + KubeconfigSecretSuffix, Namespace: "default"}, Data: map[string][]byte{"value": []byte(testKubeconfigA)}}

	scheme := runtime.NewScheme()
	corev1.AddToScheme(scheme)
	hubClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp, secret).Build()

	factory := &fakeClusterFactory{}
	p := New(scheme, hubClient, 10*time.Millisecond)
	p.newCluster = factory.newCluster

	aware := &recordingAwareInternal{engaged: make(chan string, 5)}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() { _ = p.Run(ctx, &fakeManagerInternal{aware: aware}) }()

	select {
	case <-aware.engaged:
	case <-ctx.Done():
		t.Fatal("timed out waiting for engagement")
	}

	if len(factory.clusters) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(factory.clusters))
	}
	oldCluster := factory.clusters[0]

	if err := hubClient.Delete(ctx, cp); err != nil {
		t.Fatalf("failed to delete ClusterProfile: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	select {
	case <-oldCluster.stopped:
	case <-time.After(500 * time.Millisecond):
		t.Error("cluster was not stopped after ClusterProfile deletion")
	}

	_, err := p.Get(ctx, "us")
	if err == nil {
		t.Error("expected ErrClusterNotFound after deletion, got nil")
	}
}

func TestProvider_ClusterKey_DetectsServerChange(t *testing.T) {
	h1 := sha256.New()
	h1.Write([]byte("https://127.0.0.1:6443"))
	hashA := hex.EncodeToString(h1.Sum(nil))

	h2 := sha256.New()
	h2.Write([]byte("https://192.168.1.100:6443"))
	hashB := hex.EncodeToString(h2.Sum(nil))

	if hashA == hashB {
		t.Fatal("expected different cluster keys for different server URLs")
	}
}

// ── Existing tests (kept) ────────────────────────────────────────

func TestProvider_IndexField_ReplaysOnFutureEngagement(t *testing.T) {
	cp := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": ClusterProfileAPIVersion, "kind": ClusterProfileKind, "metadata": map[string]interface{}{"name": "us", "namespace": "default"}, "spec": map[string]interface{}{"clusterManager": map[string]interface{}{"name": "us-fleet-manager"}}}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "us" + KubeconfigSecretSuffix, Namespace: "default"}, Data: map[string][]byte{"value": []byte(testKubeconfigA)}}

	scheme := runtime.NewScheme()
	corev1.AddToScheme(scheme)
	hubClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp, secret).Build()

	p := New(scheme, hubClient, 10*time.Millisecond)
	indexer := &fakeFieldIndexer{}
	p.newCluster = func(cfg *rest.Config, scheme *runtime.Scheme) (cluster.Cluster, error) {
		return &fakeCluster{indexer: indexer, started: make(chan struct{}), stopped: make(chan struct{})}, nil
	}

	if err := p.IndexField(context.Background(), &corev1.Pod{}, "spec.nodeName", func(client.Object) []string { return nil }); err != nil {
		t.Fatalf("unexpected error registering index: %v", err)
	}
	if indexer.calls != 0 {
		t.Fatalf("expected 0 calls before any cluster is engaged, got %d", indexer.calls)
	}

	aware := &recordingAwareInternal{engaged: make(chan string, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() { _ = p.Run(ctx, &fakeManagerInternal{aware: aware}) }()

	select {
	case <-aware.engaged:
	case <-ctx.Done():
		t.Fatal("timed out waiting for engagement")
	}

	if indexer.calls != 1 {
		t.Errorf("expected the pre-registered index to be replayed once on engagement, got %d calls", indexer.calls)
	}

	if err := p.IndexField(ctx, &corev1.Pod{}, "status.phase", func(client.Object) []string { return nil }); err != nil {
		t.Fatalf("unexpected error registering second index: %v", err)
	}
	if indexer.calls != 2 {
		t.Errorf("expected IndexField to apply immediately to the already-engaged cluster, got %d calls", indexer.calls)
	}
}

type recordingAwareInternal struct {
	engaged chan string
}

func (r *recordingAwareInternal) Engage(ctx context.Context, name string, cl cluster.Cluster) error {
	r.engaged <- name
	return nil
}

type fakeManagerInternal struct {
	mcmanager.Manager
	aware *recordingAwareInternal
}

func (f *fakeManagerInternal) Engage(ctx context.Context, name string, cl cluster.Cluster) error {
	return f.aware.Engage(ctx, name, cl)
}
