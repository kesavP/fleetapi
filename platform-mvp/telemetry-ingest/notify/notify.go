// Package notify tells people, outside the UI, that the hub's detector has opened an incident: it posts one alert to Alertmanager, which sends it on (to
// Slack, in the chart's configuration).
//
// An incident is an EVENT here, not a condition: nothing in this service resolves one, so the alert is sent once, when the incident is opened, with an end
// time an hour ahead so that it expires by itself and is not repeated for ever, and the receiver is told not to say "resolved". Which incidents are worth a
// message is a severity: warning and critical by default, so the information-only ones (an over-provisioned container) never are.
//
// Telling is best effort: a failure is logged and nothing else happens, and it never holds up the detector.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/detect"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/incident"
)

// Notifier is told of each incident the detector opens.
type Notifier interface {
	Opened(ctx context.Context, project string, inc incident.Incident)
}

// Alertmanager posts an opened incident to Alertmanager's API (POST /api/v2/alerts).
type Alertmanager struct {
	URL string // Alertmanager's address, with no path
	// UIURL, when it is an http(s) address, makes each alert link to the incident's page in the UI: <UIURL>/recorded/<id>.
	UIURL       string
	MinSeverity incident.Severity // the lowest severity that is told; empty is warning
	Client      *http.Client
	Log         *slog.Logger
	Timeout     time.Duration // for one post; 5 seconds when zero
	Now         func() time.Time
}

// rank orders the severities; one that is not set, or not known, ranks as a warning, which is the default minimum.
func rank(s incident.Severity) int {
	switch s {
	case incident.Critical:
		return 2
	case incident.Warning:
		return 1
	case incident.Info:
		return 0
	}
	return 1
}

func (a *Alertmanager) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

// kindOf says which detector opened the incident, from its dedupe key.
func kindOf(key string) string {
	switch {
	case strings.HasPrefix(key, "silence:"):
		return "silence"
	case strings.HasPrefix(key, "deps:"):
		return "dependency"
	case strings.HasPrefix(key, "rightsize:"):
		return "rightsize"
	}
	return "burn"
}

const maxDescription = 800

func cut(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > maxDescription {
		return strings.TrimSpace(string(r[:maxDescription-1])) + "…"
	}
	return s
}

// link is the incident's page, or "" when there is no usable address for the UI.
func (a *Alertmanager) link(id string) string {
	u, err := url.Parse(strings.TrimSpace(a.UIURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return strings.TrimRight(u.String(), "/") + "/recorded/" + id
}

type alert struct {
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     string            `json:"startsAt"`
	EndsAt       string            `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL,omitempty"`
}

// Opened posts the incident, when it is one to tell: the detector's own, at or above MinSeverity. It returns when the post is done or has failed.
func (a *Alertmanager) Opened(ctx context.Context, project string, inc incident.Incident) {
	if a.URL == "" || inc.Source != incident.Detected {
		return
	}
	if rank(inc.Severity) < rank(a.MinSeverity) {
		return
	}
	now := a.now()
	starts := inc.StartedAt.UTC()
	if starts.IsZero() {
		starts = now
	}
	al := alert{
		Labels: map[string]string{
			"alertname": "IncidentOpened", "source": "incident", "severity": string(inc.Severity), "project": project, "app": inc.App,
			"incident": inc.ID, "kind": kindOf(inc.DedupeKey),
		},
		Annotations: map[string]string{"summary": inc.Title, "description": cut(inc.Summary)},
		StartsAt:    starts.Format(time.RFC3339),
		EndsAt:      now.Add(time.Hour).Format(time.RFC3339),
	}
	if l := a.link(inc.ID); l != "" {
		al.GeneratorURL = l
	}
	body, err := json.Marshal([]alert{al})
	if err != nil {
		a.warn(project, inc, err.Error())
		return
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.URL, "/")+"/api/v2/alerts", bytes.NewReader(body))
	if err != nil {
		a.warn(project, inc, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := a.Client
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		a.warn(project, inc, "cannot reach Alertmanager")
		return
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		a.warn(project, inc, "Alertmanager answered HTTP "+strconv.Itoa(res.StatusCode))
	}
}

func (a *Alertmanager) warn(project string, inc incident.Incident, why string) {
	if a.Log != nil {
		a.Log.Warn("telling Alertmanager of an incident failed", "project", project, "incident", inc.ID, "reason", why)
	}
}

// Notifying is the detector's incidents with a Notifier told of each one that is newly created. What it passes on is exactly what the inner one returns.
type Notifying struct {
	detect.Incidents
	n  Notifier
	wg sync.WaitGroup
}

// Wrap tells n of each incident `inner` creates, and nothing else changes. Telling happens in the background: Create does not wait for it.
func Wrap(inner detect.Incidents, n Notifier) *Notifying { return &Notifying{Incidents: inner, n: n} }

func (w *Notifying) Create(ctx context.Context, project, actor string, in incident.CreateInput) (incident.Incident, bool, error) {
	inc, created, err := w.Incidents.Create(ctx, project, actor, in)
	if err == nil && created {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			w.n.Opened(context.WithoutCancel(ctx), project, inc)
		}()
	}
	return inc, created, err
}

// Wait returns when every telling that has begun has finished. A service has no use for it; a test does.
func (w *Notifying) Wait() { w.wg.Wait() }
