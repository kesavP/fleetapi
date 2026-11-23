package kube

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/sojoner/kro-lab/platform-mvp/cluster-collector/wire"
)

var t0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func om(ns, name, uid string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(uid), CreationTimestamp: metav1.NewTime(t0.Add(-time.Hour)), Labels: map[string]string{"app": name}}
}

func q(s string) resource.Quantity { return resource.MustParse(s) }

// secretMarkers are put wherever a careless mapper might copy from; none may come out.
var secretMarkers = []string{"SECRET-ANNOTATION", "SECRET-ENV", "SECRET-CMD", "SECRET-ARG", "SECRET-VOLUME", "SECRET-ENVFROM", "SECRET-TRIGGER", "SECRET-HOSTPATH", "SECRET-PULL"}

func nasty(c corev1.Container) corev1.Container {
	c.Env = []corev1.EnvVar{{Name: "DB_PASSWORD", Value: "SECRET-ENV"}}
	c.EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "SECRET-ENVFROM"}}}}
	c.Command = []string{"sh", "-c", "curl -u admin:SECRET-CMD"}
	c.Args = []string{"--token=SECRET-ARG"}
	return c
}

func nastySpec(containers ...corev1.Container) corev1.PodSpec {
	for i := range containers {
		containers[i] = nasty(containers[i])
	}
	return corev1.PodSpec{
		Containers:       containers,
		Volumes:          []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "SECRET-VOLUME"}}}, {Name: "h", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/SECRET-HOSTPATH"}}}},
		ImagePullSecrets: []corev1.LocalObjectReference{{Name: "SECRET-PULL"}},
	}
}

func withAnn(m metav1.ObjectMeta) metav1.ObjectMeta {
	m.Annotations = map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "SECRET-ANNOTATION"}
	return m
}

func everything() []wire.Object {
	c := corev1.Container{Name: "app", Image: "shop/web:1", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: q("100m")}}}
	pod := &corev1.Pod{ObjectMeta: withAnn(om("shop", "web-5d6f8c7b9-xk2lp", "p1")), Spec: nastySpec(c)}
	so := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "keda.sh/v1alpha1", "kind": "ScaledObject",
		"metadata": map[string]any{"name": "web", "namespace": "shop", "uid": "so1", "annotations": map[string]any{"a": "SECRET-ANNOTATION"}},
		"spec": map[string]any{
			"scaleTargetRef": map[string]any{"name": "web"},
			"triggers":       []any{map[string]any{"type": "rabbitmq", "metadata": map[string]any{"host": "amqp://user:SECRET-TRIGGER@mq", "queueName": "orders"}, "authenticationRef": map[string]any{"name": "SECRET-TRIGGER"}}},
		},
	}}
	return []wire.Object{
		NodeObject(&corev1.Node{ObjectMeta: withAnn(om("", "node-a", "n1")), Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule}}}}),
		NamespaceObject(&corev1.Namespace{ObjectMeta: withAnn(om("", "shop", "ns1"))}),
		DeploymentObject(&appsv1.Deployment{ObjectMeta: withAnn(om("shop", "web", "d1")), Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: nastySpec(c)}}}),
		StatefulSetObject(&appsv1.StatefulSet{ObjectMeta: withAnn(om("shop", "db", "s1")), Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{Spec: nastySpec(c)}}}),
		DaemonSetObject(&appsv1.DaemonSet{ObjectMeta: withAnn(om("shop", "agent", "ds1")), Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: nastySpec(c)}}}),
		CronJobObject(&batchv1.CronJob{ObjectMeta: withAnn(om("shop", "cleanup", "cj1")), Spec: batchv1.CronJobSpec{Schedule: "0 * * * *", JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: nastySpec(c)}}}}}),
		PodObject(pod, Owners{}),
		PVCObject(&corev1.PersistentVolumeClaim{ObjectMeta: withAnn(om("shop", "data", "pv1"))}),
		HPAObject(&autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: withAnn(om("shop", "web", "h1"))}),
		ScaledObjectObject(so),
	}
}

