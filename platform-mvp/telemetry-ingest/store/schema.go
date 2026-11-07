package store

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// One database serves every project. Each table leads its sort key with
// Project, so a project's data is contiguous on disk and a query for one
// project never reads another's granules.
//
// Metrics are split in two: metrics_samples is narrow (hash, time, value) and
// metrics_series stores each label set once. Putting the labels on every
// sample row would multiply storage by the label size for no benefit.
//
// migrations is append-only: a released entry is never edited, because it has
// already been applied somewhere. A change to a table is a new entry.
var migrations = [][]string{
	{ // 1: initial schema
		`CREATE TABLE IF NOT EXISTS metrics_samples (
	Project    LowCardinality(String),
	MetricName LowCardinality(String),
	SeriesHash UInt64,
	Timestamp  DateTime64(3, 'UTC') CODEC(Delta, ZSTD(1)),
	Value      Float64 CODEC(Gorilla, ZSTD(1))
) ENGINE = MergeTree
PARTITION BY toDate(Timestamp)
ORDER BY (Project, MetricName, SeriesHash, Timestamp)
SETTINGS ttl_only_drop_parts = 1`,

		`CREATE TABLE IF NOT EXISTS metrics_series (
	Project    LowCardinality(String),
	MetricName LowCardinality(String),
	SeriesHash UInt64,
	Labels     Map(LowCardinality(String), String) CODEC(ZSTD(1)),
	LastSeen   DateTime64(3, 'UTC') CODEC(Delta, ZSTD(1)),
	INDEX idx_label_keys   mapKeys(Labels)   TYPE bloom_filter(0.01) GRANULARITY 4,
	INDEX idx_label_values mapValues(Labels) TYPE bloom_filter(0.01) GRANULARITY 4
) ENGINE = ReplacingMergeTree(LastSeen)
ORDER BY (Project, MetricName, SeriesHash)`,

		`CREATE TABLE IF NOT EXISTS metrics_metadata (
	Project          LowCardinality(String),
	MetricFamilyName LowCardinality(String),
	Type             LowCardinality(String),
	Help             String CODEC(ZSTD(1)),
	Unit             LowCardinality(String),
	UpdatedAt        DateTime DEFAULT now()
) ENGINE = ReplacingMergeTree(UpdatedAt)
ORDER BY (Project, MetricFamilyName)`,

		`CREATE TABLE IF NOT EXISTS traces_spans (
	Project         LowCardinality(String),
	Timestamp       DateTime64(9, 'UTC') CODEC(Delta, ZSTD(1)),
	TraceId         String CODEC(ZSTD(1)),
	SpanId          String CODEC(ZSTD(1)),
	ParentSpanId    String CODEC(ZSTD(1)),
	TraceState      String CODEC(ZSTD(1)),
	SpanName        LowCardinality(String),
	SpanKind        LowCardinality(String),
	ServiceName     LowCardinality(String),
	DurationNs      UInt64 CODEC(ZSTD(1)),
	StatusCode      LowCardinality(String),
	StatusMessage   String CODEC(ZSTD(1)),
	ResourceAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
	SpanAttributes     Map(LowCardinality(String), String) CODEC(ZSTD(1)),
	EventTimestamps Array(DateTime64(9, 'UTC')) CODEC(ZSTD(1)),
	EventNames      Array(String) CODEC(ZSTD(1)),
	EventAttributes Array(Map(LowCardinality(String), String)) CODEC(ZSTD(1)),
	LinkTraceIds    Array(String) CODEC(ZSTD(1)),
	LinkSpanIds     Array(String) CODEC(ZSTD(1)),
	LinkAttributes  Array(Map(LowCardinality(String), String)) CODEC(ZSTD(1)),
	INDEX idx_trace_id TraceId    TYPE bloom_filter(0.001) GRANULARITY 1,
	INDEX idx_duration DurationNs TYPE minmax GRANULARITY 1
) ENGINE = MergeTree
PARTITION BY toDate(Timestamp)
ORDER BY (Project, ServiceName, SpanName, toDateTime(Timestamp))
SETTINGS ttl_only_drop_parts = 1`,

		`CREATE TABLE IF NOT EXISTS logs_records (
	Project        LowCardinality(String),
	Timestamp      DateTime64(9, 'UTC') CODEC(Delta, ZSTD(1)),
	TraceId        String CODEC(ZSTD(1)),
	SpanId         String CODEC(ZSTD(1)),
	SeverityText   LowCardinality(String),
	SeverityNumber Int32 CODEC(ZSTD(1)),
	ServiceName    LowCardinality(String),
	Body           String CODEC(ZSTD(1)),
	ResourceAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
	LogAttributes      Map(LowCardinality(String), String) CODEC(ZSTD(1)),
	INDEX idx_trace_id TraceId TYPE bloom_filter(0.001) GRANULARITY 1,
	INDEX idx_body     Body    TYPE tokenbf_v1(32768, 3, 0) GRANULARITY 1
) ENGINE = MergeTree
PARTITION BY toDate(Timestamp)
ORDER BY (Project, ServiceName, toDateTime(Timestamp))
SETTINGS ttl_only_drop_parts = 1`,

		`CREATE TABLE IF NOT EXISTS profiling_samples (
	Project     LowCardinality(String),
	ServiceName LowCardinality(String),
	Type        LowCardinality(String),
	Start       DateTime64(9, 'UTC') CODEC(Delta, ZSTD(1)),
	End         DateTime64(9, 'UTC') CODEC(Delta, ZSTD(1)),
	Labels      Map(LowCardinality(String), String) CODEC(ZSTD(1)),
	Value       Int64 CODEC(ZSTD(1)),
	StackHash   UInt64 CODEC(ZSTD(1))
) ENGINE = MergeTree
PARTITION BY toDate(Start)
ORDER BY (Project, ServiceName, Type, toDateTime(Start))
SETTINGS ttl_only_drop_parts = 1`,

		`CREATE TABLE IF NOT EXISTS profiling_stacks (
	Project   LowCardinality(String),
	StackHash UInt64,
	Frames    Array(String) CODEC(ZSTD(3)),
	LastSeen  DateTime64(3, 'UTC') CODEC(Delta, ZSTD(1))
) ENGINE = ReplacingMergeTree(LastSeen)
ORDER BY (Project, StackHash)`,

		`CREATE TABLE IF NOT EXISTS profiling_services (
	Project     LowCardinality(String),
	ServiceName LowCardinality(String),
	Type        LowCardinality(String),
	LastSeen    DateTime64(9, 'UTC')
) ENGINE = ReplacingMergeTree(LastSeen)
ORDER BY (Project, ServiceName, Type)`,

		`CREATE MATERIALIZED VIEW IF NOT EXISTS profiling_services_mv TO profiling_services AS
SELECT Project, ServiceName, Type, max(End) AS LastSeen
FROM profiling_samples GROUP BY Project, ServiceName, Type`,
	},
}

