package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/inventory"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/store"
)

type fakeInvWriter struct {
	mu      sync.Mutex
	err     error
	project []string
	batches []inventory.Batch
}

func (f *fakeInvWriter) WriteInventoryBatch(_ context.Context, project string, b inventory.Batch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.project = append(f.project, project)
	f.batches = append(f.batches, b)
	return nil
}

func invIngest(t *testing.T, w InventoryWriter, maxBody int64) (*httptest.Server, *Counters) {
	t.Helper()
	mux := http.NewServeMux()
	c := NewCounters()
	(&Ingest{Projects: registry(t), Sink: &fakeSink{}, MaxBody: maxBody, Log: quiet, Count: c, Inventory: w}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, c
}

func goodInventory() inventory.Batch {
	return inventory.Batch{
		Version: inventory.Version, Cluster: "gke", CollectedAt: time.Now().Add(-time.Second),
		Objects: []inventory.Object{{Kind: inventory.Deployment, UID: "dep-uid-1", Namespace: "shop", Name: "cartservice", Created: time.Now().Add(-time.Hour),
			Data: map[string]any{"desired": 0, "ready": 0}}},
		Events: []inventory.Event{{UID: "ev1", Namespace: "shop", Kind: "Deployment", Name: "cartservice", Type: "Normal", Reason: "ScalingReplicaSet", Message: "Scaled down replica set to 0", Count: 1, Last: time.Now()}},
	}
}

func postInv(t *testing.T, srv *httptest.Server, key string, body any, headers ...string) (int, string) {
	t.Helper()
	var raw []byte
	if s, ok := body.(string); ok {
		raw = []byte(s)
	} else {
		raw, _ = json.Marshal(body)
	}
	req, _ := http.NewRequest("POST", srv.URL+"/v1/inventory", bytes.NewReader(raw))
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i+1] == "-" {
			req.Header.Del(headers[i])
		} else {
			req.Header.Set(headers[i], headers[i+1])
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(res.Body)
	return res.StatusCode, buf.String()
}

func TestInventoryIsStoredForTheKeysProject(t *testing.T) {
	w := &fakeInvWriter{}
	srv, c := invIngest(t, w, 1<<20)
	code, body := postInv(t, srv, usIngest, goodInventory())
	if code != 200 || !strings.Contains(body, `"objects":1`) || !strings.Contains(body, `"events":1`) {
		t.Fatalf("%d %s", code, body)
	}
	if len(w.batches) != 1 || w.project[0] != "us" || w.batches[0].Objects[0].Name != "cartservice" {
		t.Fatalf("%+v %v", w.batches, w.project)
	}
	// the project is the key's, whatever the body says
	var m map[string]any
	raw, _ := json.Marshal(goodInventory())
	_ = json.Unmarshal(raw, &m)
	m["project"] = "eu"
	if code, _ := postInv(t, srv, usIngest, m); code != 200 || w.project[1] != "us" {
		t.Fatalf("%d %v", code, w.project)
	}
	if !strings.Contains(metricsText(c), `telemetry_ingest_requests_total{signal="inventory",code="200"} 2`) {
		t.Fatalf("%s", metricsText(c))
	}
}

func TestInventoryNeedsAnIngestKey(t *testing.T) {
	w := &fakeInvWriter{}
	srv, _ := invIngest(t, w, 1<<20)
	for name, key := range map[string]string{"none": "", "a read key": usRead, "an unknown key": "nope-nope-nope-nope-0"} {
		if code, _ := postInv(t, srv, key, goodInventory()); code != 401 {
			t.Errorf("%s: %d", name, code)
		}
	}
	if len(w.batches) != 0 {
		t.Fatal("something was stored without a key")
	}
}

func TestInventoryThatIsNotValidIsRefusedAndNothingIsStored(t *testing.T) {
	w := &fakeInvWriter{}
	srv, _ := invIngest(t, w, 1<<20)
	mutate := func(f func(*inventory.Batch)) inventory.Batch { b := goodInventory(); f(&b); return b }
	for name, c := range map[string]struct {
		body any
		want string
	}{
		"not JSON":         {"{nope", "not a batch"},
		"wrong version":    {mutate(func(b *inventory.Batch) { b.Version = 9 }), "version 9"},
		"clock ahead":      {mutate(func(b *inventory.Batch) { b.CollectedAt = time.Now().Add(time.Hour) }), "future"},
		"annotations sent": {mutate(func(b *inventory.Batch) { b.Objects[0].Data["annotations"] = map[string]any{"a": "b"} }), "may not carry annotations"},
		"a secret kind":    {mutate(func(b *inventory.Batch) { b.Objects[0].Kind = "secret" }), "not one of"},
		"bad cluster":      {mutate(func(b *inventory.Batch) { b.Cluster = "Prod!" }), "cluster"},
	} {
		code, body := postInv(t, srv, usIngest, c.body)
		if code != 400 || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	if len(w.batches) != 0 {
		t.Fatalf("%d batches stored", len(w.batches))
	}
}

func TestInventoryNeedsJSON(t *testing.T) {
	srv, _ := invIngest(t, &fakeInvWriter{}, 1<<20)
	for _, ct := range []string{"text/plain", "application/x-protobuf", "-"} {
		if code, _ := postInv(t, srv, usIngest, goodInventory(), "Content-Type", ct); code != 415 {
			t.Errorf("%q: %d", ct, code)
		}
	}
	if code, _ := postInv(t, srv, usIngest, goodInventory(), "Content-Type", "application/json; charset=utf-8"); code != 200 {
		t.Errorf("with a charset: %d", code)
	}
}

func TestAHeartbeatIsAnEmptyBatch(t *testing.T) {
	w := &fakeInvWriter{}
	srv, _ := invIngest(t, w, 1<<20)
	if code, body := postInv(t, srv, usIngest, inventory.Batch{Version: inventory.Version, Cluster: "gke", CollectedAt: time.Now()}); code != 200 || len(w.batches) != 1 {
		t.Fatalf("%d %s %d", code, body, len(w.batches))
	}
}

func TestAFullBatchFitsEvenWhenTheOperatorsBodyLimitIsSmall(t *testing.T) {
	// --max-body-bytes is sized for the other signals; a collector's batch of a few hundred objects is bigger than a small limit.
	w := &fakeInvWriter{}
	srv, _ := invIngest(t, w, 1<<10)
	b := goodInventory()
	for i := 0; i < 50; i++ {
		o := b.Objects[0]
		o.UID = "dep-uid-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		o.Name = "svc-" + o.UID
		b.Objects = append(b.Objects, o)
	}
	if code, body := postInv(t, srv, usIngest, b); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
}

func TestInventoryStorageFailureIsRetriedAndLeaksNothing(t *testing.T) {
	w := &fakeInvWriter{err: errors.New("clickhouse at 10.0.0.9:9000 refused the password hunter2")}
	srv, _ := invIngest(t, w, 1<<20)
	code, body := postInv(t, srv, usIngest, goodInventory())
	if code != 500 || strings.Contains(body, "hunter2") || strings.Contains(body, "10.0.0.9") {
		t.Fatalf("%d %s", code, body)
	}
}

func TestTheInventoryEndpointIsOnlyThereWhenThereIsAPlaceToStore(t *testing.T) {
	mux := http.NewServeMux()
	(&Ingest{Projects: registry(t), Sink: &fakeSink{}, MaxBody: 1 << 20, Log: quiet, Count: NewCounters()}).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if code, _ := postInv(t, srv, usIngest, goodInventory()); code != 404 && code != 405 {
		t.Fatalf("%d", code)
	}
}

// ---- reading ----

type fakeInvReader struct {
	store.Reader
	mu      sync.Mutex
	project string
	list    store.InventoryQuery
	events  store.InventoryEventQuery
	hist    struct {
		cluster, kind, uid string
		limit              int
	}
}

func (f *fakeInvReader) InventorySummary(_ context.Context, p string) ([]store.InventoryCluster, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.project = p
	return []store.InventoryCluster{{Cluster: "gke", LastReceived: time.Unix(1700000000, 0).UTC(), Counts: map[string]int{"pod": 12}}}, nil
}
func (f *fakeInvReader) ListInventory(_ context.Context, q store.InventoryQuery) ([]store.InventoryObject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.list = q
	return []store.InventoryObject{{Cluster: "gke", Kind: "deployment", UID: "u", Namespace: "shop", Name: "cartservice", Data: json.RawMessage(`{"desired":0}`)}}, nil
}
func (f *fakeInvReader) InventoryHistory(_ context.Context, p, cluster, kind, uid string, limit int) ([]store.InventoryVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.project = p
	f.hist.cluster, f.hist.kind, f.hist.uid, f.hist.limit = cluster, kind, uid, limit
	return []store.InventoryVersion{{Version: time.Unix(1700000000, 0).UTC(), Name: "cartservice", Data: json.RawMessage(`{"desired":3}`)}}, nil
}
func (f *fakeInvReader) ListInventoryEvents(_ context.Context, q store.InventoryEventQuery) ([]store.InventoryEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = q
	return []store.InventoryEvent{{Cluster: "gke", UID: "e", Kind: "Pod", Name: "x", Type: "Warning", Reason: "BackOff", Count: 4}}, nil
}

func TestInventoryReadsAreScopedToTheKeysProject(t *testing.T) {
	fr := &fakeInvReader{}
	srv := dbQueryServer(t, fr)
	base := srv.URL + "/api/v1/projects/"
	for name, c := range map[string]struct {
		path, key string
		want      int
	}{
		"no key":          {"us/inventory", "", 401},
		"ingest key":      {"us/inventory", usIngest, 403},
		"another project": {"us/inventory/pods", euRead, 403},
		"unknown project": {"nosuch/inventory/events", usRead, 403},
	} {
		if code, _ := get(t, base+c.path, c.key); code != c.want {
			t.Errorf("%s: %d", name, code)
		}
	}
	if fr.project != "" || fr.list.Project != "" {
		t.Fatal("the reader was asked for a rejected request")
	}
	code, body := get(t, base+"us/inventory", usRead)
	if code != 200 || fr.project != "us" || !strings.Contains(body, `"cluster":"gke"`) || !strings.Contains(body, `"pod":12`) {
		t.Fatalf("%d %s", code, body)
	}
}

func TestInventoryListParameters(t *testing.T) {
	fr := &fakeInvReader{}
	srv := dbQueryServer(t, fr)
	base := srv.URL + "/api/v1/projects/us/inventory/"
	if code, body := get(t, base+"workloads?cluster=gke&namespace=shop&name=cartservice&limit=5", usRead); code != 200 || !strings.Contains(body, `"desired":0`) {
		t.Fatalf("%d %s", code, body)
	}
	q := fr.list
	if q.Project != "us" || q.Cluster != "gke" || q.Namespace != "shop" || q.Name != "cartservice" || q.Limit != 5 || len(q.Kinds) != 4 {
		t.Fatalf("%+v", q)
	}
	if code, _ := get(t, base+"pods", usRead); code != 200 || fr.list.Limit != 200 || len(fr.list.Kinds) != 1 || fr.list.Kinds[0] != "pod" {
		t.Fatalf("the defaults: %+v", fr.list)
	}
	for name, path := range map[string]string{
		"an unknown kind": "secrets",
		"a bad cluster":   "pods?cluster=A%20B",
		"a bad namespace": "pods?namespace=a%3Bb",
		"a limit too big": "pods?limit=100000",
		"a limit of zero": "pods?limit=0",
		"a name with a /": "pods?name=a%2Fb",
	} {
		if code, _ := get(t, base+path, usRead); code != 400 {
			t.Errorf("%s: %d", name, code)
		}
	}
}

func TestInventoryHistoryAndEvents(t *testing.T) {
	fr := &fakeInvReader{}
	srv := dbQueryServer(t, fr)
	base := srv.URL + "/api/v1/projects/us/inventory/"
	if code, body := get(t, base+"deployments/dep-uid-1/history?cluster=gke&limit=7", usRead); code != 200 || !strings.Contains(body, `"desired":3`) {
		t.Fatalf("%d %s", code, body)
	}
	if fr.hist.kind != "deployment" || fr.hist.uid != "dep-uid-1" || fr.hist.cluster != "gke" || fr.hist.limit != 7 {
		t.Fatalf("%+v", fr.hist)
	}
	if code, _ := get(t, base+"workloads/dep-uid-1/history", usRead); code != 400 {
		t.Errorf("history of a union of kinds: %d", code)
	}
	if code, _ := get(t, base+"deployments/a%20b/history", usRead); code != 400 {
		t.Errorf("a bad uid: %d", code)
	}
	if code, body := get(t, base+"events?from=1700000000&to=1700003600&namespace=shop&kind=Pod&name=web&type=Warning&reason=BackOff&limit=3&cluster=gke", usRead); code != 200 || !strings.Contains(body, `"reason":"BackOff"`) {
		t.Fatalf("%d %s", code, body)
	}
	e := fr.events
	if e.Project != "us" || e.Cluster != "gke" || e.Namespace != "shop" || e.Kind != "Pod" || e.Name != "web" || e.Type != "Warning" || e.Reason != "BackOff" || e.Limit != 3 || e.To.Sub(e.From) != time.Hour {
		t.Fatalf("%+v", e)
	}
	for name, path := range map[string]string{"a type": "events?type=Error", "a kind": "events?kind=Pod%3B", "a reason": "events?reason=a%20b"} {
		if code, _ := get(t, base+path, usRead); code != 400 {
			t.Errorf("%s: %d", name, code)
		}
	}
}
