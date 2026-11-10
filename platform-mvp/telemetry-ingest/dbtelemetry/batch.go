// Package dbtelemetry is the contract between the database collector (platform-mvp/db-collector) and the ingest service: the JSON a collector
// posts to /v1/db, and the checks the service makes on it before anything is stored.
//
// The two are separate Go modules that build on their own, so this is a copy of the shape, not an import: the collector's Batch and this one
// agree through the JSON and its Version. Adding a field does not change the version (an older service ignores it); changing what a field
// means does.
//
// The service does not take the collector's word that statement text has been normalized (the literals taken out): a tripwire refuses text
// that still has a quoted string in it. That catches a collector that does not normalize at all; it cannot catch a number or an unquoted word
// that is data, so normalizing remains the collector's job, and the ingest key is a trust boundary: it belongs to a collector that is trusted.
package dbtelemetry

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const Version = 1

const (
	MaxStatements  = 500
	MaxSessions    = 50
	MaxSlowQueries = 500
	MaxPools       = 500
	MaxQueryBytes  = 4096 // the collector cuts at 2048; this is the most the service will keep
	MaxExtra       = 32
	maxWaitKeys    = 100
	maxStates      = 20
)

type Statement struct {
	QueryID  string             `json:"query_id"`
	Database string             `json:"database"`
	User     string             `json:"user"`
	Query    string             `json:"query"`
	Calls    uint64             `json:"calls"`
	TotalMs  float64            `json:"total_ms"`
	Rows     uint64             `json:"rows"`
	Extra    map[string]float64 `json:"extra"`
	Other    bool               `json:"other"`
}

type Session struct {
	PID         int64   `json:"pid"`
	Database    string  `json:"database"`
	User        string  `json:"user"`
	Application string  `json:"application"`
	State       string  `json:"state"`
	WaitType    string  `json:"wait_type"`
	Wait        string  `json:"wait"`
	QueryMs     float64 `json:"query_ms"`
	XactMs      float64 `json:"xact_ms"`
	BlockedBy   []int64 `json:"blocked_by"`
	Query       string  `json:"query"`
}

type Activity struct {
	Total          int            `json:"total"`
	MaxConnections int            `json:"max_connections"`
	ByState        map[string]int `json:"by_state"`
	Waiting        map[string]int `json:"waiting"`
	Blocked        int            `json:"blocked"`
	LongestQueryMs float64        `json:"longest_query_ms"`
	LongestXactMs  float64        `json:"longest_xact_ms"`
	LongestIdleXMs float64        `json:"longest_idle_in_xact_ms"`
	Sessions       []Session      `json:"sessions"`
}

type SlowQuery struct {
	Time       time.Time `json:"time"`
	DurationMs float64   `json:"duration_ms"`
	Database   string    `json:"database"`
	User       string    `json:"user"`
	Kind       string    `json:"kind"`
	PID        int64     `json:"pid"`
	Query      string    `json:"query"`
}

// Pool is one pool of a connection pooler (see the collector's pgbouncer engine).
type Pool struct {
	Database    string  `json:"database"`
	User        string  `json:"user"`
	Mode        string  `json:"mode"`
	ClActive    int     `json:"cl_active"`
	ClWaiting   int     `json:"cl_waiting"`
	SvActive    int     `json:"sv_active"`
	SvIdle      int     `json:"sv_idle"`
	SvUsed      int     `json:"sv_used"`
	SvTested    int     `json:"sv_tested"`
	SvLogin     int     `json:"sv_login"`
	MaxWaitMs   float64 `json:"max_wait_ms"`
	PoolSize    int     `json:"pool_size"`
	ReservePool int     `json:"reserve_pool"`
	Paused      bool    `json:"paused"`
	Disabled    bool    `json:"disabled"`
}

// PoolStats is what went through a pooler for one database during the batch's interval.
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

type Pools struct {
	Pools         []Pool      `json:"pools"`
	Stats         []PoolStats `json:"stats"`
	MaxClientConn int         `json:"max_client_conn"`
	Clients       int         `json:"clients"`
	Waiting       int         `json:"waiting"`
	MaxWaitMs     float64     `json:"max_wait_ms"`
	Dropped       int         `json:"dropped"`
}

type Capability struct {
	Available bool     `json:"available"`
	Problems  []string `json:"problems"`
}

