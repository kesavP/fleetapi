package store

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/model"
)

// Reader is what the query API needs. Every method takes the project first
// and puts it in the WHERE clause, so one project can never see another's rows.
type Reader interface {
	Ping(ctx context.Context) error
	ListMetrics(ctx context.Context, project string) ([]string, error)
	ListSeries(ctx context.Context, project, name string, match map[string]string, limit int) ([]SeriesInfo, error)
	QueryMetric(ctx context.Context, q MetricQuery) (MetricResult, error)
	TraceServices(ctx context.Context, project string, from, to time.Time) ([]string, error)
	SearchSpans(ctx context.Context, q SpanQuery) ([]model.Span, error)
	GetTrace(ctx context.Context, project, traceID string) ([]model.Span, error)
	LogServices(ctx context.Context, project string, from, to time.Time) ([]string, error)
	SearchLogs(ctx context.Context, q LogQuery) ([]model.LogRecord, error)
	ListProfiles(ctx context.Context, project string) ([]ProfileService, error)
	TopStacks(ctx context.Context, q ProfileQuery) ([]StackValue, error)
	// ClickHouseInfo: how the service is connected and how much this project stores. `share` adds whole-table sizes and the server's disks.
	ClickHouseInfo(ctx context.Context, project string, share bool) (ClickHouseInfo, error)
	// What the database collectors report (see dbtelemetry): the targets, their statement statistics, their sessions, their slow statements.
	ListDatabases(ctx context.Context, project string) ([]DBTarget, error)
	TopStatements(ctx context.Context, q StatementQuery) (StatementsResult, error)
	ActivitySeries(ctx context.Context, project, target string, from, to time.Time, step int) (ActivityResult, error)
	SlowQueries(ctx context.Context, f SlowQueryFilter) ([]SlowQueryRow, error)
	PoolSeries(ctx context.Context, project, target string, from, to time.Time, step int) (PoolsResult, error)
	// What the cluster collectors report (see inventory): what is in each cluster, how an object changed, and its events.
	InventorySummary(ctx context.Context, project string) ([]InventoryCluster, error)
	ListInventory(ctx context.Context, q InventoryQuery) ([]InventoryObject, error)
	InventoryHistory(ctx context.Context, project, cluster, kind, uid string, limit int) ([]InventoryVersion, error)
	ListInventoryEvents(ctx context.Context, q InventoryEventQuery) ([]InventoryEvent, error)
}

type SeriesInfo struct {
	Labels map[string]string `json:"labels"`
}

type Point struct {
	T int64   `json:"t"` // unix milliseconds
	V float64 `json:"v"`
}

type MetricSeries struct {
	Labels map[string]string `json:"labels"`
	Points []Point           `json:"points"`
}

type MetricQuery struct {
	Project  string
	Name     string
	Match    map[string]string
	From, To time.Time
	StepSec  int  // 0 = raw samples
	Rate     bool // per-second rate of a counter; needs StepSec
	// FromZero, with Rate, treats a series that was created inside the window as a
	// counter that started at zero there, so its first value counts as an increase.
	// Without it, a rate needs two readings and a counter's first appearance (an
	// error counter's first error, say) is invisible.
	FromZero  bool
	MaxSeries int
	MaxPoints int
}

type MetricResult struct {
	Series    []MetricSeries `json:"series"`
	Truncated bool           `json:"truncated"`
}

type SpanQuery struct {
	Project, Service, Name, Status string
	From, To                       time.Time
	MinDurationNs                  uint64
	RootOnly                       bool
	Limit                          int
}

type LogQuery struct {
	Project, Service, Severity, Contains, TraceID string
	From, To                                      time.Time
	Limit                                         int
}

type ProfileService struct {
	Service  string    `json:"service"`
	Type     string    `json:"type"`
	LastSeen time.Time `json:"last_seen"`
}

type ProfileQuery struct {
	Project, Service, Type string
	From, To               time.Time
	Limit                  int
}

type StackValue struct {
	Frames []string `json:"frames"` // leaf first
	Value  int64    `json:"value"`
}

func ms(t time.Time) int64 { return t.UnixMilli() }

// inList renders hashes for an IN clause. They are uint64 values read from
// ClickHouse, so formatting them directly cannot inject anything.
func inList(h []uint64) string {
	var b strings.Builder
	for i, v := range h {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatUint(v, 10))
	}
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	k := make([]string, 0, len(m))
	for key := range m {
		k = append(k, key)
	}
	sort.Strings(k)
	return k
}

