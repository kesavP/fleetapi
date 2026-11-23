// Package postgres is the PostgreSQL engine of the collector: statement statistics from pg_stat_statements, what the sessions are doing from
// pg_stat_activity, and the slow statements the server writes to its log when log_min_duration_statement is set.
//
// The collector connects as a role of its own and only reads. Its session is set up so that it cannot do harm even if the role could: every
// session is read-only, with short timeouts on statements and locks, and every query it sends starts with a marker comment, which is how its
// own statements are told apart from the application's (in pg_stat_statements, in pg_stat_activity and in the log).
//
// A role for it needs no privileges beyond:
//
//	CREATE ROLE db_collector LOGIN PASSWORD '...';
//	GRANT pg_monitor TO db_collector;      -- (or pg_read_all_stats) to see query text and other users' sessions
//	-- in the database named in the connection: CREATE EXTENSION pg_stat_statements;
package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sojoner/kro-lab/platform-mvp/db-collector/collector"
)

func init() { collector.Register(Engine{}) }

type Engine struct{}

func (Engine) Name() string { return "postgres" }

func (Engine) Signals() collector.Signals {
	return collector.Signals{Statements: true, Activity: true, SlowLog: true}
}

const (
	textBytes = 8192 // how much of a statement's text is read from the server (it is cut to collector.MaxQueryBytes after normalizing)
	// the session limits: a collector that is slow or stuck must not hold the database
	statementTimeout = 5 * time.Second
	lockTimeout      = time.Second
)

// Open prepares a connection pool. It does not connect: a database that is down when the collector starts is not a reason to refuse to start,
// and the pool connects (and reconnects) when it is first asked for something.
func (Engine) Open(ctx context.Context, t collector.TargetConfig) (collector.Source, error) {
	cfg, err := pgxpool.ParseConfig(t.DSN)
	if err != nil {
		return nil, fmt.Errorf("target %s: the connection string is not valid (%s)", t.Name, scrub(err.Error(), t.DSN))
	}
	cfg.MaxConns = 2 // statements and activity are read at the same time, never more
	cfg.MaxConnLifetime = time.Hour
	cfg.HealthCheckPeriod = 30 * time.Second
	cc := cfg.ConnConfig
	cc.ConnectTimeout = 5 * time.Second
	// simple protocol: no prepared statements, so it works behind a pooler in transaction mode, and the few values it sends are escaped by the driver
	cc.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	if cc.RuntimeParams == nil {
		cc.RuntimeParams = map[string]string{}
	}
	for k, v := range map[string]string{
		"application_name":                    "db-collector",
		"default_transaction_read_only":       "on",
		"statement_timeout":                   strconv.Itoa(int(statementTimeout / time.Millisecond)),
		"lock_timeout":                        strconv.Itoa(int(lockTimeout / time.Millisecond)),
		"idle_in_transaction_session_timeout": "10000",
	} {
		cc.RuntimeParams[k] = v
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("target %s: %s", t.Name, scrub(err.Error(), t.DSN))
	}
	return &source{pool: pool, cfg: t, secrets: secretsOf(cc)}, nil
}

func (Engine) SlowLog(t collector.TargetConfig) (collector.SlowLogSource, error) {
	if t.SlowLog.Path == "" {
		return nil, errors.New("slowLog.path is required: the file PostgreSQL writes its log to")
	}
	return &Tailer{Path: t.SlowLog.Path, FromStart: t.SlowLog.FromStart}, nil
}

// SetOnError lets the scheduler hear about what the follower cannot read.
func (t *Tailer) SetOnError(f func(error)) { t.OnError = f }

type source struct {
	pool    *pgxpool.Pool
	cfg     collector.TargetConfig
	secrets []string

	mu       sync.Mutex
	stmtSQL  string
	extras   []string
	noInfo   bool // pg_stat_statements_info does not exist (before PostgreSQL 14)
	maxConns int
}

func (s *source) Close() { s.pool.Close() }

// redactedError is an error whose text has had the connection's secrets taken out.
type redactedError struct {
	msg   string
	cause error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.cause }

func secretsOf(cc *pgx.ConnConfig) []string {
	var out []string
	if cc.Password != "" {
		out = append(out, cc.Password)
	}
	return out
}

// scrub takes the DSN and the password out of a message: a driver's error can quote the connection string it was given.
func scrub(msg, dsn string) string {
	if dsn != "" {
		msg = strings.ReplaceAll(msg, dsn, "<dsn>")
	}
	if cfg, err := pgx.ParseConfig(dsn); err == nil && cfg.Password != "" {
		msg = strings.ReplaceAll(msg, cfg.Password, "***")
	}
	return msg
}

