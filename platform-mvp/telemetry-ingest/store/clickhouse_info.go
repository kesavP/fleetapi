package store

import (
	"context"
	"log/slog"
	"time"
)

// What the Configuration page's ClickHouse tab shows: how the service is connected, how much each kind of data takes, and (when the
// service is told to share it) the server's disks.
//
// What never leaves the service: the address, the user and the password. They are not in these types at all, so no path through the code can
// return them; the page draws "<hidden>" itself.
//
// Per project by default. Whole-table sizes and the disks describe the platform every project shares, so they are returned only when
// asked for (the service's --expose-cluster-info): a key that reads one project should not learn how much another stores.

// ClickHouseInfo is the answer.
type ClickHouseInfo struct {
	Connection ChConnection `json:"connection"`
	Storage    []ChStorage  `json:"storage"`
	Cluster    *ChCluster   `json:"cluster"` // nil unless shared
	Warnings   []string     `json:"warnings"`
}

type ChConnection struct {
	Reachable bool    `json:"reachable"`
	Error     string  `json:"error,omitempty"` // a fixed sentence, never the driver's message (it can carry the address)
	Protocol  string  `json:"protocol"`        // "native"
	Database  string  `json:"database"`
	TLS       bool    `json:"tls"`
	Version   string  `json:"version,omitempty"`
	PingMs    float64 `json:"ping_ms"`
}

// ChStorage is one kind of data (metrics, traces, logs, profiles).
type ChStorage struct {
	Type           string     `json:"type"`
	Rows           int64      `json:"rows"`            // this project's rows in the main table of the kind
	EstimatedBytes *int64     `json:"estimated_bytes"` // this project's share of the compressed size, by its share of the rows; nil when unknown
	Compression    float64    `json:"compression"`     // uncompressed over compressed, for the kind's tables; 0 when unknown
	TTLSeconds     int64      `json:"ttl_seconds"`     // the retention the service is configured with
	Oldest         *time.Time `json:"oldest"`          // this project's oldest row; nil when it has none
	TableBytes     *int64     `json:"table_bytes,omitempty"`
	TableRows      *int64     `json:"table_rows,omitempty"`
}

type ChCluster struct {
	Servers []ChServer `json:"servers"`
}

type ChServer struct {
	Host  string   `json:"host"`
	Disks []ChDisk `json:"disks"`
}

type ChDisk struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Type  string `json:"type"`
	Free  int64  `json:"free"`
	Total int64  `json:"total"`
	// Unlimited: an object-storage disk (a bucket) has no size, and ClickHouse reports it as 2^64-1. Free and Total are then 0.
	Unlimited bool `json:"unlimited,omitempty"`
	// Used: what ClickHouse has stored on the disk, all databases together.
	Used int64 `json:"used"`
}

// diskSize turns ClickHouse's free and total space into numbers that fit a JSON integer, and says when the disk has no limit.
func diskSize(free, total uint64) (f, t int64, unlimited bool) {
	if total >= 1<<62 {
		return 0, 0, true
	}
	if free > total {
		free = total
	}
	return int64(free), int64(total), false
}

// chKinds: each kind of data, the table that holds the rows people think of (its "main" table), the column that dates them, and the
// other tables that belong to it.
var chKinds = []struct {
	name   string
	main   string
	dated  string
	tables []string
	ttl    func(TTLs) time.Duration
}{
	{"metrics", "metrics_samples", "Timestamp", []string{"metrics_samples", "metrics_series", "metrics_metadata"}, func(t TTLs) time.Duration { return t.Metrics }},
	{"traces", "traces_spans", "Timestamp", []string{"traces_spans"}, func(t TTLs) time.Duration { return t.Traces }},
	{"logs", "logs_records", "Timestamp", []string{"logs_records"}, func(t TTLs) time.Duration { return t.Logs }},
	{"profiles", "profiling_samples", "Start", []string{"profiling_samples", "profiling_stacks", "profiling_services"}, func(t TTLs) time.Duration { return t.Profiles }},
	{"databases", "db_statements", "Time", []string{"db_statements", "db_activity", "db_slow_queries", "db_targets", "db_pool_samples", "db_pools", "db_pool_stats"}, func(t TTLs) time.Duration { return t.Databases }},
}

// chTable is a whole table, every project's rows together.
type chTable struct{ rows, compressed, uncompressed uint64 }

// chMine is one project's rows in a table.
type chMine struct {
	rows   uint64
	oldest time.Time
}

// buildStorage works out the storage rows from what was read. Pure. `parts` is nil when the sizes could not be read.
func buildStorage(ttl TTLs, parts map[string]chTable, mine map[string]chMine, share bool) []ChStorage {
	out := make([]ChStorage, 0, len(chKinds))
	for _, k := range chKinds {
		s := ChStorage{Type: k.name, TTLSeconds: int64(k.ttl(ttl) / time.Second)}
		if m, ok := mine[k.main]; ok {
			s.Rows = int64(m.rows)
			if m.rows > 0 {
				t := m.oldest.UTC()
				s.Oldest = &t
			}
		}
		if parts != nil {
			var comp, uncomp, rows float64
			var tableBytes, tableRows int64
			var est float64
			for _, name := range k.tables {
				p := parts[name]
				comp += float64(p.compressed)
				uncomp += float64(p.uncompressed)
				rows += float64(p.rows)
				tableBytes += int64(p.compressed)
				if name == k.main {
					tableRows = int64(p.rows)
				}
				// This project's share of the table's bytes is its share of the table's rows. Rows differ in size, so it is an estimate.
				if p.rows > 0 {
					est += float64(p.compressed) * float64(mine[name].rows) / float64(p.rows)
				}
			}
			e := int64(est + 0.5)
			s.EstimatedBytes = &e
			if comp > 0 {
				s.Compression = uncomp / comp
			}
			if share {
				s.TableBytes = &tableBytes
				s.TableRows = &tableRows
			}
		}
		out = append(out, s)
	}
	return out
}

