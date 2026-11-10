package pgbouncer

import (
	"strconv"
	"strings"

	"github.com/sojoner/kro-lab/platform-mvp/db-collector/collector"
)

// Table is what an admin console command answered: the names of the columns, and each row by column name. PgBouncer has changed the columns
// of its SHOW commands from version to version (SHOW DATABASES has "reserve_pool" in 1.22 and "reserve_pool_size" in 1.26; SHOW STATS has
// gained columns), so nothing here reads a column by position, and a column that is not there is a figure that is not known, not an error.
type Table struct {
	Cols []string
	Rows []map[string]string
}

func (t Table) Has(col string) bool {
	for _, c := range t.Cols {
		if c == col {
			return true
		}
	}
	return false
}

func atoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func atof(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}

// first returns the first of the named columns that the row has.
func first(row map[string]string, names ...string) (string, bool) {
	for _, n := range names {
		if v, ok := row[n]; ok {
			return v, true
		}
	}
	return "", false
}

// admin is the pseudo database of the console itself. Its pool is the collector's own connection, so it is not reported.
const admin = "pgbouncer"

// config reads a setting from SHOW CONFIG (rows of key, value, default, changeable).
func config(cfg Table) map[string]string {
	out := map[string]string{}
	for _, r := range cfg.Rows {
		if k := r["key"]; k != "" {
			out[k] = r["value"]
		}
	}
	return out
}

// BuildSnapshot turns what the console answered into a snapshot: the pools, with the limits that apply to each (the database's own pool_size
// when it has one, else the default), and the counters of each database since PgBouncer started counting, in the units the PoolDiffer uses
// (times in milliseconds: PgBouncer counts microseconds).
func BuildSnapshot(pools, stats, databases, cfg Table) collector.PoolSnapshot {
	conf := config(cfg)
	defPool, defReserve := atoi(conf["default_pool_size"]), atoi(conf["reserve_pool_size"])
	type limits struct {
		size, reserve int
		paused, off   bool
	}
	byDB := map[string]limits{}
	for _, r := range databases.Rows {
		name := r["name"]
		l := limits{size: atoi(r["pool_size"]), paused: r["paused"] == "1", off: r["disabled"] == "1"}
		if v, ok := first(r, "reserve_pool_size", "reserve_pool"); ok {
			l.reserve = atoi(v)
		}
		if l.size == 0 {
			l.size = defPool
		}
		if l.reserve == 0 {
			l.reserve = defReserve
		}
		byDB[name] = l
	}

	snap := collector.PoolSnapshot{Counters: map[string]map[string]float64{}, MaxClientConn: atoi(conf["max_client_conn"])}
	for _, r := range pools.Rows {
		db := r["database"]
		if db == admin {
			continue
		}
		l := byDB[db]
		if _, known := byDB[db]; !known {
			l = limits{size: defPool, reserve: defReserve}
		}
		p := collector.Pool{
			Database: db, User: r["user"], Mode: r["pool_mode"],
			ClActive: atoi(r["cl_active"]), ClWaiting: atoi(r["cl_waiting"]),
			SvActive: atoi(r["sv_active"]), SvIdle: atoi(r["sv_idle"]), SvUsed: atoi(r["sv_used"]), SvTested: atoi(r["sv_tested"]), SvLogin: atoi(r["sv_login"]),
			// the oldest waiting client's wait: whole seconds, and the microseconds beyond them
			MaxWaitMs: atof(r["maxwait"])*1000 + atof(r["maxwait_us"])/1000,
			PoolSize:  l.size, ReservePool: l.reserve, Paused: l.paused, Disabled: l.off,
		}
		if p.Mode == "" {
			p.Mode = conf["pool_mode"]
		}
		snap.Pools = append(snap.Pools, p)
	}
	for _, r := range stats.Rows {
		db := r["database"]
		if db == admin || db == "" {
			continue
		}
		c := map[string]float64{}
		set := func(name string, cols []string, scale float64) {
			if v, ok := first(r, cols...); ok {
				c[name] = atof(v) / scale
			}
		}
		set("xacts", []string{"total_xact_count"}, 1)
		set("queries", []string{"total_query_count", "total_requests"}, 1)
		set("bytes_in", []string{"total_received"}, 1)
		set("bytes_out", []string{"total_sent"}, 1)
		set("xact_ms", []string{"total_xact_time"}, 1000) // microseconds
		set("query_ms", []string{"total_query_time"}, 1000)
		set("wait_ms", []string{"total_wait_time"}, 1000)
		snap.Counters[db] = c
	}
	return snap
}

// settings are the pooler's settings that say how pooling behaves; they go with the capabilities.
var settings = []string{"pool_mode", "default_pool_size", "min_pool_size", "reserve_pool_size", "reserve_pool_timeout", "max_client_conn", "max_db_connections", "max_user_connections", "max_db_client_connections", "ignore_startup_parameters", "server_idle_timeout", "query_wait_timeout"}

func csvHas(list, name string) bool {
	for _, p := range strings.Split(list, ",") {
		if strings.TrimSpace(p) == name {
			return true
		}
	}
	return false
}

// BuildCapabilities says whether the pools can be read and what to change when they cannot. `user` is who the collector connects as; `showErr`
// is why SHOW POOLS failed, or nil. The user is warned about when it can do more than look: PgBouncer's console lets an admin PAUSE and KILL
// pools and SHUTDOWN the pooler, and the collector only ever reads, so it should not hold that power.
func BuildCapabilities(version string, cfg Table, user string, showErr error) collector.Capabilities {
	conf := config(cfg)
	caps := collector.Capabilities{Engine: "pgbouncer", Version: strings.TrimPrefix(version, "PgBouncer "), Settings: map[string]string{}}
	for _, k := range settings {
		if v, ok := conf[k]; ok {
			caps.Settings[k] = v
		}
	}
	pools := collector.Capability{Available: showErr == nil}
	if showErr != nil {
		pools.Problems = append(pools.Problems, "the admin console did not answer SHOW POOLS ("+showErr.Error()+"): the collector's user must be in stats_users (which can only look) in pgbouncer.ini, and the connection string must name the database pgbouncer")
	}
	if user != "" && csvHas(conf["admin_users"], user) {
		pools.Problems = append(pools.Problems, "the user "+user+" is in admin_users, which can PAUSE, KILL and SHUTDOWN the pooler: the collector only reads, so give it a user in stats_users instead")
	}
	if conf["pool_mode"] == "session" {
		caps.Settings["note"] = "pool_mode is session: a client holds its server connection until it disconnects, so waiting clients mean the pool is held by idle clients"
	}
	caps.Pools = pools
	return caps
}
