package inventory

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func pod() Object {
	return Object{
		Kind: Pod, UID: "0a1b2c3d-0000-4000-8000-000000000001", Namespace: "shop", Name: "web-5d6f8c7b9-xk2lp",
		Labels:  map[string]string{"app": "web"},
		Created: now.Add(-time.Hour),
		Data: map[string]any{
			"phase": "Running", "node": "node-a", "ready": true, "restarts": 2,
			"containers": []any{map[string]any{"name": "app", "image": "shop/web:1.2", "ready": true, "restarts": 2,
				"requests": map[string]any{"cpu_m": 100, "memory_bytes": 134217728}}},
		},
	}
}

func good() Batch {
	return Batch{
		Version: Version, Cluster: "gke", CollectedAt: now,
		Objects: []Object{pod(), {Kind: Node, UID: "node-uid-1", Name: "node-a", Created: now.Add(-24 * time.Hour), Data: map[string]any{"ready": true, "kubelet": "v1.31.2"}}},
		Deleted: []Ref{{Kind: Pod, UID: "gone-uid", Namespace: "shop", Name: "old"}},
		Sweeps:  []Sweep{{Kind: Pod, Before: now}},
		Events:  []Event{{UID: "ev-1", Namespace: "shop", Kind: "Pod", Name: "web-5d6f8c7b9-xk2lp", Type: "Warning", Reason: "BackOff", Message: "Back-off restarting failed container", Count: 3, First: now.Add(-time.Minute), Last: now}},
	}
}

func TestAGoodBatchIsAccepted(t *testing.T) {
	if err := good().Validate(now); err != nil {
		t.Fatal(err)
	}
}

func TestEveryKindHasAttributesAndAPath(t *testing.T) {
	for _, k := range Kinds {
		if len(Attributes[k]) == 0 {
			t.Errorf("%s has no attribute list", k)
		}
		if !ValidKind(k) {
			t.Errorf("%s is not valid", k)
		}
	}
	if len(Attributes) != len(Kinds) {
		t.Errorf("Attributes has %d kinds, Kinds %d", len(Attributes), len(Kinds))
	}
	for seg, want := range map[string]string{"pods": Pod, "nodes": Node, "pvcs": PVC, "hpas": HPA, "scaledobjects": ScaledObject, "deployments": Deployment} {
		if got := KindsOfPath(seg); len(got) != 1 || got[0] != want {
			t.Errorf("%s -> %v", seg, got)
		}
	}
	if got := KindsOfPath("workloads"); len(got) != 4 {
		t.Errorf("workloads -> %v", got)
	}
	if KindsOfPath("secrets") != nil || KindsOfPath("") != nil {
		t.Error("an unknown segment must give nothing")
	}
}

func TestBatchRefusals(t *testing.T) {
	cases := map[string]struct {
		mut  func(*Batch)
		want string
	}{
		"version":           {func(b *Batch) { b.Version = 2 }, "version 2"},
		"no version":        {func(b *Batch) { b.Version = 0 }, "version 0"},
		"cluster":           {func(b *Batch) { b.Cluster = "Prod Cluster" }, "cluster must be"},
		"no time":           {func(b *Batch) { b.CollectedAt = time.Time{} }, "collected_at is required"},
		"future":            {func(b *Batch) { b.CollectedAt = now.Add(time.Hour) }, "in the future"},
		"old":               {func(b *Batch) { b.CollectedAt = now.Add(-72 * time.Hour); b.Sweeps = nil }, "old"},
		"too many objects":  {func(b *Batch) { b.Objects = make([]Object, MaxObjects+1) }, "at most"},
		"too many events":   {func(b *Batch) { b.Events = make([]Event, MaxEvents+1) }, "at most"},
		"duplicate object":  {func(b *Batch) { b.Objects = append(b.Objects, pod()) }, "twice"},
		"bad kind":          {func(b *Batch) { b.Objects[0].Kind = "secret" }, "not one of"},
		"bad uid":           {func(b *Batch) { b.Objects[0].UID = "has space" }, "uid"},
		"empty name":        {func(b *Batch) { b.Objects[0].Name = "" }, "name"},
		"pod needs ns":      {func(b *Batch) { b.Objects[0].Namespace = "" }, "needs a namespace"},
		"node has no ns":    {func(b *Batch) { b.Objects[1].Namespace = "shop" }, "no namespace"},
		"created in future": {func(b *Batch) { b.Objects[0].Created = now.Add(time.Hour) }, "created"},
		"deleted kind":      {func(b *Batch) { b.Deleted[0].Kind = "x" }, "deleted[0]"},
		"deleted uid":       {func(b *Batch) { b.Deleted[0].UID = "" }, "deleted[0]"},
		"sweep kind":        {func(b *Batch) { b.Sweeps[0].Kind = "x" }, "not known"},
		"sweep twice":       {func(b *Batch) { b.Sweeps = append(b.Sweeps, b.Sweeps[0]) }, "twice"},
		"sweep after batch": {func(b *Batch) { b.Sweeps[0].Before = now.Add(time.Second) }, "no later than"},
		"sweep no time":     {func(b *Batch) { b.Sweeps[0].Before = time.Time{} }, "before must be set"},
		"event reason":      {func(b *Batch) { b.Events[0].Reason = "bad reason!" }, "reason"},
		"event type":        {func(b *Batch) { b.Events[0].Type = "" }, "type"},
		"event last":        {func(b *Batch) { b.Events[0].Last = time.Time{} }, "last"},
		"event first":       {func(b *Batch) { b.Events[0].First = b.Events[0].Last.Add(time.Minute) }, "first is after last"},
		"event message":     {func(b *Batch) { b.Events[0].Message = strings.Repeat("x", MaxMessage+1) }, "message"},
		"event control":     {func(b *Batch) { b.Events[0].Message = "a\x00b" }, "message"},
		"event bad utf8":    {func(b *Batch) { b.Events[0].Message = "a\xffb" }, "message"},
		"event count":       {func(b *Batch) { b.Events[0].Count = -1 }, "count"},
		"event kind":        {func(b *Batch) { b.Events[0].Kind = "Pod; DROP" }, "kind"},
	}
	for name, c := range cases {
		b := good()
		c.mut(&b)
		err := b.Validate(now)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, c.want)
		}
	}
}

