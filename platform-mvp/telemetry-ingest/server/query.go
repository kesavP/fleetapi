package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/store"
)

const (
	queryTimeout = 30 * time.Second
	maxMatchers  = 20
	maxStepSec   = 86400
)

// Query serves the read API. A read key belongs to one project and only that
// project's path answers to it; the project then goes into every SQL WHERE.
type Query struct {
	Projects     Resolver
	Reader       store.Reader
	Log          *slog.Logger
	DefaultRange time.Duration
	MaxSeries    int
	MaxPoints    int
	// ShareClusterInfo adds the whole-table sizes and the server's disks to /clickhouse. They describe the platform every project shares, so
	// they are off unless the operator asks for them (--expose-cluster-info).
	ShareClusterInfo bool
}

type paramError string

func (e paramError) Error() string { return string(e) }

func (q *Query) Register(mux *http.ServeMux) {
	const base = "GET /api/v1/projects/{project}/"
	mux.HandleFunc(base+"metrics", q.wrap(q.listMetrics))
	mux.HandleFunc(base+"metrics/series", q.wrap(q.listSeries))
	mux.HandleFunc(base+"metrics/query_range", q.wrap(q.queryRange))
	mux.HandleFunc(base+"traces/services", q.wrap(q.traceServices))
	mux.HandleFunc(base+"traces", q.wrap(q.searchSpans))
	mux.HandleFunc(base+"traces/{trace_id}", q.wrap(q.getTrace))
	mux.HandleFunc(base+"logs/services", q.wrap(q.logServices))
	mux.HandleFunc(base+"logs", q.wrap(q.searchLogs))
	mux.HandleFunc(base+"profiles", q.wrap(q.listProfiles))
	mux.HandleFunc(base+"profiles/top", q.wrap(q.topStacks))
	mux.HandleFunc(base+"clickhouse", q.wrap(q.clickhouseInfo))
	mux.HandleFunc(base+"databases", q.wrap(q.listDatabases))
	mux.HandleFunc(base+"databases/{target}/statements", q.wrap(q.databaseStatements))
	mux.HandleFunc(base+"databases/{target}/activity", q.wrap(q.databaseActivity))
	mux.HandleFunc(base+"databases/{target}/slow", q.wrap(q.databaseSlow))
	mux.HandleFunc(base+"databases/{target}/pools", q.wrap(q.databasePools))
}

type queryFunc func(ctx context.Context, project string, w http.ResponseWriter, r *http.Request) (any, error)

// wrap authenticates. A key that is valid for another project, or for none,
// gets the same 403 for any project path, so the answer does not reveal which
// projects exist.
func (q *Query) wrap(fn queryFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := apiKey(r)
		if key == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing API key"})
			return
		}
		project, ok := q.Projects.ReadProject(key)
		if !ok || project != r.PathValue("project") {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "this key cannot read that project"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
		defer cancel()
		out, err := fn(ctx, project, w, r)
		var pe paramError
		switch {
		case errors.As(err, &pe):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": pe.Error()})
		case err != nil:
			q.Log.Error("query failed", "project", project, "path", r.URL.Path, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "query failed"})
		case out != nil:
			writeJSON(w, http.StatusOK, map[string]any{"data": emptyIfNil(out)})
		}
	}
}

func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return time.Time{}, fmt.Errorf("%q is neither RFC 3339 nor unix seconds", s)
	}
	return time.Unix(0, int64(f*1e9)).UTC(), nil
}

func (q *Query) timeRange(r *http.Request) (time.Time, time.Time, error) {
	v := r.URL.Query()
	to := time.Now().UTC()
	if s := v.Get("to"); s != "" {
		t, err := parseTime(s)
		if err != nil {
			return to, to, paramError("to: " + err.Error())
		}
		to = t
	}
	from := to.Add(-q.DefaultRange)
	if s := v.Get("from"); s != "" {
		t, err := parseTime(s)
		if err != nil {
			return from, to, paramError("from: " + err.Error())
		}
		from = t
	}
	if !from.Before(to) {
		return from, to, paramError("from must be before to")
	}
	return from, to, nil
}

func intParam(r *http.Request, name string, def, min, max int) (int, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < min || n > max {
		return 0, paramError(fmt.Sprintf("%s must be an integer from %d to %d", name, min, max))
	}
	return n, nil
}

func required(r *http.Request, name string) (string, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return "", paramError(name + " is required")
	}
	return v, nil
}

// matchers reads match.<label>=<value> pairs.
func matchers(r *http.Request) (map[string]string, error) {
	m := map[string]string{}
	for k, vs := range r.URL.Query() {
		if label, ok := strings.CutPrefix(k, "match."); ok && label != "" {
			if len(vs) != 1 {
				return nil, paramError(k + " may be given once")
			}
			m[label] = vs[0]
		}
	}
	if len(m) > maxMatchers {
		return nil, paramError(fmt.Sprintf("at most %d match.* parameters", maxMatchers))
	}
	return m, nil
}

func (q *Query) listMetrics(ctx context.Context, project string, _ http.ResponseWriter, _ *http.Request) (any, error) {
	return q.Reader.ListMetrics(ctx, project)
}

func (q *Query) listSeries(ctx context.Context, project string, _ http.ResponseWriter, r *http.Request) (any, error) {
	name, err := required(r, "name")
	if err != nil {
		return nil, err
	}
	m, err := matchers(r)
	if err != nil {
		return nil, err
	}
	limit, err := intParam(r, "limit", 100, 1, q.MaxSeries)
	if err != nil {
		return nil, err
	}
	return q.Reader.ListSeries(ctx, project, name, m, limit)
}