// The mappers pick fields by name: nothing that holds a secret may reach the wire, whatever the object has in it.
func TestNothingSecretIsEverReported(t *testing.T) {
	for _, o := range everything() {
		raw, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range secretMarkers {
			if strings.Contains(string(raw), m) {
				t.Errorf("a %s carries %q: %s", o.Kind, m, raw)
			}
		}
	}
}

func TestEveryMappedObjectCarriesOnlyWhatItsKindMayAndFits(t *testing.T) {
	seen := map[string]bool{}
	for _, o := range everything() {
		seen[o.Kind] = true
		allow := allowed(o.Kind)
		for k := range o.Data {
			if !allow[k] {
				t.Errorf("a %s carries %q, which is not one of its attributes", o.Kind, k)
			}
		}
		if _, err := Fit(o); err != nil {
			t.Errorf("a %s does not fit: %v", o.Kind, err)
		}
	}
	for _, k := range wire.Kinds {
		if !seen[k] {
			t.Errorf("no test object for kind %s", k)
		}
	}
}

func TestNode(t *testing.T) {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: "n1", Labels: map[string]string{"topology.kubernetes.io/zone": "us-central1-a", "topology.kubernetes.io/region": "us-central1", "node.kubernetes.io/instance-type": "e2-standard-4"}},
		Spec:       corev1.NodeSpec{Unschedulable: true, Taints: []corev1.Taint{{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule}, {Key: "node.kubernetes.io/unreachable", Effect: corev1.TaintEffectNoExecute}}},
		Status: corev1.NodeStatus{
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}, {Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse}},
			Capacity:    corev1.ResourceList{corev1.ResourceCPU: q("4"), corev1.ResourceMemory: q("16Gi"), corev1.ResourcePods: q("110"), corev1.ResourceEphemeralStorage: q("100Gi")},
			Allocatable: corev1.ResourceList{corev1.ResourceCPU: q("3920m"), corev1.ResourceMemory: q("14Gi")},
			NodeInfo:    corev1.NodeSystemInfo{KubeletVersion: "v1.31.2", Architecture: "amd64", ContainerRuntimeVersion: "containerd://1.7.2", OSImage: "Ubuntu 22.04"},
			Addresses:   []corev1.NodeAddress{{Type: corev1.NodeExternalIP, Address: "34.1.1.1"}, {Type: corev1.NodeInternalIP, Address: "10.0.0.5"}},
		},
	}
	o := NodeObject(n)
	d := o.Data
	if o.Kind != "node" || o.Namespace != "" || d["ready"] != true || d["unschedulable"] != true || d["zone"] != "us-central1-a" || d["region"] != "us-central1" || d["instance_type"] != "e2-standard-4" || d["internal_ip"] != "10.0.0.5" || d["kubelet"] != "v1.31.2" {
		t.Fatalf("%+v", o)
	}
	if c := d["capacity"].(map[string]any); c["cpu_m"] != int64(4000) || c["memory_bytes"] != int64(16<<30) || c["pods"] != int64(110) || c["ephemeral_bytes"] != int64(100<<30) {
		t.Fatalf("capacity %+v", c)
	}
	if a := d["allocatable"].(map[string]any); a["cpu_m"] != int64(3920) {
		t.Fatalf("allocatable %+v", a)
	}
	if tt := d["taints"].([]any); len(tt) != 2 || tt[0] != "dedicated=gpu:NoSchedule" || tt[1] != "node.kubernetes.io/unreachable:NoExecute" {
		t.Fatalf("taints %+v", tt)
	}
	if c := d["conditions"].(map[string]any); c["Ready"] != "True" || c["MemoryPressure"] != "False" {
		t.Fatalf("conditions %+v", c)
	}
	if NodeObject(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}}).Data["ready"] != false {
		t.Fatal("a node with no Ready condition is not ready")
	}
}

