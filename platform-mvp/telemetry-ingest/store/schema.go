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
	{ // 2: incidents. An append-only log of what happened to each incident; the incident as it stands is folded from its events (see the
		// incident package). Not a retention table: no TTL, an incident is a record. Nothing is ever updated, so a plain MergeTree is enough.
		// The sort key puts one incident's events together and in order; a given (Project, IncidentID, Seq) must be written once, which the
		// service guarantees by writing one change at a time (ClickHouse cannot enforce it).
		`CREATE TABLE IF NOT EXISTS incident_events (
	Project    LowCardinality(String),
	IncidentID String,
	Seq        UInt32,
	EventID    String,
	Time       DateTime64(3, 'UTC') CODEC(Delta, ZSTD(1)),
	Type       LowCardinality(String),
	Actor      String,
	DedupeKey  String,
	Data       String CODEC(ZSTD(3))
) ENGINE = MergeTree
ORDER BY (Project, IncidentID, Seq)`,
	},
	{ // 3: what the databases themselves report (db-collector): statement statistics, session samples, slow statements, and what each target allows
		// The query text in every table is normalized by the collector (no literals); the service refuses text that is not.
		`CREATE TABLE IF NOT EXISTS db_targets (
	Project      LowCardinality(String),
	Target       LowCardinality(String),
	Time         DateTime64(3, 'UTC') CODEC(Delta, ZSTD(1)),
	Engine       LowCardinality(String),
	Version      String,
	Capabilities String CODEC(ZSTD(3))
) ENGINE = MergeTree
PARTITION BY toDate(Time)
ORDER BY (Project, Target, Time)
SETTINGS ttl_only_drop_parts = 1`,

		`CREATE TABLE IF NOT EXISTS db_statements (
	Project    LowCardinality(String),
	Target     LowCardinality(String),
	Time       DateTime64(3, 'UTC') CODEC(Delta, ZSTD(1)),
	IntervalMs UInt32,
	QueryID    String,
	Database   LowCardinality(String),
	User       LowCardinality(String),
	Query      String CODEC(ZSTD(3)),
	Calls      UInt64,
	TotalMs    Float64 CODEC(Gorilla, ZSTD(1)),
	Rows       UInt64,
	Other      UInt8,
	Extra      Map(LowCardinality(String), Float64) CODEC(ZSTD(1))
) ENGINE = MergeTree
PARTITION BY toDate(Time)
ORDER BY (Project, Target, QueryID, Time)
SETTINGS ttl_only_drop_parts = 1`,

		`CREATE TABLE IF NOT EXISTS db_activity (
	Project           LowCardinality(String),
	Target            LowCardinality(String),
	Time              DateTime64(3, 'UTC') CODEC(Delta, ZSTD(1)),
	Total             UInt32,
	MaxConnections    UInt32,
	Active            UInt32,
	Idle              UInt32,
	IdleInTx          UInt32,
	Blocked           UInt32,
	LongestQueryMs    Float64,
	LongestXactMs     Float64,
	LongestIdleInTxMs Float64,
	ByState           Map(LowCardinality(String), UInt32),
	Waiting           Map(String, UInt32),
	Sessions          String CODEC(ZSTD(3))
) ENGINE = MergeTree
PARTITION BY toDate(Time)
ORDER BY (Project, Target, Time)
SETTINGS ttl_only_drop_parts = 1`,

		`CREATE TABLE IF NOT EXISTS db_slow_queries (
	Project    LowCardinality(String),
	Target     LowCardinality(String),
	Time       DateTime64(3, 'UTC') CODEC(Delta, ZSTD(1)),
	DurationMs Float64,
	Database   LowCardinality(String),
	User       LowCardinality(String),
	Kind       LowCardinality(String),
	PID        UInt32,
	Query      String CODEC(ZSTD(3))
) ENGINE = MergeTree
PARTITION BY toDate(Time)
ORDER BY (Project, Target, Time)
SETTINGS ttl_only_drop_parts = 1`,
	},
	{ // 4: connection poolers (db-collector's pgbouncer engine). One row a sample with the totals (so a series does not depend on the list of
		// pools being complete), one row a pool a sample, and one row a database an interval for what went through.
		`CREATE TABLE IF NOT EXISTS db_pool_samples (
	Project       LowCardinality(String),
	Target        LowCardinality(String),
	Time          DateTime64(3, 'UTC') CODEC(Delta, ZSTD(1)),
	IntervalMs    UInt32,
	Clients       UInt32,
	Waiting       UInt32,
	MaxWaitMs     Float64,
	MaxClientConn UInt32,
	Dropped       UInt32
) ENGINE = MergeTree
PARTITION BY toDate(Time)
ORDER BY (Project, Target, Time)
SETTINGS ttl_only_drop_parts = 1`,

		`CREATE TABLE IF NOT EXISTS db_pools (
	Project     LowCardinality(String),
	Target      LowCardinality(String),
	Time        DateTime64(3, 'UTC') CODEC(Delta, ZSTD(1)),
	Database    LowCardinality(String),
	User        LowCardinality(String),
	Mode        LowCardinality(String),
	ClActive    UInt32,
	ClWaiting   UInt32,
	SvActive    UInt32,
	SvIdle      UInt32,
	SvUsed      UInt32,
	SvTested    UInt32,
	SvLogin     UInt32,
	MaxWaitMs   Float64,
	PoolSize    UInt32,
	ReservePool UInt32,
	Paused      UInt8,
	Disabled    UInt8
) ENGINE = MergeTree
PARTITION BY toDate(Time)
ORDER BY (Project, Target, Time, Database, User)
SETTINGS ttl_only_drop_parts = 1`,

		`CREATE TABLE IF NOT EXISTS db_pool_stats (
	Project    LowCardinality(String),
	Target     LowCardinality(String),
	Time       DateTime64(3, 'UTC') CODEC(Delta, ZSTD(1)),
	IntervalMs UInt32,
	Database   LowCardinality(String),
	Xacts      UInt64,
	Queries    UInt64,
	BytesIn    UInt64,
	BytesOut   UInt64,
	XactMs     Float64,
	QueryMs    Float64,
	WaitMs     Float64
) ENGINE = MergeTree
PARTITION BY toDate(Time)
ORDER BY (Project, Target, Database, Time)
SETTINGS ttl_only_drop_parts = 1`,
	},
	{ // 5: cluster inventory (cluster-collector). Objects are kept as versions: a new row whenever one changed, and a row with Deleted = 1 when it
		// went; what is current is the newest row of each object. Events are rows too (one a change of an event's count), batches one a report.
		`CREATE TABLE IF NOT EXISTS inventory_objects (
	Project   LowCardinality(String),
	Cluster   LowCardinality(String),
	Kind      LowCardinality(String),
	UID       String,
	Namespace LowCardinality(String),
	Name      String,
	Version   DateTime64(3, 'UTC') CODEC(Delta, ZSTD(1)),
	Deleted   UInt8,
	Created   DateTime64(3, 'UTC'),
	Labels    Map(LowCardinality(String), String) CODEC(ZSTD(1)),
	Data      String CODEC(ZSTD(3))
) ENGINE = MergeTree
PARTITION BY toDate(Version)
ORDER BY (Project, Cluster, Kind, UID, Version)
SETTINGS ttl_only_drop_parts = 1`,

		`CREATE TABLE IF NOT EXISTS inventory_events (
	Project    LowCardinality(String),
	Cluster    LowCardinality(String),
	UID        String,
	Namespace  LowCardinality(String),
	ObjectKind LowCardinality(String),
	ObjectName String,
	ObjectUID  String,
	Type       LowCardinality(String),
	Reason     LowCardinality(String),
	Message    String CODEC(ZSTD(3)),
	Count      UInt32,
	FirstTime  DateTime64(3, 'UTC'),
	LastTime   DateTime64(3, 'UTC') CODEC(Delta, ZSTD(1)),
	Source     LowCardinality(String)
) ENGINE = MergeTree
PARTITION BY toDate(LastTime)
ORDER BY (Project, Cluster, LastTime, UID)
SETTINGS ttl_only_drop_parts = 1`,

		`CREATE TABLE IF NOT EXISTS inventory_batches (
	Project     LowCardinality(String),
	Cluster     LowCardinality(String),
	ReceivedAt  DateTime64(3, 'UTC') CODEC(Delta, ZSTD(1)),
	CollectedAt DateTime64(3, 'UTC'),
	Objects     UInt32,
	Deleted     UInt32,
	Events      UInt32,
	Sweeps      UInt32
) ENGINE = MergeTree
PARTITION BY toDate(ReceivedAt)
ORDER BY (Project, Cluster, ReceivedAt)
SETTINGS ttl_only_drop_parts = 1`,
	},
	{ // 6: cluster enrollment (see the enroll package): one-time tokens and the ingest keys they are exchanged for, as an event log. Only hashes of
		// either are stored. No TTL: it is a record of who was let in, and small.
		`CREATE TABLE IF NOT EXISTS enrollment_events (
	Project   LowCardinality(String),
	ID        String,
	Time      DateTime64(3, 'UTC'),
	Type      LowCardinality(String),
	Actor     String,
	Cluster   String,
	TokenHash String,
	ExpiresAt DateTime64(3, 'UTC'),
	KeyHash   String
) ENGINE = MergeTree
ORDER BY (Project, ID, Time)`,
	},
}

