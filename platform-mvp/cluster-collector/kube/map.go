// Package kube reads a cluster's Kubernetes API and turns what is in it into the collector's objects (package wire).
//
// The mappers in this file are the PRIVACY BOUNDARY of the collector. Each picks the few fields it reports by name; none copies an object or a
// section of one. Annotations (which hold a last-applied copy of the whole object, and what operators put there), environment variables,
// commands and arguments, volumes, ConfigMap and Secret contents are never read. The service holds a list of what each kind may carry and
// refuses the rest, but the first line of defence is that these do not pick them.
package kube

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/sojoner/kro-lab/platform-mvp/cluster-collector/wire"
)

// ---- small helpers ----

func clean(s string, max int) string {
	s = strings.ToValidUTF8(s, "?")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > max {
		s = s[:max]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}

func text(s string) string { return clean(s, wire.MaxString) }

func labelsOf(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if k != "" && len(k) <= 253 {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) > wire.MaxLabels {
		keys = keys[:wire.MaxLabels]
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[clean(k, 253)] = clean(m[k], 253)
	}
	return out
}

func stamp(t *metav1.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func quantity(q resource.Quantity) int64 { return q.Value() }

// amounts is a resource list as the numbers the UI shows: cores as millicores, memory and storage as bytes, pods as a count. Zero is left out.
func amounts(rl corev1.ResourceList) map[string]any {
	out := map[string]any{}
	if q, ok := rl[corev1.ResourceCPU]; ok && q.MilliValue() > 0 {
		out["cpu_m"] = q.MilliValue()
	}
	if q, ok := rl[corev1.ResourceMemory]; ok && q.Value() > 0 {
		out["memory_bytes"] = quantity(q)
	}
	if q, ok := rl[corev1.ResourcePods]; ok && q.Value() > 0 {
		out["pods"] = quantity(q)
	}
	if q, ok := rl[corev1.ResourceEphemeralStorage]; ok && q.Value() > 0 {
		out["ephemeral_bytes"] = quantity(q)
	}
	return out
}

func conditionsOf[T any](list []T, kind func(T) (string, string)) map[string]any {
	out := map[string]any{}
	for _, c := range list {
		t, s := kind(c)
		if t != "" {
			out[clean(t, 100)] = clean(s, 20)
		}
	}
	return out
}

// specContainers is what a workload's pod template runs: name, image, requests and limits. Not the environment, the command or the mounts.
func specContainers(cs []corev1.Container) []any {
	out := make([]any, 0, len(cs))
	for _, c := range cs {
		if len(out) >= wire.MaxList {
			break
		}
		m := map[string]any{"name": text(c.Name), "image": text(c.Image)}
		if r := amounts(c.Resources.Requests); len(r) > 0 {
			m["requests"] = r
		}
		if l := amounts(c.Resources.Limits); len(l) > 0 {
			m["limits"] = l
		}
		out = append(out, m)
	}
	return out
}

func base(kind string, m metav1.ObjectMeta) wire.Object {
	return wire.Object{Kind: kind, UID: string(m.UID), Namespace: m.Namespace, Name: m.Name, Labels: labelsOf(m.Labels), Created: m.CreationTimestamp.UTC()}
}

func replicas(p *int32) int64 {
	if p == nil {
		return 1
	}
	return int64(*p)
}

// ---- cluster-scoped ----

func NodeObject(n *corev1.Node) wire.Object {
	o := base(wire.Node, n.ObjectMeta)
	o.Namespace = ""
	ready := false
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			ready = c.Status == corev1.ConditionTrue
		}
	}
	taints := []any{}
	for _, t := range n.Spec.Taints {
		if len(taints) >= 20 {
			break
		}
		s := clean(t.Key, 100)
		if t.Value != "" {
			s += "=" + clean(t.Value, 100)
		}
		taints = append(taints, s+":"+string(t.Effect))
	}
	ip := ""
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			ip = a.Address
			break
		}
	}
	first := func(keys ...string) string {
		for _, k := range keys {
			if v := n.Labels[k]; v != "" {
				return text(v)
			}
		}
		return ""
	}
	o.Data = map[string]any{
		"ready":         ready,
		"conditions":    conditionsOf(n.Status.Conditions, func(c corev1.NodeCondition) (string, string) { return string(c.Type), string(c.Status) }),
		"unschedulable": n.Spec.Unschedulable,
		"taints":        taints,
		"kubelet":       text(n.Status.NodeInfo.KubeletVersion),
		"os":            text(n.Status.NodeInfo.OSImage),
		"arch":          text(n.Status.NodeInfo.Architecture),
		"runtime":       text(n.Status.NodeInfo.ContainerRuntimeVersion),
		"zone":          first("topology.kubernetes.io/zone", "failure-domain.beta.kubernetes.io/zone"),
		"region":        first("topology.kubernetes.io/region", "failure-domain.beta.kubernetes.io/region"),
		"instance_type": first("node.kubernetes.io/instance-type", "beta.kubernetes.io/instance-type"),
		"internal_ip":   text(ip),
		"capacity":      amounts(n.Status.Capacity),
		"allocatable":   amounts(n.Status.Allocatable),
	}
	return o
}

