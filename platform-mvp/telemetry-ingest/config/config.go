// Package config turns command-line flags into a validated Config. Helm
// values are the source of truth; the flag defaults only keep a bare
// `telemetry-ingest` runnable.
package config

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

type Config struct {
	Listen       string // HTTP: ingest (/v1/*) and query (/api/v1/*)
	GRPCListen   string // OTLP/gRPC for traces and logs; empty disables
	ProjectsFile string

	ClickHouseAddrs          []string
	ClickHouseDatabase       string
	ClickHouseUser           string
	ClickHousePassword       string
	ClickHouseTLS            bool
	ClickHouseCreateDatabase bool

	TTLMetrics  time.Duration
	TTLTraces   time.Duration
	TTLLogs     time.Duration
	TTLProfiles time.Duration

	BatchRows     int
	MaxBuffered   int
	FlushInterval time.Duration
	MaxBodyBytes  int64
	ReloadEvery   time.Duration
}

func Parse(args []string) (Config, error) {
	fs := flag.NewFlagSet("telemetry-ingest", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var c Config
	var addrs, passwordFile string
	fs.StringVar(&c.Listen, "listen", ":8080", "Address for the HTTP ingest and query API")
	fs.StringVar(&c.GRPCListen, "grpc-listen", ":4317", "Address for OTLP/gRPC traces and logs (empty = disabled)")
	fs.StringVar(&c.ProjectsFile, "projects-file", "", "JSON file mapping projects to ingest and read API keys (required)")
	fs.StringVar(&addrs, "clickhouse-addr", "localhost:9000", "Comma-separated ClickHouse native-protocol addresses")
	fs.StringVar(&c.ClickHouseDatabase, "clickhouse-database", "telemetry", "ClickHouse database")
	fs.StringVar(&c.ClickHouseUser, "clickhouse-user", "default", "ClickHouse user")
	fs.StringVar(&passwordFile, "clickhouse-password-file", "", "File holding the ClickHouse password (empty = no password)")
	fs.BoolVar(&c.ClickHouseTLS, "clickhouse-tls", false, "Connect to ClickHouse over TLS")
	fs.BoolVar(&c.ClickHouseCreateDatabase, "clickhouse-create-database", true, "Create the database when it is missing")
	fs.DurationVar(&c.TTLMetrics, "ttl-metrics", 14*24*time.Hour, "Metric retention")
	fs.DurationVar(&c.TTLTraces, "ttl-traces", 7*24*time.Hour, "Trace retention")
	fs.DurationVar(&c.TTLLogs, "ttl-logs", 7*24*time.Hour, "Log retention")
	fs.DurationVar(&c.TTLProfiles, "ttl-profiles", 7*24*time.Hour, "Profile retention")
	fs.IntVar(&c.BatchRows, "batch-rows", 10000, "Rows per ClickHouse insert")
	fs.IntVar(&c.MaxBuffered, "max-buffered-rows", 200000, "Rows buffered per signal before ingest answers 503")
	fs.DurationVar(&c.FlushInterval, "flush-interval", 5*time.Second, "Longest a row waits before it is inserted")
	fs.Int64Var(&c.MaxBodyBytes, "max-body-bytes", 32<<20, "Largest request body, compressed and decompressed")
	fs.DurationVar(&c.ReloadEvery, "reload-interval", 10*time.Second, "How often the projects file is re-read")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	for _, a := range strings.Split(addrs, ",") {
		if a = strings.TrimSpace(a); a != "" {
			c.ClickHouseAddrs = append(c.ClickHouseAddrs, a)
		}
	}
	if passwordFile != "" {
		b, err := os.ReadFile(passwordFile)
		if err != nil {
			return Config{}, fmt.Errorf("read clickhouse-password-file: %w", err)
		}
		c.ClickHousePassword = strings.TrimSpace(string(b))
	}

	switch {
	case c.ProjectsFile == "":
		return Config{}, fmt.Errorf("projects-file is required")
	case len(c.ClickHouseAddrs) == 0:
		return Config{}, fmt.Errorf("clickhouse-addr must list at least one address")
	case !identifier(c.ClickHouseDatabase):
		return Config{}, fmt.Errorf("clickhouse-database %q must be letters, digits and underscores", c.ClickHouseDatabase)
	case c.TTLMetrics < time.Hour, c.TTLTraces < time.Hour, c.TTLLogs < time.Hour, c.TTLProfiles < time.Hour:
		return Config{}, fmt.Errorf("every ttl must be at least 1h")
	case c.BatchRows < 1:
		return Config{}, fmt.Errorf("batch-rows must be at least 1")
	case c.MaxBuffered < c.BatchRows:
		return Config{}, fmt.Errorf("max-buffered-rows must be at least batch-rows")
	case c.FlushInterval <= 0, c.ReloadEvery <= 0:
		return Config{}, fmt.Errorf("flush-interval and reload-interval must be positive")
	case c.MaxBodyBytes < 1<<10:
		return Config{}, fmt.Errorf("max-body-bytes must be at least 1024")
	}
	return c, nil
}

// identifier guards the one value that is spliced into SQL text.
func identifier(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
