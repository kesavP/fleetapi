package kube

import (
	"context"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/sojoner/kro-lab/platform-mvp/cluster-collector/wire"
)

// Event modes: which events are reported.
const (
	EventsWarning = "warning" // only Warning events: what went wrong (the default)
	EventsAll     = "all"     // Normal ones too: scaling, scheduling, pulling, killing
	EventsNone    = "none"
)

var scaledObjects = schema.GroupVersionResource{Group: "keda.sh", Version: "v1alpha1", Resource: "scaledobjects"}

type Options struct {
	ExcludeNamespaces []string
	Events            string
	PageSize          int64 // objects a list call asks for; 0 = 500
}

// Snapshot is what one look at the cluster found.
type Snapshot struct {
	Objects []wire.Object
	Events  []wire.Event
	// Listed is the kinds whose whole list was read. A kind that could not be (a permission is missing, the API timed out) is in Problems
	// instead, and what is known of it must be left as it is: an empty answer from a failed call is not "everything was deleted".
	Listed   map[string]bool
	Problems map[string]error
	// Skipped counts objects left out because they could not be reported (see Fit).
	Skipped int
}

// Source reads the cluster. dynamic may be nil: KEDA ScaledObjects are then not looked for.
type Source struct {
	kc  kubernetes.Interface
	dc  dynamic.Interface
	opt Options
	ex  map[string]bool
}

func New(kc kubernetes.Interface, dc dynamic.Interface, opt Options) *Source {
	if opt.PageSize <= 0 {
		opt.PageSize = 500
	}
	if opt.Events == "" {
		opt.Events = EventsWarning
	}
	ex := map[string]bool{}
	for _, n := range opt.ExcludeNamespaces {
		ex[strings.TrimSpace(n)] = true
	}
	return &Source{kc: kc, dc: dc, opt: opt, ex: ex}
}

// collect reads every page of a list.
func collect[T any](ctx context.Context, size int64, list func(context.Context, metav1.ListOptions) ([]T, string, error)) ([]T, error) {
	var all []T
	opts := metav1.ListOptions{Limit: size}
	for {
		items, cont, err := list(ctx, opts)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if cont == "" {
			return all, nil
		}
		opts.Continue = cont
	}
}

