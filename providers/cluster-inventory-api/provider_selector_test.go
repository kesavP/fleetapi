package clusterinventoryapi

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testSelectorKey   = "platform.example.com/offers-vms"
	testSelectorValue = "true"
)

func labeledClusterProfile(name string, labels map[string]string) *unstructured.Unstructured {
	cp := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": ClusterProfileAPIVersion,
		"kind":       ClusterProfileKind,
		"metadata":   map[string]interface{}{"name": name, "namespace": "default"},
		"spec":       map[string]interface{}{"clusterManager": map[string]interface{}{"name": name + "-fleet-manager"}},
	}}
	cp.SetLabels(labels)
	return cp
}

func kubeconfigSecretFor(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name + KubeconfigSecretSuffix, Namespace: "default"},
		Data:       map[string][]byte{"value": []byte(testKubeconfigA)},
	}
}

// startSelectorProvider runs a provider over the given ClusterProfiles and
// returns it with its hub client and the channel of engaged cluster names.
func startSelectorProvider(t *testing.T, selector map[string]string, cps ...*unstructured.Unstructured) (*Provider, client.Client, *fakeClusterFactory, chan string) {
	t.Helper()
	scheme := runtime.NewScheme()
	corev1.AddToScheme(scheme)

	objs := []client.Object{}
	for _, cp := range cps {
		objs = append(objs, cp, kubeconfigSecretFor(cp.GetName()))
	}
	hubClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	factory := &fakeClusterFactory{}
	p := New(scheme, hubClient, 10*time.Millisecond)
	p.newCluster = factory.newCluster
	p.SetLabelSelector(selector)

	aware := &recordingAwareInternal{engaged: make(chan string, 10)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = p.Run(ctx, &fakeManagerInternal{aware: aware}) }()

	return p, hubClient, factory, aware.engaged
}

func TestProvider_Selector_EngagesOnlyMatchingClusters(t *testing.T) {
	p, _, _, engaged := startSelectorProvider(t,
		map[string]string{testSelectorKey: testSelectorValue},
		labeledClusterProfile("us", nil),
		labeledClusterProfile("eu", map[string]string{testSelectorKey: testSelectorValue}),
	)

	select {
	case name := <-engaged:
		if name != "eu" {
			t.Fatalf("expected eu engaged, got %s", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for eu engagement")
	}

	select {
	case name := <-engaged:
		t.Fatalf("unexpected engagement of %s: it does not match the selector", name)
	case <-time.After(200 * time.Millisecond):
	}

	if _, err := p.Get(context.Background(), "us"); err == nil {
		t.Error("expected us to be unknown to a selector-scoped provider")
	}
}

func TestProvider_Selector_LabelRemovalDisengagesCluster(t *testing.T) {
	eu := labeledClusterProfile("eu", map[string]string{testSelectorKey: testSelectorValue})
	p, hubClient, factory, engaged := startSelectorProvider(t,
		map[string]string{testSelectorKey: testSelectorValue}, eu)

	select {
	case <-engaged:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for eu engagement")
	}
	if len(factory.clusters) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(factory.clusters))
	}

	current := labeledClusterProfile("eu", nil)
	if err := hubClient.Get(context.Background(), client.ObjectKeyFromObject(eu), current); err != nil {
		t.Fatalf("getting eu: %v", err)
	}
	current.SetLabels(nil)
	if err := hubClient.Update(context.Background(), current); err != nil {
		t.Fatalf("removing label: %v", err)
	}

	select {
	case <-factory.clusters[0].stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("cluster was not stopped after its label stopped matching")
	}
	if _, err := p.Get(context.Background(), "eu"); err == nil {
		t.Error("expected ErrClusterNotFound after label removal")
	}
}

func TestProvider_NoSelector_EngagesAllClusters(t *testing.T) {
	_, _, _, engaged := startSelectorProvider(t, nil,
		labeledClusterProfile("us", nil),
		labeledClusterProfile("eu", map[string]string{testSelectorKey: testSelectorValue}),
	)

	got := map[string]bool{}
	for len(got) < 2 {
		select {
		case name := <-engaged:
			got[name] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out: engaged only %v", got)
		}
	}
}
