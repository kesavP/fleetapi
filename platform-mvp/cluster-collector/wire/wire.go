// Package wire is the JSON a cluster collector posts to telemetry-ingest's /v1/inventory. It is a copy of the shape the service reads
// (telemetry-ingest/inventory), not an import of it: the two are separate Go modules that build on their own, and agree through the JSON and
// its Version. The service decides what it accepts (Attributes below must not list anything it does not), and refuses the rest.
package wire

import "time"

const Version = 1

// Kinds of object.
const (
	Node         = "node"
	Namespace    = "namespace"
	Deployment   = "deployment"
	StatefulSet  = "statefulset"
	DaemonSet    = "daemonset"
	CronJob      = "cronjob"
	Pod          = "pod"
	PVC          = "pvc"
	HPA          = "hpa"
	ScaledObject = "scaledobject"
)

// Kinds lists them.
var Kinds = []string{Node, Namespace, Deployment, StatefulSet, DaemonSet, CronJob, Pod, PVC, HPA, ScaledObject}

// ClusterScoped kinds have no namespace.
var ClusterScoped = map[string]bool{Node: true, Namespace: true}

// Limits of a batch, as the service enforces them.
const (
	MaxObjects = 500
	MaxDeleted = 1000
	MaxEvents  = 500
	MaxLabels  = 64
	MaxMessage = 1000
	MaxString  = 512
	MaxList    = 64
)

// Attributes is what each kind carries, by top-level name: the same lists the service holds. The mappers' tests check that nothing they
// produce is outside them, and the end-to-end test that the service accepts what they produce.
var Attributes = map[string][]string{
	Node:         {"ready", "conditions", "unschedulable", "taints", "kubelet", "os", "arch", "runtime", "zone", "region", "instance_type", "internal_ip", "capacity", "allocatable"},
	Namespace:    {"phase"},
	Deployment:   {"desired", "current", "ready", "available", "updated", "unavailable", "generation", "observed_generation", "paused", "strategy", "containers"},
	StatefulSet:  {"desired", "current", "ready", "available", "updated", "unavailable", "generation", "observed_generation", "paused", "strategy", "containers"},
	DaemonSet:    {"desired", "current", "ready", "available", "updated", "unavailable", "misscheduled", "generation", "observed_generation", "strategy", "containers"},
	CronJob:      {"schedule", "suspend", "active", "last_schedule", "containers"},
	Pod:          {"phase", "reason", "node", "ip", "owner_kind", "owner_name", "workload_kind", "workload_name", "ready", "conditions", "qos", "started", "terminating", "restarts", "containers"},
	PVC:          {"phase", "storage_class", "requested_bytes", "capacity_bytes", "access_modes", "volume"},
	HPA:          {"target_kind", "target_name", "min", "max", "current", "desired", "metrics", "conditions", "last_scale"},
	ScaledObject: {"target_kind", "target_name", "min", "max", "triggers", "ready", "active", "fallback", "paused", "conditions"},
}

type Object struct {
	Kind      string            `json:"kind"`
	UID       string            `json:"uid"`
	Namespace string            `json:"namespace,omitempty"`
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels,omitempty"`
	Created   time.Time         `json:"created"`
	Data      map[string]any    `json:"data"`
}

type Ref struct {
	Kind      string `json:"kind"`
	UID       string `json:"uid"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
}

// Sweep: after a complete resync of a kind, whatever of that kind was not sent since Before is gone.
type Sweep struct {
	Kind   string    `json:"kind"`
	Before time.Time `json:"before"`
}

type Event struct {
	UID       string    `json:"uid"`
	Namespace string    `json:"namespace,omitempty"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	ObjectUID string    `json:"object_uid,omitempty"`
	Type      string    `json:"type"`
	Reason    string    `json:"reason"`
	Message   string    `json:"message"`
	Count     int       `json:"count"`
	First     time.Time `json:"first"`
	Last      time.Time `json:"last"`
	Source    string    `json:"source,omitempty"`
}

type Batch struct {
	Version     int       `json:"version"`
	Cluster     string    `json:"cluster,omitempty"`
	CollectedAt time.Time `json:"collected_at"`
	Objects     []Object  `json:"objects,omitempty"`
	Deleted     []Ref     `json:"deleted,omitempty"`
	Sweeps      []Sweep   `json:"sweeps,omitempty"`
	Events      []Event   `json:"events,omitempty"`
}
