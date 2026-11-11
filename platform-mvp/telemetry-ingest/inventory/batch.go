// Package inventory is the contract between the cluster collector (platform-mvp/cluster-collector) and the ingest service: the JSON a
// collector posts to /v1/inventory, and the checks the service makes on it before anything is stored.
//
// A collector reads one cluster's Kubernetes API and reports what is in it: nodes, namespaces, workloads (deployments, stateful sets, daemon
// sets, cron jobs), pods, volume claims, autoscalers (HPA and KEDA ScaledObject) and events. It sends what CHANGED since its last report, a
// full copy now and then (a "resync"), and a sweep after a resync that says which objects were not in it.
//
// The two are separate Go modules that build on their own, so this is a copy of the shape, not an import: they agree through the JSON and its
// Version. Adding an attribute does not change the version (an older service refuses an attribute it does not know, which is the point: see
// Attributes); changing what one means does.
//
// What a collector may send is decided HERE and not by the collector: each kind has a list of the attributes it may carry, and a batch with
// another one is refused. Annotations, environment variables, command lines, ConfigMap and Secret contents are not on any list, so a collector
// that sent them (by mistake or because it was changed) is turned away, not stored.
package inventory

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

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

// Kinds lists them, in the order a summary shows them.
var Kinds = []string{Node, Namespace, Deployment, StatefulSet, DaemonSet, CronJob, Pod, PVC, HPA, ScaledObject}

// Workloads are the kinds a pod can belong to.
var Workloads = []string{Deployment, StatefulSet, DaemonSet, CronJob}

// clusterScoped kinds have no namespace of their own.
var clusterScoped = map[string]bool{Node: true, Namespace: true}

// plural is how a kind is named in the read API's paths.
var plural = map[string]string{
	"nodes": Node, "namespaces": Namespace, "deployments": Deployment, "statefulsets": StatefulSet, "daemonsets": DaemonSet,
	"cronjobs": CronJob, "pods": Pod, "pvcs": PVC, "hpas": HPA, "scaledobjects": ScaledObject,
}

// KindsOfPath is the kinds a path segment such as "pods" or "workloads" stands for; nil when it is none.
func KindsOfPath(segment string) []string {
	if segment == "workloads" {
		return Workloads
	}
	if k, ok := plural[segment]; ok {
		return []string{k}
	}
	return nil
}

// ValidKind reports whether k is one of Kinds.
func ValidKind(k string) bool {
	for _, x := range Kinds {
		if x == k {
			return true
		}
	}
	return false
}

// Limits of one batch. The collector sends in chunks well under them.
const (
	MaxObjects    = 500
	MaxDeleted    = 1000
	MaxEvents     = 500
	MaxLabels     = 64
	MaxObjectData = 16 << 10 // bytes of an object's attributes, as JSON
	MaxMessage    = 1000
	maxString     = 512 // an attribute's text
	maxDepth      = 4
	maxList       = 64 // elements of a list, keys of a map
	maxKey        = 100
	skew          = 5 * time.Minute
	maxAge        = 48 * time.Hour // how old a batch may be: the collector retries a failed one, but not for ever
)

// Attributes is what each kind may carry, by top-level name. See docs/platform-mvp/23-cluster-collector.md for what they mean.
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

var (
	uidRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	nameRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,252}$`)
	clusterRE  = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$`)
	reasonRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	typeRE     = regexp.MustCompile(`^[A-Za-z]{1,16}$`)
	kindNameRE = regexp.MustCompile(`^[A-Za-z]{1,63}$`)
)

// ValidCluster reports whether s is a usable cluster name.
func ValidCluster(s string) bool { return clusterRE.MatchString(s) }

// Object is one thing in the cluster as it stands. Data holds the attributes of its kind.
type Object struct {
	Kind      string            `json:"kind"`
	UID       string            `json:"uid"`
	Namespace string            `json:"namespace,omitempty"`
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels,omitempty"`
	Created   time.Time         `json:"created"`
	Data      map[string]any    `json:"data"`
}