type ttlRule struct {
	table string
	expr  string // the time column, already converted with toDateTime
	ttl   func(TTLs) time.Duration
}

// Retention is applied on every start, not only at creation, so changing a
// flag takes effect. materialize_ttl_after_modify is off: the new period
// applies to merges from now on, without rewriting existing parts.
var ttlRules = []ttlRule{
	{"metrics_samples", "toDateTime(Timestamp)", func(t TTLs) time.Duration { return t.Metrics }},
	{"metrics_series", "toDateTime(LastSeen)", func(t TTLs) time.Duration { return t.Metrics }},
	{"traces_spans", "toDateTime(Timestamp)", func(t TTLs) time.Duration { return t.Traces }},
	{"logs_records", "toDateTime(Timestamp)", func(t TTLs) time.Duration { return t.Logs }},
	{"profiling_samples", "toDateTime(Start)", func(t TTLs) time.Duration { return t.Profiles }},
	{"profiling_stacks", "toDateTime(LastSeen)", func(t TTLs) time.Duration { return t.Profiles }},
	{"profiling_services", "toDateTime(LastSeen)", func(t TTLs) time.Duration { return t.Profiles }},
}

// Migrate creates what is missing and applies retention. It is safe to run on
// every start and from one replica at a time.
func (s *Store) Migrate(ctx context.Context) error {
	if err := s.conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
	Version   UInt32,
	AppliedAt DateTime DEFAULT now()
) ENGINE = MergeTree ORDER BY Version`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	applied := map[uint32]bool{}
	rows, err := s.conn.Query(ctx, "SELECT Version FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v uint32
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for i, stmts := range migrations {
		v := uint32(i + 1)
		if applied[v] {
			continue
		}
		for _, q := range stmts {
			if err := s.conn.Exec(ctx, q); err != nil {
				return fmt.Errorf("migration %d: %w", v, err)
			}
		}
		if err := s.conn.Exec(ctx, "INSERT INTO schema_migrations (Version) VALUES (?)", v); err != nil {
			return fmt.Errorf("record migration %d: %w", v, err)
		}
	}
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"materialize_ttl_after_modify": 0}))
	for _, r := range ttlRules {
		secs := int64(r.ttl(s.ttl) / time.Second)
		q := fmt.Sprintf("ALTER TABLE %s MODIFY TTL %s + INTERVAL %d SECOND", r.table, r.expr, secs)
		if err := s.conn.Exec(ctx, q); err != nil {
			return fmt.Errorf("set retention on %s: %w", r.table, err)
		}
	}
	return nil
}