// What a collector may send is decided here: an attribute that is not on the kind's list is refused, whatever it is called.
func TestAnAttributeNotOnTheListIsRefused(t *testing.T) {
	for _, attr := range []string{"annotations", "env", "command", "args", "secret", "password", "token", "volumes", "spec", "metadata", "status"} {
		b := good()
		b.Objects[0].Data[attr] = "x"
		err := b.Validate(now)
		if err == nil || !strings.Contains(err.Error(), attr) || !strings.Contains(err.Error(), "may not carry") {
			t.Errorf("%s: err = %v", attr, err)
		}
	}
}

func TestAnAttributeOfAnotherKindIsRefused(t *testing.T) {
	b := good()
	b.Objects[0].Data["desired"] = 3 // a deployment's, not a pod's
	if err := b.Validate(now); err == nil {
		t.Fatal("a pod must not carry a deployment's attribute")
	}
}

func TestDataIsBounded(t *testing.T) {
	deep := map[string]any{"a": map[string]any{"b": map[string]any{"c": map[string]any{"d": 1}}}}
	cases := map[string]struct {
		set  func(map[string]any)
		want string
	}{
		"nested too deep": {func(d map[string]any) { d["conditions"] = deep }, "nested"},
		"long text":       {func(d map[string]any) { d["reason"] = strings.Repeat("x", maxString+1) }, "text of at most"},
		"control in text": {func(d map[string]any) { d["reason"] = "a\nb" }, "control"},
		"long list":       {func(d map[string]any) { d["containers"] = make([]any, maxList+1) }, "elements"},
		"long key":        {func(d map[string]any) { d["conditions"] = map[string]any{strings.Repeat("k", maxKey+1): "x"} }, "key"},
		"too many keys": {func(d map[string]any) {
			m := map[string]any{}
			for i := 0; i <= maxList; i++ {
				m[string(rune('a'+i%26))+strings.Repeat("z", i/26)] = 1
			}
			d["conditions"] = m
		}, "keys"},
		"too big": {func(d map[string]any) {
			l := make([]any, 0)
			for i := 0; i < 40; i++ {
				l = append(l, strings.Repeat("y", 500))
			}
			d["containers"] = l
		}, "bytes"},
		"a kind not allowed": {func(d map[string]any) { d["reason"] = struct{ X int }{1} }, "not allowed"},
	}
	for name, c := range cases {
		b := good()
		c.set(b.Objects[0].Data)
		err := b.Validate(now)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

func TestLabelsAreBounded(t *testing.T) {
	for name, labels := range map[string]map[string]string{
		"empty key":  {"": "x"},
		"long value": {"a": strings.Repeat("v", 254)},
		"control":    {"a": "b\x01"},
	} {
		b := good()
		b.Objects[0].Labels = labels
		if err := b.Validate(now); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	b := good()
	b.Objects[0].Labels = map[string]string{}
	for i := 0; i <= MaxLabels; i++ {
		b.Objects[0].Labels[strings.Repeat("l", i+1)] = "v"
	}
	if err := b.Validate(now); err == nil || !strings.Contains(err.Error(), "labels") {
		t.Errorf("too many labels: %v", err)
	}
}

// A collector's JSON comes back with numbers as float64 and lists as []any: that is what the service validates.
func TestABatchSurvivesJSON(t *testing.T) {
	raw, err := json.Marshal(good())
	if err != nil {
		t.Fatal(err)
	}
	var b Batch
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	if err := b.Validate(now); err != nil {
		t.Fatal(err)
	}
	if got := b.Objects[0].Data["restarts"]; got != float64(2) {
		t.Fatalf("restarts = %#v", got)
	}
}

func TestAnEmptyBatchIsAHeartbeat(t *testing.T) {
	b := Batch{Version: Version, CollectedAt: now}
	if err := b.Validate(now); err != nil {
		t.Fatalf("an empty batch (a collector saying it is alive) must be accepted: %v", err)
	}
}

func TestValidCluster(t *testing.T) {
	for s, want := range map[string]bool{"gke": true, "prod-eu.1": true, "a": true, "": false, "-a": false, "A": false, "a b": false, strings.Repeat("a", 65): false} {
		if ValidCluster(s) != want {
			t.Errorf("ValidCluster(%q) = %v", s, !want)
		}
	}
}
