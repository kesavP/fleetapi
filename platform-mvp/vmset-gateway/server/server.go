// Package server is the gateway's HTTP shell. It holds no credentials of its
// own: every request is forwarded to the hub API server with the caller's
// bearer token, so Kubernetes RBAC decides what each caller may do.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/vmset-gateway/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

const (
	maxBodyBytes = 64 << 10
	maxListLimit = 200
)

// ClientFactory returns a hub client that acts as the holder of token.
type ClientFactory func(token string) (dynamic.Interface, error)

// NewClientFactory builds clients from base's address and CA only. Any
// credential in base is dropped, so a caller can never be served with the
// gateway's own identity.
func NewClientFactory(base *rest.Config) ClientFactory {
	return func(token string) (dynamic.Interface, error) {
		cfg := rest.AnonymousClientConfig(base)
		cfg.BearerToken = token
		return dynamic.NewForConfig(cfg)
	}
}

type Server struct {
	Clients   ClientFactory
	Resource  string // plural of the VmSet resource, as Kro generated it
	Rules     model.Rules
	Heartbeat time.Duration // SSE keep-alive period
	Log       *slog.Logger
}

func (s *Server) gvr() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: model.Group, Version: model.Version, Resource: s.Resource}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /v1/tenants/{tenant}/vmsets", s.authed(s.create))
	mux.HandleFunc("GET /v1/tenants/{tenant}/vmsets", s.authed(s.list))
	mux.HandleFunc("GET /v1/tenants/{tenant}/vmsets/{name}", s.authed(s.get))
	mux.HandleFunc("DELETE /v1/tenants/{tenant}/vmsets/{name}", s.authed(s.remove))
	mux.HandleFunc("GET /v1/tenants/{tenant}/vmsets/{name}/events", s.authed(s.events))
	return s.logged(mux)
}

type handler func(w http.ResponseWriter, r *http.Request, dc dynamic.Interface, tenant string)

// authed requires a bearer token and a valid tenant, then hands the handler a
// hub client acting as the caller.
func (s *Server) authed(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "bearer token required")
			return
		}
		tenant := r.PathValue("tenant")
		if !model.ValidTenant(tenant) {
			writeError(w, http.StatusBadRequest, "invalid tenant")
			return
		}
		dc, err := s.Clients(token)
		if err != nil {
			s.Log.Error("building hub client", "error", err)
			writeError(w, http.StatusInternalServerError, "gateway misconfigured")
			return
		}
		h(w, r, dc, tenant)
	}
}

func (s *Server) create(w http.ResponseWriter, r *http.Request, dc dynamic.Interface, tenant string) {
	var req model.CreateRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	obj, err := model.Build(tenant, req, s.Rules)
	var verr *model.ValidationError
	if errors.As(err, &verr) {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error": map[string]interface{}{"code": http.StatusBadRequest, "message": "invalid request", "problems": verr.Problems},
		})
		return
	}
	created, err := dc.Resource(s.gvr()).Namespace(tenant).Create(r.Context(), obj, metav1.CreateOptions{})
	if err != nil {
		writeKubeError(w, err)
		return
	}
	w.Header().Set("Location", fmt.Sprintf("/v1/tenants/%s/vmsets/%s", tenant, created.GetName()))
	writeJSON(w, http.StatusCreated, model.Summarize(created))
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, dc dynamic.Interface, tenant string) {
	obj, err := dc.Resource(s.gvr()).Namespace(tenant).Get(r.Context(), r.PathValue("name"), metav1.GetOptions{})
	if err != nil {
		writeKubeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, model.Summarize(obj))
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, dc dynamic.Interface, tenant string) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxListLimit {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("limit must be between 1 and %d", maxListLimit))
			return
		}
		limit = n
	}
	list, err := dc.Resource(s.gvr()).Namespace(tenant).List(r.Context(), metav1.ListOptions{
		Limit: int64(limit), Continue: r.URL.Query().Get("continue"),
	})
	if err != nil {
		writeKubeError(w, err)
		return
	}
	items := make([]model.VmSet, 0, len(list.Items))
	for i := range list.Items {
		items = append(items, model.Summarize(&list.Items[i]))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": items, "continue": list.GetContinue()})
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request, dc dynamic.Interface, tenant string) {
	err := dc.Resource(s.gvr()).Namespace(tenant).Delete(r.Context(), r.PathValue("name"), metav1.DeleteOptions{})
	if err != nil {
		writeKubeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// events streams the VmSet's state as server-sent events: a snapshot first,
// then one "update" per change, and "deleted" when the object goes away.
func (s *Server) events(w http.ResponseWriter, r *http.Request, dc dynamic.Interface, tenant string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	name := r.PathValue("name")
	res := dc.Resource(s.gvr()).Namespace(tenant)

	// Watching from the snapshot's resourceVersion leaves no gap between the
	// state the client is shown and the changes it then receives.
	snapshot, err := res.Get(r.Context(), name, metav1.GetOptions{})
	if err != nil {
		writeKubeError(w, err)
		return
	}
	w2, err := res.Watch(r.Context(), metav1.ListOptions{
		FieldSelector:   "metadata.name=" + name,
		ResourceVersion: snapshot.GetResourceVersion(),
	})
	if err != nil {
		writeKubeError(w, err)
		return
	}
	defer w2.Stop()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // keep nginx from buffering the stream
	w.WriteHeader(http.StatusOK)
	sendEvent(w, "update", model.Summarize(snapshot))
	flusher.Flush()

	heartbeat := time.NewTicker(s.Heartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			flusher.Flush()
		case ev, open := <-w2.ResultChan():
			if !open {
				return
			}
			switch ev.Type {
			case watch.Added, watch.Modified:
				if u, ok := ev.Object.(*unstructured.Unstructured); ok {
					sendEvent(w, "update", model.Summarize(u))
				}
			case watch.Deleted:
				sendEvent(w, "deleted", map[string]string{"name": name})
				flusher.Flush()
				return
			default: // watch.Error: end the stream; the client reconnects and gets a fresh snapshot
				return
			}
			flusher.Flush()
		}
	}
}

func sendEvent(w http.ResponseWriter, event string, v interface{}) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]interface{}{"error": map[string]interface{}{"code": code, "message": msg}})
}

// writeKubeError passes the hub's verdict (403, 404, 409, ...) through with
// its own status code; anything that is not an API status means the hub could
// not be reached.
func writeKubeError(w http.ResponseWriter, err error) {
	var st apierrors.APIStatus
	if errors.As(err, &st) {
		status := st.Status()
		code := int(status.Code)
		if code < 400 || code > 599 {
			code = http.StatusBadGateway
		}
		writeError(w, code, status.Message)
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	writeError(w, http.StatusBadGateway, "hub API server unreachable")
}

// statusRecorder captures the response code for the access log.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// logged writes one access-log line per request. It never records headers,
// so bearer tokens cannot reach the log.
func (s *Server) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.Log.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.code, "duration", time.Since(start).String())
	})
}