// ClickHouseInfo reads the answer for a project. `share` adds the whole-table sizes and the server's disks. It does not fail because one
// part could not be read (a database user that may not read the system tables, say): that part is left out and a warning says so. Only an
// unreachable database is reported as such, with a fixed sentence.
func (s *Store) ClickHouseInfo(ctx context.Context, project string, share bool) (ClickHouseInfo, error) {
	info := ClickHouseInfo{
		Connection: ChConnection{Protocol: "native", Database: s.database, TLS: s.tls},
		Storage:    []ChStorage{},
		Warnings:   []string{},
	}
	started := time.Now()
	if err := s.conn.Ping(ctx); err != nil {
		slog.Warn("clickhouse info: ping failed", "error", err)
		info.Connection.Error = "ClickHouse did not answer."
		return info, nil
	}
	info.Connection.Reachable = true
	info.Connection.PingMs = float64(time.Since(started).Microseconds()) / 1000
	if err := s.conn.QueryRow(ctx, "SELECT version()").Scan(&info.Connection.Version); err != nil {
		slog.Warn("clickhouse info: version failed", "error", err)
	}

	parts, err := s.tableSizes(ctx)
	if err != nil {
		slog.Warn("clickhouse info: sizes failed", "error", err)
		info.Warnings = append(info.Warnings, "Table sizes are not available: the database user cannot read the system tables.")
		parts = nil
	}
	mine, err := s.projectRows(ctx, project)
	if err != nil {
		return info, err
	}
	info.Storage = buildStorage(s.ttl, parts, mine, share)

	if share {
		cluster, err := s.disks(ctx)
		if err != nil {
			slog.Warn("clickhouse info: disks failed", "error", err)
			info.Warnings = append(info.Warnings, "The server's disks are not available: the database user cannot read the system tables.")
		} else {
			info.Cluster = cluster
		}
	}
	return info, nil
}

// tableSizes: every table of the database, all projects together.
func (s *Store) tableSizes(ctx context.Context) (map[string]chTable, error) {
	rows, err := s.conn.Query(ctx, `SELECT table, sum(rows), sum(data_compressed_bytes), sum(data_uncompressed_bytes)
FROM system.parts WHERE active AND database = ? GROUP BY table`, s.database)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]chTable{}
	for rows.Next() {
		var name string
		var t chTable
		if err := rows.Scan(&name, &t.rows, &t.compressed, &t.uncompressed); err != nil {
			return nil, err
		}
		out[name] = t
	}
	return out, rows.Err()
}

// projectRows: this project's rows in each table, and its oldest. The table names are constants of this package, never input, so they may
// be spliced in; the project is a parameter.
func (s *Store) projectRows(ctx context.Context, project string) (map[string]chMine, error) {
	out := map[string]chMine{}
	for _, k := range chKinds {
		for _, table := range k.tables {
			var m chMine
			if table == k.main {
				if err := s.conn.QueryRow(ctx, "SELECT count(), min("+k.dated+") FROM "+table+" WHERE Project = ?", project).Scan(&m.rows, &m.oldest); err != nil {
					return nil, err
				}
			} else if err := s.conn.QueryRow(ctx, "SELECT count() FROM "+table+" WHERE Project = ?", project).Scan(&m.rows); err != nil {
				return nil, err
			}
			out[table] = m
		}
	}
	return out, nil
}

// disks: the server this service is connected to, and its disks.
func (s *Store) disks(ctx context.Context) (*ChCluster, error) {
	var host string
	if err := s.conn.QueryRow(ctx, "SELECT hostName()").Scan(&host); err != nil {
		return nil, err
	}
	rows, err := s.conn.Query(ctx, "SELECT name, path, type, free_space, total_space FROM system.disks ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	srv := ChServer{Host: host, Disks: []ChDisk{}}
	for rows.Next() {
		var d ChDisk
		var free, total uint64
		if err := rows.Scan(&d.Name, &d.Path, &d.Type, &free, &total); err != nil {
			return nil, err
		}
		d.Free, d.Total, d.Unlimited = diskSize(free, total)
		srv.Disks = append(srv.Disks, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	used, err := s.conn.Query(ctx, "SELECT disk_name, sum(bytes_on_disk) FROM system.parts WHERE active GROUP BY disk_name")
	if err != nil {
		return nil, err
	}
	defer used.Close()
	bytesOn := map[string]int64{}
	for used.Next() {
		var name string
		var n uint64
		if err := used.Scan(&name, &n); err != nil {
			return nil, err
		}
		bytesOn[name] = int64(n)
	}
	if err := used.Err(); err != nil {
		return nil, err
	}
	for i := range srv.Disks {
		srv.Disks[i].Used = bytesOn[srv.Disks[i].Name]
	}
	return &ChCluster{Servers: []ChServer{srv}}, nil
}
