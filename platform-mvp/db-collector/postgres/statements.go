package postgres

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sojoner/kro-lab/platform-mvp/db-collector/collector"
)

// pg_stat_statements has had a different set of columns in nearly every version: total_time became total_exec_time in 13, toplevel came in 14,
// stats_since in 17, blk_read_time became shared_blk_read_time in 17. So the query is not written once: it is built from the columns the view
// has on THIS server, which are asked for first. A column that is not there is not selected, and its figure is simply absent.

// extras are the counters kept besides calls, time and rows: (name in the view, name reported). The reported name is the newest one, so that a
// fleet with several versions reports one name.
var extras = []struct{ column, name string }{
	{"shared_blks_hit", "shared_blks_hit"},
	{"shared_blks_read", "shared_blks_read"},
	{"shared_blks_dirtied", "shared_blks_dirtied"},
	{"shared_blks_written", "shared_blks_written"},
	{"temp_blks_read", "temp_blks_read"},
	{"temp_blks_written", "temp_blks_written"},
	{"wal_bytes", "wal_bytes"},
	{"total_plan_time", "total_plan_time"},
	{"blk_read_time", "shared_blk_read_time"},
	{"shared_blk_read_time", "shared_blk_read_time"},
	{"blk_write_time", "shared_blk_write_time"},
	{"shared_blk_write_time", "shared_blk_write_time"},
}

// StatementsQuery is the SELECT for a server whose pg_stat_statements has `columns`, and the names of the extra counters it selects, in the
// order they come after the fixed columns. ok is false when the view lacks what a snapshot cannot do without (calls, time, rows, queryid, query).
func StatementsQuery(columns map[string]bool, textBytes int) (sql string, extraNames []string, ok bool) {
	total := ""
	switch {
	case columns["total_exec_time"]:
		total = "total_exec_time"
	case columns["total_time"]:
		total = "total_time"
	}
	for _, need := range []string{"queryid", "query", "calls", "rows"} {
		if !columns[need] {
			return "", nil, false
		}
	}
	if total == "" {
		return "", nil, false
	}
	var sel []string
	sel = append(sel,
		"COALESCE(d.datname, '')",
		"COALESCE(r.rolname, '')",
		"COALESCE(s.queryid::text, md5(s.query))",
		fmt.Sprintf("left(s.query, %d)", textBytes),
		"s.calls::float8",
		fmt.Sprintf("s.%s::float8", total),
		"s.rows::float8",
	)
	if columns["toplevel"] {
		sel = append(sel, "s.toplevel::text")
	} else {
		sel = append(sel, "'t'")
	}
	if columns["stats_since"] {
		sel = append(sel, "s.stats_since::text")
	} else {
		sel = append(sel, "''")
	}
	seen := map[string]bool{}
	for _, e := range extras {
		if !columns[e.column] || seen[e.name] {
			continue
		}
		seen[e.name] = true
		sel = append(sel, fmt.Sprintf("COALESCE(s.%s, 0)::float8", e.column))
		extraNames = append(extraNames, e.name)
	}
	sql = "/* db-collector */ SELECT " + strings.Join(sel, ", ") +
		" FROM pg_stat_statements s LEFT JOIN pg_database d ON d.oid = s.dbid LEFT JOIN pg_roles r ON r.oid = s.userid" +
		" WHERE s.query NOT LIKE '/* db-collector */%'"
	return sql, extraNames, true
}

// ColumnsQuery lists the columns of a view or table in pg_catalog or the extension's schema, for StatementsQuery.
const ColumnsQuery = `/* db-collector */ SELECT a.attname FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid WHERE c.relname = $1 AND c.relkind IN ('v', 'r') AND a.attnum > 0 AND NOT a.attisdropped`

// statementRow is one row of the statements query, as it is scanned.
type statementRow struct {
	Database, User, QueryID, Query string
	Calls, TotalMs, Rows           float64
	TopLevel, Since                string
	Extra                          []float64
}

// BuildSnapshot turns rows into a snapshot: the text normalized (the privacy boundary: this is where a statement's literals go), the key made
// of what identifies a statement, and the counters in the units the Differ works in.
func BuildSnapshot(rows []statementRow, extraNames []string, epoch string) collector.Snapshot {
	s := collector.Snapshot{Epoch: epoch, Stats: make([]collector.Cumulative, 0, len(rows))}
	for _, r := range rows {
		c := collector.Cumulative{
			Key:      strings.Join([]string{r.Database, r.User, r.QueryID, r.TopLevel}, "\x00"),
			QueryID:  r.QueryID,
			Database: r.Database,
			User:     r.User,
			Query:    collector.Normalize(r.Query, collector.MaxQueryBytes),
			Calls:    toUint(r.Calls),
			TotalMs:  r.TotalMs,
			Rows:     toUint(r.Rows),
			Reset:    r.Since,
		}
		if len(r.Extra) > 0 {
			c.Extra = make(map[string]float64, len(r.Extra))
			for i, v := range r.Extra {
				if i < len(extraNames) && v != 0 {
					c.Extra[extraNames[i]] = v
				}
			}
		}
		s.Stats = append(s.Stats, c)
	}
	sort.Slice(s.Stats, func(i, j int) bool { return s.Stats[i].Key < s.Stats[j].Key })
	return s
}

func toUint(f float64) uint64 {
	if f <= 0 {
		return 0
	}
	return uint64(f)
}
