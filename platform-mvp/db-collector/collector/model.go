// Package collector is the model of a database collector: what a database engine can report (statement statistics, what its sessions are
// doing now, which statements were slow), the figures every engine is reduced to, the scheduling that asks for them, and the sinks that carry
// them away. An engine (the postgres package is the first) plugs in by implementing the small interfaces in engine.go; nothing here knows
// about one engine's catalog views, and nothing in an engine knows about scheduling, retries or the network.
//
// What is collected is deliberately narrow. Statement text is ALWAYS passed through Normalize first, so literals never leave the collector;
// every list is capped, so one busy database cannot make a batch without a bound; and an engine is only ever asked to read its own catalog
// views and logs, never to run anything of the user's.
package collector

import "time"

// BatchVersion is the version of the payload (Batch). It goes up when a field changes meaning; adding a field does not change it.
const BatchVersion = 1

// Statement is one query's activity during one interval: how many times it ran, how long that took in all, how many rows it returned. The
// figures are for the interval, not since the server started (see Differ); Extra holds what only some engines have (for PostgreSQL, buffer
// hits and reads, temporary blocks, WAL bytes), keyed by the engine's own name for it.
type Statement struct {
	QueryID  string             `json:"query_id"`
	Database string             `json:"database,omitempty"`
	User     string             `json:"user,omitempty"`
	Query    string             `json:"query"` // normalized: no literals
	Calls    uint64             `json:"calls"`
	TotalMs  float64            `json:"total_ms"`
	Rows     uint64             `json:"rows"`
	Extra    map[string]float64 `json:"extra,omitempty"`
	// Other marks the one row that adds up every statement that did not make the top: so the totals of a batch are the whole database's.
	Other bool `json:"other,omitempty"`
}

// Session is one connection that is doing something long, or is stuck: the interesting few of an activity sample.
type Session struct {
	PID         int64   `json:"pid"`
	Database    string  `json:"database,omitempty"`
	User        string  `json:"user,omitempty"`
	Application string  `json:"application,omitempty"`
	State       string  `json:"state"`
	WaitType    string  `json:"wait_type,omitempty"`
	Wait        string  `json:"wait,omitempty"`
	QueryMs     float64 `json:"query_ms"`             // how long the current statement has been running (or the session idle in it)
	XactMs      float64 `json:"xact_ms,omitempty"`    // how long the transaction has been open
	BlockedBy   []int64 `json:"blocked_by,omitempty"` // the sessions this one is waiting for
	Query       string  `json:"query,omitempty"`      // normalized
}

// Activity is what a database's sessions were doing at one moment.
type Activity struct {
	Total          int            `json:"total"`           // client sessions
	MaxConnections int            `json:"max_connections"` // what the server allows (0 when it could not be read)
	ByState        map[string]int `json:"by_state"`        // active, idle, idle in transaction, ...
	Waiting        map[string]int `json:"waiting"`         // "Lock:relation" -> how many active sessions wait for it
	Blocked        int            `json:"blocked"`         // sessions waiting for another session
	LongestQueryMs float64        `json:"longest_query_ms"`
	LongestXactMs  float64        `json:"longest_xact_ms"`
	LongestIdleXMs float64        `json:"longest_idle_in_xact_ms"`
	Sessions       []Session      `json:"sessions,omitempty"` // the longest running and the blocked, capped
}

// SlowQuery is a statement the server logged because it took longer than its threshold (PostgreSQL's log_min_duration_statement).
type SlowQuery struct {
	Time       time.Time `json:"time"`
	DurationMs float64   `json:"duration_ms"`
	Database   string    `json:"database,omitempty"`
	User       string    `json:"user,omitempty"`
	Kind       string    `json:"kind"` // statement, execute
	PID        int64     `json:"pid,omitempty"`
	Query      string    `json:"query"` // normalized
}