func TestWorkloadsScaledToZeroAreReportedAsSuch(t *testing.T) {
	zero := int32(0)
	d := DeploymentObject(&appsv1.Deployment{ObjectMeta: om("shop", "cartservice", "d1"), Spec: appsv1.DeploymentSpec{Replicas: &zero, Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType}}, Status: appsv1.DeploymentStatus{ObservedGeneration: 4}})
	if d.Data["desired"] != int64(0) || d.Data["ready"] != int64(0) || d.Data["strategy"] != "RollingUpdate" || d.Data["observed_generation"] != int64(4) {
		t.Fatalf("%+v", d.Data)
	}
	// an unset replica count means one
	if got := DeploymentObject(&appsv1.Deployment{ObjectMeta: om("shop", "x", "d2")}).Data["desired"]; got != int64(1) {
		t.Fatalf("a deployment with no replicas set wants %v, not 1", got)
	}
	if got := StatefulSetObject(&appsv1.StatefulSet{ObjectMeta: om("shop", "x", "s2")}).Data["desired"]; got != int64(1) {
		t.Fatalf("a stateful set with no replicas set wants %v, not 1", got)
	}
	ds := DaemonSetObject(&appsv1.DaemonSet{ObjectMeta: om("shop", "agent", "ds"), Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 3, NumberReady: 2, NumberUnavailable: 1, NumberMisscheduled: 1}})
	if ds.Data["desired"] != int64(3) || ds.Data["ready"] != int64(2) || ds.Data["unavailable"] != int64(1) || ds.Data["misscheduled"] != int64(1) {
		t.Fatalf("%+v", ds.Data)
	}
	cj := CronJobObject(&batchv1.CronJob{ObjectMeta: om("shop", "c", "cj"), Spec: batchv1.CronJobSpec{Schedule: "*/5 * * * *", Suspend: ptr.To(true)}, Status: batchv1.CronJobStatus{LastScheduleTime: &metav1.Time{Time: t0}, Active: []corev1.ObjectReference{{}, {}}}})
	if cj.Data["suspend"] != true || cj.Data["active"] != int64(2) || cj.Data["last_schedule"] != "2026-11-10T12:00:00Z" || cj.Data["schedule"] != "*/5 * * * *" {
		t.Fatalf("%+v", cj.Data)
	}
}

func TestWorkloadContainersCarryImageAndResourcesButNothingElse(t *testing.T) {
	c := corev1.Container{Name: "app", Image: "shop/web:1.2", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: q("250m"), corev1.ResourceMemory: q("128Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: q("256Mi")},
	}}
	d := DeploymentObject(&appsv1.Deployment{ObjectMeta: om("shop", "web", "d"), Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{nasty(c)}}}}})
	cs := d.Data["containers"].([]any)
	m := cs[0].(map[string]any)
	if len(cs) != 1 || m["name"] != "app" || m["image"] != "shop/web:1.2" {
		t.Fatalf("%+v", cs)
	}
	if r := m["requests"].(map[string]any); r["cpu_m"] != int64(250) || r["memory_bytes"] != int64(128<<20) {
		t.Fatalf("requests %+v", r)
	}
	if l := m["limits"].(map[string]any); l["memory_bytes"] != int64(256<<20) || l["cpu_m"] != nil {
		t.Fatalf("limits %+v", l)
	}
	for k := range m {
		if k != "name" && k != "image" && k != "requests" && k != "limits" {
			t.Errorf("a container carries %q", k)
		}
	}
}

