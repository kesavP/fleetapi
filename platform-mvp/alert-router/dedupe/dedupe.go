// Package dedupe stops an unchanged firing alert from being forwarded again
// within a window. Alertmanager re-sends a whole group whenever one member
// changes, so without this every notification repeats its unchanged alerts.
package dedupe

import (
	"sync"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/alert-router/model"
)

// maxEntries bounds memory if fingerprints churn faster than they expire.
const maxEntries = 10000

type Deduper struct {
	TTL time.Duration // 0 disables deduplication
	Now func() time.Time

	mu        sync.Mutex
	seen      map[string]time.Time // fingerprint -> when it was last forwarded as firing
	lastSweep time.Time
}

func New(ttl time.Duration) *Deduper {
	return &Deduper{TTL: ttl, Now: time.Now, seen: map[string]time.Time{}}
}

// Filter returns the alerts that should be forwarded. A firing alert already
// forwarded within the TTL is dropped. A resolved alert is always forwarded
// and clears the entry, so the same alert firing again notifies again.
func (d *Deduper) Filter(alerts []model.Alert) (forward []model.Alert, dropped int) {
	if d.TTL <= 0 {
		return alerts, 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.Now()
	// Lookups below check the TTL themselves, so sweeping only bounds memory
	// and does not need to run on every call.
	if now.Sub(d.lastSweep) >= d.TTL/2 {
		d.expire(now)
		d.lastSweep = now
	}

	for _, a := range alerts {
		if a.Status == model.StatusResolved {
			delete(d.seen, a.Fingerprint)
			forward = append(forward, a)
			continue
		}
		if at, ok := d.seen[a.Fingerprint]; ok && now.Sub(at) < d.TTL {
			dropped++
			continue
		}
		if len(d.seen) < maxEntries {
			d.seen[a.Fingerprint] = now
		}
		forward = append(forward, a)
	}
	return forward, dropped
}

// Forget undoes Filter for alerts that were not actually delivered. Without
// it, Alertmanager's retry of a failed notification would be dropped as a
// repeat and the alert lost.
func (d *Deduper) Forget(alerts []model.Alert) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range alerts {
		if a.Status == model.StatusFiring {
			delete(d.seen, a.Fingerprint)
		}
	}
}

func (d *Deduper) expire(now time.Time) {
	for fp, at := range d.seen {
		if now.Sub(at) >= d.TTL {
			delete(d.seen, fp)
		}
	}
}