// Pool is one connection pool of a connection pooler (PgBouncer's pool of a database and user): how many clients are using it and waiting for
// it, how many server connections it holds in each state, and how long the oldest waiting client has waited. A pooler in front of a database
// hides connection exhaustion from the database itself: the database sees a few busy connections while clients queue at the pooler, so this
// is where it shows.
type Pool struct {
	Database    string  `json:"database"`
	User        string  `json:"user"`
	Mode        string  `json:"mode,omitempty"` // session, transaction or statement
	ClActive    int     `json:"cl_active"`      // clients that have a server connection now
	ClWaiting   int     `json:"cl_waiting"`     // clients waiting for one
	SvActive    int     `json:"sv_active"`      // server connections serving a client
	SvIdle      int     `json:"sv_idle"`        // ready for a client
	SvUsed      int     `json:"sv_used"`        // idle, but not yet reset for the next client
	SvTested    int     `json:"sv_tested"`
	SvLogin     int     `json:"sv_login"`     // being opened
	MaxWaitMs   float64 `json:"max_wait_ms"`  // how long the oldest waiting client has waited
	PoolSize    int     `json:"pool_size"`    // the limit on server connections that applies (0: not known)
	ReservePool int     `json:"reserve_pool"` // extra connections allowed when clients wait too long
	Paused      bool    `json:"paused,omitempty"`
	Disabled    bool    `json:"disabled,omitempty"`
}

// PoolStats is what went through the pooler for one database during one interval: transactions and queries, bytes, and the time they took,
// including the time clients spent WAITING for a server connection (WaitMs), which is the cost of a pool that is too small.
type PoolStats struct {
	Database string  `json:"database"`
	Xacts    uint64  `json:"xacts"`
	Queries  uint64  `json:"queries"`
	BytesIn  uint64  `json:"bytes_in"`
	BytesOut uint64  `json:"bytes_out"`
	XactMs   float64 `json:"xact_ms"`
	QueryMs  float64 `json:"query_ms"`
	WaitMs   float64 `json:"wait_ms"`
}

// Pools is a pooler's pools at one moment, with what went through it since the last sample. The totals cover every pool, even those left out
// of the list by the cap.
type Pools struct {
	Pools         []Pool      `json:"pools"`
	Stats         []PoolStats `json:"stats,omitempty"`
	MaxClientConn int         `json:"max_client_conn,omitempty"` // the pooler's limit on client connections (0: not known)
	Clients       int         `json:"clients"`                   // client connections in all pools
	Waiting       int         `json:"waiting"`                   // of them, waiting for a server connection
	MaxWaitMs     float64     `json:"max_wait_ms"`               // the longest any client has waited
	Dropped       int         `json:"dropped,omitempty"`         // pools over the cap, left out of the list
}

// Capability says whether one kind of data can be collected from a target, and if not, what to change. A collector that cannot collect
// something says so, in words a person can act on; it does not fail, and it does not stay silent.
type Capability struct {
	Available bool     `json:"available"`
	Problems  []string `json:"problems,omitempty"`
}

// Capabilities is what the collector found out about a target: which engine and version, what it can read, and the server settings the
// collection depends on.
type Capabilities struct {
	Engine     string            `json:"engine"`
	Version    string            `json:"version,omitempty"`
	Statements Capability        `json:"statements"`
	Activity   Capability        `json:"activity"`
	SlowLog    Capability        `json:"slow_log"`
	Pools      Capability        `json:"pools"`
	Settings   map[string]string `json:"settings,omitempty"`
}

// Batch is what goes to the sink: one target, one moment, any of the kinds of data. A batch is never empty.
type Batch struct {
	Version     int       `json:"version"`
	Target      string    `json:"target"`
	Engine      string    `json:"engine"`
	CollectedAt time.Time `json:"collected_at"`
	// IntervalSeconds is how long the statements' (or the pools') figures cover: the time since the previous snapshot.
	IntervalSeconds float64       `json:"interval_seconds,omitempty"`
	Statements      []Statement   `json:"statements,omitempty"`
	Activity        *Activity     `json:"activity,omitempty"`
	Pools           *Pools        `json:"pools,omitempty"` // with IntervalSeconds: the time its Stats cover
	SlowQueries     []SlowQuery   `json:"slow_queries,omitempty"`
	SlowDropped     int           `json:"slow_dropped,omitempty"` // slow queries over the cap, left out
	Capabilities    *Capabilities `json:"capabilities,omitempty"`
}

// Empty reports whether the batch carries nothing.
func (b Batch) Empty() bool {
	return len(b.Statements) == 0 && b.Activity == nil && b.Pools == nil && len(b.SlowQueries) == 0 && b.Capabilities == nil
}

// Limits on what one batch may hold. The ingest side enforces the same ones; the collector keeps itself within them.
const (
	MaxStatements  = 500
	MaxSessions    = 50
	MaxSlowQueries = 500
	MaxPools       = 500
	MaxQueryBytes  = 2048
)