func TestPodWorkloadIsFoundThroughItsReplicaSetAndJob(t *testing.T) {
	yes := true
	ow := Owners{
		ReplicaSets: map[types.UID]Owner{"rs1": {Kind: "deployment", Name: "web"}},
		Jobs:        map[types.UID]Owner{"job1": {Kind: "cronjob", Name: "cleanup"}},
	}
	pod := func(refs ...metav1.OwnerReference) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: om("shop", "p-0", "p")}
		p.OwnerReferences = refs
		return p
	}
	for name, c := range map[string]struct {
		pod                     *corev1.Pod
		ownerKind, workloadKind string
		ownerName, workloadName string
	}{
		"a deployment's pod":                  {pod(metav1.OwnerReference{Kind: "ReplicaSet", Name: "web-5d6f8c7b9", UID: "rs1", Controller: &yes}), "replicaset", "deployment", "web-5d6f8c7b9", "web"},
		"a cron job's pod":                    {pod(metav1.OwnerReference{Kind: "Job", Name: "cleanup-2929", UID: "job1", Controller: &yes}), "job", "cronjob", "cleanup-2929", "cleanup"},
		"a stateful set's pod":                {pod(metav1.OwnerReference{Kind: "StatefulSet", Name: "db", UID: "s", Controller: &yes}), "statefulset", "statefulset", "db", "db"},
		"a daemon set's pod":                  {pod(metav1.OwnerReference{Kind: "DaemonSet", Name: "agent", UID: "ds", Controller: &yes}), "daemonset", "daemonset", "agent", "agent"},
		"a replica set with none":             {pod(metav1.OwnerReference{Kind: "ReplicaSet", Name: "orphan-1", UID: "unknown", Controller: &yes}), "replicaset", "replicaset", "orphan-1", "orphan-1"},
		"a custom controller":                 {pod(metav1.OwnerReference{Kind: "Rollout", Name: "web", UID: "r", Controller: &yes}), "rollout", "rollout", "web", "web"},
		"a bare pod":                          {pod(), "", "pod", "", "p-0"},
		"the controller, not the first owner": {pod(metav1.OwnerReference{Kind: "Foo", Name: "x", UID: "x"}, metav1.OwnerReference{Kind: "StatefulSet", Name: "db", UID: "s", Controller: &yes}), "statefulset", "statefulset", "db", "db"},
	} {
		d := PodObject(c.pod, ow).Data
		if d["owner_kind"] != c.ownerKind || d["owner_name"] != c.ownerName || d["workload_kind"] != c.workloadKind || d["workload_name"] != c.workloadName {
			t.Errorf("%s: %v %v -> %v %v", name, d["owner_kind"], d["owner_name"], d["workload_kind"], d["workload_name"])
		}
	}
}

func TestPodStates(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: om("shop", "web-0", "p"),
		Spec: corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{
			{Name: "sidecar", Image: "proxy:1"},
			{Name: "app", Image: "web:1", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: q("100m")}}},
		}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, PodIP: "10.1.2.3", QOSClass: corev1.PodQOSBurstable, StartTime: &metav1.Time{Time: t0},
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}, {Type: corev1.PodScheduled, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", RestartCount: 7, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}}},
				{Name: "sidecar", Ready: true, RestartCount: 1, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
		},
	}
	d := PodObject(p, Owners{}).Data
	if d["phase"] != "Running" || d["node"] != "node-a" || d["ip"] != "10.1.2.3" || d["qos"] != "Burstable" || d["ready"] != false || d["restarts"] != int64(8) || d["terminating"] != false || d["started"] != "2026-11-10T12:00:00Z" {
		t.Fatalf("%+v", d)
	}
	cs := d["containers"].([]any)
	app, side := cs[0].(map[string]any), cs[1].(map[string]any)
	if app["name"] != "app" || side["name"] != "sidecar" {
		t.Fatalf("containers are sorted by name: %+v", cs)
	}
	if app["state"] != "waiting" || app["reason"] != "CrashLoopBackOff" || app["restarts"] != int64(7) || app["last_reason"] != "OOMKilled" || app["last_exit_code"] != int64(137) || app["ready"] != false {
		t.Fatalf("app %+v", app)
	}
	if side["state"] != "running" || side["ready"] != true || side["restarts"] != int64(1) {
		t.Fatalf("sidecar %+v", side)
	}
	if r := app["requests"].(map[string]any); r["cpu_m"] != int64(100) {
		t.Fatalf("requests %+v", r)
	}

	// terminated
	p.Status.ContainerStatuses[0] = corev1.ContainerStatus{Name: "app", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 2}}}
	if c := PodObject(p, Owners{}).Data["containers"].([]any)[0].(map[string]any); c["state"] != "terminated" || c["reason"] != "Error" || c["exit_code"] != int64(2) {
		t.Fatalf("%+v", c)
	}

	// pending and unschedulable: no container statuses at all
	pend := &corev1.Pod{ObjectMeta: om("shop", "web-1", "p2"), Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "web:1"}}},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable"}}}}
	pd := PodObject(pend, Owners{}).Data
	if pd["reason"] != "Unschedulable" || pd["phase"] != "Pending" || pd["containers"].([]any)[0].(map[string]any)["state"] != "waiting" {
		t.Fatalf("%+v", pd)
	}
	// evicted: the pod's own reason wins
	ev := &corev1.Pod{ObjectMeta: om("shop", "web-2", "p3"), Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted"}}
	if PodObject(ev, Owners{}).Data["reason"] != "Evicted" {
		t.Fatal("reason")
	}
	// terminating
	now := metav1.NewTime(t0)
	term := &corev1.Pod{ObjectMeta: om("shop", "web-3", "p4")}
	term.DeletionTimestamp = &now
	if PodObject(term, Owners{}).Data["terminating"] != true {
		t.Fatal("terminating")
	}
}

