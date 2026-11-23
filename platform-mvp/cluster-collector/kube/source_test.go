package kube

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/sojoner/kro-lab/platform-mvp/cluster-collector/wire"
)

var bg = context.Background()

func scaledObject(ns, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "keda.sh/v1alpha1", "kind": "ScaledObject",
		"metadata": map[string]any{"name": name, "namespace": ns, "uid": "so-" + name},
		"spec":     map[string]any{"scaleTargetRef": map[string]any{"name": "web"}},
	}}
}

func dyn(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{scaledObjects: "ScaledObjectList"}, objs...)
}

func cluster() []runtime.Object {
	yes := true
	return []runtime.Object{
		&corev1.Node{ObjectMeta: om("", "node-a", "n1")},
		&corev1.Namespace{ObjectMeta: om("", "shop", "ns1")},
		&corev1.Namespace{ObjectMeta: om("", "kube-system", "ns2")},
		&appsv1.Deployment{ObjectMeta: om("shop", "web", "d1")},
		&appsv1.StatefulSet{ObjectMeta: om("shop", "db", "s1")},
		&appsv1.DaemonSet{ObjectMeta: om("kube-system", "agent", "ds1")},
		&batchv1.CronJob{ObjectMeta: om("shop", "cleanup", "cj1")},
		&appsv1.ReplicaSet{ObjectMeta: func() metav1.ObjectMeta {
			m := om("shop", "web-5d6f8c7b9", "rs1")
			m.OwnerReferences = []metav1.OwnerReference{{Kind: "Deployment", Name: "web", UID: "d1", Controller: &yes}}
			return m
		}()},
		&corev1.Pod{ObjectMeta: func() metav1.ObjectMeta {
			m := om("shop", "web-5d6f8c7b9-xk2lp", "p1")
			m.OwnerReferences = []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-5d6f8c7b9", UID: "rs1", Controller: &yes}}
			return m
		}()},
		&corev1.Pod{ObjectMeta: om("kube-system", "dns-0", "p2")},
		&corev1.PersistentVolumeClaim{ObjectMeta: om("shop", "data", "pv1")},
		&corev1.Event{ObjectMeta: func() metav1.ObjectMeta { m := om("shop", "ev1", "e1"); return m }(), InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "web-5d6f8c7b9-xk2lp"}, Type: "Warning", Reason: "BackOff", Count: 3, LastTimestamp: metav1.NewTime(t0)},
		&corev1.Event{ObjectMeta: om("shop", "ev2", "e2"), InvolvedObject: corev1.ObjectReference{Kind: "Deployment", Name: "web"}, Type: "Normal", Reason: "ScalingReplicaSet", LastTimestamp: metav1.NewTime(t0)},
		&corev1.Event{ObjectMeta: om("kube-system", "ev3", "e3"), InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "dns-0"}, Type: "Warning", Reason: "Unhealthy", LastTimestamp: metav1.NewTime(t0)},
	}
}

func byKind(s Snapshot) map[string][]wire.Object {
	out := map[string][]wire.Object{}
	for _, o := range s.Objects {
		out[o.Kind] = append(out[o.Kind], o)
	}
	return out
}

func TestSnapshotReadsEveryKindAndLinksPodsToTheirWorkload(t *testing.T) {
	src := New(fake.NewSimpleClientset(cluster()...), dyn(scaledObject("shop", "worker")), Options{Events: EventsAll})
	s := src.Snapshot(bg)
	if len(s.Problems) != 0 || s.Skipped != 0 {
		t.Fatalf("problems %v, skipped %d", s.Problems, s.Skipped)
	}
	for _, k := range append(append([]string{}, wire.Kinds...), "event") {
		if !s.Listed[k] {
			t.Errorf("%s was not read", k)
		}
	}
	got := byKind(s)
	want := map[string]int{"node": 1, "namespace": 2, "deployment": 1, "statefulset": 1, "daemonset": 1, "cronjob": 1, "pod": 2, "pvc": 1, "scaledobject": 1, "hpa": 0}
	for k, n := range want {
		if len(got[k]) != n {
			t.Errorf("%s: %d objects, want %d", k, len(got[k]), n)
		}
	}
	for _, o := range s.Objects {
		if o.Kind == "replicaset" || o.Kind == "job" {
			t.Errorf("an object that is only read to find workloads was reported: %s", o.Kind)
		}
	}
	var web wire.Object
	for _, p := range got["pod"] {
		if p.Name == "web-5d6f8c7b9-xk2lp" {
			web = p
		}
	}
	if web.Data["workload_kind"] != "deployment" || web.Data["workload_name"] != "web" {
		t.Fatalf("the pod's workload: %+v", web.Data)
	}
	if len(s.Events) != 3 {
		t.Fatalf("events: %+v", s.Events)
	}
	// a stable order
	for i := 1; i < len(s.Objects); i++ {
		a, b := s.Objects[i-1], s.Objects[i]
		if a.Kind > b.Kind {
			t.Fatalf("not sorted by kind at %d", i)
		}
	}
}

