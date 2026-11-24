//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/inventory"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/model"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/summary"
)

// The instant summary's three sources on what ClickHouse holds: a rollout of shop/checkout (generation 4 -> 5, new image), the Kubernetes events
// of its namespace, and the process lineage fleet-node-agent exports as log records.
func TestSummarySourcesInClickHouse(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	t0 := time.Now().UTC().Truncate(time.Millisecond).Add(-20 * time.Minute)
	dep := func(generation int, image string) inventory.Object {
		return inventory.Object{Kind: inventory.Deployment, UID: "dep-1", Namespace: "shop", Name: "checkout", Created: t0.Add(-time.Hour),
			Data: map[string]any{"desired": 2, "ready": 2, "generation": generation, "containers": []any{map[string]any{"name": "app", "image": image}}}}
	}
	send := func(b inventory.Batch) {
		t.Helper()
		if err := b.Validate(time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := r.st.WriteInventoryBatch(ctx, "us", b); err != nil {
			t.Fatal(err)
		}
	}
	send(inventory.Batch{Version: 1, Cluster: "gke", CollectedAt: t0, Objects: []inventory.Object{dep(4, "shop/checkout:1.4")}})
	send(inventory.Batch{Version: 1, Cluster: "gke", CollectedAt: t0.Add(5 * time.Minute), Objects: []inventory.Object{dep(5, "shop/checkout:1.5")}})
	send(inventory.Batch{Version: 1, Cluster: "gke", CollectedAt: t0.Add(6 * time.Minute), Events: []inventory.Event{
		{UID: "e1", Namespace: "shop", Kind: "Pod", Name: "checkout-7d9f8-x2bq4", Type: "Warning", Reason: "BackOff", Message: "Back-off", Count: 3, First: t0.Add(6 * time.Minute), Last: t0.Add(8 * time.Minute)},
		{UID: "e2", Namespace: "other", Kind: "Pod", Name: "x-1", Type: "Warning", Reason: "BackOff", Message: "Back-off", Count: 1, First: t0.Add(6 * time.Minute), Last: t0.Add(6 * time.Minute)},
	}})

	rollouts, err := r.st.SummaryRollouts(ctx, "us", "shop", "checkout", t0.Add(-time.Minute), time.Now())
	if err != nil || len(rollouts) != 1 || rollouts[0].Generation != 5 || rollouts[0].Before[0] != "shop/checkout:1.4" || rollouts[0].After[0] != "shop/checkout:1.5" ||
		!rollouts[0].At.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("rollouts %+v %v", rollouts, err)
	}
	if none, err := r.st.SummaryRollouts(ctx, "eu", "shop", "checkout", t0.Add(-time.Minute), time.Now()); err != nil || len(none) != 0 {
		t.Fatalf("eu sees us's rollouts: %+v %v", none, err)
	}

	kube, err := r.st.SummaryKubeEvents(ctx, "us", "shop", t0, time.Now())
	if err != nil || len(kube) != 1 || kube[0].Reason != "BackOff" || kube[0].Count != 3 || kube[0].Name != "checkout-7d9f8-x2bq4" {
		t.Fatalf("kube %+v %v", kube, err)
	}

	proc := func(at time.Time, service, source string, attrs map[string]string) model.LogRecord {
		la := map[string]string{summary.AttrLogSource: source}
		for k, v := range attrs {
			la[k] = v
		}
		return model.LogRecord{Project: "us", Time: at, SeverityText: "INFO", SeverityNumber: 9, Service: service, Body: "process event",
			ResourceAttrs: map[string]string{summary.AttrContainer: "/k8s/shop/checkout-7d9f8-x2bq4/app"}, LogAttrs: la}
	}
	if err := r.st.WriteLogs(ctx, []model.LogRecord{
		proc(t0.Add(7*time.Minute), "/k8s/shop/checkout", summary.ProcessSource, map[string]string{summary.AttrEvent: summary.ProcessOOMKill, summary.AttrExe: "/usr/bin/java"}),
		proc(t0.Add(8*time.Minute), "/k8s/shop/checkout", summary.ProcessSource, map[string]string{summary.AttrEvent: summary.ProcessExitFailed, summary.AttrExe: "/app/worker", summary.AttrExitCode: "2"}),
		proc(t0.Add(8*time.Minute), "/k8s/shop/checkout", "stdout", map[string]string{summary.AttrEvent: "not a process record"}),          // an application log
		proc(t0.Add(8*time.Minute), "/k8s/shop/cart", summary.ProcessSource, map[string]string{summary.AttrEvent: summary.ProcessOOMKill}), // another service
	}); err != nil {
		t.Fatal(err)
	}
	procs, err := r.st.SummaryProcesses(ctx, "us", "/k8s/shop/checkout", t0, time.Now())
	if err != nil || len(procs) != 2 || procs[0].Kind != summary.ProcessOOMKill || procs[0].Container != "/k8s/shop/checkout-7d9f8-x2bq4/app" ||
		procs[1].ExitCode != 2 || procs[1].Exe != "/app/worker" {
		t.Fatalf("processes %+v %v", procs, err)
	}
}
