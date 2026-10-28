package dedupe

import (
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/alert-router/model"
)

func firing(fp string) model.Alert { return model.Alert{Fingerprint: fp, Status: model.StatusFiring} }
func resolved(fp string) model.Alert {
	return model.Alert{Fingerprint: fp, Status: model.StatusResolved}
}

func clocked(ttl time.Duration) (*Deduper, *time.Time) {
	now := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	d := New(ttl)
	d.Now = func() time.Time { return now }
	return d, &now
}

func TestRepeatedFiringAlertIsDroppedWithinTTL(t *testing.T) {
	d, now := clocked(10 * time.Minute)

	if fwd, dropped := d.Filter([]model.Alert{firing("a")}); len(fwd) != 1 || dropped != 0 {
		t.Fatalf("first sighting: forwarded %d, dropped %d", len(fwd), dropped)
	}
	*now = now.Add(5 * time.Minute)
	if fwd, dropped := d.Filter([]model.Alert{firing("a"), firing("b")}); len(fwd) != 1 || fwd[0].Fingerprint != "b" || dropped != 1 {
		t.Errorf("repeat within TTL: forwarded %v, dropped %d; want only the new alert b", fwd, dropped)
	}
}

func TestFiringAlertIsForwardedAgainAfterTTL(t *testing.T) {
	d, now := clocked(10 * time.Minute)
	d.Filter([]model.Alert{firing("a")})
	*now = now.Add(10 * time.Minute)
	if fwd, _ := d.Filter([]model.Alert{firing("a")}); len(fwd) != 1 {
		t.Error("an alert still firing after the TTL should be forwarded again")
	}
}

func TestResolvedAlwaysPassesAndRearmsTheAlert(t *testing.T) {
	d, _ := clocked(10 * time.Minute)
	d.Filter([]model.Alert{firing("a")})
	if fwd, dropped := d.Filter([]model.Alert{resolved("a")}); len(fwd) != 1 || dropped != 0 {
		t.Fatalf("resolved: forwarded %d, dropped %d", len(fwd), dropped)
	}
	if fwd, _ := d.Filter([]model.Alert{firing("a")}); len(fwd) != 1 {
		t.Error("an alert that fires again after resolving must notify again, even inside the TTL")
	}
}

func TestZeroTTLDisablesDedupe(t *testing.T) {
	d, _ := clocked(0)
	d.Filter([]model.Alert{firing("a")})
	if fwd, dropped := d.Filter([]model.Alert{firing("a")}); len(fwd) != 1 || dropped != 0 {
		t.Errorf("forwarded %d, dropped %d; want everything forwarded", len(fwd), dropped)
	}
}

func TestMemoryIsBounded(t *testing.T) {
	d, _ := clocked(time.Hour)
	for i := 0; i < maxEntries+50; i++ {
		d.Filter([]model.Alert{firing(string(rune('a'+i%26)) + time.Duration(i).String())})
	}
	if len(d.seen) > maxEntries {
		t.Errorf("tracked %d fingerprints, cap is %d", len(d.seen), maxEntries)
	}
}

func TestForgetLetsARetryThrough(t *testing.T) {
	d, _ := clocked(10 * time.Minute)
	fwd, _ := d.Filter([]model.Alert{firing("a")})
	d.Forget(fwd) // delivery failed, so Alertmanager will retry
	if again, dropped := d.Filter([]model.Alert{firing("a")}); len(again) != 1 || dropped != 0 {
		t.Errorf("retry after a failed delivery: forwarded %d, dropped %d; want it forwarded", len(again), dropped)
	}
}
