// Package server is the router's HTTP shell. It authenticates Alertmanager,
// turns each webhook into alerts, drops unchanged repeats, and hands the rest
// to the sinks.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/alert-router/dedupe"
	"github.com/sojoner/kro-lab/platform-mvp/alert-router/model"
	"github.com/sojoner/kro-lab/platform-mvp/alert-router/sink"
)

const maxBodyBytes = 1 << 20

// Registration adds a sink to the pipeline. A Required sink failing makes the
// router answer 5xx so Alertmanager retries; other failures are counted and
// logged but never trigger a retry.
type Registration struct {
	Sink     sink.Sink
	Required bool
}

type Server struct {
	// Token returns the bearer token Alertmanager must present. It is read on
	// every request so a rotated Secret takes effect without a restart.
	Token       func() (string, error)
	Sinks       []Registration
	Deduper     *dedupe.Deduper
	SinkTimeout time.Duration
	Log         *slog.Logger

	Metrics Metrics
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /metrics", s.Metrics.serve)
	mux.HandleFunc("POST /alerts", s.alerts)
	return s.logged(mux)
}

type response struct {
	Received  int `json:"received"`
	Deduped   int `json:"deduped"`
	Delivered int `json:"delivered"`
}

func (s *Server) alerts(w http.ResponseWriter, r *http.Request) {
	switch s.authenticate(r) {
	case authMisconfigured:
		// Fail closed: with no readable token nothing is accepted.
		writeError(w, http.StatusServiceUnavailable, "router has no usable token")
		return
	case authDenied:
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "bearer token required")
		return
	}

	hook, err := model.Parse(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload too large")
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	alerts := model.Normalize(hook)
	s.Metrics.add(&s.Metrics.received, len(alerts))

	forward, dropped := s.Deduper.Filter(alerts)
	s.Metrics.add(&s.Metrics.deduped, dropped)
	resp := response{Received: len(alerts), Deduped: dropped}
	if len(forward) == 0 {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	if s.deliver(r.Context(), forward) {
		resp.Delivered = len(forward)
		s.Metrics.add(&s.Metrics.delivered, len(forward))
		writeJSON(w, http.StatusOK, resp)
		return
	}
	// Alertmanager will retry; the retry must not be dropped as a repeat.
	s.Deduper.Forget(forward)
	writeError(w, http.StatusInternalServerError, "required sink failed")
}

// deliver sends the batch to every sink concurrently, each under its own
// deadline, and reports whether every required sink accepted it.
func (s *Server) deliver(ctx context.Context, alerts []model.Alert) bool {
	var wg sync.WaitGroup
	failed := make([]bool, len(s.Sinks))
	for i, reg := range s.Sinks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sctx, cancel := context.WithTimeout(ctx, s.SinkTimeout)
			defer cancel()
			if err := reg.Sink.Send(sctx, alerts); err != nil {
				failed[i] = true
				s.Metrics.sinkError(reg.Sink.Name())
				s.Log.Error("sink failed", "sink", reg.Sink.Name(), "required", reg.Required, "error", err.Error())
			}
		}()
	}
	wg.Wait()
	for i, reg := range s.Sinks {
		if reg.Required && failed[i] {
			return false
		}
	}
	return true
}

type authResult int

const (
	authOK authResult = iota
	authDenied
	authMisconfigured
)

func (s *Server) authenticate(r *http.Request) authResult {
	want, err := s.Token()
	if err != nil || want == "" {
		return authMisconfigured
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return authDenied
	}
	// Hashing first makes the comparison independent of the token's length.
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	if subtle.ConstantTimeCompare(a[:], b[:]) != 1 {
		return authDenied
	}
	return authOK
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

// logged writes one access-log line per request. It never records headers,
// so the bearer token cannot reach the log.
func (s *Server) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			return // probe and scrape noise
		}
		s.Log.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.code, "duration", time.Since(start).String())
	})
}