func TestPVC(t *testing.T) {
	c := &corev1.PersistentVolumeClaim{
		ObjectMeta: om("shop", "data", "pv"),
		Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: ptr.To("standard-rwo"), VolumeName: "pvc-123", AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: q("10Gi")}}},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: q("10Gi")}},
	}
	d := PVCObject(c).Data
	if d["phase"] != "Bound" || d["storage_class"] != "standard-rwo" || d["requested_bytes"] != int64(10<<30) || d["capacity_bytes"] != int64(10<<30) || d["volume"] != "pvc-123" || d["access_modes"].([]any)[0] != "ReadWriteOnce" {
		t.Fatalf("%+v", d)
	}
	// pending: no capacity yet, no class
	if d := PVCObject(&corev1.PersistentVolumeClaim{ObjectMeta: om("shop", "p", "pv2"), Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending}}).Data; d["phase"] != "Pending" || d["capacity_bytes"] != int64(0) || d["storage_class"] != "" {
		t.Fatalf("%+v", d)
	}
}

func TestHPA(t *testing.T) {
	util80, util45 := int32(80), int32(45)
	h := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: om("shop", "web", "h"),
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "web"}, MinReplicas: ptr.To(int32(2)), MaxReplicas: 10,
			Metrics: []autoscalingv2.MetricSpec{{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{Name: corev1.ResourceCPU, Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: &util80}}}},
		},
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{
			CurrentReplicas: 3, DesiredReplicas: 4, LastScaleTime: &metav1.Time{Time: t0},
			CurrentMetrics: []autoscalingv2.MetricStatus{{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricStatus{Name: corev1.ResourceCPU, Current: autoscalingv2.MetricValueStatus{AverageUtilization: &util45}}}},
			Conditions:     []autoscalingv2.HorizontalPodAutoscalerCondition{{Type: autoscalingv2.ScalingActive, Status: corev1.ConditionTrue}, {Type: autoscalingv2.AbleToScale, Status: corev1.ConditionTrue}},
		},
	}
	d := HPAObject(h).Data
	if d["target_kind"] != "deployment" || d["target_name"] != "web" || d["min"] != int64(2) || d["max"] != int64(10) || d["current"] != int64(3) || d["desired"] != int64(4) || d["last_scale"] != "2026-11-10T12:00:00Z" {
		t.Fatalf("%+v", d)
	}
	m := d["metrics"].([]any)[0].(map[string]any)
	if m["type"] != "Resource" || m["name"] != "cpu" || m["target"] != "80%" || m["current"] != "45%" {
		t.Fatalf("metrics %+v", m)
	}
	if c := d["conditions"].(map[string]any); c["ScalingActive"] != "True" {
		t.Fatalf("conditions %+v", c)
	}
	// metrics not read yet: a target and no current
	h.Status.CurrentMetrics = nil
	if m := HPAObject(h).Data["metrics"].([]any)[0].(map[string]any); m["target"] != "80%" || m["current"] != nil {
		t.Fatalf("%+v", m)
	}
	// no minimum set means one
	h.Spec.MinReplicas = nil
	if HPAObject(h).Data["min"] != int64(1) {
		t.Fatal("min")
	}
}

