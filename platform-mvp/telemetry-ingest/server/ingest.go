package server

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"

	"github.com/google/pprof/profile"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/pipeline"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/wire"
	logsv1 "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	tracesv1 "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type Ingest struct {
	Projects Resolver
	Sink     Sink
	MaxBody  int64
	Log      *slog.Logger
	Count    *Counters
}

type httpError struct {
	status int
	msg    string
}

func (e httpError) Error() string { return e.msg }

func badRequest(format string, a ...any) error {
	return httpError{http.StatusBadRequest, fmt.Sprintf(format, a...)}
}

func (s *Ingest) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/metrics", s.handle("metrics", s.metrics))
	mux.HandleFunc("POST /v1/traces", s.handle("traces", s.traces))
	mux.HandleFunc("POST /v1/logs", s.handle("logs", s.logs))
	mux.HandleFunc("POST /v1/profiles", s.handle("profiles", s.profiles))
}

type handler func(project string, w http.ResponseWriter, r *http.Request) error

// handle authenticates, runs fn and maps its error to a status. A missing or
// unknown key is 401: there is no default project to fall back to.
func (s *Ingest) handle(signal string, fn handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		project, ok := s.Projects.IngestProject(apiKey(r))
		if !ok {
			s.Count.Inc("telemetry_ingest_requests_total", "signal", signal, "code", "401")
			http.Error(w, "missing or invalid API key", http.StatusUnauthorized)
			return
		}
		sw := &statusWriter{ResponseWriter: w}
		var code int
		if err := fn(project, sw, r); err != nil {
			code = s.fail(sw, signal, project, err)
		} else if code = sw.status; code == 0 {
			code = http.StatusOK
		}
		s.Count.Inc("telemetry_ingest_requests_total", "signal", signal, "code", strconv.Itoa(code))
	}
}

// statusWriter remembers the status a handler wrote, so the request counter
// reports what the agent received (204 for remote-write, for instance).
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (s *Ingest) fail(w http.ResponseWriter, signal, project string, err error) int {
	var he httpError
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &he):
		http.Error(w, he.msg, he.status)
		return he.status
	case errors.Is(err, wire.ErrTooLarge), errors.As(err, &mbe):
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, pipeline.ErrOverloaded):
		// The agent retries on 5xx; telling it to wait beats dropping the data.
		w.Header().Set("Retry-After", "5")
		http.Error(w, "storage is behind, retry later", http.StatusServiceUnavailable)
		return http.StatusServiceUnavailable
	}
	s.Log.Error("ingest failed", "signal", signal, "project", project, "error", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
	return http.StatusInternalServerError
}

func (s *Ingest) body(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(w, r.Body, s.MaxBody))
}

func (s *Ingest) metrics(project string, w http.ResponseWriter, r *http.Request) error {
	raw, err := s.body(w, r)
	if err != nil {
		return err
	}
	if enc := r.Header.Get("Content-Encoding"); enc != "snappy" {
		return badRequest("remote-write needs Content-Encoding: snappy, got %q", enc)
	}
	rw, err := wire.DecodeRemoteWrite(raw, s.MaxBody)
	if errors.Is(err, wire.ErrTooLarge) {
		return err
	}
	if err != nil {
		return badRequest("invalid remote-write body: %v", err)
	}
	samples, series, meta := wire.MetricRows(project, rw)
	if err := s.Sink.AddMetrics(samples, series, meta); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// decodeOTLP reads an OTLP/HTTP request in either of its two encodings and
// returns the content type, because the reply must use the same one.
func (s *Ingest) decodeOTLP(w http.ResponseWriter, r *http.Request, in proto.Message) (string, error) {
	raw, err := s.body(w, r)
	if err != nil {
		return "", err
	}
	data, err := wire.Decompress(r.Header.Get("Content-Encoding"), raw, s.MaxBody)
	if errors.Is(err, wire.ErrTooLarge) {
		return "", err
	}
	if err != nil {
		return "", badRequest("cannot decode body: %v", err)
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch ct {
	case "application/x-protobuf":
		if err := proto.Unmarshal(data, in); err != nil {
			return "", badRequest("invalid protobuf: %v", err)
		}
	case "application/json":
		data, err = wire.NormalizeOTLPJSON(data)
		if err != nil {
			return "", badRequest("invalid json: %v", err)
		}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, in); err != nil {
			return "", badRequest("invalid json: %v", err)
		}
	default:
		return "", httpError{http.StatusUnsupportedMediaType, "Content-Type must be application/x-protobuf or application/json"}
	}
	return ct, nil
}

// otlpQueue decodes, queues, then replies, so a refused request is never
// acknowledged.
func (s *Ingest) otlpQueue(w http.ResponseWriter, r *http.Request, in, out proto.Message, queue func() error) error {
	ct, err := s.decodeOTLP(w, r, in)
	if err != nil {
		return err
	}
	if err := queue(); err != nil {
		return err
	}
	var b []byte
	if ct == "application/json" {
		b, err = protojson.Marshal(out)
	} else {
		b, err = proto.Marshal(out)
	}
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", ct)
	_, _ = w.Write(b)
	return nil
}

func (s *Ingest) traces(project string, w http.ResponseWriter, r *http.Request) error {
	req := &tracesv1.ExportTraceServiceRequest{}
	return s.otlpQueue(w, r, req, &tracesv1.ExportTraceServiceResponse{}, func() error {
		return s.Sink.AddSpans(wire.Spans(project, req))
	})
}

func (s *Ingest) logs(project string, w http.ResponseWriter, r *http.Request) error {
	req := &logsv1.ExportLogsServiceRequest{}
	return s.otlpQueue(w, r, req, &logsv1.ExportLogsServiceResponse{}, func() error {
		return s.Sink.AddLogs(wire.Logs(project, req))
	})
}

func (s *Ingest) profiles(project string, w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	service := q.Get("service.name")
	if service == "" {
		return badRequest("service.name is required")
	}
	labels := map[string]string{}
	for k, vs := range q {
		if k != "service.name" && len(vs) == 1 {
			labels[k] = vs[0]
		}
	}
	raw, err := s.body(w, r)
	if err != nil {
		return err
	}
	data, err := wire.Decompress(r.Header.Get("Content-Encoding"), raw, s.MaxBody)
	if errors.Is(err, wire.ErrTooLarge) {
		return err
	}
	if err != nil {
		return badRequest("cannot decode body: %v", err)
	}
	p, err := profile.ParseData(data) // also accepts gzip, which pprof clients send
	if err != nil {
		return badRequest("invalid pprof profile: %v", err)
	}
	samples, stacks := wire.ProfileRows(project, service, labels, p)
	if err := s.Sink.AddProfile(samples, stacks); err != nil {
		return err
	}
	// 200, not 204: the node-agent counts any status other than 200 as a failed
	// upload (profiling.go post), although it accepts any 2xx for metrics.
	w.WriteHeader(http.StatusOK)
	return nil
}
