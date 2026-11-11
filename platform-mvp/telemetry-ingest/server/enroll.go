package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/enroll"
)

const (
	enrollTimeout = 30 * time.Second
	maxEnrollBody = 4 << 10
)

// Enroll serves cluster enrollment (see the enroll package).
//
// A person with a MANAGE key makes an enrollment under /api/v1/projects/{project}/enrollments, and is given a one-time token, once. The cluster
// redeems the token with POST /v1/enroll, which has NO API key (the token is what proves it) and is answered with a key of the cluster's own, once.
// A read key (or a manage key) lists a project's enrollments, never their tokens; revoking one takes a manage key.
//
// /v1/enroll is open to anyone who can reach the service, so it is made hard to use as a way to guess: a token is 256 bits, every attempt counts
// against a per-address and a global limit, anything that is not shaped like a token is turned away without a query, and every reason a token may
// be bad (never made, used, revoked, expired) is the same answer.
type Enroll struct {
	Projects Resolver
	Manager  *enroll.Manager
	Log      *slog.Logger
	Count    *Counters
	Limiter  *RateLimiter // nil: DefaultEnrollLimiter()
	once     sync.Once
}

func (h *Enroll) Register(mux *http.ServeMux) {
	h.once.Do(func() {
		if h.Limiter == nil {
			h.Limiter = DefaultEnrollLimiter()
		}
	})
	const base = "/api/v1/projects/{project}/enrollments"
	mux.HandleFunc("GET "+base, h.manage(false, h.list))
	mux.HandleFunc("POST "+base, h.manage(true, h.create))
	mux.HandleFunc("DELETE "+base+"/{id}", h.manage(true, h.revoke))
	mux.HandleFunc("POST /v1/enroll", h.redeem)
}

type enrollFunc func(ctx context.Context, c call) (int, any, error)

func (h *Enroll) manage(write bool, fn enrollFunc) http.HandlerFunc {
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
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "this key cannot change that project's enrollments"})
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
				if !enroll.ValidActor(a) {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "X-Actor must be 1 to 64 letters, digits, spaces and . _ @ + -, starting with a letter or digit"})
					return
				}
				actor = a
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), enrollTimeout)
		defer cancel()
		status, body, err := fn(ctx, call{project: project, actor: actor, w: w, r: r})
		if err != nil {
			h.fail(w, project, r, err)
			return
		}
		writeJSON(w, status, body)
	}
}

func (h *Enroll) fail(w http.ResponseWriter, project string, r *http.Request, err error) {
	var pe paramError
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("the request body is larger than %d bytes", maxIncidentBody)})
	case errors.As(err, &pe):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": pe.Error()})
	case errors.Is(err, errUnsupportedMedia):
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "send application/json"})
	case errors.Is(err, enroll.ErrInvalid):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": strings.TrimPrefix(err.Error(), "invalid: ")})
	case errors.Is(err, enroll.ErrTooMany):
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": strings.TrimPrefix(err.Error(), "too many pending: ")})
	case errors.Is(err, enroll.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such enrollment"})
	default:
		h.Log.Error("enrollment request failed", "project", project, "path", r.URL.Path, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "request failed"})
	}
}

func (h *Enroll) list(ctx context.Context, c call) (int, any, error) {
	l, err := h.Manager.List(ctx, c.project)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"data": l}, nil
}

type createRequest struct {
	Cluster    string `json:"cluster"`
	TTLMinutes int    `json:"ttl_minutes"`
}

// created is an enrollment with the token it was made with: the only time the token is in an answer.
type created struct {
	enroll.Enrollment
	Token string `json:"token"`
}

func (h *Enroll) create(ctx context.Context, c call) (int, any, error) {
	var req createRequest
	if err := decode(c, &req); err != nil {
		return 0, nil, err
	}
	if req.TTLMinutes < 0 || req.TTLMinutes > int(enroll.MaxTTL/time.Minute) {
		return 0, nil, paramError(fmt.Sprintf("ttl_minutes must be between 1 and %d (omit it for %d)", int(enroll.MaxTTL/time.Minute), int(enroll.DefaultTTL/time.Minute)))
	}
	e, token, err := h.Manager.Create(ctx, c.project, c.actor, req.Cluster, time.Duration(req.TTLMinutes)*time.Minute)
	if err != nil {
		return 0, nil, err
	}
	h.Log.Info("enrollment made", "project", c.project, "cluster", e.Cluster, "enrollment", e.ID, "by", c.actor, "expires", e.ExpiresAt)
	return http.StatusCreated, map[string]any{"data": created{Enrollment: e, Token: token}}, nil
}

func (h *Enroll) revoke(ctx context.Context, c call) (int, any, error) {
	e, err := h.Manager.Revoke(ctx, c.project, c.r.PathValue("id"), c.actor)
	if err != nil {
		return 0, nil, err
	}
	h.Log.Info("enrollment revoked", "project", c.project, "cluster", e.Cluster, "enrollment", e.ID, "by", c.actor)
	return http.StatusOK, map[string]any{"data": e}, nil
}