// MigrationCount is how many migrations the service knows: what the schema_migrations table holds once they have all been applied.
func MigrationCount() int { return len(migrations) }

type ttlRule struct {
	table string
	expr  string // the time column, already converted with toDateTime
	ttl   func(TTLs) time.Duration
	big   bool // takes the storage policy and moves old data to the cold volume; the small lookup tables stay on the default disk
}

// Retention is applied on every start, not only at creation, so changing a
// flag takes effect. materialize_ttl_after_modify is off: the new period
// applies to merges from now on, without rewriting existing parts.
var ttlRules = []ttlRule{
	{"metrics_samples", "toDateTime(Timestamp)", func(t TTLs) time.Duration { return t.Metrics }, true},
	{"metrics_series", "toDateTime(LastSeen)", func(t TTLs) time.Duration { return t.Metrics }, false},
	{"traces_spans", "toDateTime(Timestamp)", func(t TTLs) time.Duration { return t.Traces }, true},
	{"logs_records", "toDateTime(Timestamp)", func(t TTLs) time.Duration { return t.Logs }, true},
	{"profiling_samples", "toDateTime(Start)", func(t TTLs) time.Duration { return t.Profiles }, true},
	{"profiling_stacks", "toDateTime(LastSeen)", func(t TTLs) time.Duration { return t.Profiles }, false},
	{"profiling_services", "toDateTime(LastSeen)", func(t TTLs) time.Duration { return t.Profiles }, false},
	{"db_targets", "toDateTime(Time)", func(t TTLs) time.Duration { return t.Databases }, false},
	{"db_statements", "toDateTime(Time)", func(t TTLs) time.Duration { return t.Databases }, false},
	{"db_activity", "toDateTime(Time)", func(t TTLs) time.Duration { return t.Databases }, false},
	{"db_slow_queries", "toDateTime(Time)", func(t TTLs) time.Duration { return t.Databases }, false},
	{"db_pool_samples", "toDateTime(Time)", func(t TTLs) time.Duration { return t.Databases }, false},
	{"db_pools", "toDateTime(Time)", func(t TTLs) time.Duration { return t.Databases }, false},
	{"db_pool_stats", "toDateTime(Time)", func(t TTLs) time.Duration { return t.Databases }, false},
	{"inventory_objects", "toDateTime(Version)", func(t TTLs) time.Duration { return t.Inventory }, false},
	{"inventory_events", "toDateTime(LastTime)", func(t TTLs) time.Duration { return t.Inventory }, false},
	{"inventory_batches", "toDateTime(ReceivedAt)", func(t TTLs) time.Duration { return t.Inventory }, false},
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
		if r.ttl(s.ttl) <= 0 {
			continue // no retention set for this kind of data: it is kept (a caller that does not store it has nothing to set)
		}
		if r.big && s.tier.Policy != "" {
			// The new policy must contain the old one's disks and volumes (the default policy's "default" volume), or ClickHouse refuses.
			q := fmt.Sprintf("ALTER TABLE %s MODIFY SETTING storage_policy = '%s'", r.table, s.tier.Policy)
			if err := s.conn.Exec(ctx, q); err != nil {
				return fmt.Errorf("set storage policy %q on %s: %w", s.tier.Policy, r.table, err)
			}
		}
		q := ttlStatement(r, s.ttl, s.tier)
		if err := s.conn.Exec(ctx, q); err != nil {
			return fmt.Errorf("set retention on %s: %w", r.table, err)
		}
	}
	return nil
}

// ttlStatement is the ALTER that sets a table's retention: delete after the TTL and, for a big table with tiering on, move to the
// cold volume after Tier.After. The move is listed first, as ClickHouse evaluates the rules by their time.
func ttlStatement(r ttlRule, ttl TTLs, tier Tiering) string {
	del := fmt.Sprintf("%s + INTERVAL %d SECOND DELETE", r.expr, int64(r.ttl(ttl)/time.Second))
	if r.big && tier.Policy != "" && tier.Volume != "" && tier.After > 0 {
		move := fmt.Sprintf("%s + INTERVAL %d SECOND TO VOLUME '%s'", r.expr, int64(tier.After/time.Second), tier.Volume)
		return fmt.Sprintf("ALTER TABLE %s MODIFY TTL %s, %s", r.table, move, del)
	}
	return fmt.Sprintf("ALTER TABLE %s MODIFY TTL %s", r.table, del)
}
