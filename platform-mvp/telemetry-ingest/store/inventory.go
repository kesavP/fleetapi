package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/inventory"
)

// CurrentWindow is how recent an object's newest version must be for the object to count as in the cluster. A collector re-sends everything
// now and then (its resync, by default every half hour), so an object it has stopped reporting for a day is one nobody is looking after: the
// collector is gone, or the cluster is.
const CurrentWindow = 24 * time.Hour

// InventoryObject is an object as it stands: the newest version of it.
type InventoryObject struct {
	Cluster   string            `json:"cluster"`
	Kind      string            `json:"kind"`
	UID       string            `json:"uid"`
	Namespace string            `json:"namespace,omitempty"`
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels"`
	Created   time.Time         `json:"created"`
	Observed  time.Time         `json:"observed"` // when the collector last reported it
	Data      json.RawMessage   `json:"data"`
}

// InventoryVersion is one version of an object, or the record that it was deleted.
type InventoryVersion struct {
	Version   time.Time       `json:"version"`
	Deleted   bool            `json:"deleted"`
	Namespace string          `json:"namespace,omitempty"`
	Name      string          `json:"name"`
	Data      json.RawMessage `json:"data"`
}

// InventoryEvent is a Kubernetes event as it stands: its newest count and message.
type InventoryEvent struct {
	Cluster   string    `json:"cluster"`
	UID       string    `json:"uid"`
	Namespace string    `json:"namespace,omitempty"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	ObjectUID string    `json:"object_uid,omitempty"`
	Type      string    `json:"type"`
	Reason    string    `json:"reason"`
	Message   string    `json:"message"`
	Count     uint32    `json:"count"`
	First     time.Time `json:"first"`
	Last      time.Time `json:"last"`
	Source    string    `json:"source,omitempty"`
}

// InventoryCluster is what a project has heard from one collector, and what it holds from it.
type InventoryCluster struct {
	Cluster       string         `json:"cluster"`
	LastReceived  time.Time      `json:"last_received"`  // when the service last heard from the collector, by its own clock
	LastCollected time.Time      `json:"last_collected"` // when the collector said it had looked, by its clock
	Counts        map[string]int `json:"counts"`         // objects now, by kind
}

type InventoryQuery struct {
	Project, Cluster string
	Kinds            []string
	Namespace, Name  string
	Limit            int
}

type InventoryEventQuery struct {
	Project, Cluster, Namespace, Kind, Name, Type, Reason string
	From, To                                              time.Time
	Limit                                                 int
}

func tombstoneVersion(at time.Time) time.Time { return at.Add(time.Millisecond) }

// WriteInventoryBatch stores what a cluster collector sent (inventory.Batch.Validate has passed). Objects go in as new versions, deletions and
// sweeps as rows that say Deleted, events as rows, and last a row for the report itself, which is what tells how recently the collector was
// heard from. A failure part way is a failure of the request, so the collector sends the batch again: that repeats the rows already written,
// which are the same rows (same versions), so what is current does not change.
func (s *Store) WriteInventoryBatch(ctx context.Context, project string, b inventory.Batch) error {
	cluster := b.Cluster
	if cluster == "" {
		cluster = project
	}
	at := b.CollectedAt.UTC()
	err := insert(ctx, s, "INSERT INTO inventory_objects (Project, Cluster, Kind, UID, Namespace, Name, Version, Deleted, Created, Labels, Data)", b.Objects,
		func(o inventory.Object) []any {
			data, _ := json.Marshal(nonNilData(o.Data))
			return []any{project, cluster, o.Kind, o.UID, o.Namespace, o.Name, at, uint8(0), o.Created.UTC(), mp(o.Labels), string(data)}
		})
	if err != nil {
		return err
	}
	gone := b.Deleted
	// A sweep: what is current in the cluster for that kind, and was last reported before the cut, is gone. This runs after the objects above
	// are in, so what this batch re-sent is not swept (its versions are at the cut, not before it).
	for _, sw := range b.Sweeps {
		refs, err := s.sweepCandidates(ctx, project, cluster, sw)
		if err != nil {
			return err
		}
		gone = append(gone, refs...)
	}
	err = insert(ctx, s, "INSERT INTO inventory_objects (Project, Cluster, Kind, UID, Namespace, Name, Version, Deleted, Created, Labels, Data)", gone,
		func(r inventory.Ref) []any {
			return []any{project, cluster, r.Kind, r.UID, r.Namespace, r.Name, tombstoneVersion(at), uint8(1), time.Unix(0, 0).UTC(), map[string]string{}, "{}"}
		})
	if err != nil {
		return err
	}
	err = insert(ctx, s, "INSERT INTO inventory_events (Project, Cluster, UID, Namespace, ObjectKind, ObjectName, ObjectUID, Type, Reason, Message, Count, FirstTime, LastTime, Source)", b.Events,
		func(e inventory.Event) []any {
			first := e.First
			if first.IsZero() {
				first = e.Last
			}
			return []any{project, cluster, e.UID, e.Namespace, e.Kind, e.Name, e.ObjectUID, e.Type, e.Reason, e.Message, uint32(e.Count), first.UTC(), e.Last.UTC(), e.Source}
		})
	if err != nil {
		return err
	}
	type report struct{ received time.Time }
	return insert(ctx, s, "INSERT INTO inventory_batches (Project, Cluster, ReceivedAt, CollectedAt, Objects, Deleted, Events, Sweeps)", []report{{time.Now().UTC()}},
		func(r report) []any {
			return []any{project, cluster, r.received, at, uint32(len(b.Objects)), uint32(len(gone)), uint32(len(b.Events)), uint32(len(b.Sweeps))}
		})
}

func nonNilData(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// sweepCandidates are the objects of a kind that are current and whose newest version is older than the sweep's cut.
func (s *Store) sweepCandidates(ctx context.Context, project, cluster string, sw inventory.Sweep) ([]inventory.Ref, error) {
	rows, err := s.conn.Query(ctx, `SELECT UID, argMax(Namespace, Version), argMax(Name, Version) FROM inventory_objects
WHERE Project = ? AND Cluster = ? AND Kind = ?
GROUP BY UID
HAVING argMax(Deleted, Version) = 0 AND max(Version) < fromUnixTimestamp64Milli(?)`, project, cluster, sw.Kind, sw.Before.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []inventory.Ref
	for rows.Next() {
		r := inventory.Ref{Kind: sw.Kind}
		if err := rows.Scan(&r.UID, &r.Namespace, &r.Name); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// kindList is a quoted list of kinds for an IN clause. They are checked against the fixed list, so nothing but a known kind is spliced in.
func kindList(kinds []string) (string, error) {
	q := make([]string, 0, len(kinds))
	for _, k := range kinds {
		if !inventory.ValidKind(k) {
			return "", fmt.Errorf("unknown kind %q", k)
		}
		q = append(q, "'"+k+"'")
	}
	if len(q) == 0 {
		return "", fmt.Errorf("no kind")
	}
	return strings.Join(q, ","), nil
}

// ListInventory is the objects of the given kinds that are current.
func (s *Store) ListInventory(ctx context.Context, q InventoryQuery) ([]InventoryObject, error) {
	kinds, err := kindList(q.Kinds)
	if err != nil {
		return nil, err
	}
	query := fmt.Sprintf(`SELECT Cluster, Kind, UID, argMax(Namespace, Version), argMax(Name, Version), argMax(Labels, Version), argMax(Created, Version), max(Version), argMax(Data, Version)
FROM inventory_objects
WHERE Project = ? AND Kind IN (%s) AND Version >= fromUnixTimestamp64Milli(?)`, kinds)
	args := []any{q.Project, time.Now().Add(-CurrentWindow).UnixMilli()}
	if q.Cluster != "" {
		query += " AND Cluster = ?"
		args = append(args, q.Cluster)
	}
	query += " GROUP BY Cluster, Kind, UID HAVING argMax(Deleted, Version) = 0"
	if q.Namespace != "" {
		query += " AND argMax(Namespace, Version) = ?"
		args = append(args, q.Namespace)
	}
	if q.Name != "" {
		query += " AND argMax(Name, Version) = ?"
		args = append(args, q.Name)
	}
	query += fmt.Sprintf(" ORDER BY Cluster, Kind, argMax(Namespace, Version), argMax(Name, Version) LIMIT %d", q.Limit)
	rows, err := s.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InventoryObject{}
	for rows.Next() {
		var o InventoryObject
		var data string
		if err := rows.Scan(&o.Cluster, &o.Kind, &o.UID, &o.Namespace, &o.Name, &o.Labels, &o.Created, &o.Observed, &data); err != nil {
			return nil, err
		}
		o.Created, o.Observed = o.Created.UTC(), o.Observed.UTC()
		o.Data = rawJSON(data)
		out = append(out, o)
	}
	return out, rows.Err()
}

func rawJSON(s string) json.RawMessage {
	if !json.Valid([]byte(s)) {
		return json.RawMessage("{}")
	}
	return json.RawMessage(s)
}

// InventoryHistory is the versions of one object, newest first: what changed in it and when, and the record of its deletion.
func (s *Store) InventoryHistory(ctx context.Context, project, cluster, kind, uid string, limit int) ([]InventoryVersion, error) {
	if !inventory.ValidKind(kind) {
		return nil, fmt.Errorf("unknown kind %q", kind)
	}
	query := `SELECT Version, Deleted, Namespace, Name, Data FROM inventory_objects WHERE Project = ? AND Kind = ? AND UID = ?`
	args := []any{project, kind, uid}
	if cluster != "" {
		query += " AND Cluster = ?"
		args = append(args, cluster)
	}
	// the same version can be stored twice (a batch that was sent again): it is one version
	query += fmt.Sprintf(" GROUP BY Version, Deleted, Namespace, Name, Data ORDER BY Version DESC LIMIT %d", limit)
	rows, err := s.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InventoryVersion{}
	for rows.Next() {
		var v InventoryVersion
		var del uint8
		var data string
		if err := rows.Scan(&v.Version, &del, &v.Namespace, &v.Name, &data); err != nil {
			return nil, err
		}
		v.Version, v.Deleted, v.Data = v.Version.UTC(), del == 1, rawJSON(data)
		out = append(out, v)
	}
	return out, rows.Err()
}

// ListInventoryEvents is the events matching the query, newest first, each as it last stood.
func (s *Store) ListInventoryEvents(ctx context.Context, q InventoryEventQuery) ([]InventoryEvent, error) {
	query := `SELECT Cluster, UID, argMax(Namespace, LastTime), argMax(ObjectKind, LastTime), argMax(ObjectName, LastTime), argMax(ObjectUID, LastTime),
 argMax(Type, LastTime), argMax(Reason, LastTime), argMax(Message, LastTime), argMax(Count, LastTime), min(FirstTime), max(LastTime), argMax(Source, LastTime)
FROM inventory_events WHERE Project = ? AND LastTime >= fromUnixTimestamp64Milli(?) AND LastTime <= fromUnixTimestamp64Milli(?)`
	args := []any{q.Project, ms(q.From), ms(q.To)}
	for _, f := range []struct{ col, val string }{
		{"Cluster", q.Cluster}, {"Namespace", q.Namespace}, {"ObjectKind", q.Kind}, {"ObjectName", q.Name}, {"Type", q.Type}, {"Reason", q.Reason},
	} {
		if f.val != "" {
			query += " AND " + f.col + " = ?"
			args = append(args, f.val)
		}
	}
	query += fmt.Sprintf(" GROUP BY Cluster, UID ORDER BY max(LastTime) DESC, UID LIMIT %d", q.Limit)
	rows, err := s.conn.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InventoryEvent{}
	for rows.Next() {
		var e InventoryEvent
		if err := rows.Scan(&e.Cluster, &e.UID, &e.Namespace, &e.Kind, &e.Name, &e.ObjectUID, &e.Type, &e.Reason, &e.Message, &e.Count, &e.First, &e.Last, &e.Source); err != nil {
			return nil, err
		}
		e.First, e.Last = e.First.UTC(), e.Last.UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// InventorySummary is, for each cluster a collector has reported, when it last did and what is in it now.
func (s *Store) InventorySummary(ctx context.Context, project string) ([]InventoryCluster, error) {
	rows, err := s.conn.Query(ctx, `SELECT Cluster, max(ReceivedAt), max(CollectedAt) FROM inventory_batches WHERE Project = ? GROUP BY Cluster ORDER BY Cluster`, project)
	if err != nil {
		return nil, err
	}
	var out []InventoryCluster
	idx := map[string]int{}
	for rows.Next() {
		c := InventoryCluster{Counts: map[string]int{}}
		if err := rows.Scan(&c.Cluster, &c.LastReceived, &c.LastCollected); err != nil {
			rows.Close()
			return nil, err
		}
		c.LastReceived, c.LastCollected = c.LastReceived.UTC(), c.LastCollected.UTC()
		idx[c.Cluster] = len(out)
		out = append(out, c)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	counts, err := s.conn.Query(ctx, `SELECT Cluster, Kind, count() FROM (
 SELECT Cluster, Kind, UID FROM inventory_objects WHERE Project = ? AND Version >= fromUnixTimestamp64Milli(?)
 GROUP BY Cluster, Kind, UID HAVING argMax(Deleted, Version) = 0) GROUP BY Cluster, Kind`, project, time.Now().Add(-CurrentWindow).UnixMilli())
	if err != nil {
		return nil, err
	}
	defer counts.Close()
	for counts.Next() {
		var cluster, kind string
		var n uint64
		if err := counts.Scan(&cluster, &kind, &n); err != nil {
			return nil, err
		}
		if i, ok := idx[cluster]; ok {
			out[i].Counts[kind] = int(n)
		}
	}
	if out == nil {
		out = []InventoryCluster{}
	}
	return out, counts.Err()
}
