package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/incident"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/rules"
)

// Rules serves the settings that decide when the detectors speak, under /api/v1/projects/{project}/rules:
//
//	GET  .../rules          read or manage key   every rule as it is in force for the project: the default, or what was saved
//	PUT  .../rules/{name}   manage key           save a new version of one rule (see the rules package for the names and their limits)
//
// The same keys as the incidents API: the key a browser holds can look and never touch, and a key for another project, or for none, gets the same
// answer whatever the project. A save is a new version with who made it (the X-Actor header, "api" without one) and when; with an expected_version in the
// body (or an If-Match header) it is refused when the rule has moved on since it was read, so two people do not overwrite each other. Every answer is
// {"data": ...}, or {"error": "..."} with a status that says what kind.
type Rules struct {
	Projects Resolver
	Manager  *rules.Manager
	Log      *slog.Logger
}

func (h *Rules) Register(mux *http.ServeMux) {
	const base = "/api/v1/projects/{project}/rules"
	mux.HandleFunc("GET "+base, h.serve(false, h.list))
	mux.HandleFunc("PUT "+base+"/{name}", h.serve(true, h.put))
}

func (h *Rules) serve(write bool, fn incidentFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		key := apiKey(r)
		if key == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing API key"})
			return
		}
		want := r.PathValue("project")
		var project string
		var ok bool
		if write {
			project, ok = h.Projects.ManageProject(key)
			if !ok || project != want {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "this key cannot change that project's rules"})
				return
			}
		} else {
			if project, ok = h.Projects.ReadProject(key); !ok {
				project, ok = h.Projects.ManageProject(key)
			}
			if !ok || project != want {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "this key cannot read that project"})
				return
			}
		}
		actor := "api"
		if write {
			if a := r.Header.Get("X-Actor"); a != "" {
				if !incident.ValidActor(a) {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "X-Actor must be 1 to 64 letters, digits, spaces and . _ @ + -, starting with a letter or digit"})
					return
				}
				actor = a
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), incidentTimeout)
		defer cancel()
		status, body, err := fn(ctx, call{project: project, actor: actor, w: w, r: r})
		if err != nil {
			h.fail(w, project, r, err)
			return
		}
		writeJSON(w, status, body)
	}
}

// fail says what was wrong with the caller's input, and nothing of what went wrong inside.
func (h *Rules) fail(w http.ResponseWriter, project string, r *http.Request, err error) {
	var pe paramError
	var ce *rules.ConflictError
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("the request body is larger than %d bytes", maxIncidentBody)})
	case errors.As(err, &pe):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": pe.Error()})
	case errors.Is(err, errUnsupportedMedia):
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "send application/json"})
	case errors.Is(err, rules.ErrUnknownRule):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such rule"})
	case errors.Is(err, rules.ErrInvalid):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": strings.TrimPrefix(err.Error(), "invalid: ")})
	case errors.As(err, &ce):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "current_version": ce.Current})
	default:
		h.Log.Error("rules request failed", "project", project, "path", r.URL.Path, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "request failed"})
	}
}

type ruleSettings struct {
	Enabled    bool    `json:"enabled"`
	Percent    float64 `json:"percent"`
	Days       int     `json:"days"`
	Percentile float64 `json:"percentile"`
}

type ruleJSON struct {
	Name string `json:"name"`
	ruleSettings
	Version   uint32       `json:"version"`
	Saved     bool         `json:"saved"`
	UpdatedAt *time.Time   `json:"updated_at,omitempty"`
	UpdatedBy string       `json:"updated_by,omitempty"`
	Default   ruleSettings `json:"default"`
}

func settingsJSON(s rules.Settings) ruleSettings {
	return ruleSettings{Enabled: s.Enabled, Percent: s.Percent, Days: s.Days, Percentile: s.Percentile}
}

func viewJSON(v rules.View) ruleJSON {
	out := ruleJSON{Name: v.Name, ruleSettings: settingsJSON(v.Settings), Version: v.Version, Saved: v.Saved, UpdatedBy: v.UpdatedBy, Default: settingsJSON(v.Default)}
	if !v.UpdatedAt.IsZero() {
		t := v.UpdatedAt.UTC()
		out.UpdatedAt = &t
	}
	return out
}

func (h *Rules) list(ctx context.Context, c call) (int, any, error) {
	vs, err := h.Manager.List(ctx, c.project)
	if err != nil {
		return 0, nil, err
	}
	out := make([]ruleJSON, len(vs))
	for i, v := range vs {
		out[i] = viewJSON(v)
	}
	return http.StatusOK, map[string]any{"data": out}, nil
}

type putBody struct {
	Enabled         *bool    `json:"enabled"`
	Percent         *float64 `json:"percent"`
	Days            *int     `json:"days"`
	Percentile      *float64 `json:"percentile"`
	ExpectedVersion *uint32  `json:"expected_version"`
}

// expectedRuleVersion is the version the caller read, from If-Match or the body (both must agree), or nil. Unlike an incident's, 0 is a version here: it
// says "I read the default, and nothing had been saved".
func expectedRuleVersion(r *http.Request, inBody *uint32) (*uint32, error) {
	var out *uint32
	if h := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`); h != "" {
		n, err := strconv.ParseUint(h, 10, 32)
		if err != nil {
			return nil, paramError(`If-Match must be the version read, such as "3"`)
		}
		v := uint32(n)
		out = &v
	}
	if inBody != nil {
		if out != nil && *out != *inBody {
			return nil, paramError("If-Match and expected_version disagree")
		}
		out = inBody
	}
	return out, nil
}

func (h *Rules) put(ctx context.Context, c call) (int, any, error) {
	var b putBody
	if err := decode(c, &b); err != nil {
		return 0, nil, err
	}
	switch {
	case b.Enabled == nil:
		return 0, nil, paramError("enabled is required")
	case b.Percent == nil:
		return 0, nil, paramError("percent is required")
	case b.Days == nil:
		return 0, nil, paramError("days is required")
	case b.Percentile == nil:
		return 0, nil, paramError("percentile is required")
	}
	exp, err := expectedRuleVersion(c.r, b.ExpectedVersion)
	if err != nil {
		return 0, nil, err
	}
	v, err := h.Manager.Put(ctx, c.project, c.r.PathValue("name"), rules.Settings{Enabled: *b.Enabled, Percent: *b.Percent, Days: *b.Days, Percentile: *b.Percentile}, c.actor, exp)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"data": viewJSON(v)}, nil
}
