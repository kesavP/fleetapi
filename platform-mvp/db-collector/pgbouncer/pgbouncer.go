// Package pgbouncer is the PgBouncer engine of the collector: the state of its connection pools, read from its admin console.
//
// A connection pooler in front of a database hides one failure from the database itself: when the pool is too small or its servers are slow,
// clients queue at the pooler, the database sees a handful of busy connections, and the application sees latency that nothing in the database
// explains. What shows it is here: clients waiting (cl_waiting), how long the oldest has waited (maxwait), server connections against the
// pool size, and, over time, how much of the clients' time went on waiting for a server connection.
//
// The collector connects to the console (the database named "pgbouncer") as a user in stats_users, which can only look, and issues a fixed
// list of read-only SHOW commands; any other text is refused before it is sent. A user that is in admin_users is reported, because the
// console would let it PAUSE, KILL and SHUTDOWN. It works in every pool mode: the console is the pooler's own and never goes through a pool.
//
//	; pgbouncer.ini
//	stats_users = collector
//	; userlist.txt (or auth_query): the collector's user and password
//
// The console accepts only simple-protocol queries, which is what the collector uses; it rejects "SELECT 1" and ";" (so no ping is made), and
// it rejects most startup parameters, so only application_name is sent.
package pgbouncer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sojoner/kro-lab/platform-mvp/db-collector/collector"
)

func init() { collector.Register(Engine{}) }

type Engine struct{}

func (Engine) Name() string { return "pgbouncer" }

func (Engine) Signals() collector.Signals { return collector.Signals{Pools: true} }

func (Engine) SlowLog(collector.TargetConfig) (collector.SlowLogSource, error) {
	return nil, errors.New("pgbouncer has no slow-statement log: collect that from the database behind it")
}

// The commands the collector may send. Nothing else is ever sent, whatever calls show().
const (
	cmdVersion   = "SHOW VERSION"
	cmdPools     = "SHOW POOLS"
	cmdStats     = "SHOW STATS"
	cmdDatabases = "SHOW DATABASES"
	cmdConfig    = "SHOW CONFIG"
)

var allowed = map[string]bool{cmdVersion: true, cmdPools: true, cmdStats: true, cmdDatabases: true, cmdConfig: true}

// Open prepares the connection. It does not connect: a pooler that is down when the collector starts is not a reason to stop.
func (Engine) Open(ctx context.Context, t collector.TargetConfig) (collector.Source, error) {
	cfg, err := pgxpool.ParseConfig(t.DSN)
	if err != nil {
		return nil, fmt.Errorf("target %s: the connection string is not valid (%s)", t.Name, collector.Scrub(err.Error(), t.DSN))
	}
	if cfg.ConnConfig.Database != admin {
		return nil, fmt.Errorf("target %s: the connection string must name the database %q, which is PgBouncer's admin console (it names %q)", t.Name, admin, cfg.ConnConfig.Database)
	}
	cfg.MaxConns = 1
	cfg.MaxConnLifetime = time.Hour
	cc := cfg.ConnConfig
	cc.ConnectTimeout = 5 * time.Second
	cc.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol // the console accepts nothing else
	cc.RuntimeParams = map[string]string{"application_name": "db-collector"}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("target %s: %s", t.Name, collector.Scrub(err.Error(), t.DSN))
	}
	return &source{pool: pool, cfg: t, user: cc.User, secret: cc.Password}, nil
}

type source struct {
	pool   *pgxpool.Pool
	cfg    collector.TargetConfig
	user   string
	secret string
}

func (s *source) Close() { s.pool.Close() }

func (s *source) wrap(err error) error {
	return collector.Redact(err, s.cfg.DSN, s.secret)
}

// show runs one of the allowed commands and returns its answer as a Table.
func (s *source) show(ctx context.Context, cmd string) (Table, error) {
	if !allowed[cmd] {
		return Table{}, fmt.Errorf("refusing to send %q to the admin console: only SHOW commands the collector needs are allowed", cmd)
	}
	rows, err := s.pool.Query(ctx, cmd)
	if err != nil {
		return Table{}, s.wrap(err)
	}
	defer rows.Close()
	var t Table
	for _, f := range rows.FieldDescriptions() {
		t.Cols = append(t.Cols, f.Name)
	}
	for rows.Next() {
		raw := rows.RawValues()
		row := make(map[string]string, len(t.Cols))
		for i, c := range t.Cols {
			if i < len(raw) && raw[i] != nil {
				row[c] = string(raw[i])
			}
		}
		t.Rows = append(t.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return Table{}, s.wrap(err)
	}
	return t, nil
}

func (s *source) PoolSnapshot(ctx context.Context) (collector.PoolSnapshot, error) {
	var tables [4]Table
	for i, cmd := range []string{cmdPools, cmdStats, cmdDatabases, cmdConfig} {
		t, err := s.show(ctx, cmd)
		if err != nil {
			return collector.PoolSnapshot{}, err
		}
		tables[i] = t
	}
	return BuildSnapshot(tables[0], tables[1], tables[2], tables[3]), nil
}

func (s *source) Capabilities(ctx context.Context) (collector.Capabilities, error) {
	v, err := s.show(ctx, cmdVersion)
	if err != nil {
		// not even the version: the pooler cannot be asked (down, wrong port, wrong password): that is an error, not a finding
		return collector.Capabilities{Engine: "pgbouncer"}, err
	}
	version := ""
	if len(v.Rows) > 0 {
		for _, c := range v.Cols {
			version = v.Rows[0][c]
			break
		}
	}
	cfg, cfgErr := s.show(ctx, cmdConfig)
	_, poolsErr := s.show(ctx, cmdPools)
	if cfgErr != nil && poolsErr == nil {
		poolsErr = cfgErr
	}
	return BuildCapabilities(strings.TrimSpace(version), cfg, s.user, poolsErr), nil
}