// Ref names an object that is gone.
type Ref struct {
	Kind      string `json:"kind"`
	UID       string `json:"uid"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
}

// Sweep says: after the objects of this batch (and the batches of the same resync before it), every object of this kind that has not been
// sent since Before is gone. A collector sends it only after a resync of that kind that it knows to have been complete.
type Sweep struct {
	Kind   string    `json:"kind"`
	Before time.Time `json:"before"`
}

// Event is a Kubernetes event: something that happened to an object.
type Event struct {
	UID       string    `json:"uid"`
	Namespace string    `json:"namespace,omitempty"`
	Kind      string    `json:"kind"` // of the object it is about
	Name      string    `json:"name"`
	ObjectUID string    `json:"object_uid,omitempty"`
	Type      string    `json:"type"` // Normal or Warning
	Reason    string    `json:"reason"`
	Message   string    `json:"message"`
	Count     int       `json:"count"`
	First     time.Time `json:"first"`
	Last      time.Time `json:"last"`
	Source    string    `json:"source,omitempty"`
}

// Batch is one report. Every object of it was observed at CollectedAt, by the collector's clock.
type Batch struct {
	Version     int       `json:"version"`
	Cluster     string    `json:"cluster,omitempty"` // the cluster's name; the project's when empty
	CollectedAt time.Time `json:"collected_at"`
	Objects     []Object  `json:"objects,omitempty"`
	Deleted     []Ref     `json:"deleted,omitempty"`
	Sweeps      []Sweep   `json:"sweeps,omitempty"`
	Events      []Event   `json:"events,omitempty"`
}

// Validate says what is wrong with a batch, or nil. now is the service's clock.
func (b Batch) Validate(now time.Time) error {
	if b.Version != Version {
		return fmt.Errorf("version %d is not supported (this service reads version %d)", b.Version, Version)
	}
	if b.Cluster != "" && !ValidCluster(b.Cluster) {
		return fmt.Errorf("cluster must be lower-case letters, digits and . _ -, starting and ending with a letter or digit, at most 64 characters")
	}
	switch {
	case b.CollectedAt.IsZero():
		return fmt.Errorf("collected_at is required")
	case b.CollectedAt.After(now.Add(skew)):
		return fmt.Errorf("collected_at is in the future (more than %v ahead of this service's clock): is the collector's clock right?", skew)
	case b.CollectedAt.Before(now.Add(-maxAge)):
		return fmt.Errorf("collected_at is more than %v old", maxAge)
	}
	if len(b.Objects) > MaxObjects || len(b.Deleted) > MaxDeleted || len(b.Events) > MaxEvents {
		return fmt.Errorf("a batch holds at most %d objects, %d deletions and %d events", MaxObjects, MaxDeleted, MaxEvents)
	}
	seen := map[[2]string]bool{}
	for i, o := range b.Objects {
		if err := o.validate(now); err != nil {
			return fmt.Errorf("objects[%d] (%s %s): %w", i, o.Kind, o.Name, err)
		}
		k := [2]string{o.Kind, o.UID}
		if seen[k] {
			return fmt.Errorf("objects[%d]: %s %s is in the batch twice", i, o.Kind, o.UID)
		}
		seen[k] = true
	}
	for i, r := range b.Deleted {
		if !ValidKind(r.Kind) || !uidRE.MatchString(r.UID) {
			return fmt.Errorf("deleted[%d]: needs a known kind and a uid", i)
		}
		if (r.Namespace != "" && !nameRE.MatchString(r.Namespace)) || (r.Name != "" && !nameRE.MatchString(r.Name)) {
			return fmt.Errorf("deleted[%d]: namespace or name is not valid", i)
		}
	}
	swept := map[string]bool{}
	for i, s := range b.Sweeps {
		switch {
		case !ValidKind(s.Kind):
			return fmt.Errorf("sweeps[%d]: kind %q is not known", i, s.Kind)
		case swept[s.Kind]:
			return fmt.Errorf("sweeps[%d]: %s is swept twice", i, s.Kind)
		case s.Before.IsZero() || s.Before.After(b.CollectedAt):
			return fmt.Errorf("sweeps[%d]: before must be set and no later than collected_at", i)
		}
		swept[s.Kind] = true
	}
	for i, e := range b.Events {
		if err := e.validate(now); err != nil {
			return fmt.Errorf("events[%d]: %w", i, err)
		}
	}
	return nil
}

func (o Object) validate(now time.Time) error {
	if !ValidKind(o.Kind) {
		return fmt.Errorf("kind %q is not one of %s", o.Kind, strings.Join(Kinds, ", "))
	}
	if !uidRE.MatchString(o.UID) {
		return fmt.Errorf("uid is not valid")
	}
	if !nameRE.MatchString(o.Name) {
		return fmt.Errorf("name is not valid")
	}
	switch {
	case clusterScoped[o.Kind] && o.Namespace != "":
		return fmt.Errorf("a %s has no namespace", o.Kind)
	case !clusterScoped[o.Kind] && !nameRE.MatchString(o.Namespace):
		return fmt.Errorf("a %s needs a namespace", o.Kind)
	}
	if o.Created.After(now.Add(skew)) {
		return fmt.Errorf("created is in the future")
	}
	if len(o.Labels) > MaxLabels {
		return fmt.Errorf("at most %d labels", MaxLabels)
	}
	for k, v := range o.Labels {
		if k == "" || len(k) > 253 || len(v) > 253 || !utf8.ValidString(k) || !utf8.ValidString(v) || hasControl(k) || hasControl(v) {
			return fmt.Errorf("label %q is not valid (a key of 1 to 253 and a value of at most 253 characters, no control characters)", clip(k, 40))
		}
	}
	allowed := map[string]bool{}
	for _, a := range Attributes[o.Kind] {
		allowed[a] = true
	}
	var extra []string
	for k := range o.Data {
		if !allowed[k] {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return fmt.Errorf("a %s may not carry %s (it may carry: %s)", o.Kind, strings.Join(extra, ", "), strings.Join(Attributes[o.Kind], ", "))
	}
	raw, err := json.Marshal(o.Data)
	if err != nil {
		return fmt.Errorf("data cannot be encoded: %v", err)
	}
	if len(raw) > MaxObjectData {
		return fmt.Errorf("data is %d bytes; at most %d", len(raw), MaxObjectData)
	}
	for k, v := range o.Data {
		if err := checkValue(k, v, 1); err != nil {
			return err
		}
	}
	return nil
}

func (e Event) validate(now time.Time) error {
	switch {
	case !uidRE.MatchString(e.UID):
		return fmt.Errorf("uid is not valid")
	case e.Namespace != "" && !nameRE.MatchString(e.Namespace):
		return fmt.Errorf("namespace is not valid")
	case !kindNameRE.MatchString(e.Kind):
		return fmt.Errorf("kind is not valid")
	case !nameRE.MatchString(e.Name):
		return fmt.Errorf("name is not valid")
	case e.ObjectUID != "" && !uidRE.MatchString(e.ObjectUID):
		return fmt.Errorf("object_uid is not valid")
	case !typeRE.MatchString(e.Type):
		return fmt.Errorf("type is not valid")
	case !reasonRE.MatchString(e.Reason):
		return fmt.Errorf("reason is not valid")
	case len(e.Message) > MaxMessage || !utf8.ValidString(e.Message) || hasControl(strings.ReplaceAll(e.Message, "\n", " ")):
		return fmt.Errorf("message must be text of at most %d bytes", MaxMessage)
	case e.Count < 0 || e.Count > 1_000_000_000:
		return fmt.Errorf("count is not valid")
	case e.Last.IsZero() || e.Last.After(now.Add(skew)):
		return fmt.Errorf("last must be set and not in the future")
	case !e.First.IsZero() && e.First.After(e.Last):
		return fmt.Errorf("first is after last")
	case len(e.Source) > 100 || !utf8.ValidString(e.Source) || hasControl(e.Source):
		return fmt.Errorf("source is not valid")
	}
	return nil
}

// checkValue bounds an attribute: text, numbers, booleans, lists and maps of those, a few levels deep.
func checkValue(path string, v any, depth int) error {
	if depth > maxDepth {
		return fmt.Errorf("%s is nested more than %d levels", path, maxDepth)
	}
	switch x := v.(type) {
	case nil, bool, float64, int, int64, uint64:
		return nil
	case string:
		if len(x) > maxString || !utf8.ValidString(x) || hasControl(x) {
			return fmt.Errorf("%s: text of at most %d bytes, with no control characters", path, maxString)
		}
	case []any:
		if len(x) > maxList {
			return fmt.Errorf("%s has more than %d elements", path, maxList)
		}
		for _, e := range x {
			if err := checkValue(path, e, depth+1); err != nil {
				return err
			}
		}
	case []string:
		if len(x) > maxList {
			return fmt.Errorf("%s has more than %d elements", path, maxList)
		}
		for _, e := range x {
			if err := checkValue(path, e, depth+1); err != nil {
				return err
			}
		}
	case map[string]any:
		if len(x) > maxList {
			return fmt.Errorf("%s has more than %d keys", path, maxList)
		}
		for k, e := range x {
			if len(k) > maxKey || !utf8.ValidString(k) || hasControl(k) {
				return fmt.Errorf("%s has a key that is too long or not text", path)
			}
			if err := checkValue(path+"."+k, e, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%s has a value of a kind that is not allowed", path)
	}
	return nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