func NamespaceObject(n *corev1.Namespace) wire.Object {
	o := base(wire.Namespace, n.ObjectMeta)
	o.Namespace = ""
	o.Data = map[string]any{"phase": string(n.Status.Phase)}
	return o
}

// ---- workloads ----

func DeploymentObject(d *appsv1.Deployment) wire.Object {
	o := base(wire.Deployment, d.ObjectMeta)
	o.Data = map[string]any{
		"desired": replicas(d.Spec.Replicas), "current": int64(d.Status.Replicas), "ready": int64(d.Status.ReadyReplicas),
		"available": int64(d.Status.AvailableReplicas), "updated": int64(d.Status.UpdatedReplicas), "unavailable": int64(d.Status.UnavailableReplicas),
		"generation": d.Generation, "observed_generation": d.Status.ObservedGeneration, "paused": d.Spec.Paused,
		"strategy": string(d.Spec.Strategy.Type), "containers": specContainers(d.Spec.Template.Spec.Containers),
	}
	return o
}

func StatefulSetObject(s *appsv1.StatefulSet) wire.Object {
	o := base(wire.StatefulSet, s.ObjectMeta)
	o.Data = map[string]any{
		"desired": replicas(s.Spec.Replicas), "current": int64(s.Status.Replicas), "ready": int64(s.Status.ReadyReplicas),
		"available": int64(s.Status.AvailableReplicas), "updated": int64(s.Status.UpdatedReplicas),
		"generation": s.Generation, "observed_generation": s.Status.ObservedGeneration,
		"strategy": string(s.Spec.UpdateStrategy.Type), "containers": specContainers(s.Spec.Template.Spec.Containers),
	}
	return o
}

func DaemonSetObject(d *appsv1.DaemonSet) wire.Object {
	o := base(wire.DaemonSet, d.ObjectMeta)
	o.Data = map[string]any{
		"desired": int64(d.Status.DesiredNumberScheduled), "current": int64(d.Status.CurrentNumberScheduled), "ready": int64(d.Status.NumberReady),
		"available": int64(d.Status.NumberAvailable), "updated": int64(d.Status.UpdatedNumberScheduled), "unavailable": int64(d.Status.NumberUnavailable),
		"misscheduled": int64(d.Status.NumberMisscheduled), "generation": d.Generation, "observed_generation": d.Status.ObservedGeneration,
		"strategy": string(d.Spec.UpdateStrategy.Type), "containers": specContainers(d.Spec.Template.Spec.Containers),
	}
	return o
}

func CronJobObject(c *batchv1.CronJob) wire.Object {
	o := base(wire.CronJob, c.ObjectMeta)
	suspend := false
	if c.Spec.Suspend != nil {
		suspend = *c.Spec.Suspend
	}
	o.Data = map[string]any{
		"schedule": text(c.Spec.Schedule), "suspend": suspend, "active": int64(len(c.Status.Active)), "last_schedule": stamp(c.Status.LastScheduleTime),
		"containers": specContainers(c.Spec.JobTemplate.Spec.Template.Spec.Containers),
	}
	return o
}

// ---- pods ----

// Owner is what a ReplicaSet or a Job belongs to: the deployment or the cron job a pod is really part of.
type Owner struct{ Kind, Name string }

// Owners resolves a pod's workload through the objects between them. The collector lists ReplicaSets and Jobs for this and reports neither.
type Owners struct {
	ReplicaSets map[types.UID]Owner
	Jobs        map[types.UID]Owner
}