func TestEventModes(t *testing.T) {
	for mode, want := range map[string]int{EventsWarning: 2, EventsAll: 3, EventsNone: 0} {
		s := New(fake.NewSimpleClientset(cluster()...), nil, Options{Events: mode}).Snapshot(bg)
		if len(s.Events) != want {
			t.Errorf("%s: %d events, want %d", mode, len(s.Events), want)
		}
		if mode == EventsNone && (s.Listed["event"] || s.Problems["event"] != nil) {
			t.Error("with no events asked for, events must be neither read nor a problem (nothing may be inferred from their absence)")
		}
		if mode != EventsNone && !s.Listed["event"] {
			t.Errorf("%s: events not read", mode)
		}
	}
	// the default is warnings
	if s := New(fake.NewSimpleClientset(cluster()...), nil, Options{}).Snapshot(bg); len(s.Events) != 2 {
		t.Errorf("default: %d events", len(s.Events))
	}
}

func TestExcludedNamespacesAreLeftOutEverywhere(t *testing.T) {
	s := New(fake.NewSimpleClientset(cluster()...), dyn(scaledObject("kube-system", "x")), Options{Events: EventsAll, ExcludeNamespaces: []string{"kube-system", " "}}).Snapshot(bg)
	for _, o := range s.Objects {
		if o.Namespace == "kube-system" || (o.Kind == "namespace" && o.Name == "kube-system") {
			t.Errorf("%s %s/%s was reported", o.Kind, o.Namespace, o.Name)
		}
	}
	for _, e := range s.Events {
		if e.Namespace == "kube-system" {
			t.Errorf("an event of an excluded namespace: %+v", e)
		}
	}
	if len(byKind(s)["node"]) != 1 || len(byKind(s)["namespace"]) != 1 {
		t.Errorf("nodes and the other namespaces stay: %+v", byKind(s))
	}
}

// A kind that cannot be read is a problem, not an empty list: the Runner leaves what it knows of that kind alone.
func TestAKindThatCannotBeReadIsAProblemNotAnEmptyList(t *testing.T) {
	cs := fake.NewSimpleClientset(cluster()...)
	cs.PrependReactor("list", "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "deployments"}, "", errors.New("no"))
	})
	s := New(cs, nil, Options{}).Snapshot(bg)
	if s.Listed["deployment"] || s.Problems["deployment"] == nil {
		t.Fatalf("deployments: listed %v, problem %v", s.Listed["deployment"], s.Problems["deployment"])
	}
	if !s.Listed["pod"] || !s.Listed["node"] {
		t.Fatal("one kind failing must not stop the others")
	}
	if len(byKind(s)["deployment"]) != 0 {
		t.Fatal("objects of a kind that was not read")
	}
}

func TestPodsAreNotReportedWhenTheirOwnersCannotBeResolved(t *testing.T) {
	for _, resource := range []string{"replicasets", "jobs", "pods"} {
		cs := fake.NewSimpleClientset(cluster()...)
		cs.PrependReactor("list", resource, func(clienttesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("timeout") })
		s := New(cs, nil, Options{}).Snapshot(bg)
		if s.Listed["pod"] || s.Problems["pod"] == nil || len(byKind(s)["pod"]) != 0 {
			t.Errorf("%s failing: pods listed %v, problem %v", resource, s.Listed["pod"], s.Problems["pod"])
		}
		if !s.Listed["deployment"] {
			t.Errorf("%s failing took the deployments with it", resource)
		}
	}
}

