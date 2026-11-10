package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/incident"
)

const (
	incidentTimeout = 30 * time.Second
	maxIncidentBody = 64 << 10
)

// Incidents serves the incident lifecycle API under /api/v1/projects/{project}/incidents.
//
// Who may do what: a READ key (or a manage key) reads a project's incidents; changing one takes a MANAGE key, which is a different key,
// so the key a browser holds can look and never touch. As with the rest of the API, a key for another project, or for none, gets the same
// 403 whatever the project, so the answer does not say which projects exist.
//
// Every response is {"data": ...}; an error is {"error": "..."} with a status that says what kind: 400 for input that is wrong, 401 for no key,
// 403 for a key that may not, 404 for no such incident, 409 for a change that the incident's state or version does not allow (the body
// then says what would be allowed or what the version is now), 413 for a body that is too big, 415 for one that is not JSON.
type Incidents struct {
	Projects Resolver
	Manager  *incident.Manager
	Log      *slog.Logger
}

func (h *Incidents) Register(mux *http.ServeMux) {
	const base = "/api/v1/projects/{project}/incidents"
	mux.HandleFunc("GET "+base, h.read(h.list))
	mux.HandleFunc("GET "+base+"/{id}", h.read(h.get))
	mux.HandleFunc("GET "+base+"/{id}/events", h.read(h.events))
	mux.HandleFunc("GET "+base+"/{id}/evidence", h.read(h.evidence))
	mux.HandleFunc("POST "+base, h.write(h.create))
	mux.HandleFunc("PATCH "+base+"/{id}", h.write(h.update))
	mux.HandleFunc("POST "+base+"/{id}/transitions", h.write(h.transition))
	mux.HandleFunc("POST "+base+"/{id}/comments", h.write(h.comment))
	mux.HandleFunc("POST "+base+"/{id}/evidence", h.write(h.addEvidence))
	mux.HandleFunc("DELETE "+base+"/{id}/evidence/{evidence_id}", h.write(h.removeEvidence))
}

type call struct {
	project, actor string
	w              http.ResponseWriter
	r              *http.Request
}

type incidentFunc func(ctx context.Context, c call) (status int, body any, err error)

func (h *Incidents) read(fn incidentFunc) http.HandlerFunc  { return h.serve(false, fn) }
func (h *Incidents) write(fn incidentFunc) http.HandlerFunc { return h.serve(true, fn) }

func (h *Incidents) serve(write bool, fn incidentFunc) http.HandlerFunc {
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
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "this key cannot change that project's incidents"})
				return
			}
		} else {
			project, ok = h.Projects.ReadProject(key)
			if !ok {
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

// fail turns what the Manager says into a status and a body. What it says about the caller's input is passed on; anything else (the database,
// a record that will not fold) is logged here and answered with a plain sentence, so nothing internal reaches the caller.
func (h *Incidents) fail(w http.ResponseWriter, project string, r *http.Request, err error) {
	var pe paramError
	var te *incident.TransitionError
	var ce *incident.ConflictError
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("the request body is larger than %d bytes", maxIncidentBody)})
	case errors.As(err, &pe):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": pe.Error()})
	case errors.Is(err, errUnsupportedMedia):
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "send application/json"})
	case errors.Is(err, incident.ErrInvalid):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": strings.TrimPrefix(err.Error(), "invalid: ")})
	case errors.Is(err, incident.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such incident"})
	case errors.As(err, &te):
		allowed := make([]string, len(te.Allowed))
		for i, s := range te.Allowed {
			allowed[i] = string(s)
		}
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "from": te.From, "allowed": allowed})
	case errors.As(err, &ce):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "current_version": ce.Current})
	case errors.Is(err, incident.ErrCorrupt):
		h.Log.Error("incident record does not fold", "project", project, "path", r.URL.Path, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "the incident's record is inconsistent"})
	default:
		h.Log.Error("incident request failed", "project", project, "path", r.URL.Path, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "request failed"})
	}
}

var errUnsupportedMedia = errors.New("unsupported media type")

