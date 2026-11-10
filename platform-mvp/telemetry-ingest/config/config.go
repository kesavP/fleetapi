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
	// ExposeClusterInfo lets a project's read key see the whole-table sizes and the server's disks on the ClickHouse information page.
	// They describe the platform every project shares, so they are off unless asked for.
	ExposeClusterInfo bool

	// Tiered storage. StoragePolicy names a ClickHouse storage policy (set up in the server's configuration, e.g. a local disk plus a
	// bucket) for the four big tables; ColdVolume is its slow volume and ColdAfter how old data must be before it moves there.
	// Empty StoragePolicy: everything stays on the default disk.
	StoragePolicy string
	ColdVolume    string
	ColdAfter     time.Duration

	TTLMetrics  time.Duration
	TTLTraces   time.Duration
	TTLLogs     time.Duration
	TTLProfiles time.Duration
	// TTLDatabases is how long what the database collectors report is kept: statement statistics, session samples, slow statements.
	TTLDatabases time.Duration

	// Incident detection: every DetectInterval each project's inbound request metrics are judged against an availability and a latency
	// objective, and an incident is opened for an application that burns its error budget DetectBurn times faster than allowed. Zero
	// DetectInterval switches it off.
	DetectInterval       time.Duration
	DetectLookback       time.Duration
	DetectMaxAge         time.Duration
	DetectAvailability   float64
	DetectLatency        float64
	DetectLatencySeconds float64
	DetectBurn           float64

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
	fs.BoolVar(&c.ExposeClusterInfo, "expose-cluster-info", false, "Show whole-table sizes and the server's disks to every project's read key (they describe the platform all projects share)")
	fs.StringVar(&c.StoragePolicy, "storage-policy", "", "ClickHouse storage policy for the metrics, traces, logs and profile tables (empty = the default disk only)")
	fs.StringVar(&c.ColdVolume, "cold-volume", "", "The storage policy's slow volume, where old data moves (needs --storage-policy and --cold-after)")
	fs.DurationVar(&c.ColdAfter, "cold-after", 0, "Move data older than this to the cold volume (0 = never); must be shorter than every ttl")
	fs.DurationVar(&c.TTLMetrics, "ttl-metrics", 14*24*time.Hour, "Metric retention")
	fs.DurationVar(&c.TTLTraces, "ttl-traces", 7*24*time.Hour, "Trace retention")
	fs.DurationVar(&c.TTLLogs, "ttl-logs", 7*24*time.Hour, "Log retention")
	fs.DurationVar(&c.TTLProfiles, "ttl-profiles", 7*24*time.Hour, "Profile retention")
	fs.DurationVar(&c.TTLDatabases, "ttl-databases", 7*24*time.Hour, "Retention of what database collectors report")
	fs.IntVar(&c.BatchRows, "batch-rows", 10000, "Rows per ClickHouse insert")
	fs.IntVar(&c.MaxBuffered, "max-buffered-rows", 200000, "Rows buffered per signal before ingest answers 503")
	fs.DurationVar(&c.FlushInterval, "flush-interval", 5*time.Second, "Longest a row waits before it is inserted")
	fs.Int64Var(&c.MaxBodyBytes, "max-body-bytes", 32<<20, "Largest request body, compressed and decompressed")
	fs.DurationVar(&c.DetectInterval, "detect-interval", 0, "How often to look for incidents (0 = detection off). Run ONE replica when it is on: see incident.Manager")
	fs.DurationVar(&c.DetectLookback, "detect-lookback", 3*time.Hour, "How far back each look reaches (at least 1h05m)")
	fs.DurationVar(&c.DetectMaxAge, "detect-max-age", 30*time.Minute, "A breach that already ended is opened only if it ended this recently")
	fs.Float64Var(&c.DetectAvailability, "detect-availability", 0.999, "Share of requests that should not fail")
	fs.Float64Var(&c.DetectLatency, "detect-latency", 0.999, "Share of requests that should be faster than detect-latency-seconds")
	fs.Float64Var(&c.DetectLatencySeconds, "detect-latency-seconds", 0.5, "A request slower than this counts against the latency objective")
	fs.Float64Var(&c.DetectBurn, "detect-burn", 14.4, "Burn rate at which an incident is declared (14.4 = a 30-day budget gone in two days)")
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
	case c.TTLMetrics < time.Hour, c.TTLTraces < time.Hour, c.TTLLogs < time.Hour, c.TTLProfiles < time.Hour, c.TTLDatabases < time.Hour:
		return Config{}, fmt.Errorf("every ttl must be at least 1h")
	case c.StoragePolicy != "" && !identifier(c.StoragePolicy):
		return Config{}, fmt.Errorf("storage-policy %q must be letters, digits and underscores", c.StoragePolicy)
	case c.ColdVolume != "" && !identifier(c.ColdVolume):
		return Config{}, fmt.Errorf("cold-volume %q must be letters, digits and underscores", c.ColdVolume)
	case c.ColdAfter < 0:
		return Config{}, fmt.Errorf("cold-after must not be negative")
	case c.ColdAfter > 0 && (c.StoragePolicy == "" || c.ColdVolume == ""):
		return Config{}, fmt.Errorf("cold-after needs storage-policy and cold-volume")
	case c.ColdAfter > 0 && c.ColdAfter < time.Hour:
		return Config{}, fmt.Errorf("cold-after must be at least 1h")
	case c.ColdAfter > 0 && c.ColdAfter >= min(c.TTLMetrics, c.TTLTraces, c.TTLLogs, c.TTLProfiles):
		return Config{}, fmt.Errorf("cold-after must be shorter than every ttl, or data would be deleted before it moved")
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