func (s *Store) ListMetrics(ctx context.Context, project string) ([]string, error) {
	rows, err := s.conn.Query(ctx, "SELECT DISTINCT MetricName FROM metrics_series WHERE Project = ? ORDER BY MetricName LIMIT 20000", project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

type seriesRow struct {
	hash   uint64
	labels map[string]string
}

func (s *Store) series(ctx context.Context, project, name string, match map[string]string, limit int) ([]seriesRow, error) {
	q := "SELECT SeriesHash, argMax(Labels, LastSeen) FROM metrics_series WHERE Project = ? AND MetricName = ?"
	args := []any{project, name}
	for _, k := range sortedKeys(match) {
		q += " AND Labels[?] = ?"
		args = append(args, k, match[k])
	}
	q += fmt.Sprintf(" GROUP BY SeriesHash ORDER BY SeriesHash LIMIT %d", limit+1)
	rows, err := s.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []seriesRow
	for rows.Next() {
		var r seriesRow
		if err := rows.Scan(&r.hash, &r.labels); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) ListSeries(ctx context.Context, project, name string, match map[string]string, limit int) ([]SeriesInfo, error) {
	rs, err := s.series(ctx, project, name, match, limit)
	if err != nil {
		return nil, err
	}
	if len(rs) > limit {
		rs = rs[:limit]
	}
	out := make([]SeriesInfo, 0, len(rs))
	for _, r := range rs {
		out = append(out, SeriesInfo{Labels: r.labels})
	}
	return out, nil
}

func (s *Store) QueryMetric(ctx context.Context, q MetricQuery) (MetricResult, error) {
	res := MetricResult{Series: []MetricSeries{}} // [] rather than null when nothing matches
	rs, err := s.series(ctx, q.Project, q.Name, q.Match, q.MaxSeries)
	if err != nil {
		return res, err
	}
	if len(rs) > q.MaxSeries {
		rs, res.Truncated = rs[:q.MaxSeries], true
	}
	if len(rs) == 0 {
		return res, nil
	}
	byHash := make(map[uint64]*MetricSeries, len(rs))
	hashes := make([]uint64, 0, len(rs))
	for _, r := range rs {
		byHash[r.hash] = &MetricSeries{Labels: r.labels, Points: []Point{}}
		hashes = append(hashes, r.hash)
	}

	// When a series was first stored, only needed for FromZero. A series counts as born in
	// the window only if its first sample ever (within retention) is inside it: a counter
	// that already existed holds a large accumulated value, which must not become a spike.
	var firstSample map[uint64]int64
	if q.Rate && q.FromZero {
		if firstSample, err = s.firstSamples(ctx, q.Project, q.Name, q.To, hashes); err != nil {
			return res, err
		}
	}

	valueExpr, timeExpr := "Value", "toUnixTimestamp64Milli(Timestamp)"
	group := ""
	if q.StepSec > 0 {
		stepMs := int64(q.StepSec) * 1000
		timeExpr = fmt.Sprintf("intDiv(toUnixTimestamp64Milli(Timestamp), %d) * %d", stepMs, stepMs)
		valueExpr = "avg(Value)"
		if q.Rate {
			valueExpr = "argMax(Value, Timestamp)" // a counter's last reading in the bucket
		}
		group = " GROUP BY SeriesHash, t"
	}
	query := fmt.Sprintf(`SELECT SeriesHash, %s AS t, %s FROM metrics_samples
WHERE Project = ? AND MetricName = ? AND SeriesHash IN (%s)
AND Timestamp >= fromUnixTimestamp64Milli(?) AND Timestamp <= fromUnixTimestamp64Milli(?)%s
ORDER BY SeriesHash, t LIMIT %d`, timeExpr, valueExpr, inList(hashes), group, q.MaxPoints+1)
	rows, err := s.conn.Query(ctx, query, q.Project, q.Name, ms(q.From), ms(q.To))
	if err != nil {
		return res, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var h uint64
		var t int64
		var v float64
		if err := rows.Scan(&h, &t, &v); err != nil {
			return res, err
		}
		if n++; n > q.MaxPoints {
			res.Truncated = true
			break
		}
		if sr := byHash[h]; sr != nil {
			sr.Points = append(sr.Points, Point{T: t, V: v})
		}
	}
	if err := rows.Err(); err != nil {
		return res, err
	}
	for _, r := range rs {
		sr := byHash[r.hash]
		if q.Rate {
			if first, known := firstSample[r.hash]; q.FromZero && known && first >= ms(q.From) {
				sr.Points = RateFromZero(sr.Points, int64(q.StepSec)*1000)
			} else {
				sr.Points = Rate(sr.Points)
			}
		}
		sr.Points = finite(sr.Points)
		if len(sr.Points) > 0 {
			res.Series = append(res.Series, *sr)
		}
	}
	return res, nil
}

// firstSamples returns, in unix milliseconds, the time of each series' first stored
// sample. The sort key (project, metric, series, time) makes this a read of just those
// series.
func (s *Store) firstSamples(ctx context.Context, project, name string, to time.Time, hashes []uint64) (map[uint64]int64, error) {
	rows, err := s.conn.Query(ctx, fmt.Sprintf(`SELECT SeriesHash, toUnixTimestamp64Milli(min(Timestamp)) FROM metrics_samples
WHERE Project = ? AND MetricName = ? AND SeriesHash IN (%s) AND Timestamp <= fromUnixTimestamp64Milli(?) GROUP BY SeriesHash`, inList(hashes)),
		project, name, ms(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uint64]int64, len(hashes))
	for rows.Next() {
		var h uint64
		var t int64
		if err := rows.Scan(&h, &t); err != nil {
			return nil, err
		}
		out[h] = t
	}
	return out, rows.Err()
}

// RateFromZero is Rate for a counter known to have started at zero just before its
// first reading: that reading is an increase that happened within one step, so it
// becomes a rate of value/step at the first point instead of being dropped.
func RateFromZero(p []Point, stepMs int64) []Point {
	if len(p) == 0 || stepMs <= 0 {
		return Rate(p)
	}
	return Rate(append([]Point{{T: p[0].T - stepMs, V: 0}}, p...))
}

// Rate converts readings of a counter into per-second rates. A reading lower
// than the one before it means the counter restarted from zero, so the new
// value itself is the increase.
func Rate(p []Point) []Point {
	if len(p) < 2 {
		return nil
	}
	out := make([]Point, 0, len(p)-1)
	for i := 1; i < len(p); i++ {
		dt := float64(p[i].T-p[i-1].T) / 1000
		if dt <= 0 {
			continue
		}
		dv := p[i].V - p[i-1].V
		if dv < 0 {
			dv = p[i].V
		}
		out = append(out, Point{T: p[i].T, V: dv / dt})
	}
	return out
}

// JSON cannot carry NaN or Inf, and Prometheus stale markers are NaN.
func finite(p []Point) []Point {
	out := p[:0]
	for _, x := range p {
		if !math.IsNaN(x.V) && !math.IsInf(x.V, 0) {
			out = append(out, x)
		}
	}
	return out
}

const spanCols = `Timestamp, TraceId, SpanId, ParentSpanId, TraceState, SpanName, SpanKind, ServiceName, DurationNs,
StatusCode, StatusMessage, ResourceAttributes, SpanAttributes, EventTimestamps, EventNames, EventAttributes,
LinkTraceIds, LinkSpanIds, LinkAttributes`

func scanSpans(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}, project string) ([]model.Span, error) {
	var out []model.Span
	for rows.Next() {
		sp := model.Span{Project: project}
		if err := rows.Scan(&sp.Time, &sp.TraceID, &sp.SpanID, &sp.ParentSpanID, &sp.TraceState, &sp.Name, &sp.Kind,
			&sp.Service, &sp.DurationNs, &sp.StatusCode, &sp.StatusMessage, &sp.ResourceAttrs, &sp.SpanAttrs,
			&sp.EventTimes, &sp.EventNames, &sp.EventAttrs, &sp.LinkTraceIDs, &sp.LinkSpanIDs, &sp.LinkAttrs); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

func (s *Store) SearchSpans(ctx context.Context, q SpanQuery) ([]model.Span, error) {
	where := "Project = ? AND Timestamp >= fromUnixTimestamp64Nano(?) AND Timestamp <= fromUnixTimestamp64Nano(?)"
	args := []any{q.Project, q.From.UnixNano(), q.To.UnixNano()}
	if q.Service != "" {
		where += " AND ServiceName = ?"
		args = append(args, q.Service)
	}
	if q.Name != "" {
		where += " AND SpanName = ?"
		args = append(args, q.Name)
	}
	if q.Status != "" {
		where += " AND StatusCode = ?"
		args = append(args, q.Status)
	}
	if q.MinDurationNs > 0 {
		where += " AND DurationNs >= ?"
		args = append(args, q.MinDurationNs)
	}
	if q.RootOnly {
		where += " AND ParentSpanId = ''"
	}
	rows, err := s.conn.Query(ctx, fmt.Sprintf("SELECT %s FROM traces_spans WHERE %s ORDER BY Timestamp DESC LIMIT %d", spanCols, where, q.Limit), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSpans(rows, q.Project)
}

func (s *Store) GetTrace(ctx context.Context, project, traceID string) ([]model.Span, error) {
	rows, err := s.conn.Query(ctx, fmt.Sprintf("SELECT %s FROM traces_spans WHERE Project = ? AND TraceId = ? ORDER BY Timestamp LIMIT 5000", spanCols), project, traceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSpans(rows, project)
}

func (s *Store) services(ctx context.Context, table, project string, from, to time.Time) ([]string, error) {
	// table is one of two constants below, never caller input.
	rows, err := s.conn.Query(ctx, fmt.Sprintf(`SELECT DISTINCT ServiceName FROM %s WHERE Project = ?
AND Timestamp >= fromUnixTimestamp64Nano(?) AND Timestamp <= fromUnixTimestamp64Nano(?) ORDER BY ServiceName LIMIT 5000`, table),
		project, from.UnixNano(), to.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) TraceServices(ctx context.Context, project string, from, to time.Time) ([]string, error) {
	return s.services(ctx, "traces_spans", project, from, to)
}

func (s *Store) LogServices(ctx context.Context, project string, from, to time.Time) ([]string, error) {
	return s.services(ctx, "logs_records", project, from, to)
}

func (s *Store) SearchLogs(ctx context.Context, q LogQuery) ([]model.LogRecord, error) {
	where := "Project = ? AND Timestamp >= fromUnixTimestamp64Nano(?) AND Timestamp <= fromUnixTimestamp64Nano(?)"
	args := []any{q.Project, q.From.UnixNano(), q.To.UnixNano()}
	if q.Service != "" {
		where += " AND ServiceName = ?"
		args = append(args, q.Service)
	}
	if q.Severity != "" {
		where += " AND SeverityText = ?"
		args = append(args, strings.ToUpper(q.Severity))
	}
	if q.TraceID != "" {
		where += " AND TraceId = ?"
		args = append(args, q.TraceID)
	}
	if q.Contains != "" {
		where += " AND positionCaseInsensitive(Body, ?) > 0"
		args = append(args, q.Contains)
	}
	rows, err := s.conn.Query(ctx, fmt.Sprintf(`SELECT Timestamp, TraceId, SpanId, SeverityText, SeverityNumber, ServiceName, Body,
ResourceAttributes, LogAttributes FROM logs_records WHERE %s ORDER BY Timestamp DESC LIMIT %d`, where, q.Limit), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.LogRecord
	for rows.Next() {
		l := model.LogRecord{Project: q.Project}
		if err := rows.Scan(&l.Time, &l.TraceID, &l.SpanID, &l.SeverityText, &l.SeverityNumber, &l.Service, &l.Body, &l.ResourceAttrs, &l.LogAttrs); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) ListProfiles(ctx context.Context, project string) ([]ProfileService, error) {
	rows, err := s.conn.Query(ctx, `SELECT ServiceName, Type, max(LastSeen) FROM profiling_services WHERE Project = ?
GROUP BY ServiceName, Type ORDER BY ServiceName, Type LIMIT 5000`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProfileService
	for rows.Next() {
		var p ProfileService
		if err := rows.Scan(&p.Service, &p.Type, &p.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// TopStacks returns the stacks with the largest summed value in the window.
func (s *Store) TopStacks(ctx context.Context, q ProfileQuery) ([]StackValue, error) {
	rows, err := s.conn.Query(ctx, fmt.Sprintf(`SELECT StackHash, sum(Value) AS v FROM profiling_samples
WHERE Project = ? AND ServiceName = ? AND Type = ?
AND Start >= fromUnixTimestamp64Nano(?) AND Start <= fromUnixTimestamp64Nano(?)
GROUP BY StackHash ORDER BY v DESC LIMIT %d`, q.Limit), q.Project, q.Service, q.Type, q.From.UnixNano(), q.To.UnixNano())
	if err != nil {
		return nil, err
	}
	var hashes []uint64
	vals := map[uint64]int64{}
	for rows.Next() {
		var h uint64
		var v int64
		if err := rows.Scan(&h, &v); err != nil {
			rows.Close()
			return nil, err
		}
		hashes = append(hashes, h)
		vals[h] = v
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(hashes) == 0 {
		return nil, nil
	}
	srows, err := s.conn.Query(ctx, fmt.Sprintf("SELECT StackHash, argMax(Frames, LastSeen) FROM profiling_stacks WHERE Project = ? AND StackHash IN (%s) GROUP BY StackHash", inList(hashes)), q.Project)
	if err != nil {
		return nil, err
	}
	defer srows.Close()
	frames := map[uint64][]string{}
	for srows.Next() {
		var h uint64
		var f []string
		if err := srows.Scan(&h, &f); err != nil {
			return nil, err
		}
		frames[h] = f
	}
	if err := srows.Err(); err != nil {
		return nil, err
	}
	out := make([]StackValue, 0, len(hashes))
	for _, h := range hashes { // already in descending value order
		f, ok := frames[h]
		if !ok {
			f = []string{"(stack not found)"}
		}
		out = append(out, StackValue{Frames: f, Value: vals[h]})
	}
	return out, nil
}