func (q *Query) queryRange(ctx context.Context, project string, _ http.ResponseWriter, r *http.Request) (any, error) {
	name, err := required(r, "name")
	if err != nil {
		return nil, err
	}
	m, err := matchers(r)
	if err != nil {
		return nil, err
	}
	from, to, err := q.timeRange(r)
	if err != nil {
		return nil, err
	}
	step, err := intParam(r, "step", 0, 0, maxStepSec)
	if err != nil {
		return nil, err
	}
	rate := r.URL.Query().Get("rate") == "true"
	if rate && step == 0 {
		return nil, paramError("rate=true needs a step (seconds)")
	}
	fromZero := r.URL.Query().Get("from_zero") == "true"
	if fromZero && !rate {
		return nil, paramError("from_zero=true only applies with rate=true")
	}
	return q.Reader.QueryMetric(ctx, store.MetricQuery{
		Project: project, Name: name, Match: m, From: from, To: to,
		StepSec: step, Rate: rate, FromZero: fromZero, MaxSeries: q.MaxSeries, MaxPoints: q.MaxPoints,
	})
}

func (q *Query) traceServices(ctx context.Context, project string, _ http.ResponseWriter, r *http.Request) (any, error) {
	from, to, err := q.timeRange(r)
	if err != nil {
		return nil, err
	}
	return q.Reader.TraceServices(ctx, project, from, to)
}

func (q *Query) searchSpans(ctx context.Context, project string, _ http.ResponseWriter, r *http.Request) (any, error) {
	from, to, err := q.timeRange(r)
	if err != nil {
		return nil, err
	}
	limit, err := intParam(r, "limit", 100, 1, 1000)
	if err != nil {
		return nil, err
	}
	minMs, err := intParam(r, "min_duration_ms", 0, 0, 24*3600*1000)
	if err != nil {
		return nil, err
	}
	v := r.URL.Query()
	status := strings.ToUpper(v.Get("status"))
	if status != "" && status != "OK" && status != "ERROR" && status != "UNSET" {
		return nil, paramError("status must be ok, error or unset")
	}
	return q.Reader.SearchSpans(ctx, store.SpanQuery{
		Project: project, Service: v.Get("service"), Name: v.Get("name"), Status: status,
		From: from, To: to, MinDurationNs: uint64(minMs) * 1_000_000,
		RootOnly: v.Get("root") == "true", Limit: limit,
	})
}

func (q *Query) getTrace(ctx context.Context, project string, _ http.ResponseWriter, r *http.Request) (any, error) {
	id := strings.ToLower(r.PathValue("trace_id"))
	if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		return nil, paramError("trace id must be 32 hex characters")
	}
	return q.Reader.GetTrace(ctx, project, id)
}

func (q *Query) logServices(ctx context.Context, project string, _ http.ResponseWriter, r *http.Request) (any, error) {
	from, to, err := q.timeRange(r)
	if err != nil {
		return nil, err
	}
	return q.Reader.LogServices(ctx, project, from, to)
}

func (q *Query) searchLogs(ctx context.Context, project string, _ http.ResponseWriter, r *http.Request) (any, error) {
	from, to, err := q.timeRange(r)
	if err != nil {
		return nil, err
	}
	limit, err := intParam(r, "limit", 100, 1, 1000)
	if err != nil {
		return nil, err
	}
	v := r.URL.Query()
	return q.Reader.SearchLogs(ctx, store.LogQuery{
		Project: project, Service: v.Get("service"), Severity: v.Get("severity"),
		Contains: v.Get("q"), TraceID: strings.ToLower(v.Get("trace_id")),
		From: from, To: to, Limit: limit,
	})
}

// clickhouseInfo: how the service is connected to ClickHouse and how much this project stores there. The address and credentials are not in
// the answer (see store.ClickHouseInfo).
func (q *Query) clickhouseInfo(ctx context.Context, project string, _ http.ResponseWriter, _ *http.Request) (any, error) {
	return q.Reader.ClickHouseInfo(ctx, project, q.ShareClusterInfo)
}

func (q *Query) listProfiles(ctx context.Context, project string, _ http.ResponseWriter, _ *http.Request) (any, error) {
	return q.Reader.ListProfiles(ctx, project)
}

// topStacks answers as JSON, or with format=folded as text that flamegraph
// tools read: root;...;leaf <value>, one stack per line.
func (q *Query) topStacks(ctx context.Context, project string, w http.ResponseWriter, r *http.Request) (any, error) {
	service, err := required(r, "service")
	if err != nil {
		return nil, err
	}
	typ, err := required(r, "type")
	if err != nil {
		return nil, err
	}
	from, to, err := q.timeRange(r)
	if err != nil {
		return nil, err
	}
	limit, err := intParam(r, "limit", 100, 1, 5000)
	if err != nil {
		return nil, err
	}
	stacks, err := q.Reader.TopStacks(ctx, store.ProfileQuery{Project: project, Service: service, Type: typ, From: from, To: to, Limit: limit})
	if err != nil {
		return nil, err
	}
	if r.URL.Query().Get("format") != "folded" {
		return stacks, nil
	}
	var b strings.Builder
	for _, s := range stacks {
		for i := len(s.Frames) - 1; i >= 0; i-- {
			b.WriteString(strings.NewReplacer(";", ":", "\n", " ").Replace(s.Frames[i]))
			if i > 0 {
				b.WriteByte(';')
			}
		}
		fmt.Fprintf(&b, " %d\n", s.Value)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
	return nil, nil
}

// emptyIfNil turns a nil slice into an empty one: Go marshals nil as null,
// and a client iterating a result expects [] when nothing matched.
func emptyIfNil(v any) any {
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Slice && rv.IsNil() {
		return reflect.MakeSlice(rv.Type(), 0, 0).Interface()
	}
	return v
}