func TestKEDAIsOptional(t *testing.T) {
	// no dynamic client: not looked for, and neither read nor a problem
	if s := New(fake.NewSimpleClientset(), nil, Options{}).Snapshot(bg); s.Listed["scaledobject"] || s.Problems["scaledobject"] != nil {
		t.Fatalf("%v %v", s.Listed["scaledobject"], s.Problems["scaledobject"])
	}
	// the CRD is not installed: the API says not found, and the cluster has no ScaledObjects (so any it had are gone)
	d := dyn()
	d.PrependReactor("list", "scaledobjects", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(scaledObjects.GroupResource(), "")
	})
	if s := New(fake.NewSimpleClientset(), d, Options{}).Snapshot(bg); !s.Listed["scaledobject"] || s.Problems["scaledobject"] != nil {
		t.Fatalf("not installed: listed %v, problem %v", s.Listed["scaledobject"], s.Problems["scaledobject"])
	}
	// any other error is a problem
	d = dyn()
	d.PrependReactor("list", "scaledobjects", func(clienttesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("timeout") })
	if s := New(fake.NewSimpleClientset(), d, Options{}).Snapshot(bg); s.Listed["scaledobject"] || s.Problems["scaledobject"] == nil {
		t.Fatalf("an error: listed %v, problem %v", s.Listed["scaledobject"], s.Problems["scaledobject"])
	}
}

func TestObjectsThatCannotBeReportedAreCountedAndTheRestGoOn(t *testing.T) {
	objs := append(cluster(), &corev1.Pod{ObjectMeta: om("shop", "Bad Name!", "p9")}, &corev1.Pod{ObjectMeta: om("shop", "ok-pod", "")})
	s := New(fake.NewSimpleClientset(objs...), nil, Options{}).Snapshot(bg)
	if s.Skipped != 2 || len(byKind(s)["pod"]) != 2 {
		t.Fatalf("skipped %d, pods %d", s.Skipped, len(byKind(s)["pod"]))
	}
}

func TestEveryPageOfAListIsRead(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var calls []string
	cs.PrependReactor("list", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		opts := a.(clienttesting.ListActionImpl).ListOptions
		calls = append(calls, opts.Continue)
		switch opts.Continue {
		case "":
			return true, &corev1.PodList{ListMeta: metav1.ListMeta{Continue: "page2"}, Items: []corev1.Pod{{ObjectMeta: om("shop", "a", "u1")}}}, nil
		case "page2":
			return true, &corev1.PodList{ListMeta: metav1.ListMeta{Continue: "page3"}, Items: []corev1.Pod{{ObjectMeta: om("shop", "b", "u2")}}}, nil
		}
		return true, &corev1.PodList{Items: []corev1.Pod{{ObjectMeta: om("shop", "c", "u3")}}}, nil
	})
	s := New(cs, nil, Options{PageSize: 1}).Snapshot(bg)
	if n := len(byKind(s)["pod"]); n != 3 || len(calls) != 3 {
		t.Fatalf("%d pods from %v", n, calls)
	}
	// a failure on a later page is a failure of the whole list: a half list must not look like a complete one
	cs2 := fake.NewSimpleClientset()
	cs2.PrependReactor("list", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.(clienttesting.ListActionImpl).ListOptions.Continue == "" {
			return true, &corev1.PodList{ListMeta: metav1.ListMeta{Continue: "p2"}, Items: []corev1.Pod{{ObjectMeta: om("shop", "a", "u1")}}}, nil
		}
		return true, nil, apierrors.NewResourceExpired("continue token expired")
	})
	if s := New(cs2, nil, Options{PageSize: 1}).Snapshot(bg); s.Listed["pod"] || s.Problems["pod"] == nil {
		t.Fatalf("a half list was taken for a whole one: listed %v", s.Listed["pod"])
	}
	_ = ptr.To(1)
}
