package pgbouncer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sojoner/kro-lab/platform-mvp/db-collector/collector"
)

// tbl builds a Table from "a|b|c" lines: the first is the column names.
func tbl(lines ...string) Table {
	t := Table{Cols: strings.Split(lines[0], "|")}
	for _, l := range lines[1:] {
		vals := strings.Split(l, "|")
		row := map[string]string{}
		for i, c := range t.Cols {
			if i < len(vals) {
				row[c] = vals[i]
			}
		}
		t.Rows = append(t.Rows, row)
	}
	return t
}

// What PgBouncer 1.26.1 answered (columns and rows as the console gave them), in transaction mode with default_pool_size = 2.
var (
	pools126 = tbl("database|user|cl_active|cl_waiting|cl_active_cancel_req|cl_waiting_cancel_req|sv_active|sv_active_cancel|sv_being_canceled|sv_idle|sv_used|sv_tested|sv_login|maxwait|maxwait_us|pool_mode|load_balance_hosts",
		"pgbouncer|pgbouncer|1|0|0|0|0|0|0|0|0|0|0|0|0|statement|",
		"shop|app|0|0|0|0|0|0|0|1|0|0|0|0|0|transaction|")
	stats126 = tbl("database|total_server_assignment_count|total_xact_count|total_query_count|total_received|total_sent|total_xact_time|total_query_time|total_wait_time|total_client_parse_count|total_server_parse_count|total_bind_count|total_client_login_count|avg_server_assignment_count|avg_xact_count|avg_query_count|avg_recv|avg_sent|avg_xact_time|avg_query_time|avg_wait_time|avg_client_parse_count|avg_server_parse_count|avg_bind_count|avg_client_login_count",
		"pgbouncer|0|3|3|0|0|0|0|0|0|0|0|3|0|0|0|0|0|0|0|0|0|0|0|0",
		"shop|1|1|1|14|108|757|757|121914|0|0|0|1|0|0|0|2|23|757|757|121914|0|0|0|0")
	dbs126 = tbl("name|host|port|database|force_user|pool_size|min_pool_size|reserve_pool_size|server_lifetime|pool_mode|load_balance_hosts|max_connections|current_connections|max_client_connections|current_client_connections|paused|disabled",
		"pgbouncer||6432|pgbouncer|pgbouncer|2|0|0|3600|statement||0|0|0|1|0|0",
		"shop|pgbp-pg|5432|shop||2|0|1|3600|||0|1|0|0|0|0")
	cfg126 = tbl("key|value|default|changeable",
		"admin_users|pgbadmin||yes", "default_pool_size|2|20|yes", "ignore_startup_parameters|extra_float_digits||yes", "max_client_conn|100|100|yes",
		"pool_mode|transaction|session|yes", "reserve_pool_size|1|0|yes", "stats_users|collector||yes")
)

// PgBouncer 1.22.1: fewer columns, and "reserve_pool" where the newer one says "reserve_pool_size".
var (
	pools122 = tbl("database|user|cl_active|cl_waiting|cl_active_cancel_req|cl_waiting_cancel_req|sv_active|sv_active_cancel|sv_being_canceled|sv_idle|sv_used|sv_tested|sv_login|maxwait|maxwait_us|pool_mode",
		"pgbouncer|pgbouncer|1|0|0|0|0|0|0|0|0|0|0|0|0|statement",
		"shop|app|6|4|0|0|2|0|0|0|0|0|0|3|250000|transaction")
	stats122 = tbl("database|total_xact_count|total_query_count|total_received|total_sent|total_xact_time|total_query_time|total_wait_time|avg_xact_count|avg_query_count|avg_recv|avg_sent|avg_xact_time|avg_query_time|avg_wait_time",
		"shop|10|12|1000|2000|5000000|4000000|3000000|0|0|0|0|0|0|0")
	dbs122 = tbl("name|host|port|database|force_user|pool_size|min_pool_size|reserve_pool|pool_mode|max_connections|current_connections|paused|disabled",
		"shop|pg|5432|shop||2|0|1||0|2|0|0")
)