func controllerOf(refs []metav1.OwnerReference) *metav1.OwnerReference {
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller {
			return &refs[i]
		}
	}
	if len(refs) > 0 {
		return &refs[0]
	}
	return nil
}

// workload is the workload kind and name a pod belongs to; a pod with no owner is its own.
func (ow Owners) workload(p *corev1.Pod) (ownerKind, ownerName, kind, name string) {
	c := controllerOf(p.OwnerReferences)
	if c == nil {
		return "", "", "pod", p.Name
	}
	ownerKind, ownerName = strings.ToLower(c.Kind), c.Name
	switch c.Kind {
	case "ReplicaSet":
		if o, ok := ow.ReplicaSets[c.UID]; ok {
			return ownerKind, ownerName, o.Kind, o.Name
		}
	case "Job":
		if o, ok := ow.Jobs[c.UID]; ok {
			return ownerKind, ownerName, o.Kind, o.Name
		}
	}
	return ownerKind, ownerName, ownerKind, ownerName
}

func PodObject(p *corev1.Pod, ow Owners) wire.Object {
	o := base(wire.Pod, p.ObjectMeta)
	ownerKind, ownerName, wk, wn := ow.workload(p)
	ready := false
	reason := text(p.Status.Reason)
	for _, c := range p.Status.Conditions {
		switch c.Type {
		case corev1.PodReady:
			ready = c.Status == corev1.ConditionTrue
		case corev1.PodScheduled:
			if c.Status == corev1.ConditionFalse && reason == "" {
				reason = text(c.Reason) // Unschedulable
			}
		}
	}
	specs := map[string]corev1.Container{}
	for _, c := range p.Spec.Containers {
		specs[c.Name] = c
	}
	statuses := map[string]corev1.ContainerStatus{}
	for _, s := range p.Status.ContainerStatuses {
		statuses[s.Name] = s
	}
	names := make([]string, 0, len(specs))
	for n := range specs {
		names = append(names, n)
	}
	sort.Strings(names)
	var restarts int64
	containers := make([]any, 0, len(names))
	for _, n := range names {
		if len(containers) >= wire.MaxList {
			break
		}
		spec := specs[n]
		m := map[string]any{"name": text(n), "image": text(spec.Image), "state": "waiting", "ready": false, "restarts": int64(0)}
		if r := amounts(spec.Resources.Requests); len(r) > 0 {
			m["requests"] = r
		}
		if l := amounts(spec.Resources.Limits); len(l) > 0 {
			m["limits"] = l
		}
		if s, ok := statuses[n]; ok {
			m["ready"] = s.Ready
			m["restarts"] = int64(s.RestartCount)
			restarts += int64(s.RestartCount)
			switch {
			case s.State.Running != nil:
				m["state"] = "running"
			case s.State.Terminated != nil:
				m["state"] = "terminated"
				m["reason"] = text(s.State.Terminated.Reason)
				m["exit_code"] = int64(s.State.Terminated.ExitCode)
			case s.State.Waiting != nil:
				m["reason"] = text(s.State.Waiting.Reason)
			}
			if t := s.LastTerminationState.Terminated; t != nil {
				m["last_reason"] = text(t.Reason)
				m["last_exit_code"] = int64(t.ExitCode)
			}
		}
		containers = append(containers, m)
	}
	o.Data = map[string]any{
		"phase": string(p.Status.Phase), "reason": reason, "node": text(p.Spec.NodeName), "ip": text(p.Status.PodIP),
		"owner_kind": ownerKind, "owner_name": text(ownerName), "workload_kind": wk, "workload_name": text(wn),
		"ready":      ready,
		"conditions": conditionsOf(p.Status.Conditions, func(c corev1.PodCondition) (string, string) { return string(c.Type), string(c.Status) }),
		"qos":        string(p.Status.QOSClass), "started": stamp(p.Status.StartTime), "terminating": p.DeletionTimestamp != nil,
		"restarts": restarts, "containers": containers,
	}
	return o
}

// ---- volumes and autoscalers ----

