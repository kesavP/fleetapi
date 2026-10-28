package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/sojoner/kro-lab/platform-mvp/vm-binding-controller/adapter"
	"github.com/sojoner/kro-lab/platform-mvp/vm-binding-controller/domain"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func reconcileRequest(e *env, name string) (reconcile.Result, error) {
	return e.r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKey{Namespace: hubNS, Name: name},
	})
}

func domainRequest(t *testing.T, replicas int64) domain.Request {
	t.Helper()
	r, err := adapter.RequestFromObject(requestObj("demo-eu", replicas), "default")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReconcile_CreatesSecretAndVMsOnSpoke(t *testing.T) {
	e := newEnv([]client.Object{requestObj("demo-eu", 2), hubSecret("#!/bin/sh")}, nil, true)

	res, err := reconcileRequest(e, "demo-eu")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != testConfig.RequeueInterval {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, testConfig.RequeueInterval)
	}

	if got := vmNames(t, e.spoke, tenantNS); !reflect.DeepEqual(got, []string{"demo-eu-0", "demo-eu-1"}) {
		t.Errorf("VMs = %v", got)
	}
	secret := &corev1.Secret{}
	if err := e.spoke.Get(context.Background(), client.ObjectKey{Namespace: tenantNS, Name: "demo-cloudinit"}, secret); err != nil {
		t.Fatalf("spoke secret missing: %v", err)
	}
	if string(secret.Data["userdata"]) != "#!/bin/sh" {
		t.Errorf("spoke secret data = %v", secret.Data)
	}

	hub := hubRequest(t, e.hub, "demo-eu")
	if !controllerutil.ContainsFinalizer(hub, Finalizer) {
		t.Error("finalizer not set on the hub request")
	}
	if phase, ready := regionStatus(t, hub); phase != "Pending" || ready != 0 {
		t.Errorf("status = %s/%d, want Pending/0", phase, ready)
	}
}