type Capabilities struct {
	Engine     string            `json:"engine"`
	Version    string            `json:"version"`
	Statements Capability        `json:"statements"`
	Activity   Capability        `json:"activity"`
	SlowLog    Capability        `json:"slow_log"`
	Pools      Capability        `json:"pools"`
	Settings   map[string]string `json:"settings"`
}

type Batch struct {
	Version         int           `json:"version"`
	Target          string        `json:"target"`
	Engine          string        `json:"engine"`
	CollectedAt     time.Time     `json:"collected_at"`
	IntervalSeconds float64       `json:"interval_seconds"`
	Statements      []Statement   `json:"statements"`
	Activity        *Activity     `json:"activity"`
	Pools           *Pools        `json:"pools"`
	SlowQueries     []SlowQuery   `json:"slow_queries"`
	SlowDropped     int           `json:"slow_dropped"`
	Capabilities    *Capabilities `json:"capabilities"`
}

var (
	targetRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$`)
	engineRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,30}$`)
	nameRE   = regexp.MustCompile(`^[A-Za-z0-9_:.-]{1,64}$`)
)

// ValidTarget says whether s can be a target's name: it is used in a URL path and in queries, so it is a short, plain word.
func ValidTarget(s string) bool { return targetRE.MatchString(s) }

func badText(s string, max int) bool {
	if len(s) > max || !utf8.ValidString(s) {
		return true
	}
	for _, r := range s {
		if r < 0x20 && r != '\t' && r != '\n' || r == 0x7f {
			return true
		}
	}
	return false
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0 }

// Normalized is the tripwire: false if the text still has a single-quoted string or a dollar-quoted body outside a quoted identifier.
func Normalized(q string) bool {
	for i := 0; i < len(q); i++ {
		switch q[i] {
		case '"':
			i++
			for i < len(q) && q[i] != '"' {
				i++
			}
		case '\'':
			return false
		case '$':
			if i+1 < len(q) && (q[i+1] == '$' || q[i+1] == '_' || q[i+1] >= 'a' && q[i+1] <= 'z' || q[i+1] >= 'A' && q[i+1] <= 'Z') {
				if j := strings.IndexByte(q[i+1:], '$'); j >= 0 && !strings.ContainsAny(q[i+1:i+1+j], " ,()") {
					return false
				}
			}
		}
	}
	return true
}