func PVCObject(c *corev1.PersistentVolumeClaim) wire.Object {
	o := base(wire.PVC, c.ObjectMeta)
	class := ""
	if c.Spec.StorageClassName != nil {
		class = text(*c.Spec.StorageClassName)
	}
	modes := make([]any, 0, len(c.Spec.AccessModes))
	for _, m := range c.Spec.AccessModes {
		modes = append(modes, string(m))
	}
	o.Data = map[string]any{
		"phase": string(c.Status.Phase), "storage_class": class, "access_modes": modes, "volume": text(c.Spec.VolumeName),
		"requested_bytes": quantity(c.Spec.Resources.Requests[corev1.ResourceStorage]), "capacity_bytes": quantity(c.Status.Capacity[corev1.ResourceStorage]),
	}
	return o
}

func metricName(m autoscalingv2.MetricSpec) string {
	switch m.Type {
	case autoscalingv2.ResourceMetricSourceType:
		if m.Resource != nil {
			return string(m.Resource.Name)
		}
	case autoscalingv2.PodsMetricSourceType:
		if m.Pods != nil {
			return m.Pods.Metric.Name
		}
	case autoscalingv2.ObjectMetricSourceType:
		if m.Object != nil {
			return m.Object.Metric.Name
		}
	case autoscalingv2.ExternalMetricSourceType:
		if m.External != nil {
			return m.External.Metric.Name
		}
	case autoscalingv2.ContainerResourceMetricSourceType:
		if m.ContainerResource != nil {
			return string(m.ContainerResource.Name)
		}
	}
	return ""
}

func target(t autoscalingv2.MetricTarget) string {
	switch {
	case t.AverageUtilization != nil:
		return fmt.Sprintf("%d%%", *t.AverageUtilization)
	case t.AverageValue != nil:
		return t.AverageValue.String()
	case t.Value != nil:
		return t.Value.String()
	}
	return ""
}

func current(v autoscalingv2.MetricValueStatus) string {
	switch {
	case v.AverageUtilization != nil:
		return fmt.Sprintf("%d%%", *v.AverageUtilization)
	case v.AverageValue != nil:
		return v.AverageValue.String()
	case v.Value != nil:
		return v.Value.String()
	}
	return ""
}

func HPAObject(h *autoscalingv2.HorizontalPodAutoscaler) wire.Object {
	o := base(wire.HPA, h.ObjectMeta)
	min := int64(1)
	if h.Spec.MinReplicas != nil {
		min = int64(*h.Spec.MinReplicas)
	}
	metrics := []any{}
	for i, m := range h.Spec.Metrics {
		if len(metrics) >= wire.MaxList {
			break
		}
		e := map[string]any{"type": string(m.Type), "name": text(metricName(m))}
		var tg autoscalingv2.MetricTarget
		switch {
		case m.Resource != nil:
			tg = m.Resource.Target
		case m.Pods != nil:
			tg = m.Pods.Target
		case m.Object != nil:
			tg = m.Object.Target
		case m.External != nil:
			tg = m.External.Target
		case m.ContainerResource != nil:
			tg = m.ContainerResource.Target
		}
		e["target"] = text(target(tg))
		// the status lists the metrics in the spec's order, when it has read them
		if i < len(h.Status.CurrentMetrics) {
			s := h.Status.CurrentMetrics[i]
			switch {
			case s.Resource != nil:
				e["current"] = text(current(s.Resource.Current))
			case s.Pods != nil:
				e["current"] = text(current(s.Pods.Current))
			case s.Object != nil:
				e["current"] = text(current(s.Object.Current))
			case s.External != nil:
				e["current"] = text(current(s.External.Current))
			case s.ContainerResource != nil:
				e["current"] = text(current(s.ContainerResource.Current))
			}
		}
		metrics = append(metrics, e)
	}
	o.Data = map[string]any{
		"target_kind": strings.ToLower(h.Spec.ScaleTargetRef.Kind), "target_name": text(h.Spec.ScaleTargetRef.Name),
		"min": min, "max": int64(h.Spec.MaxReplicas), "current": int64(h.Status.CurrentReplicas), "desired": int64(h.Status.DesiredReplicas),
		"metrics": metrics,
		"conditions": conditionsOf(h.Status.Conditions, func(c autoscalingv2.HorizontalPodAutoscalerCondition) (string, string) {
			return string(c.Type), string(c.Status)
		}),
		"last_scale": stamp(h.Status.LastScaleTime),
	}
	return o
}

