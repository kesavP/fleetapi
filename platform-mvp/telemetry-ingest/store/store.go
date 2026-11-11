// Package store is the ClickHouse side: connection, schema, batch writers and
// the queries behind the read API.
package store

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/model"
)

type TTLs struct{ Metrics, Traces, Logs, Profiles, Databases, Inventory time.Duration }

type Options struct {
	Addrs          []string
	Database       string // validated by config: it is spliced into CREATE DATABASE
	User           string
	Password       string
	TLS            bool
	CreateDatabase bool
	TTL            TTLs
	Tier           Tiering
}

// Tiering moves the big tables' old data to a slow volume. The zero value is no tiering. The names are spliced into SQL, so the
// config validates them; Policy and Volume are set up in the ClickHouse server's own configuration.
type Tiering struct {
	Policy string        // storage policy for the big tables; "" = leave them on the default disk
	Volume string        // the policy's slow volume
	After  time.Duration // data older than this moves; 0 = never
}

type Store struct {
	conn      driver.Conn
	ttl       TTLs
	tier      Tiering
	database  string // for the information page: where the tables are
	tls       bool
	closeOnce sync.Once
	closeErr  error
}

func connect(o Options, database string) (driver.Conn, error) {
	opt := &clickhouse.Options{
		Addr:         o.Addrs,
		Auth:         clickhouse.Auth{Database: database, Username: o.User, Password: o.Password},
		DialTimeout:  10 * time.Second,
		ReadTimeout:  60 * time.Second,
		MaxOpenConns: 10,
		Compression:  &clickhouse.Compression{Method: clickhouse.CompressionLZ4},
	}
	if o.TLS {
		opt.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return clickhouse.Open(opt)
}

// Open connects, creating the database first when asked to.
func Open(ctx context.Context, o Options) (*Store, error) {
	if o.CreateDatabase {
		c, err := connect(o, "default")
		if err != nil {
			return nil, err
		}
		err = c.Exec(ctx, "CREATE DATABASE IF NOT EXISTS "+o.Database)
		_ = c.Close()
		if err != nil {
			return nil, fmt.Errorf("create database %s: %w", o.Database, err)
		}
	}
	conn, err := connect(o, o.Database)
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &Store{conn: conn, ttl: o.TTL, tier: o.Tier, database: o.Database, tls: o.TLS}, nil
}

func (s *Store) Ping(ctx context.Context) error { return s.conn.Ping(ctx) }

// Close is safe to call more than once: the driver blocks on a second close.
func (s *Store) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.conn.Close() })
	return s.closeErr
}

func insert[T any](ctx context.Context, s *Store, query string, rows []T, vals func(T) []any) error {
	if len(rows) == 0 {
		return nil
	}
	b, err := s.conn.PrepareBatch(ctx, query)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := b.Append(vals(r)...); err != nil {
			_ = b.Abort()
			return err
		}
	}
	return b.Send()
}

// ClickHouse Map and Array columns want values, not nils.
func mp(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func strs(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func times(t []time.Time) []time.Time {
	if t == nil {
		return []time.Time{}
	}
	return t
}

func maps(m []map[string]string) []map[string]string {
	if m == nil {
		return []map[string]string{}
	}
	return m
}

func (s *Store) WriteSamples(ctx context.Context, rows []model.Sample) error {
	return insert(ctx, s, "INSERT INTO metrics_samples (Project, MetricName, SeriesHash, Timestamp, Value)", rows,
		func(r model.Sample) []any { return []any{r.Project, r.Name, r.Hash, r.Time, r.Value} })
}

func (s *Store) WriteSeries(ctx context.Context, rows []model.Series) error {
	return insert(ctx, s, "INSERT INTO metrics_series (Project, MetricName, SeriesHash, Labels, LastSeen)", rows,
		func(r model.Series) []any { return []any{r.Project, r.Name, r.Hash, mp(r.Labels), r.Seen} })
}

func (s *Store) WriteMetricMeta(ctx context.Context, rows []model.MetricMeta) error {
	return insert(ctx, s, "INSERT INTO metrics_metadata (Project, MetricFamilyName, Type, Help, Unit)", rows,
		func(r model.MetricMeta) []any { return []any{r.Project, r.Family, r.Type, r.Help, r.Unit} })
}

func (s *Store) WriteSpans(ctx context.Context, rows []model.Span) error {
	return insert(ctx, s, `INSERT INTO traces_spans (Project, Timestamp, TraceId, SpanId, ParentSpanId, TraceState,
SpanName, SpanKind, ServiceName, DurationNs, StatusCode, StatusMessage, ResourceAttributes, SpanAttributes,
EventTimestamps, EventNames, EventAttributes, LinkTraceIds, LinkSpanIds, LinkAttributes)`, rows,
		func(r model.Span) []any {
			return []any{r.Project, r.Time, r.TraceID, r.SpanID, r.ParentSpanID, r.TraceState,
				r.Name, r.Kind, r.Service, r.DurationNs, r.StatusCode, r.StatusMessage, mp(r.ResourceAttrs), mp(r.SpanAttrs),
				times(r.EventTimes), strs(r.EventNames), maps(r.EventAttrs), strs(r.LinkTraceIDs), strs(r.LinkSpanIDs), maps(r.LinkAttrs)}
		})
}

func (s *Store) WriteLogs(ctx context.Context, rows []model.LogRecord) error {
	return insert(ctx, s, `INSERT INTO logs_records (Project, Timestamp, TraceId, SpanId, SeverityText, SeverityNumber,
ServiceName, Body, ResourceAttributes, LogAttributes)`, rows,
		func(r model.LogRecord) []any {
			return []any{r.Project, r.Time, r.TraceID, r.SpanID, r.SeverityText, r.SeverityNumber,
				r.Service, r.Body, mp(r.ResourceAttrs), mp(r.LogAttrs)}
		})
}

func (s *Store) WriteProfileSamples(ctx context.Context, rows []model.ProfileSample) error {
	return insert(ctx, s, "INSERT INTO profiling_samples (Project, ServiceName, Type, Start, End, Labels, Value, StackHash)", rows,
		func(r model.ProfileSample) []any {
			return []any{r.Project, r.Service, r.Type, r.Start, r.End, mp(r.Labels), r.Value, r.StackHash}
		})
}

func (s *Store) WriteStacks(ctx context.Context, rows []model.Stack) error {
	return insert(ctx, s, "INSERT INTO profiling_stacks (Project, StackHash, Frames, LastSeen)", rows,
		func(r model.Stack) []any { return []any{r.Project, r.Hash, strs(r.Frames), r.Seen} })
}
