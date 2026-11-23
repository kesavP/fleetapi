package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/inventory"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/summary"
)

// What an incident's instant summary reads (package summary). Each is bounded so that one summary is a handful of small queries.
const (
	summaryKubeLimit     = 500
	summaryHistoryLimit  = 200
	summaryProcessLimit  = 500
	summaryWorkloadLimit = 10
)

// SummaryKubeEvents is the Kubernetes events of a namespace that were live in [from, to].
func (s *Store) SummaryKubeEvents(ctx context.Context, project, namespace string, from, to time.Time) ([]summary.KubeEvent, error) {
	evs, err := s.ListInventoryEvents(ctx, InventoryEventQuery{Project: project, Namespace: namespace, From: from, To: to, Limit: summaryKubeLimit})
	if err != nil {
		return nil, err
	}
	out := make([]summary.KubeEvent, 0, len(evs))
	for _, e := range evs {
		out = append(out, summary.KubeEvent{Namespace: e.Namespace, Kind: e.Kind, Name: e.Name, Type: e.Type, Reason: e.Reason, Message: e.Message,
			Count: e.Count, First: e.First, Last: e.Last})
	}
	return out, nil
}

type workloadData struct {
	Generation int64 `json:"generation"`
	Containers []struct {
		Image string `json:"image"`
	} `json:"containers"`
}

func imagesOf(d workloadData) []string {
	out := make([]string, 0, len(d.Containers))
	for _, c := range d.Containers {
		if c.Image != "" {
			out = append(out, c.Image)
		}
	}
	return out
}

// SummaryRollouts is the spec changes of the workload namespace/name (whichever of Deployment, StatefulSet and DaemonSet it is, in any cluster)
// in [from, to]: each version whose generation went up from the version before it.
func (s *Store) SummaryRollouts(ctx context.Context, project, namespace, name string, from, to time.Time) ([]summary.Rollout, error) {
	objs, err := s.ListInventory(ctx, InventoryQuery{Project: project, Kinds: []string{inventory.Deployment, inventory.StatefulSet, inventory.DaemonSet},
		Namespace: namespace, Name: name, Limit: summaryWorkloadLimit})
	if err != nil {
		return nil, err
	}
	var out []summary.Rollout
	for _, o := range objs {
		versions, err := s.InventoryHistory(ctx, project, o.Cluster, o.Kind, o.UID, summaryHistoryLimit)
		if err != nil {
			return nil, err
		}
		out = append(out, rolloutsOf(o.Kind, namespace, name, versions, from, to)...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// rolloutsOf finds the versions (newest first, as InventoryHistory gives them) whose generation went up, in [from, to].
func rolloutsOf(kind, namespace, name string, versions []InventoryVersion, from, to time.Time) []summary.Rollout {
	var out []summary.Rollout
	var prev *workloadData
	for i := len(versions) - 1; i >= 0; i-- {
		v := versions[i]
		if v.Deleted {
			prev = nil
			continue
		}
		var d workloadData
		if json.Unmarshal(v.Data, &d) != nil {
			continue
		}
		if prev != nil && d.Generation > prev.Generation && !v.Version.Before(from) && !v.Version.After(to) {
			out = append(out, summary.Rollout{Kind: kind, Namespace: namespace, Name: name, At: v.Version, Generation: d.Generation,
				Before: imagesOf(*prev), After: imagesOf(d)})
		}
		prev = &d
	}
	return out
}

// SummaryProcesses is the process lineage fleet-node-agent exported for a service (an application id) in [from, to]: log records with
// log.source=process.
func (s *Store) SummaryProcesses(ctx context.Context, project, service string, from, to time.Time) ([]summary.ProcessEvent, error) {
	rows, err := s.conn.Query(ctx, fmt.Sprintf(`SELECT Timestamp, ResourceAttributes, LogAttributes FROM logs_records
WHERE Project = ? AND ServiceName = ? AND Timestamp >= fromUnixTimestamp64Nano(?) AND Timestamp <= fromUnixTimestamp64Nano(?)
 AND LogAttributes[?] = ? ORDER BY Timestamp LIMIT %d`, summaryProcessLimit),
		project, service, from.UnixNano(), to.UnixNano(), summary.AttrLogSource, summary.ProcessSource)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []summary.ProcessEvent
	for rows.Next() {
		var ts time.Time
		var res, attrs map[string]string
		if err := rows.Scan(&ts, &res, &attrs); err != nil {
			return nil, err
		}
		if p, ok := processEventOf(ts, res, attrs); ok {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

func processEventOf(ts time.Time, res, attrs map[string]string) (summary.ProcessEvent, bool) {
	kind := attrs[summary.AttrEvent]
	if kind == "" {
		return summary.ProcessEvent{}, false
	}
	container := attrs[summary.AttrContainer]
	if container == "" {
		container = res[summary.AttrContainer]
	}
	code, _ := strconv.Atoi(attrs[summary.AttrExitCode])
	count, _ := strconv.Atoi(attrs[summary.AttrCount])
	return summary.ProcessEvent{At: ts.UTC(), Kind: kind, Container: container, Exe: attrs[summary.AttrExe], ParentExe: attrs[summary.AttrParentExe],
		ExitCode: code, Signal: attrs[summary.AttrSignal], Count: count}, true
}