func TestPgBouncer126(t *testing.T) {
	s := BuildSnapshot(pools126, stats126, dbs126, cfg126)
	if len(s.Pools) != 1 {
		t.Fatalf("the console's own pool is the collector's connection and is not reported: %+v", s.Pools)
	}
	p := s.Pools[0]
	if p.Database != "shop" || p.User != "app" || p.Mode != "transaction" || p.SvIdle != 1 || p.ClActive != 0 || p.ClWaiting != 0 || p.PoolSize != 2 || p.ReservePool != 1 || p.MaxWaitMs != 0 {
		t.Fatalf("%+v", p)
	}
	if s.MaxClientConn != 100 {
		t.Fatalf("%d", s.MaxClientConn)
	}
	c, ok := s.Counters["shop"]
	if !ok || len(s.Counters) != 1 {
		t.Fatalf("%v", s.Counters)
	}
	// the times are microseconds in the console and milliseconds here
	if c["xacts"] != 1 || c["queries"] != 1 || c["bytes_in"] != 14 || c["bytes_out"] != 108 || c["xact_ms"] != 0.757 || c["query_ms"] != 0.757 || c["wait_ms"] != 121.914 {
		t.Fatalf("%v", c)
	}
}

func TestPgBouncer122AndAPoolWithClientsWaiting(t *testing.T) {
	s := BuildSnapshot(pools122, stats122, dbs122, tbl("key|value|default|changeable", "default_pool_size|20|20|yes", "reserve_pool_size|0|0|yes", "max_client_conn|200|100|yes"))
	if len(s.Pools) != 1 {
		t.Fatalf("%+v", s.Pools)
	}
	p := s.Pools[0]
	if p.ClActive != 6 || p.ClWaiting != 4 || p.SvActive != 2 || p.PoolSize != 2 || p.ReservePool != 1 {
		t.Fatalf("the database's own limits win over the defaults, and the older column name is read: %+v", p)
	}
	if p.MaxWaitMs != 3250 {
		t.Fatalf("3 seconds and 250000 microseconds more: %v ms", p.MaxWaitMs)
	}
	if s.Counters["shop"]["wait_ms"] != 3000 || s.Counters["shop"]["xact_ms"] != 5000 {
		t.Fatalf("%v", s.Counters["shop"])
	}
	if _, has := s.Counters["shop"]["bytes_in"]; !has {
		t.Fatal("a column that is there is read")
	}
}

func TestLimitsThatAreNotSetOnTheDatabaseComeFromTheDefaults(t *testing.T) {
	dbs := tbl("name|pool_size|reserve_pool_size|paused|disabled", "shop|0|0|1|0", "other|5|2|0|1")
	pools := tbl("database|user|cl_active|cl_waiting|sv_active|sv_idle|maxwait|maxwait_us|pool_mode",
		"shop|a|1|0|1|0|0|0|", "other|b|0|0|0|0|0|0|session", "unlisted|c|0|0|0|0|0|0|")
	s := BuildSnapshot(pools, tbl("database"), dbs, tbl("key|value", "default_pool_size|20", "reserve_pool_size|3", "pool_mode|transaction"))
	by := map[string]collector.Pool{}
	for _, p := range s.Pools {
		by[p.Database] = p
	}
	if p := by["shop"]; p.PoolSize != 20 || p.ReservePool != 3 || !p.Paused || p.Mode != "transaction" {
		t.Fatalf("defaults, and the mode of the pooler when the pool does not say: %+v", p)
	}
	if p := by["other"]; p.PoolSize != 5 || p.ReservePool != 2 || !p.Disabled || p.Mode != "session" {
		t.Fatalf("%+v", p)
	}
	if p := by["unlisted"]; p.PoolSize != 20 || p.ReservePool != 3 {
		t.Fatalf("a pool whose database is not listed gets the defaults: %+v", p)
	}
}

func TestOldStatsColumnsAreRead(t *testing.T) {
	// before PgBouncer 1.8 the query count was "total_requests"
	s := BuildSnapshot(tbl("database"), tbl("database|total_requests|total_received|total_sent|total_query_time", "shop|50|10|20|2000000"), tbl("name"), tbl("key|value"))
	c := s.Counters["shop"]
	if c["queries"] != 50 || c["query_ms"] != 2000 {
		t.Fatalf("%v", c)
	}
	if _, has := c["xacts"]; has {
		t.Fatal("a figure that is not in the answer is not made up")
	}
}

