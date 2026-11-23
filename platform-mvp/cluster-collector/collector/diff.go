// Package collector decides what to send and sends it: it compares each look at the cluster with what it last reported, sends only what
// changed (and everything now and then, so that a report lost for good is repaired), and tells the service what has gone.
package collector

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/cluster-collector/kube"
	"github.com/sojoner/kro-lab/platform-mvp/cluster-collector/wire"
)

// EventKey stands for events in a snapshot's Listed and Problems, beside the object kinds.
const EventKey = "event"

type key [2]string // kind, uid

type known struct {
	hash string
	ns   string
	name string
}

type eventSig struct {
	count int
	last  time.Time
}

// Differ remembers what has been reported. It changes only when a Plan is committed, which the Runner does after every batch of the plan was
// accepted: a plan that failed part way is made again, from the same memory, on the next look.
type Differ struct {
	objects map[key]known
	events  map[string]eventSig
}

func NewDiffer() *Differ {
	return &Differ{objects: map[key]known{}, events: map[string]eventSig{}}
}

// Plan is what to report for one look.
type Plan struct {
	Objects []wire.Object
	Deleted []wire.Ref
	Events  []wire.Event
	Sweeps  []wire.Sweep // only in a full resync, for the kinds that were read completely
	Full    bool

	objects map[key]known
	events  map[string]eventSig
}

// Empty reports whether the plan has nothing to say.
func (p *Plan) Empty() bool {
	return len(p.Objects)+len(p.Deleted)+len(p.Events)+len(p.Sweeps) == 0
}

func hash(o wire.Object) string {
	raw, _ := json.Marshal(struct {
		Namespace, Name string
		Labels          map[string]string
		Data            map[string]any
	}{o.Namespace, o.Name, o.Labels, o.Data})
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:12])
}

// Plan compares a snapshot with what was reported. at is the time of the look. With full, every object is included and the kinds that were
// read completely are swept: the service then drops anything of those kinds that was not in this report.
func (d *Differ) Plan(s kube.Snapshot, at time.Time, full bool) *Plan {
	p := &Plan{Full: full, objects: map[key]known{}, events: map[string]eventSig{}}
	// what is known of a kind that could not be read is kept as it is
	for k, v := range d.objects {
		if !s.Listed[k[0]] {
			p.objects[k] = v
		}
	}
	for _, o := range s.Objects {
		k := key{o.Kind, o.UID}
		h := hash(o)
		p.objects[k] = known{hash: h, ns: o.Namespace, name: o.Name}
		if prev, ok := d.objects[k]; full || !ok || prev.hash != h {
			p.Objects = append(p.Objects, o)
		}
	}
	for k, v := range d.objects {
		if !s.Listed[k[0]] {
			continue
		}
		if _, still := p.objects[k]; !still {
			p.Deleted = append(p.Deleted, wire.Ref{Kind: k[0], UID: k[1], Namespace: v.ns, Name: v.name})
		}
	}
	sort.Slice(p.Deleted, func(i, j int) bool {
		a, b := p.Deleted[i], p.Deleted[j]
		return a.Kind+"/"+a.UID < b.Kind+"/"+b.UID
	})
	if full {
		for _, kind := range wire.Kinds {
			if s.Listed[kind] {
				p.Sweeps = append(p.Sweeps, wire.Sweep{Kind: kind, Before: at})
			}
		}
	}

	if s.Listed[EventKey] {
		for _, e := range s.Events {
			sig := eventSig{e.Count, e.Last}
			p.events[e.UID] = sig
			if prev, ok := d.events[e.UID]; !ok || prev != sig {
				p.Events = append(p.Events, e)
			}
		}
	} else {
		for k, v := range d.events {
			p.events[k] = v
		}
	}
	return p
}

// Commit records that the plan was reported.
func (d *Differ) Commit(p *Plan) {
	d.objects, d.events = p.objects, p.events
}

// maxBytes bounds a batch's objects, well under what the service reads (12 MiB).
const maxBytes = 6 << 20

// Batches cuts a plan into the requests to make, in order: every one is stamped with the time of the look (that is what a sweep is
// measured against), the sweeps ride on the last, and a plan with nothing in it is one empty batch: the collector saying it is alive.
func (p *Plan) Batches(cluster string, at time.Time) []wire.Batch {
	var out []wire.Batch
	fresh := func() wire.Batch { return wire.Batch{Version: wire.Version, Cluster: cluster, CollectedAt: at.UTC()} }
	cur, size := fresh(), 0
	flush := func() {
		if len(cur.Objects)+len(cur.Deleted)+len(cur.Events) > 0 {
			out = append(out, cur)
		}
		cur, size = fresh(), 0
	}
	for _, o := range p.Objects {
		raw, _ := json.Marshal(o)
		if len(cur.Objects) >= wire.MaxObjects || size+len(raw) > maxBytes {
			flush()
		}
		cur.Objects = append(cur.Objects, o)
		size += len(raw)
	}
	for _, r := range p.Deleted {
		if len(cur.Deleted) >= wire.MaxDeleted {
			flush()
		}
		cur.Deleted = append(cur.Deleted, r)
	}
	for _, e := range p.Events {
		if len(cur.Events) >= wire.MaxEvents {
			flush()
		}
		cur.Events = append(cur.Events, e)
	}
	switch {
	case len(cur.Objects)+len(cur.Deleted)+len(cur.Events) > 0:
		cur.Sweeps = p.Sweeps
		out = append(out, cur)
	case len(out) > 0:
		out[len(out)-1].Sweeps = p.Sweeps
	default:
		cur.Sweeps = p.Sweeps
		out = append(out, cur) // a heartbeat
	}
	return out
}
