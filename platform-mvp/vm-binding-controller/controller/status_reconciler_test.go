package controller

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
)

func statusEnv(e *env) *StatusReconciler {
	return &StatusReconciler{Hub: e.hub, Clusters: e.r.Clusters, Config: testConfig}
}

func vmEvent(name string) mcreconcile.Request {
	return mcreconcile.Request{
		Request:     reconcile.Request{NamespacedName: client.ObjectKey{Namespace: tenantNS, Name: name}},
		ClusterName: testRegion,
	}
}

func TestStatusReconciler_AllVMsReadyMakesRequestReady(t *testing.T) {
	r := domainRequest(t, 2)
	e := newEnv([]client.Object{requestObj("demo-eu", 2)},
		[]client.Object{existingVM(r, "demo-eu-0", true), existingVM(r, "demo-eu-1", true)}, true)

	if _, err := statusEnv(e).Reconcile(context.Background(), vmEvent("demo-eu-1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if phase, ready := regionStatus(t, hubRequest(t, e.hub, "demo-eu")); phase != "Ready" || ready != 2 {
		t.Errorf("status = %s/%d, want Ready/2", phase, ready)
	}
}

func TestStatusReconciler_PartialReadinessStaysPending(t *testing.T) {
	r := domainRequest(t, 2)
	e := newEnv([]client.Object{requestObj("demo-eu", 2)},
		[]client.Object{existingVM(r, "demo-eu-0", true), existingVM(r, "demo-eu-1", false)}, true)

	if _, err := statusEnv(e).Reconcile(context.Background(), vmEvent("demo-eu-0")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if phase, ready := regionStatus(t, hubRequest(t, e.hub, "demo-eu")); phase != "Pending" || ready != 1 {
		t.Errorf("status = %s/%d, want Pending/1", phase, ready)
	}
}

func TestStatusReconciler_UnchangedStatusIsNotRewritten(t *testing.T) {
	r := domainRequest(t, 1)
	e := newEnv([]client.Object{requestObj("demo-eu", 1)}, []client.Object{existingVM(r, "demo-eu-0", true)}, true)
	sr := statusEnv(e)

	if _, err := sr.Reconcile(context.Background(), vmEvent("demo-eu-0")); err != nil {
		t.Fatal(err)
	}
	before := hubRequest(t, e.hub, "demo-eu").GetResourceVersion()
	if _, err := sr.Reconcile(context.Background(), vmEvent("demo-eu-0")); err != nil {
		t.Fatal(err)
	}
	if after := hubRequest(t, e.hub, "demo-eu").GetResourceVersion(); after != before {
		t.Errorf("resourceVersion moved %s -> %s: an unchanged status must not be written", before, after)
	}
}

func TestStatusReconciler_IgnoresEventsItCannotMapToARequest(t *testing.T) {
	r := domainRequest(t, 1)
	unlabelled := existingVM(r, "stranger", true)
	unlabelled.SetLabels(nil)
	orphan := existingVM(r, "demo-eu-0", true) // labelled, but no hub request exists

	e := newEnv(nil, []client.Object{unlabelled, orphan}, true)
	sr := statusEnv(e)
	for _, name := range []string{"stranger", "demo-eu-0", "does-not-exist"} {
		if _, err := sr.Reconcile(context.Background(), vmEvent(name)); err != nil {
			t.Errorf("event for %q must be ignored, got %v", name, err)
		}
	}
}

func TestStatusReconciler_UnknownClusterIsAnError(t *testing.T) {
	e := newEnv(nil, nil, false)
	if _, err := statusEnv(e).Reconcile(context.Background(), vmEvent("x")); err == nil {
		t.Error("expected an error for a cluster the manager no longer knows")
	}
}