// Validate checks a batch and says what is wrong with it. `now` is the service's clock.
func (b *Batch) Validate(now time.Time) error {
	if b.Version != Version {
		return fmt.Errorf("version %d is not supported: this service reads version %d", b.Version, Version)
	}
	if !targetRE.MatchString(b.Target) {
		return fmt.Errorf("target must be lower-case letters, digits and . _ -, 1 to 64 characters")
	}
	if !engineRE.MatchString(b.Engine) {
		return fmt.Errorf("engine must be a short lower-case word such as postgres")
	}
	if b.CollectedAt.IsZero() || b.CollectedAt.After(now.Add(5*time.Minute)) || b.CollectedAt.Before(now.Add(-24*time.Hour)) {
		return fmt.Errorf("collected_at must be within the last day and not in the future")
	}
	if !finite(b.IntervalSeconds) || b.IntervalSeconds > 86400 {
		return fmt.Errorf("interval_seconds is not a duration")
	}
	if len(b.Statements)+len(b.SlowQueries) == 0 && b.Activity == nil && b.Pools == nil && b.Capabilities == nil {
		return fmt.Errorf("the batch is empty")
	}
	if len(b.Statements) > MaxStatements || len(b.SlowQueries) > MaxSlowQueries {
		return fmt.Errorf("too many statements or slow queries in one batch (at most %d and %d)", MaxStatements, MaxSlowQueries)
	}
	if b.SlowDropped < 0 {
		return fmt.Errorf("slow_dropped is negative")
	}
	checkQuery := func(where, q string) error {
		if badText(q, MaxQueryBytes) {
			return fmt.Errorf("%s: the query text is too long or has control characters", where)
		}
		if !Normalized(q) {
			return fmt.Errorf("%s: the query text has a quoted literal in it: it must be normalized before it is sent", where)
		}
		return nil
	}
	for i, s := range b.Statements {
		w := fmt.Sprintf("statements[%d]", i)
		if err := checkQuery(w, s.Query); err != nil {
			return err
		}
		if !finite(s.TotalMs) || badText(s.QueryID, 64) || badText(s.Database, 128) || badText(s.User, 128) {
			return fmt.Errorf("%s: a figure or a name is not valid", w)
		}
		if len(s.Extra) > MaxExtra {
			return fmt.Errorf("%s: at most %d extra counters", w, MaxExtra)
		}
		for k, v := range s.Extra {
			if !nameRE.MatchString(k) || !finite(v) {
				return fmt.Errorf("%s: extra counter %q is not valid", w, k)
			}
		}
	}
	for i, q := range b.SlowQueries {
		w := fmt.Sprintf("slow_queries[%d]", i)
		if err := checkQuery(w, q.Query); err != nil {
			return err
		}
		if !finite(q.DurationMs) || q.Time.IsZero() || q.Time.After(now.Add(5*time.Minute)) || badText(q.Database, 128) || badText(q.User, 128) || badText(q.Kind, 32) || q.PID < 0 {
			return fmt.Errorf("%s: a figure, a time or a name is not valid", w)
		}
	}
	if a := b.Activity; a != nil {
		if a.Total < 0 || a.MaxConnections < 0 || a.Blocked < 0 || !finite(a.LongestQueryMs) || !finite(a.LongestXactMs) || !finite(a.LongestIdleXMs) {
			return fmt.Errorf("activity: a figure is not valid")
		}
		if len(a.ByState) > maxStates || len(a.Waiting) > maxWaitKeys || len(a.Sessions) > MaxSessions {
			return fmt.Errorf("activity: too many states, waits or sessions")
		}
		for k, v := range a.ByState {
			if badText(k, 64) || v < 0 {
				return fmt.Errorf("activity: state %q is not valid", k)
			}
		}
		for k, v := range a.Waiting {
			if badText(k, 128) || v < 0 {
				return fmt.Errorf("activity: wait %q is not valid", k)
			}
		}
		for i, s := range a.Sessions {
			w := fmt.Sprintf("activity.sessions[%d]", i)
			if err := checkQuery(w, s.Query); err != nil {
				return err
			}
			if !finite(s.QueryMs) || !finite(s.XactMs) || badText(s.Database, 128) || badText(s.User, 128) || badText(s.Application, 256) || badText(s.State, 64) || badText(s.WaitType, 64) || badText(s.Wait, 128) || len(s.BlockedBy) > 100 {
				return fmt.Errorf("%s: a figure or a name is not valid", w)
			}
		}
	}
	if p := b.Pools; p != nil {
		if len(p.Pools) > MaxPools || len(p.Stats) > MaxPools {
			return fmt.Errorf("pools: too many pools or statistics in one batch (at most %d)", MaxPools)
		}
		if p.MaxClientConn < 0 || p.Clients < 0 || p.Waiting < 0 || p.Dropped < 0 || !finite(p.MaxWaitMs) {
			return fmt.Errorf("pools: a total is not valid")
		}
		for i, q := range p.Pools {
			w := fmt.Sprintf("pools.pools[%d]", i)
			counts := []int{q.ClActive, q.ClWaiting, q.SvActive, q.SvIdle, q.SvUsed, q.SvTested, q.SvLogin, q.PoolSize, q.ReservePool}
			for _, n := range counts {
				if n < 0 || n > 1<<24 {
					return fmt.Errorf("%s: a count is not valid", w)
				}
			}
			if !finite(q.MaxWaitMs) || badText(q.Database, 128) || badText(q.User, 128) || badText(q.Mode, 32) || q.Database == "" {
				return fmt.Errorf("%s: a figure or a name is not valid", w)
			}
		}
		for i, st := range p.Stats {
			w := fmt.Sprintf("pools.stats[%d]", i)
			if !finite(st.XactMs) || !finite(st.QueryMs) || !finite(st.WaitMs) || badText(st.Database, 128) || st.Database == "" {
				return fmt.Errorf("%s: a figure or a name is not valid", w)
			}
		}
	}
	if c := b.Capabilities; c != nil {
		if !engineRE.MatchString(c.Engine) || badText(c.Version, 128) || len(c.Settings) > 64 {
			return fmt.Errorf("capabilities: the engine, the version or the settings are not valid")
		}
		for k, v := range c.Settings {
			if badText(k, 128) || badText(v, 1024) {
				return fmt.Errorf("capabilities: setting %q is not valid", k)
			}
		}
		for _, cap := range []Capability{c.Statements, c.Activity, c.SlowLog, c.Pools} {
			if len(cap.Problems) > 20 {
				return fmt.Errorf("capabilities: too many problems")
			}
			for _, p := range cap.Problems {
				if badText(p, 1024) {
					return fmt.Errorf("capabilities: a problem text is not valid")
				}
			}
		}
	}
	return nil
}
