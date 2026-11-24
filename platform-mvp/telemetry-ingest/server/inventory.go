package server

import (
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"regexp"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/inventory"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/store"
)

// InventoryWriter stores what a cluster collector sent.
type InventoryWriter interface {
	WriteInventoryBatch(ctx context.Context, project string, b inventory.Batch) error
}

// 500 objects of up to 16 KiB each, as JSON, is what a full batch can be.
const maxInventoryBody = 12 << 20

// inventory takes one report from a cluster collector (POST /v1/inventory, with the project's ingest key). The batch is checked before
// anything is stored (inventory.Batch.Validate); a batch that is refused is a 400 that says why, and a failure to store it is a 500, so the
// collector sends it again.
func (s *Ingest) inventory(project string, w http.ResponseWriter, r *http.Request) error {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return httpError{http.StatusUnsupportedMediaType, "send application/json"}
	}
	// Not the operator's --max-body-bytes (sized for the other signals): a full batch has to fit, and 500 objects are bounded by the checks below.
	const limit = maxInventoryBody
	raw, err := readLimited(w, r, limit)
	if err != nil {
		return err
	}
	var b inventory.Batch
	if err := json.Unmarshal(raw, &b); err != nil {
		return badRequest("the body is not a batch: %s", clip(err.Error(), 200))
	}
	if err := b.Validate(time.Now()); err != nil {
		return badRequest("%v", err)
	}
	if err := s.Inventory.WriteInventoryBatch(r.Context(), project, b); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]int{"objects": len(b.Objects), "deleted": len(b.Deleted), "sweeps": len(b.Sweeps), "events": len(b.Events)}})
	return nil
}

// ---- reading the inventory ----

var textParamRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,252}$`)

// textParam reads an optional name-like parameter: empty when absent, an error when it could not be a name.
func textParam(r *http.Request, name string) (string, error) {
	v := r.URL.Query().Get(name)
	if v != "" && !textParamRE.MatchString(v) {
		return "", paramError(name + " is not a valid name")
	}
	return v, nil
}

func clusterParam(r *http.Request) (string, error) {
	v := r.URL.Query().Get("cluster")
	if v != "" && !inventory.ValidCluster(v) {
		return "", paramError("cluster is not a valid cluster name")
	}
	return v, nil
}

func (q *Query) inventorySummary(ctx context.Context, project string, _ http.ResponseWriter, _ *http.Request) (any, error) {
	return q.Reader.InventorySummary(ctx, project)
}

func (q *Query) inventoryList(ctx context.Context, project string, _ http.ResponseWriter, r *http.Request) (any, error) {
	kinds := inventory.KindsOfPath(r.PathValue("kind"))
	if kinds == nil {
		return nil, paramError("kind must be one of nodes, namespaces, workloads, deployments, statefulsets, daemonsets, cronjobs, pods, pvcs, hpas, scaledobjects")
	}
	cluster, err := clusterParam(r)
	if err != nil {
		return nil, err
	}
	ns, err := textParam(r, "namespace")
	if err != nil {
		return nil, err
	}
	name, err := textParam(r, "name")
	if err != nil {
		return nil, err
	}
	limit, err := intParam(r, "limit", 200, 1, 1000)
	if err != nil {
		return nil, err
	}
	return q.Reader.ListInventory(ctx, store.InventoryQuery{Project: project, Cluster: cluster, Kinds: kinds, Namespace: ns, Name: name, Limit: limit})
}

func (q *Query) inventoryHistory(ctx context.Context, project string, _ http.ResponseWriter, r *http.Request) (any, error) {
	kinds := inventory.KindsOfPath(r.PathValue("kind"))
	if len(kinds) != 1 {
		return nil, paramError("history is of one kind: nodes, namespaces, deployments, statefulsets, daemonsets, cronjobs, pods, pvcs, hpas or scaledobjects")
	}
	uid := r.PathValue("uid")
	if !textParamRE.MatchString(uid) {
		return nil, paramError("uid is not valid")
	}
	cluster, err := clusterParam(r)
	if err != nil {
		return nil, err
	}
	limit, err := intParam(r, "limit", 50, 1, 200)
	if err != nil {
		return nil, err
	}
	return q.Reader.InventoryHistory(ctx, project, cluster, kinds[0], uid, limit)
}

var kindNameParamRE = regexp.MustCompile(`^[A-Za-z]{1,63}$`)

func (q *Query) inventoryEvents(ctx context.Context, project string, _ http.ResponseWriter, r *http.Request) (any, error) {
	from, to, err := q.timeRange(r)
	if err != nil {
		return nil, err
	}
	cluster, err := clusterParam(r)
	if err != nil {
		return nil, err
	}
	ns, err := textParam(r, "namespace")
	if err != nil {
		return nil, err
	}
	name, err := textParam(r, "name")
	if err != nil {
		return nil, err
	}
	reason, err := textParam(r, "reason")
	if err != nil {
		return nil, err
	}
	kind := r.URL.Query().Get("kind")
	if kind != "" && !kindNameParamRE.MatchString(kind) {
		return nil, paramError("kind is not valid (the kind of the object the event is about, such as Pod)")
	}
	typ := r.URL.Query().Get("type")
	if typ != "" && typ != "Normal" && typ != "Warning" {
		return nil, paramError("type must be Normal or Warning")
	}
	limit, err := intParam(r, "limit", 100, 1, 1000)
	if err != nil {
		return nil, err
	}
	return q.Reader.ListInventoryEvents(ctx, store.InventoryEventQuery{Project: project, Cluster: cluster, Namespace: ns, Kind: kind, Name: name, Type: typ, Reason: reason, From: from, To: to, Limit: limit})
}