// Snapshot looks at the cluster now.
func (s *Source) Snapshot(ctx context.Context) Snapshot {
	snap := Snapshot{Listed: map[string]bool{}, Problems: map[string]error{}}
	add := func(kind string, err error, objs ...wire.Object) {
		if err != nil {
			snap.Problems[kind] = err
			return
		}
		snap.Listed[kind] = true
		for _, o := range objs {
			if !wire.ClusterScoped[o.Kind] && s.ex[o.Namespace] {
				continue
			}
			if o.Kind == wire.Namespace && s.ex[o.Name] {
				continue
			}
			fit, ferr := Fit(o)
			if ferr != nil {
				snap.Skipped++
				continue
			}
			snap.Objects = append(snap.Objects, fit)
		}
	}
	size := s.opt.PageSize

	nodes, err := collect(ctx, size, func(ctx context.Context, o metav1.ListOptions) ([]corev1.Node, string, error) {
		l, err := s.kc.CoreV1().Nodes().List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return l.Items, l.Continue, nil
	})
	var objs []wire.Object
	for i := range nodes {
		objs = append(objs, NodeObject(&nodes[i]))
	}
	add(wire.Node, err, objs...)

	nss, err := collect(ctx, size, func(ctx context.Context, o metav1.ListOptions) ([]corev1.Namespace, string, error) {
		l, err := s.kc.CoreV1().Namespaces().List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return l.Items, l.Continue, nil
	})
	objs = nil
	for i := range nss {
		objs = append(objs, NamespaceObject(&nss[i]))
	}
	add(wire.Namespace, err, objs...)

	deps, err := collect(ctx, size, func(ctx context.Context, o metav1.ListOptions) ([]appsv1.Deployment, string, error) {
		l, err := s.kc.AppsV1().Deployments("").List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return l.Items, l.Continue, nil
	})
	objs = nil
	for i := range deps {
		objs = append(objs, DeploymentObject(&deps[i]))
	}
	add(wire.Deployment, err, objs...)

	sts, err := collect(ctx, size, func(ctx context.Context, o metav1.ListOptions) ([]appsv1.StatefulSet, string, error) {
		l, err := s.kc.AppsV1().StatefulSets("").List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return l.Items, l.Continue, nil
	})
	objs = nil
	for i := range sts {
		objs = append(objs, StatefulSetObject(&sts[i]))
	}
	add(wire.StatefulSet, err, objs...)

	dss, err := collect(ctx, size, func(ctx context.Context, o metav1.ListOptions) ([]appsv1.DaemonSet, string, error) {
		l, err := s.kc.AppsV1().DaemonSets("").List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return l.Items, l.Continue, nil
	})
	objs = nil
	for i := range dss {
		objs = append(objs, DaemonSetObject(&dss[i]))
	}
	add(wire.DaemonSet, err, objs...)

	cjs, err := collect(ctx, size, func(ctx context.Context, o metav1.ListOptions) ([]batchv1.CronJob, string, error) {
		l, err := s.kc.BatchV1().CronJobs("").List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return l.Items, l.Continue, nil
	})
	objs = nil
	for i := range cjs {
		objs = append(objs, CronJobObject(&cjs[i]))
	}
	add(wire.CronJob, err, objs...)

	// Pods are reported with the workload they belong to, which is found through their ReplicaSet or Job. If either list fails, the pods are
	// not reported this time rather than reported with a different workload from last time.
	rss, rerr := collect(ctx, size, func(ctx context.Context, o metav1.ListOptions) ([]appsv1.ReplicaSet, string, error) {
		l, err := s.kc.AppsV1().ReplicaSets("").List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return l.Items, l.Continue, nil
	})
	jobs, jerr := collect(ctx, size, func(ctx context.Context, o metav1.ListOptions) ([]batchv1.Job, string, error) {
		l, err := s.kc.BatchV1().Jobs("").List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return l.Items, l.Continue, nil
	})
	pods, perr := collect(ctx, size, func(ctx context.Context, o metav1.ListOptions) ([]corev1.Pod, string, error) {
		l, err := s.kc.CoreV1().Pods("").List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return l.Items, l.Continue, nil
	})
	switch {
	case perr != nil:
		add(wire.Pod, perr)
	case rerr != nil:
		add(wire.Pod, rerr)
	case jerr != nil:
		add(wire.Pod, jerr)
	default:
		ow := Owners{ReplicaSets: map[types.UID]Owner{}, Jobs: map[types.UID]Owner{}}
		for i := range rss {
			if c := controllerOf(rss[i].OwnerReferences); c != nil {
				ow.ReplicaSets[rss[i].UID] = Owner{Kind: strings.ToLower(c.Kind), Name: c.Name}
			}
		}
		for i := range jobs {
			if c := controllerOf(jobs[i].OwnerReferences); c != nil {
				ow.Jobs[jobs[i].UID] = Owner{Kind: strings.ToLower(c.Kind), Name: c.Name}
			}
		}
		objs = nil
		for i := range pods {
			objs = append(objs, PodObject(&pods[i], ow))
		}
		add(wire.Pod, nil, objs...)
	}

	pvcs, err := collect(ctx, size, func(ctx context.Context, o metav1.ListOptions) ([]corev1.PersistentVolumeClaim, string, error) {
		l, err := s.kc.CoreV1().PersistentVolumeClaims("").List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return l.Items, l.Continue, nil
	})
	objs = nil
	for i := range pvcs {
		objs = append(objs, PVCObject(&pvcs[i]))
	}
	add(wire.PVC, err, objs...)

	hpas, err := collect(ctx, size, func(ctx context.Context, o metav1.ListOptions) ([]autoscalingv2.HorizontalPodAutoscaler, string, error) {
		l, err := s.kc.AutoscalingV2().HorizontalPodAutoscalers("").List(ctx, o)
		if err != nil {
			return nil, "", err
		}
		return l.Items, l.Continue, nil
	})
	objs = nil
	for i := range hpas {
		objs = append(objs, HPAObject(&hpas[i]))
	}
	add(wire.HPA, err, objs...)

	if s.dc != nil {
		sos, err := collect(ctx, size, func(ctx context.Context, o metav1.ListOptions) ([]unstructured.Unstructured, string, error) {
			l, err := s.dc.Resource(scaledObjects).Namespace(metav1.NamespaceAll).List(ctx, o)
			if err != nil {
				return nil, "", err
			}
			return l.Items, l.GetContinue(), nil
		})
		// KEDA is optional: a cluster without its CRD answers "not found", and has no ScaledObjects (so any it had are gone).
		if err != nil && (apierrors.IsNotFound(err) || meta.IsNoMatchError(err)) {
			sos, err = nil, nil
		}
		objs = nil
		for i := range sos {
			objs = append(objs, ScaledObjectObject(&sos[i]))
		}
		add(wire.ScaledObject, err, objs...)
	}

	if s.opt.Events != EventsNone {
		evs, err := collect(ctx, size, func(ctx context.Context, o metav1.ListOptions) ([]corev1.Event, string, error) {
			if s.opt.Events == EventsWarning {
				o.FieldSelector = "type=Warning"
			}
			l, err := s.kc.CoreV1().Events("").List(ctx, o)
			if err != nil {
				return nil, "", err
			}
			return l.Items, l.Continue, nil
		})
		if err != nil {
			snap.Problems["event"] = err
		} else {
			snap.Listed["event"] = true
			for i := range evs {
				if s.opt.Events == EventsWarning && evs[i].Type != "Warning" {
					continue // not every API server applies the selector
				}
				if s.ex[evs[i].Namespace] {
					continue
				}
				if ev, ok := EventObject(&evs[i]); ok {
					snap.Events = append(snap.Events, ev)
				}
			}
		}
	}

	sort.SliceStable(snap.Objects, func(i, j int) bool {
		a, b := snap.Objects[i], snap.Objects[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	sort.SliceStable(snap.Events, func(i, j int) bool { return snap.Events[i].UID < snap.Events[j].UID })
	return snap
}