func TestReconcile_ScaleDownDeletesHighestIndexFirst(t *testing.T) {
	r := domainRequest(t, 1)
	e := newEnv([]client.Object{requestObj("demo-eu", 1), hubSecret("x")},
		[]client.Object{existingVM(r, "demo-eu-0", true), existingVM(r, "demo-eu-1", true), existingVM(r, "demo-eu-2", true)}, true)

	if _, err := reconcileRequest(e, "demo-eu"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := vmNames(t, e.spoke, tenantNS); !reflect.DeepEqual(got, []string{"demo-eu-0"}) {
		t.Errorf("VMs after scale-down = %v, want only demo-eu-0", got)
	}
	if phase, ready := regionStatus(t, hubRequest(t, e.hub, "demo-eu")); phase != "Ready" || ready != 1 {
		t.Errorf("status = %s/%d, want Ready/1", phase, ready)
	}
}

func TestReconcile_ScaleUpCreatesOnlyMissingVMs(t *testing.T) {
	r := domainRequest(t, 3)
	e := newEnv([]client.Object{requestObj("demo-eu", 3), hubSecret("x")},
		[]client.Object{existingVM(r, "demo-eu-0", true)}, true)

	if _, err := reconcileRequest(e, "demo-eu"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := vmNames(t, e.spoke, tenantNS); !reflect.DeepEqual(got, []string{"demo-eu-0", "demo-eu-1", "demo-eu-2"}) {
		t.Errorf("VMs = %v", got)
	}
}

func TestReconcile_IsIdempotent(t *testing.T) {
	e := newEnv([]client.Object{requestObj("demo-eu", 2), hubSecret("x")}, nil, true)
	for i := 0; i < 3; i++ {
		if _, err := reconcileRequest(e, "demo-eu"); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if got := vmNames(t, e.spoke, tenantNS); len(got) != 2 {
		t.Errorf("VMs = %v, want exactly 2", got)
	}
}

func TestReconcile_UpdatesDriftedSpokeSecret(t *testing.T) {
	r := domainRequest(t, 1)
	stale := adapter.SpokeSecret(hubSecret("old"), r)
	e := newEnv([]client.Object{requestObj("demo-eu", 1), hubSecret("new")}, []client.Object{stale}, true)

	if _, err := reconcileRequest(e, "demo-eu"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := &corev1.Secret{}
	_ = e.spoke.Get(context.Background(), client.ObjectKey{Namespace: tenantNS, Name: "demo-cloudinit"}, got)
	if string(got.Data["userdata"]) != "new" {
		t.Errorf("spoke secret not refreshed: %q", got.Data["userdata"])
	}
}

func TestReconcile_UnschedulableWhenRegionHasNoVMCluster(t *testing.T) {
	e := newEnv([]client.Object{requestObj("demo-eu", 2), hubSecret("x")}, nil, false)

	res, err := reconcileRequest(e, "demo-eu")
	if err != nil {
		t.Fatalf("an unschedulable request is a status, not an error: %v", err)
	}
	if res.RequeueAfter != testConfig.RequeueInterval {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, testConfig.RequeueInterval)
	}
	if phase, _ := regionStatus(t, hubRequest(t, e.hub, "demo-eu")); phase != "Unschedulable" {
		t.Errorf("phase = %s, want Unschedulable", phase)
	}
	if got := vmNames(t, e.spoke, tenantNS); len(got) != 0 {
		t.Errorf("no VMs may be created, got %v", got)
	}
}

func TestReconcile_MissingHubSecretIsRetried(t *testing.T) {
	e := newEnv([]client.Object{requestObj("demo-eu", 2)}, nil, true)
	if _, err := reconcileRequest(e, "demo-eu"); err == nil {
		t.Fatal("expected an error so the request is retried until the Secret exists")
	}
	if got := vmNames(t, e.spoke, tenantNS); len(got) != 0 {
		t.Errorf("no VMs before the Secret exists, got %v", got)
	}
}

func TestReconcile_InvalidSpecIsTerminal(t *testing.T) {
	bad := requestObj("demo-eu", 2)
	delete(bad.Object["spec"].(map[string]interface{}), "image")
	e := newEnv([]client.Object{bad, hubSecret("x")}, nil, true)

	_, err := reconcileRequest(e, "demo-eu")
	if err == nil || !errors.Is(err, reconcile.TerminalError(nil)) {
		t.Errorf("expected a terminal error, got %v", err)
	}
}

func TestReconcile_UnknownRequestIsIgnored(t *testing.T) {
	e := newEnv(nil, nil, true)
	if _, err := reconcileRequest(e, "gone"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func deleting(t *testing.T, e *env, name string) {
	t.Helper()
	obj := hubRequest(t, e.hub, name)
	if err := e.hub.Delete(context.Background(), obj); err != nil {
		t.Fatalf("deleting request: %v", err)
	}
}

func TestReconcile_DeletionCleansSpokeAndReleasesFinalizer(t *testing.T) {
	r := domainRequest(t, 2)
	req := requestObj("demo-eu", 2)
	controllerutil.AddFinalizer(req, Finalizer)
	e := newEnv([]client.Object{req, hubSecret("x")},
		[]client.Object{existingVM(r, "demo-eu-0", true), existingVM(r, "demo-eu-1", true), adapter.SpokeSecret(hubSecret("x"), r)}, true)
	deleting(t, e, "demo-eu")

	if _, err := reconcileRequest(e, "demo-eu"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := vmNames(t, e.spoke, tenantNS); len(got) != 0 {
		t.Errorf("VMs left on the spoke: %v", got)
	}
	err := e.spoke.Get(context.Background(), client.ObjectKey{Namespace: tenantNS, Name: "demo-cloudinit"}, &corev1.Secret{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("spoke secret should be gone, got err=%v", err)
	}
	if requestExists(e) {
		t.Error("request should be released (deleted) once the finalizer is removed")
	}
}

func TestReconcile_DeletionWithoutClusterReleasesImmediately(t *testing.T) {
	req := requestObj("demo-eu", 2)
	controllerutil.AddFinalizer(req, Finalizer)
	e := newEnv([]client.Object{req, hubSecret("x")}, nil, false)
	deleting(t, e, "demo-eu")

	if _, err := reconcileRequest(e, "demo-eu"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if requestExists(e) {
		t.Error("an unschedulable request must not block deletion")
	}
}

func TestReconcile_DeletionOfInvalidRequestStillReleases(t *testing.T) {
	req := requestObj("demo-eu", 2)
	delete(req.Object["spec"].(map[string]interface{}), "region")
	controllerutil.AddFinalizer(req, Finalizer)
	e := newEnv([]client.Object{req}, nil, true)
	deleting(t, e, "demo-eu")

	if _, err := reconcileRequest(e, "demo-eu"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if requestExists(e) {
		t.Error("a malformed request must not be stuck behind its finalizer")
	}
}

// requestExists reports whether the hub request is still present.
func requestExists(e *env) bool {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(adapter.RegionalVmSetRequestGVK)
	err := e.hub.Get(context.Background(), client.ObjectKey{Namespace: hubNS, Name: "demo-eu"}, u)
	return !apierrors.IsNotFound(err)
}