func TestNothingThatIsNotANumberCanBreakIt(t *testing.T) {
	s := BuildSnapshot(
		tbl("database|user|cl_active|cl_waiting|sv_active|maxwait|maxwait_us", "shop|a|x|-5|1.5|nope|"),
		tbl("database|total_xact_count|total_wait_time", "shop|abc|-7", "|1|1"),
		tbl("name|pool_size", "shop|many"),
		tbl("key|value", "default_pool_size|lots", "max_client_conn|-1"),
	)
	if len(s.Pools) != 1 || s.Pools[0].ClActive != 0 || s.Pools[0].ClWaiting != 0 || s.Pools[0].SvActive != 0 || s.Pools[0].MaxWaitMs != 0 || s.Pools[0].PoolSize != 0 {
		t.Fatalf("%+v", s.Pools)
	}
	if c := s.Counters["shop"]; c["xacts"] != 0 || c["wait_ms"] != 0 || len(s.Counters) != 1 {
		t.Fatalf("%v", s.Counters)
	}
	if s.MaxClientConn != 0 {
		t.Fatal(s.MaxClientConn)
	}
	// and an empty answer is an empty snapshot
	if e := BuildSnapshot(Table{}, Table{}, Table{}, Table{}); len(e.Pools) != 0 || len(e.Counters) != 0 {
		t.Fatalf("%+v", e)
	}
}

func TestCapabilities(t *testing.T) {
	ok := BuildCapabilities("PgBouncer 1.26.1", cfg126, "collector", nil)
	if ok.Engine != "pgbouncer" || ok.Version != "1.26.1" || !ok.Pools.Available || len(ok.Pools.Problems) != 0 {
		t.Fatalf("%+v", ok)
	}
	if ok.Settings["pool_mode"] != "transaction" || ok.Settings["default_pool_size"] != "2" || ok.Settings["max_client_conn"] != "100" || ok.Settings["ignore_startup_parameters"] != "extra_float_digits" {
		t.Fatalf("%v", ok.Settings)
	}
	if _, leaked := ok.Settings["admin_users"]; leaked {
		t.Fatal("who the admins are is not something to publish")
	}
	if ok.Statements.Available || ok.Activity.Available || ok.SlowLog.Available {
		t.Fatalf("a pooler has pools and nothing else: %+v", ok)
	}

	admin := BuildCapabilities("PgBouncer 1.26.1", cfg126, "pgbadmin", nil)
	if !admin.Pools.Available || len(admin.Pools.Problems) != 1 || !strings.Contains(admin.Pools.Problems[0], "admin_users") || !strings.Contains(admin.Pools.Problems[0], "stats_users") {
		t.Fatalf("an admin user can do more than look, and is told so: %+v", admin.Pools)
	}
	// "collector" is not matched by a longer name that contains it
	if c := BuildCapabilities("1", tbl("key|value", "admin_users|collector2, other"), "collector", nil); len(c.Pools.Problems) != 0 {
		t.Fatalf("%+v", c.Pools)
	}
	if c := BuildCapabilities("1", tbl("key|value", "admin_users|a, collector ,b"), "collector", nil); len(c.Pools.Problems) != 1 {
		t.Fatalf("a list with spaces: %+v", c.Pools)
	}

	failed := BuildCapabilities("PgBouncer 1.22.1", Table{}, "app", errors.New("ERROR: not allowed"))
	if failed.Pools.Available || len(failed.Pools.Problems) != 1 || !strings.Contains(failed.Pools.Problems[0], "stats_users") || !strings.Contains(failed.Pools.Problems[0], "not allowed") {
		t.Fatalf("%+v", failed.Pools)
	}
	if s := BuildCapabilities("1", tbl("key|value", "pool_mode|session"), "", nil); !strings.Contains(s.Settings["note"], "session") {
		t.Fatalf("%v", s.Settings)
	}
}