// decode reads one JSON object into dst: application/json only, a bounded body, no field the type does not have, nothing after it.
func decode(c call, dst any) error {
	if mt, _, err := mime.ParseMediaType(c.r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return errUnsupportedMedia
	}
	body := http.MaxBytesReader(c.w, c.r.Body, maxIncidentBody)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return err
		}
		if errors.Is(err, io.EOF) {
			return paramError("the request body is empty")
		}
		return paramError("the request body is not valid for this call: " + jsonProblem(err))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return err
		}
		return paramError("the request body has something after the JSON object")
	}
	return nil
}

// jsonProblem is the decoder's complaint without anything that echoes the request back at length.
func jsonProblem(err error) string {
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// expected is the version the caller read, from the If-Match header or the body's expected_version (both must agree), or nil.
func expected(r *http.Request, inBody *uint32) (*uint32, error) {
	var out *uint32
	if h := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`); h != "" {
		n, err := strconv.ParseUint(h, 10, 32)
		if err != nil || n == 0 {
			return nil, paramError(`If-Match must be the version read, such as "3"`)
		}
		v := uint32(n)
		out = &v
	}
	if inBody != nil {
		if *inBody == 0 {
			return nil, paramError("expected_version must be the version read (1 or more)")
		}
		if out != nil && *out != *inBody {
			return nil, paramError("If-Match and expected_version disagree")
		}
		out = inBody
	}
	return out, nil
}

func ifMatchOnly(r *http.Request) (*uint32, error) { return expected(r, nil) }

// ---- reads ----

func (h *Incidents) list(ctx context.Context, c call) (int, any, error) {
	v := c.r.URL.Query()
	f := incident.Filter{App: v.Get("app")}
	if s := v.Get("state"); s != "" {
		for _, p := range strings.Split(s, ",") {
			st := incident.State(strings.TrimSpace(p))
			if !incident.ValidState(st) {
				return 0, nil, paramError(fmt.Sprintf("state: %q is not a state", p))
			}
			f.States = append(f.States, st)
		}
	}
	if s := v.Get("severity"); s != "" {
		f.Severity = incident.Severity(s)
		if !incident.ValidSeverity(f.Severity) {
			return 0, nil, paramError("severity must be critical, warning or info")
		}
	}
	n, err := intParam(c.r, "limit", 50, 1, 200)
	if err != nil {
		return 0, nil, err
	}
	f.Limit = n
	list, skipped, err := h.Manager.List(ctx, c.project, f)
	if err != nil {
		return 0, nil, err
	}
	if list == nil {
		list = []incident.Incident{}
	}
	out := map[string]any{"data": list}
	if skipped > 0 {
		out["meta"] = map[string]int{"skipped": skipped}
	}
	return http.StatusOK, out, nil
}

func (h *Incidents) get(ctx context.Context, c call) (int, any, error) {
	inc, err := h.Manager.Get(ctx, c.project, c.r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"data": inc}, nil
}

func (h *Incidents) events(ctx context.Context, c call) (int, any, error) {
	after, err := intParam(c.r, "after", 0, 0, 1<<30)
	if err != nil {
		return 0, nil, err
	}
	limit, err := intParam(c.r, "limit", 100, 1, 500)
	if err != nil {
		return 0, nil, err
	}
	evs, err := h.Manager.Events(ctx, c.project, c.r.PathValue("id"), uint32(after), limit)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"data": evs}, nil
}

func (h *Incidents) evidence(ctx context.Context, c call) (int, any, error) {
	inc, err := h.Manager.Get(ctx, c.project, c.r.PathValue("id"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"data": inc.Evidence}, nil
}

// ---- changes ----

type createBody struct {
	Title     string            `json:"title"`
	App       string            `json:"app"`
	Severity  incident.Severity `json:"severity"`
	Source    incident.Source   `json:"source"`
	DedupeKey string            `json:"dedupe_key"`
	Summary   string            `json:"summary"`
	StartedAt *time.Time        `json:"started_at"`
}

func (h *Incidents) create(ctx context.Context, c call) (int, any, error) {
	var b createBody
	if err := decode(c, &b); err != nil {
		return 0, nil, err
	}
	in := incident.CreateInput{Title: b.Title, App: b.App, Severity: b.Severity, Source: b.Source, DedupeKey: b.DedupeKey, Summary: b.Summary}
	if b.StartedAt != nil {
		in.StartedAt = *b.StartedAt
	}
	inc, created, err := h.Manager.Create(ctx, c.project, c.actor, in)
	if err != nil {
		return 0, nil, err
	}
	c.w.Header().Set("Location", "/api/v1/projects/"+c.project+"/incidents/"+inc.ID)
	status := http.StatusCreated
	if !created {
		status = http.StatusOK // the incident for that dedupe_key already existed
	}
	return status, map[string]any{"data": inc}, nil
}

type updateBody struct {
	Title           *string            `json:"title"`
	Severity        *incident.Severity `json:"severity"`
	Summary         *string            `json:"summary"`
	Assignee        *string            `json:"assignee"`
	ExpectedVersion *uint32            `json:"expected_version"`
}

func (h *Incidents) update(ctx context.Context, c call) (int, any, error) {
	var b updateBody
	if err := decode(c, &b); err != nil {
		return 0, nil, err
	}
	exp, err := expected(c.r, b.ExpectedVersion)
	if err != nil {
		return 0, nil, err
	}
	inc, err := h.Manager.Update(ctx, c.project, c.r.PathValue("id"), c.actor, exp, incident.UpdateInput{Title: b.Title, Severity: b.Severity, Summary: b.Summary, Assignee: b.Assignee})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"data": inc}, nil
}

type transitionBody struct {
	To              incident.State      `json:"to"`
	Note            string              `json:"note"`
	Resolution      incident.Resolution `json:"resolution"`
	ExpectedVersion *uint32             `json:"expected_version"`
}

func (h *Incidents) transition(ctx context.Context, c call) (int, any, error) {
	var b transitionBody
	if err := decode(c, &b); err != nil {
		return 0, nil, err
	}
	if b.To == "" {
		return 0, nil, paramError("to is required: the state to move to")
	}
	exp, err := expected(c.r, b.ExpectedVersion)
	if err != nil {
		return 0, nil, err
	}
	inc, err := h.Manager.Transition(ctx, c.project, c.r.PathValue("id"), c.actor, exp, incident.TransitionInput{To: b.To, Note: b.Note, Resolution: b.Resolution})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"data": inc}, nil
}

type commentBody struct {
	Text            string  `json:"text"`
	ExpectedVersion *uint32 `json:"expected_version"`
}

func (h *Incidents) comment(ctx context.Context, c call) (int, any, error) {
	var b commentBody
	if err := decode(c, &b); err != nil {
		return 0, nil, err
	}
	exp, err := expected(c.r, b.ExpectedVersion)
	if err != nil {
		return 0, nil, err
	}
	inc, err := h.Manager.Comment(ctx, c.project, c.r.PathValue("id"), c.actor, exp, b.Text)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, map[string]any{"data": inc}, nil
}

type evidenceBody struct {
	incident.EvidenceRef
	ExpectedVersion *uint32 `json:"expected_version"`
}

func (h *Incidents) addEvidence(ctx context.Context, c call) (int, any, error) {
	var b evidenceBody
	if err := decode(c, &b); err != nil {
		return 0, nil, err
	}
	exp, err := expected(c.r, b.ExpectedVersion)
	if err != nil {
		return 0, nil, err
	}
	inc, ev, added, err := h.Manager.AddEvidence(ctx, c.project, c.r.PathValue("id"), c.actor, exp, b.EvidenceRef)
	if err != nil {
		return 0, nil, err
	}
	status := http.StatusCreated
	if !added {
		status = http.StatusOK // that reference was already attached
	}
	return status, map[string]any{"data": map[string]any{"evidence": ev, "incident": inc}}, nil
}

func (h *Incidents) removeEvidence(ctx context.Context, c call) (int, any, error) {
	exp, err := ifMatchOnly(c.r)
	if err != nil {
		return 0, nil, err
	}
	inc, err := h.Manager.RemoveEvidence(ctx, c.project, c.r.PathValue("id"), c.actor, exp, c.r.PathValue("evidence_id"), c.r.URL.Query().Get("reason"))
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"data": inc}, nil
}