func (s *source) wrap(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, p := range s.secrets {
		msg = strings.ReplaceAll(msg, p, "***")
	}
	msg = scrub(msg, s.cfg.DSN)
	return &redactedError{msg: msg, cause: err}
}

func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// columns returns the column names of a view, or none if there is no such view.
func (s *source) columns(ctx context.Context, view string) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, ColumnsQuery, view)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols[c] = true
	}
	return cols, rows.Err()
}

var errNoStatements = errors.New("pg_stat_statements is not available in this database (see the capabilities for what to change)")

func (s *source) Snapshot(ctx context.Context) (collector.Snapshot, error) {
	s.mu.Lock()
	sql, extras, noInfo := s.stmtSQL, s.extras, s.noInfo
	s.mu.Unlock()
	if sql == "" {
		cols, err := s.columns(ctx, "pg_stat_statements")
		if err != nil {
			return collector.Snapshot{}, s.wrap(err)
		}
		var ok bool
		if sql, extras, ok = StatementsQuery(cols, textBytes); !ok {
			return collector.Snapshot{}, errNoStatements
		}
		s.mu.Lock()
		s.stmtSQL, s.extras = sql, extras
		s.mu.Unlock()
	}
	epoch := ""
	if !noInfo {
		err := s.pool.QueryRow(ctx, "/* db-collector */ SELECT COALESCE(stats_reset::text, '') FROM pg_stat_statements_info").Scan(&epoch)
		if err != nil {
			if sqlState(err) == "42P01" { // undefined_table: PostgreSQL before 14
				s.mu.Lock()
				s.noInfo = true
				s.mu.Unlock()
			} else {
				return collector.Snapshot{}, s.wrap(err)
			}
		}
	}
	rows, err := s.pool.Query(ctx, sql)
	if err != nil {
		s.forgetQuery(err)
		return collector.Snapshot{}, s.wrap(err)
	}
	defer rows.Close()
	var out []statementRow
	for rows.Next() {
		r := statementRow{Extra: make([]float64, len(extras))}
		dests := []any{&r.Database, &r.User, &r.QueryID, &r.Query, &r.Calls, &r.TotalMs, &r.Rows, &r.TopLevel, &r.Since}
		for i := range r.Extra {
			dests = append(dests, &r.Extra[i])
		}
		if err := rows.Scan(dests...); err != nil {
			return collector.Snapshot{}, s.wrap(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		s.forgetQuery(err)
		return collector.Snapshot{}, s.wrap(err)
	}
	return BuildSnapshot(out, extras, epoch), nil
}

// forgetQuery drops the built query when the server says its columns or view are not what they were (the extension was updated or dropped).
func (s *source) forgetQuery(err error) {
	if c := sqlState(err); c == "42703" || c == "42P01" {
		s.mu.Lock()
		s.stmtSQL = ""
		s.mu.Unlock()
	}
}

func (s *source) Sample(ctx context.Context, top int) (collector.Activity, error) {
	s.mu.Lock()
	max := s.maxConns
	s.mu.Unlock()
	if max == 0 {
		var v string
		if err := s.pool.QueryRow(ctx, "/* db-collector */ SELECT current_setting('max_connections')").Scan(&v); err != nil {
			return collector.Activity{}, s.wrap(err)
		}
		max, _ = strconv.Atoi(v)
		s.mu.Lock()
		s.maxConns = max
		s.mu.Unlock()
	}
	rows, err := s.pool.Query(ctx, ActivityQuery(textBytes))
	if err != nil {
		return collector.Activity{}, s.wrap(err)
	}
	defer rows.Close()
	var out []activityRow
	for rows.Next() {
		var r activityRow
		if err := rows.Scan(&r.Now, &r.PID, &r.Database, &r.User, &r.Application, &r.State, &r.WaitType, &r.Wait, &r.QueryStart, &r.XactStart, &r.StateChange, &r.Query, &r.BlockedBy); err != nil {
			return collector.Activity{}, s.wrap(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return collector.Activity{}, s.wrap(err)
	}
	return BuildActivity(out, max, top), nil
}

// Capabilities looks at the server and says what can be collected and what to change when something cannot. It does not fail for a
// database that lacks something: that is the answer. It fails only when the server cannot be asked at all.
func (s *source) Capabilities(ctx context.Context) (collector.Capabilities, error) {
	caps := collector.Capabilities{Engine: "postgres", Settings: map[string]string{}}
	var version string
	if err := s.pool.QueryRow(ctx, "/* db-collector */ SELECT current_setting('server_version')").Scan(&version); err != nil {
		return caps, s.wrap(err)
	}
	caps.Version = version
	for _, name := range []string{"shared_preload_libraries", "log_min_duration_statement", "log_destination", "logging_collector", "track_activity_query_size", "max_connections", "track_io_timing", "compute_query_id", "pg_stat_statements.track"} {
		var v *string
		if err := s.pool.QueryRow(ctx, "/* db-collector */ SELECT current_setting($1, true)", name).Scan(&v); err == nil && v != nil {
			caps.Settings[name] = *v
		}
	}
	var privileged bool
	if err := s.pool.QueryRow(ctx, `/* db-collector */ SELECT COALESCE(r.rolsuper, false) OR (to_regrole('pg_read_all_stats') IS NOT NULL AND pg_has_role(current_user, 'pg_read_all_stats', 'member')) FROM pg_roles r WHERE r.rolname = current_user`).Scan(&privileged); err != nil {
		return caps, s.wrap(err)
	}

	// statements
	st := collector.Capability{Available: true}
	if !strings.Contains(caps.Settings["shared_preload_libraries"], "pg_stat_statements") {
		st.Available = false
		st.Problems = append(st.Problems, "pg_stat_statements is not in shared_preload_libraries: add it to postgresql.conf and restart PostgreSQL")
	}
	cols, err := s.columns(ctx, "pg_stat_statements")
	if err != nil {
		return caps, s.wrap(err)
	}
	if len(cols) == 0 {
		st.Available = false
		st.Problems = append(st.Problems, "the extension is not created in this database: run CREATE EXTENSION pg_stat_statements; in it")
	} else if _, _, ok := StatementsQuery(cols, 1); !ok {
		st.Available = false
		st.Problems = append(st.Problems, "this version of pg_stat_statements lacks columns the collector needs")
	}
	if !privileged {
		st.Problems = append(st.Problems, "the role cannot see other users' statements or their text: GRANT pg_read_all_stats (or pg_monitor) TO "+s.roleName())
	}
	if caps.Settings["compute_query_id"] == "off" {
		st.Problems = append(st.Problems, "compute_query_id is off, so statements have no id: set it to auto or on")
		st.Available = false
	}
	caps.Statements = st

	// activity
	act := collector.Capability{Available: true}
	if !privileged {
		act.Problems = append(act.Problems, "the role cannot see other users' sessions: GRANT pg_read_all_stats (or pg_monitor) TO "+s.roleName())
	}
	if n, _ := strconv.Atoi(caps.Settings["track_activity_query_size"]); n > 0 && n < 1024 {
		act.Problems = append(act.Problems, "track_activity_query_size is "+caps.Settings["track_activity_query_size"]+": statements are cut very short in pg_stat_activity")
	}
	caps.Activity = act

	// slow log
	slow := collector.Capability{}
	switch {
	case !s.cfg.SlowLog.Enabled:
		slow.Problems = append(slow.Problems, "the slow log is not enabled for this target (slowLog in the collector's configuration)")
	default:
		slow.Available = true
		if v := caps.Settings["log_min_duration_statement"]; v == "-1" || v == "" {
			slow.Available = false
			slow.Problems = append(slow.Problems, "log_min_duration_statement is off: set it (for example to 250ms) so that the server logs slow statements")
		}
		if d := caps.Settings["log_destination"]; d != "" && !strings.Contains(d, "stderr") {
			slow.Available = false
			slow.Problems = append(slow.Problems, "log_destination is "+d+": only the stderr text format is read")
		}
		if s.cfg.SlowLog.Path != "" {
			if _, err := os.Stat(s.cfg.SlowLog.Path); err != nil {
				slow.Available = false
				slow.Problems = append(slow.Problems, "the log file "+s.cfg.SlowLog.Path+" cannot be read from where the collector runs: "+errText(err))
			}
		}
		if caps.Settings["logging_collector"] == "off" && s.cfg.SlowLog.Path != "" {
			slow.Problems = append(slow.Problems, "logging_collector is off, so the server logs to its standard error: the file must be where that is written to")
		}
	}
	caps.SlowLog = slow
	return caps, nil
}

func errText(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// roleName is the role the collector connects as, for the message that says what to grant.
func (s *source) roleName() string {
	if cfg, err := pgx.ParseConfig(s.cfg.DSN); err == nil && cfg.User != "" {
		return cfg.User
	}
	return "the collector's role"
}