// ScaledObjectObject maps a KEDA ScaledObject, read as unstructured (the CRD may not be installed, and its Go types are not a dependency).
// Only the trigger TYPES are reported: a trigger's metadata is where connection strings and queue addresses live.
func ScaledObjectObject(u *unstructured.Unstructured) wire.Object {
	o := wire.Object{Kind: wire.ScaledObject, UID: string(u.GetUID()), Namespace: u.GetNamespace(), Name: u.GetName(), Labels: labelsOf(u.GetLabels()), Created: u.GetCreationTimestamp().UTC()}
	str := func(path ...string) string { s, _, _ := unstructured.NestedString(u.Object, path...); return s }
	num := func(def int64, path ...string) int64 {
		if v, found, err := unstructured.NestedInt64(u.Object, path...); err == nil && found {
			return v
		}
		return def
	}
	kind := str("spec", "scaleTargetRef", "kind")
	if kind == "" {
		kind = "Deployment"
	}
	triggers := []any{}
	if list, _, _ := unstructured.NestedSlice(u.Object, "spec", "triggers"); len(list) > 0 {
		for _, t := range list {
			if len(triggers) >= wire.MaxList {
				break
			}
			if m, ok := t.(map[string]any); ok {
				if typ, ok := m["type"].(string); ok {
					triggers = append(triggers, text(typ))
				}
			}
		}
	}
	conds := map[string]any{}
	if list, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions"); len(list) > 0 {
		for _, c := range list {
			if m, ok := c.(map[string]any); ok {
				t, _ := m["type"].(string)
				s, _ := m["status"].(string)
				if t != "" {
					conds[clean(t, 100)] = clean(s, 20)
				}
			}
		}
	}
	get := func(k string) string { s, _ := conds[k].(string); return s }
	o.Data = map[string]any{
		"target_kind": strings.ToLower(kind), "target_name": text(str("spec", "scaleTargetRef", "name")),
		"min": num(0, "spec", "minReplicaCount"), "max": num(100, "spec", "maxReplicaCount"), "triggers": triggers,
		"ready": get("Ready"), "active": get("Active"), "fallback": get("Fallback"), "paused": get("Paused"), "conditions": conds,
	}
	return o
}

// ---- events ----

var (
	reasonOK  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	reasonBad = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)
	typeOK    = regexp.MustCompile(`^[A-Za-z]{1,16}$`)
	kindOK    = regexp.MustCompile(`^[A-Za-z]{1,63}$`)
)

// EventObject maps a Kubernetes event; false when it cannot be reported (it names no object, or no time).
func EventObject(e *corev1.Event) (wire.Event, bool) {
	last := e.LastTimestamp.Time
	if last.IsZero() {
		last = e.EventTime.Time
	}
	if e.Series != nil && e.Series.LastObservedTime.Time.After(last) {
		last = e.Series.LastObservedTime.Time
	}
	if last.IsZero() {
		last = e.CreationTimestamp.Time
	}
	first := e.FirstTimestamp.Time
	if first.IsZero() || first.After(last) {
		first = last
	}
	count := int(e.Count)
	if e.Series != nil && int(e.Series.Count) > count {
		count = int(e.Series.Count)
	}
	if count < 1 {
		count = 1
	}
	reason := e.Reason
	if !reasonOK.MatchString(reason) {
		reason = strings.Trim(reasonBad.ReplaceAllString(reason, "_"), "_.-")
		if len(reason) > 128 {
			reason = reason[:128]
		}
		if !reasonOK.MatchString(reason) {
			reason = "Unknown"
		}
	}
	typ := e.Type
	if !typeOK.MatchString(typ) {
		typ = "Unknown"
	}
	src := e.ReportingController
	if src == "" {
		src = e.Source.Component
	}
	ev := wire.Event{
		UID: string(e.UID), Namespace: e.Namespace, Kind: e.InvolvedObject.Kind, Name: e.InvolvedObject.Name, ObjectUID: string(e.InvolvedObject.UID),
		Type: typ, Reason: reason, Message: clean(e.Message, wire.MaxMessage), Count: count, First: first.UTC(), Last: last.UTC(), Source: clean(src, 100),
	}
	if ev.UID == "" || ev.Name == "" || !kindOK.MatchString(ev.Kind) || last.IsZero() {
		return wire.Event{}, false
	}
	return ev, true
}