func TestScaledObjectReportsTriggerTypesOnly(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "worker", "namespace": "shop", "uid": "so"},
		"spec": map[string]any{
			"scaleTargetRef": map[string]any{"kind": "StatefulSet", "name": "worker"}, "minReplicaCount": int64(0), "maxReplicaCount": int64(20),
			"triggers": []any{map[string]any{"type": "kafka", "metadata": map[string]any{"bootstrapServers": "SECRET-TRIGGER"}}, map[string]any{"type": "cron"}},
		},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": "True"}, map[string]any{"type": "Active", "status": "False"}, map[string]any{"type": "Fallback", "status": "False"},
		}},
	}}
	o := ScaledObjectObject(u)
	d := o.Data
	if o.Kind != "scaledobject" || o.Namespace != "shop" || d["target_kind"] != "statefulset" || d["target_name"] != "worker" || d["min"] != int64(0) || d["max"] != int64(20) {
		t.Fatalf("%+v", o)
	}
	if tr := d["triggers"].([]any); len(tr) != 2 || tr[0] != "kafka" || tr[1] != "cron" {
		t.Fatalf("triggers %+v", tr)
	}
	if d["ready"] != "True" || d["active"] != "False" || d["fallback"] != "False" {
		t.Fatalf("%+v", d)
	}
	// defaults: a deployment, from zero to a hundred
	bare := ScaledObjectObject(&unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "b", "namespace": "n", "uid": "u"}, "spec": map[string]any{"scaleTargetRef": map[string]any{"name": "d"}}}}).Data
	if bare["target_kind"] != "deployment" || bare["min"] != int64(0) || bare["max"] != int64(100) {
		t.Fatalf("%+v", bare)
	}
}

func TestEvents(t *testing.T) {
	e := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", UID: "e1", CreationTimestamp: metav1.NewTime(t0)}, InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "web-0", UID: "p"},
		Type: "Warning", Reason: "BackOff", Message: "Back-off restarting failed container", Count: 4, FirstTimestamp: metav1.NewTime(t0.Add(-time.Minute)), LastTimestamp: metav1.NewTime(t0), Source: corev1.EventSource{Component: "kubelet"},
	}
	got, ok := EventObject(e)
	if !ok || got.UID != "e1" || got.Kind != "Pod" || got.Name != "web-0" || got.ObjectUID != "p" || got.Count != 4 || got.Source != "kubelet" || !got.Last.Equal(t0) || !got.First.Equal(t0.Add(-time.Minute)) || got.Namespace != "shop" {
		t.Fatalf("%+v %v", got, ok)
	}
	// a newer-style event: no last timestamp, a series
	e2 := &corev1.Event{ObjectMeta: metav1.ObjectMeta{UID: "e2", CreationTimestamp: metav1.NewTime(t0)}, InvolvedObject: corev1.ObjectReference{Kind: "Node", Name: "n"}, Type: "Normal", Reason: "NodeReady",
		EventTime: metav1.MicroTime{Time: t0.Add(time.Minute)}, Series: &corev1.EventSeries{Count: 9, LastObservedTime: metav1.MicroTime{Time: t0.Add(2 * time.Minute)}}, ReportingController: "kubelet"}
	if g, ok := EventObject(e2); !ok || g.Count != 9 || !g.Last.Equal(t0.Add(2*time.Minute)) || g.Source != "kubelet" {
		t.Fatalf("%+v %v", g, ok)
	}
	// repaired, not refused: a reason with a space, a message with control characters and a bad encoding, a count of zero
	e3 := *e
	e3.Reason, e3.Message, e3.Count = "Failed Scheduling!", "a\x00b\nc\xff"+strings.Repeat("x", 2000), 0
	g, ok := EventObject(&e3)
	if !ok || g.Reason != "Failed_Scheduling" || g.Count != 1 || len(g.Message) > wire.MaxMessage || strings.ContainsAny(g.Message, "\x00\n\xff") {
		t.Fatalf("%q %d %d %v", g.Reason, g.Count, len(g.Message), ok)
	}
	e3.Reason = "!!!"
	if g, _ := EventObject(&e3); g.Reason != "Unknown" {
		t.Fatalf("reason %q", g.Reason)
	}
	e3.Type = "Some Odd Type"
	if g, _ := EventObject(&e3); g.Type != "Unknown" {
		t.Fatalf("type %q", g.Type)
	}
	// left out: no uid, no object name, a kind that is not a word
	for name, mut := range map[string]func(*corev1.Event){
		"no uid":  func(x *corev1.Event) { x.UID = "" },
		"no name": func(x *corev1.Event) { x.InvolvedObject.Name = "" },
		"no kind": func(x *corev1.Event) { x.InvolvedObject.Kind = "" },
	} {
		x := *e
		mut(&x)
		if _, ok := EventObject(&x); ok {
			t.Errorf("%s: reported", name)
		}
	}
}