// The console can PAUSE, KILL and SHUTDOWN. Only the five SHOW commands the collector needs may ever be sent, whatever asks.
func TestOnlyTheAllowedCommandsAreEverSent(t *testing.T) {
	s := &source{} // no pool: a command that was not refused first would panic on it
	for _, cmd := range []string{"PAUSE", "RESUME", "KILL shop", "SHUTDOWN", "RELOAD", "SET default_pool_size=1", "DISABLE shop", "SHOW POOLS; SHUTDOWN", "SHOW POOLS ", " SHOW POOLS", "show pools",
		"SHOW CLIENTS", "SHOW SERVERS", "SHOW FDS", "SHOW USERS", "", "SELECT 1", "SHOW POOLS --", "SHOW  POOLS"} {
		_, err := s.show(context.Background(), cmd)
		if err == nil || !strings.Contains(err.Error(), "refusing to send") {
			t.Errorf("%q: %v", cmd, err)
		}
	}
	for _, cmd := range []string{cmdVersion, cmdPools, cmdStats, cmdDatabases, cmdConfig} {
		if !strings.HasPrefix(cmd, "SHOW ") || strings.ContainsAny(cmd, ";-") {
			t.Errorf("%q", cmd)
		}
		if !allowed[cmd] {
			t.Errorf("%q is not in the allowed list", cmd)
		}
	}
	if len(allowed) != 5 {
		t.Fatalf("the list of what may be sent grew to %d: every addition is a decision about what the collector may do to a pooler", len(allowed))
	}
}

func TestOpenRefusesAConnectionThatIsNotTheConsole(t *testing.T) {
	for _, dsn := range []string{
		"postgres://collector:s3cr3t-Pw@pgb:6432/shop",
		"postgres://collector:s3cr3t-Pw@pgb:6432/",
		"host=pgb port=6432 user=collector password=s3cr3t-Pw dbname=postgres",
	} {
		_, err := Engine{}.Open(context.Background(), collector.TargetConfig{Name: "pooler", DSN: dsn})
		if err == nil || !strings.Contains(err.Error(), `"pgbouncer"`) || !strings.Contains(err.Error(), "admin console") {
			t.Errorf("%q: %v", dsn, err)
		}
		if err != nil && strings.Contains(err.Error(), "s3cr3t-Pw") {
			t.Errorf("the password is in: %v", err)
		}
	}
	_, err := Engine{}.Open(context.Background(), collector.TargetConfig{Name: "pooler", DSN: "postgres://u:s3cr3t-Pw@h:notaport/pgbouncer"})
	if err == nil || strings.Contains(err.Error(), "s3cr3t-Pw") || !strings.Contains(err.Error(), "pooler") {
		t.Fatalf("%v", err)
	}
	src, err := Engine{}.Open(context.Background(), collector.TargetConfig{Name: "pooler", DSN: "postgres://collector:pw@127.0.0.1:1/pgbouncer?sslmode=disable"})
	if err != nil {
		t.Fatal(err)
	}
	src.Close()
}

func TestTheEngine(t *testing.T) {
	if (Engine{}).Name() != "pgbouncer" {
		t.Fatal()
	}
	if s := (Engine{}).Signals(); !s.Pools || s.Statements || s.Activity || s.SlowLog {
		t.Fatalf("%+v", s)
	}
	if _, err := (Engine{}).SlowLog(collector.TargetConfig{}); err == nil {
		t.Fatal("a pooler has no slow log")
	}
	if _, known := collector.SupportedSignals("pgbouncer"); !known {
		t.Fatal("not registered")
	}
}

func TestAnErrorFromTheConsoleNeverCarriesThePassword(t *testing.T) {
	s := &source{cfg: collector.TargetConfig{DSN: "postgres://collector:pw-Sekret-1@pgb:6432/pgbouncer"}, secret: "pw-Sekret-1"}
	err := s.wrap(errors.New("failed to connect: password pw-Sekret-1 for postgres://collector:pw-Sekret-1@pgb:6432/pgbouncer"))
	if strings.Contains(err.Error(), "pw-Sekret-1") {
		t.Fatalf("%v", err)
	}
	if !errors.Is(s.wrap(context.Canceled), context.Canceled) {
		t.Fatal("a cancelled context must still be one")
	}
}
