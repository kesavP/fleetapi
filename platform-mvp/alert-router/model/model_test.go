package model

import (
	"strings"
	"testing"
	"time"
)

const goodPayload = `{
  "version": "4", "groupKey": "{}:{alertname=\"VmSetNotReady\"}", "status": "firing", "receiver": "alert-router",
  "groupLabels": {"alertname": "VmSetNotReady"}, "commonLabels": {}, "commonAnnotations": {}, "externalURL": "http://am",
  "alerts": [
    {"status": "firing", "fingerprint": "bbb", "startsAt": "2026-10-28T10:00:00Z", "endsAt": "0001-01-01T00:00:00Z",
     "labels": {"alertname": "VmSetNotReady", "severity": "warning", "tenant": "acme-corp", "vmset": "web", "region": "us"},
     "annotations": {"summary": "VmSet web is not ready in us", "description": "stayed Pending"}},
    {"status": "resolved", "fingerprint": "aaa", "startsAt": "2026-10-28T09:00:00Z", "endsAt": "2026-10-28T09:30:00Z",
     "labels": {"alertname": "KubePodCrashLooping", "namespace": "default"}, "annotations": {}},
    {"status": "firing", "fingerprint": "ccc", "startsAt": "2026-10-28T10:05:00Z", "endsAt": "0001-01-01T00:00:00Z",
     "labels": {"alertname": "SpokeAPIUnreachable", "severity": "critical", "cluster": "eu"}, "annotations": {}}
  ]
}`

func TestParseAndNormalize(t *testing.T) {
	w, err := Parse(strings.NewReader(goodPayload))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := Normalize(w)
	if len(got) != 3 {
		t.Fatalf("alerts = %d, want 3", len(got))
	}
	// Ordered by alert name.
	if got[0].Name != "KubePodCrashLooping" || got[1].Name != "SpokeAPIUnreachable" || got[2].Name != "VmSetNotReady" {
		t.Errorf("order = %s, %s, %s", got[0].Name, got[1].Name, got[2].Name)
	}
	vm := got[2]
	if vm.Tenant != "acme-corp" || vm.VmSet != "web" || vm.Region != "us" || vm.Severity != "warning" ||
		vm.Summary != "VmSet web is not ready in us" || vm.Status != StatusFiring {
		t.Errorf("VmSet alert = %+v", vm)
	}
	if !vm.EndsAt.IsZero() {
		t.Errorf("a firing alert has no end, got %v", vm.EndsAt)
	}
	if !got[0].EndsAt.Equal(time.Date(2026, 10, 28, 9, 30, 0, 0, time.UTC)) {
		t.Errorf("a resolved alert keeps its end time, got %v", got[0].EndsAt)
	}
	if got[0].Tenant != "" || got[0].VmSet != "" {
		t.Errorf("non-VmSet alerts have no tenant or vmset: %+v", got[0])
	}
	if got[1].Cluster != "eu" || got[1].Severity != "critical" {
		t.Errorf("cluster alert = %+v", got[1])
	}
}

func TestParseRejectsInvalidPayloads(t *testing.T) {
	alert := func(mutate string) string {
		return `{"version":"4","alerts":[{"status":"firing","fingerprint":"f","labels":{"alertname":"A"}` + mutate + `}]}`
	}
	tests := map[string]string{
		"not json":            `{`,
		"wrong version":       `{"version":"3","alerts":[{"status":"firing","fingerprint":"f","labels":{"alertname":"A"}}]}`,
		"no alerts":           `{"version":"4","alerts":[]}`,
		"unknown status":      `{"version":"4","alerts":[{"status":"weird","fingerprint":"f","labels":{"alertname":"A"}}]}`,
		"missing alertname":   `{"version":"4","alerts":[{"status":"firing","fingerprint":"f","labels":{}}]}`,
		"missing fingerprint": `{"version":"4","alerts":[{"status":"firing","labels":{"alertname":"A"}}]}`,
	}
	_ = alert
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(strings.NewReader(body)); err == nil {
				t.Error("expected an error")
			}
		})
	}
}