func TestLabelsAreCappedAndCleaned(t *testing.T) {
	m := map[string]string{}
	for i := 0; i < 100; i++ {
		m[strings.Repeat("k", 1+i%50)+string(rune('a'+i%26))+string(rune('a'+i/26))] = "v"
	}
	m["long"] = strings.Repeat("v", 400)
	if got := labelsOf(m); len(got) > wire.MaxLabels {
		t.Fatalf("%d labels", len(got))
	}
	got := labelsOf(map[string]string{"a": strings.Repeat("v", 400), "b": "x\ny"})
	if len(got["a"]) != 253 || got["b"] != "x y" {
		t.Fatalf("%q %q", got["a"], got["b"])
	}
	if labelsOf(nil) != nil || labelsOf(map[string]string{}) != nil {
		t.Fatal("no labels is nil")
	}
}

func TestFit(t *testing.T) {
	ok := wire.Object{Kind: wire.Pod, UID: "u1", Namespace: "shop", Name: "web-0", Data: map[string]any{"phase": "Running"}}
	if _, err := Fit(ok); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*wire.Object){
		"unknown kind":      func(o *wire.Object) { o.Kind = "secret" },
		"empty uid":         func(o *wire.Object) { o.UID = "" },
		"bad name":          func(o *wire.Object) { o.Name = "has space" },
		"no namespace":      func(o *wire.Object) { o.Namespace = "" },
		"node with ns":      func(o *wire.Object) { o.Kind = wire.Node },
		"unknown attribute": func(o *wire.Object) { o.Data = map[string]any{"annotations": "x"} },
	} {
		o := ok
		mut(&o)
		if _, err := Fit(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// too big, with containers: cut down until it fits, the rest kept
	big := wire.Object{Kind: wire.Deployment, UID: "d", Namespace: "n", Name: "x", Data: map[string]any{"desired": 1}}
	var cs []any
	for i := 0; i < 64; i++ {
		cs = append(cs, map[string]any{"name": strings.Repeat("n", 100), "image": strings.Repeat("i", 400)})
	}
	big.Data["containers"] = cs
	fit, err := Fit(big)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(fit.Data["containers"].([]any)); n < 1 || n >= 64 || fit.Data["desired"] != 1 {
		t.Fatalf("%d containers left, data %+v", n, fit.Data)
	}
	if len(big.Data["containers"].([]any)) != 64 {
		t.Fatal("Fit changed the object it was given")
	}
	// too big with nothing to cut
	huge := wire.Object{Kind: wire.Node, UID: "n", Name: "n", Data: map[string]any{"conditions": map[string]any{}}}
	for i := 0; i < 60; i++ {
		huge.Data["conditions"].(map[string]any)[strings.Repeat("c", 90)+string(rune('a'+i%26))+string(rune('a'+i/26))] = strings.Repeat("v", 400)
	}
	if _, err := Fit(huge); err == nil {
		t.Fatal("an object too big with nothing to cut was accepted")
	}
}

func TestStamp(t *testing.T) {
	if stamp(nil) != "" || stamp(&metav1.Time{}) != "" {
		t.Fatal("no time is an empty string, not year 1")
	}
	if stamp(&metav1.Time{Time: t0}) != "2026-11-10T12:00:00Z" {
		t.Fatal("a time is RFC 3339 in UTC")
	}
}
