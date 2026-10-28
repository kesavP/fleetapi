// Package model is the router's vocabulary: Alertmanager's webhook payload
// and the flat Alert the sinks consume. It does no I/O; every function is
// deterministic.
package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"
)

// WebhookVersion is the only Alertmanager webhook payload version the router
// understands.
const WebhookVersion = "4"

const (
	StatusFiring   = "firing"
	StatusResolved = "resolved"
)

// Webhook is Alertmanager's webhook_config payload (version 4).
type Webhook struct {
	Version           string            `json:"version"`
	GroupKey          string            `json:"groupKey"`
	Status            string            `json:"status"`
	Receiver          string            `json:"receiver"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
	Alerts            []WebhookAlert    `json:"alerts"`
}

type WebhookAlert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

// Alert is one alert reduced to what a sink needs. Tenant, VmSet and Region
// are empty for alerts that do not come from the VmSet rules, and Cluster for
// those that do not concern a particular cluster.
type Alert struct {
	Name        string
	Status      string
	Severity    string
	Tenant      string
	VmSet       string
	Region      string
	Cluster     string
	Summary     string
	Description string
	Fingerprint string
	StartsAt    time.Time
	EndsAt      time.Time // zero while firing
	Labels      map[string]string
}

// Parse reads and validates a webhook payload.
func Parse(r io.Reader) (Webhook, error) {
	var w Webhook
	dec := json.NewDecoder(r)
	if err := dec.Decode(&w); err != nil {
		return Webhook{}, fmt.Errorf("invalid JSON: %w", err)
	}
	if w.Version != WebhookVersion {
		return Webhook{}, fmt.Errorf("unsupported webhook version %q, want %q", w.Version, WebhookVersion)
	}
	if len(w.Alerts) == 0 {
		return Webhook{}, errors.New("payload has no alerts")
	}
	for i, a := range w.Alerts {
		if a.Status != StatusFiring && a.Status != StatusResolved {
			return Webhook{}, fmt.Errorf("alert %d has unknown status %q", i, a.Status)
		}
		if a.Labels["alertname"] == "" {
			return Webhook{}, fmt.Errorf("alert %d has no alertname", i)
		}
		if a.Fingerprint == "" {
			return Webhook{}, fmt.Errorf("alert %d has no fingerprint", i)
		}
	}
	return w, nil
}

// Normalize flattens a payload into alerts, ordered by name then fingerprint
// so the same payload always produces the same output.
func Normalize(w Webhook) []Alert {
	out := make([]Alert, 0, len(w.Alerts))
	for _, a := range w.Alerts {
		al := Alert{
			Name:        a.Labels["alertname"],
			Status:      a.Status,
			Severity:    a.Labels["severity"],
			Tenant:      a.Labels["tenant"],
			VmSet:       a.Labels["vmset"],
			Region:      a.Labels["region"],
			Cluster:     a.Labels["cluster"],
			Summary:     a.Annotations["summary"],
			Description: a.Annotations["description"],
			Fingerprint: a.Fingerprint,
			StartsAt:    a.StartsAt,
			Labels:      a.Labels,
		}
		if a.Status == StatusResolved {
			al.EndsAt = a.EndsAt
		}
		out = append(out, al)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Fingerprint < out[j].Fingerprint
	})
	return out
}