// ---- redeeming ----

var (
	k8sNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	k8sKeyRE  = regexp.MustCompile(`^[-._a-zA-Z0-9]{1,253}$`)
)

const (
	defaultNamespace = "telemetry-agent"
	defaultSecret    = "hub-telemetry-credentials"
	defaultKeyName   = "apiKey"
)

type redeemRequest struct {
	Token      string `json:"token"`
	Namespace  string `json:"namespace"`
	SecretName string `json:"secret_name"`
	KeyName    string `json:"key_name"`
}

// secretManifest is what `kubectl apply -f -` takes: the namespace, and the Secret the node-agent and the cluster collector read. Every value
// has been checked against a pattern that has nothing in it that YAML would read as anything but text.
func secretManifest(ns, name, keyName, key, cluster, id string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %[1]s
---
apiVersion: v1
kind: Secret
metadata:
  name: %[2]s
  namespace: %[1]s
  labels:
    app.kubernetes.io/managed-by: telemetry-enroll
  annotations:
    platform.example.com/enrolled-cluster: %[5]q
    platform.example.com/enrollment: %[6]q
type: Opaque
stringData:
  %[3]s: %[4]q
`, ns, name, keyName, key, cluster, id)
}

func (h *Enroll) redeem(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	count := func(code string) {
		if h.Count != nil {
			h.Count.Inc("telemetry_enroll_requests_total", "code", code)
		}
	}
	ip := clientIP(r)
	if ok, retry := h.Limiter.Allow(ip); !ok {
		count("429")
		w.Header().Set("Retry-After", fmt.Sprint(int(retry.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many attempts; try again in a minute"})
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		count("415")
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "send application/json"})
		return
	}
	var req redeemRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEnrollBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		count("400")
		msg := "the request body is not valid"
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "the request body is too large"})
			return
		case errors.Is(err, io.EOF):
			msg = "the request body is empty"
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	format := r.URL.Query().Get("format")
	if format != "" && format != "json" && format != "secret" {
		count("400")
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "format must be json or secret"})
		return
	}
	ns, name, keyName := req.Namespace, req.SecretName, req.KeyName
	if ns == "" {
		ns = defaultNamespace
	}
	if name == "" {
		name = defaultSecret
	}
	if keyName == "" {
		keyName = defaultKeyName
	}
	if !k8sNameRE.MatchString(ns) || !k8sNameRE.MatchString(name) || !k8sKeyRE.MatchString(keyName) || keyName == "." || keyName == ".." {
		count("400")
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "namespace and secret_name must be Kubernetes names (lower-case letters, digits and -), and key_name letters, digits and - _ ."})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), enrollTimeout)
	defer cancel()
	iss, err := h.Manager.Redeem(ctx, req.Token)
	switch {
	case errors.Is(err, enroll.ErrInvalidToken):
		count("401")
		h.Log.Warn("an enrollment token was refused", "from", ip) // the token itself is never logged
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": enroll.ErrInvalidToken.Error()})
		return
	case err != nil:
		count("500")
		h.Log.Error("redeeming an enrollment token failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "request failed"})
		return
	}
	count("200")
	h.Log.Info("a cluster enrolled", "project", iss.Project, "cluster", iss.Cluster, "enrollment", iss.ID, "from", ip)
	if format == "secret" {
		w.Header().Set("Content-Type", "application/x-yaml")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, secretManifest(ns, name, keyName, iss.Key, iss.Cluster, iss.ID))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]string{"project": iss.Project, "cluster": iss.Cluster, "enrollment": iss.ID, "ingest_key": iss.Key}})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// RateLimiter counts attempts in a fixed window, per address and overall. It is small on purpose: it is here to make guessing not worth the
// trying, and to keep a flood from reaching the database, not to be fair.
type RateLimiter struct {
	PerAddress, Global int
	Window             time.Duration
	Now                func() time.Time

	mu     sync.Mutex
	start  time.Time
	global int
	per    map[string]int
}

// DefaultEnrollLimiter allows 20 attempts a minute from one address and 120 overall.
func DefaultEnrollLimiter() *RateLimiter {
	return &RateLimiter{PerAddress: 20, Global: 120, Window: time.Minute}
}

// Allow counts an attempt and says whether it may go on; when it may not, how long until the window ends.
func (l *RateLimiter) Allow(addr string) (bool, time.Duration) {
	now := time.Now()
	if l.Now != nil {
		now = l.Now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.per == nil || now.Sub(l.start) >= l.Window {
		l.start, l.global, l.per = now, 0, map[string]int{}
	}
	l.global++
	l.per[addr]++
	if l.global > l.Global || l.per[addr] > l.PerAddress {
		return false, l.Window - now.Sub(l.start)
	}
	return true, 0
}
