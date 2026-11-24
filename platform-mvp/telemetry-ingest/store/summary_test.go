package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/summary"
)

func version(at time.Time, generation int64, images ...string) InventoryVersion {
	cs := make([]map[string]string, 0, len(images))
	for _, im := range images {
		cs = append(cs, map[string]string{"name": "app", "image": im})
	}
	data, _ := json.Marshal(map[string]any{"generation": generation, "ready": 2, "containers": cs})
	return InventoryVersion{Version: at, Name: "checkout", Data: data}
}

func TestRolloutsOfFindsGenerationChangesInTheWindow(t *testing.T) {
	t0 := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	m := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }
	// newest first, as InventoryHistory returns them; in time order:
	versions := []InventoryVersion{
		version(m(50), 9, "app:3"),      // generation 8 -> 9, but after the window
		version(m(20), 8, "app:2"),      // 7 -> 8: a rollout
		version(m(15), 7, "app:1"),      // created again after the deletion: nothing to compare with, not a rollout
		{Version: m(12), Deleted: true}, // deleted
		version(m(11), 7, "app:1"),      // a status change only (generation unchanged): not a rollout
		version(m(10), 7, "app:1"),      // 6 -> 7: a rollout
		version(m(5), 6, "app:0"),       // 5 -> 6: a rollout (the spec changed, not the image)
		version(m(-100), 5, "app:0"),    // before the window
	}
	got := rolloutsOf("deployment", "shop", "checkout", versions, m(0), m(40))
	if len(got) != 3 {
		t.Fatalf("%+v", got)
	}
	want := []struct {
		at            int
		gen           int64
		before, after string
	}{{5, 6, "app:0", "app:0"}, {10, 7, "app:0", "app:1"}, {20, 8, "app:1", "app:2"}}
	for i, w := range want {
		g := got[i]
		if !g.At.Equal(m(w.at)) || g.Generation != w.gen || g.Before[0] != w.before || g.After[0] != w.after || g.Kind != "deployment" {
			t.Errorf("rollout %d: %+v, want %+v", i, g, w)
		}
	}
}

func TestProcessEventOf(t *testing.T) {
	ts := time.Date(2026, 11, 23, 10, 0, 0, 0, time.FixedZone("x", 3600))
	p, ok := processEventOf(ts, map[string]string{summary.AttrContainer: "/k8s/shop/checkout-1/app"}, map[string]string{
		summary.AttrEvent: summary.ProcessExitFailed, summary.AttrExe: "/app/worker", summary.AttrExitCode: "137", summary.AttrSignal: "SIGKILL",
	})
	if !ok || p.Kind != summary.ProcessExitFailed || p.ExitCode != 137 || p.Signal != "SIGKILL" || p.Container != "/k8s/shop/checkout-1/app" || p.At.Location() != time.UTC {
		t.Fatalf("%+v", p)
	}
	if _, ok := processEventOf(ts, nil, map[string]string{"log.source": "process"}); ok {
		t.Fatal("a record without an event kind is not a process event")
	}
}
