package server

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/dbtelemetry"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/store"
)

// DatabaseWriter stores what a database collector sent.
type DatabaseWriter interface {
	WriteDatabaseBatch(ctx context.Context, project string, b dbtelemetry.Batch) error
}

const maxDatabaseBody = 4 << 20

// databases takes one batch from a database collector (POST /v1/db, with the project's ingest key). The batch is checked before anything is
// stored (dbtelemetry.Batch.Validate), and a failure to store it is a 500 so that the collector's queue tries again. The answer says how much
// was kept.
func (s *Ingest) databases(project string, w http.ResponseWriter, r *http.Request) error {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return httpError{http.StatusUnsupportedMediaType, "send application/json"}
	}
	limit := s.MaxBody
	if limit <= 0 || limit > maxDatabaseBody {
		limit = maxDatabaseBody
	}
	raw, err := readLimited(w, r, limit)
	if err != nil {
		return err
	}
	var b dbtelemetry.Batch
	if err := json.Unmarshal(raw, &b); err != nil {
		return badRequest("the body is not a batch: %s", clip(err.Error(), 200))
	}
	if err := b.Validate(time.Now()); err != nil {
		return badRequest("%v", err)
	}
	if err := s.Databases.WriteDatabaseBatch(r.Context(), project, b); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]int{"statements": len(b.Statements), "slow_queries": len(b.SlowQueries)}})
	return nil
}

func readLimited(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---- reading what the databases reported ----

func (q *Query) targetParam(r *http.Request) (string, error) {
	t := r.PathValue("target")
	if !dbtelemetry.ValidTarget(t) {
		return "", paramError("target is not a database name")
	}
	return t, nil
}

func (q *Query) listDatabases(ctx context.Context, project string, _ http.ResponseWriter, _ *http.Request) (any, error) {
	return q.Reader.ListDatabases(ctx, project)
}

func (q *Query) databaseStatements(ctx context.Context, project string, _ http.ResponseWriter, r *http.Request) (any, error) {
	target, err := q.targetParam(r)
	if err != nil {
		return nil, err
	}
	from, to, err := q.timeRange(r)
	if err != nil {
		return nil, err
	}
	limit, err := intParam(r, "limit", 50, 1, 200)
	if err != nil {
		return nil, err
	}
	v := r.URL.Query()
	order := v.Get("order")
	if order == "" {
		order = "total_ms"
	}
	ok := false
	for _, o := range store.StatementOrders() {
		ok = ok || o == order
	}
	if !ok {
		return nil, paramError("order must be one of " + strings.Join(store.StatementOrders(), ", "))
	}
	return q.Reader.TopStatements(ctx, store.StatementQuery{Project: project, Target: target, From: from, To: to, Order: order, Limit: limit, Database: v.Get("database"), User: v.Get("user")})
}

// seriesParams reads the target, the range and the step of a time series: about 300 points however long the range, never finer than the
// 10 s the collector samples at, unless a step is asked for.
func (q *Query) seriesParams(r *http.Request) (target string, from, to time.Time, step int, err error) {
	if target, err = q.targetParam(r); err != nil {
		return
	}
	if from, to, err = q.timeRange(r); err != nil {
		return
	}
	step = int(to.Sub(from).Seconds()/300) + 1
	if step < 10 {
		step = 10
	}
	if s := r.URL.Query().Get("step"); s != "" {
		n, perr := strconv.Atoi(s)
		if perr != nil || n < 1 || n > 86400 {
			err = paramError("step must be a number of seconds from 1 to 86400")
			return
		}
		if int(to.Sub(from).Seconds())/n > q.MaxPoints/10+1 && q.MaxPoints > 0 {
			err = paramError("step is too small for this range")
			return
		}
		step = n
	}
	return
}

func (q *Query) databaseActivity(ctx context.Context, project string, _ http.ResponseWriter, r *http.Request) (any, error) {
	target, from, to, step, err := q.seriesParams(r)
	if err != nil {
		return nil, err
	}
	return q.Reader.ActivitySeries(ctx, project, target, from, to, step)
}

// databasePools: a connection pooler's pools: the peak of the clients, and of those waiting, in each bucket; the pools in the newest sample;
// and what went through each database over the window.
func (q *Query) databasePools(ctx context.Context, project string, _ http.ResponseWriter, r *http.Request) (any, error) {
	target, from, to, step, err := q.seriesParams(r)
	if err != nil {
		return nil, err
	}
	return q.Reader.PoolSeries(ctx, project, target, from, to, step)
}

func (q *Query) databaseSlow(ctx context.Context, project string, _ http.ResponseWriter, r *http.Request) (any, error) {
	target, err := q.targetParam(r)
	if err != nil {
		return nil, err
	}
	from, to, err := q.timeRange(r)
	if err != nil {
		return nil, err
	}
	limit, err := intParam(r, "limit", 100, 1, 500)
	if err != nil {
		return nil, err
	}
	f := store.SlowQueryFilter{Project: project, Target: target, From: from, To: to, Limit: limit}
	if s := r.URL.Query().Get("min_ms"); s != "" {
		n, err := strconv.ParseFloat(s, 64)
		if err != nil || n < 0 || n > 1e9 {
			return nil, paramError("min_ms must be a number of milliseconds")
		}
		f.MinMs = n
	}
	switch r.URL.Query().Get("order") {
	case "", "time":
	case "duration":
		f.Slowest = true
	default:
		return nil, paramError("order must be time or duration")
	}
	return q.Reader.SlowQueries(ctx, f)
}
