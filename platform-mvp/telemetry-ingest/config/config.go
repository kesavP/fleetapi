// Package config turns command-line flags into a validated Config. Helm
// values are the source of truth; the flag defaults only keep a bare
// `telemetry-ingest` runnable.
package config

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/costs"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/rules"
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
	// TTLInventory is how long a cluster's inventory is kept: the versions of its objects, its events and its reports. An object that has not
	// been reported (changed or resynced) for this long is gone from the history too.
	TTLInventory time.Duration

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
	// DetectSilenceAfter: an application that has stopped sending data (needs the cluster collector's inventory) is opened as an incident once it
	// has been silent this long. Zero switches it off. DetectSilenceExclude is a regular expression for the namespaces to leave out ("" = none).
	DetectSilenceAfter   time.Duration
	DetectSilenceExclude string
	// DetectDeps: also open an incident for an application that others cannot connect to: DetectDepsShare of the connection attempts into it
	// fail, and at least DetectDepsMinFailed of them, in five minutes. Needs DetectInterval. DetectDepsExclude is the namespaces to leave out.
	DetectDeps          bool
	DetectDepsShare     float64
	DetectDepsMinFailed float64
	DetectDepsExclude   string

	// Analysis: with a key, an incident can be sent (its title, summary and where its evidence is: no logs or traces) to Claude for resolution steps,
	// which are kept on it as a comment. AnalysisKey is read from the file AnalysisKeyFile names, so the key is never in an argument or an
	// environment; empty means the feature is off.
	AnalysisKey   string
	AnalysisModel string

	// Right-sizing: also open an information incident for a container that uses far less than it was given (see the detect package). Which resources, how
	// low, over how many days and by which percentile is a RULE (see the rules package): RulesDefaults are the settings of a rule nobody has saved one
	// for, and what a project saves overrides them. DetectRightsizeEvery is how often it looks (it reads days of metrics); DetectRightsizeExclude is
	// the namespaces to leave out.
	// Notifications: with an Alertmanager address, an incident the detector opens at NotifyMinSeverity or above is posted to it (see the notify package).
	// NotifyUIURL, when set, makes each message link to the incident's page in the UI.
	NotifyAlertmanagerURL string
	NotifyMinSeverity     string
	NotifyUIURL           string

	// Costs: a FEATURE, off unless asked for. When on, the hub serves an infrastructure cost report (see the costs package) and says so in /features, so the UI
	// shows its Costs page. Nodes are priced from CostPrices (an operator's prices, by instance type) and, for Azure and when CostsAzurePrices is on, from
	// the public Azure price list. CostsVCPUGBRatio is how many GiB of memory cost as much as one core, which splits a node's price between them.
	Costs            bool
	CostsAzurePrices bool
	CostsVCPUGBRatio float64
	CostPrices       map[string]costs.Override

	DetectRightsize        bool
	DetectRightsizeEvery   time.Duration
	DetectRightsizeExclude string
	RulesDefaults          map[string]rules.Settings

	BatchRows     int
	MaxBuffered   int
	FlushInterval time.Duration
	MaxBodyBytes  int64
	ReloadEvery   time.Duration
}

// DefaultSilenceExclude is the platform's own namespaces: the ones the Applications page hides until asked for.
const DefaultSilenceExclude = `^(kube-|gke-|gmp-|gatekeeper|config-management|istio-system$|asm-|anthos|cert-manager$|ingress-nginx$|linkerd|calico|tigera|cilium|flux-system$|argocd$|olm$)`

func Parse(args []string) (Config, error) {
	fs := flag.NewFlagSet("telemetry-ingest", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var c Config
	var addrs, passwordFile, analysisKeyFile, rulesDefaults, costPrices string
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
	fs.DurationVar(&c.TTLInventory, "ttl-inventory", 14*24*time.Hour, "Retention of what cluster collectors report (inventory versions, events, reports)")
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
	fs.DurationVar(&c.DetectSilenceAfter, "detect-silence-after", 5*time.Minute, "Open an incident for an application that has stopped sending data once it has been silent this long (0 = off; needs the cluster collector)")
	fs.StringVar(&c.DetectSilenceExclude, "detect-silence-exclude", DefaultSilenceExclude, "Namespaces (a regular expression) whose applications are not judged for silence; empty = none")
	fs.BoolVar(&c.DetectDeps, "detect-deps", false, "Also open an incident for an application that others cannot connect to (the callers' failed connections, from the node-agent)")
	fs.Float64Var(&c.DetectDepsShare, "detect-deps-share", 0.2, "The share of connection attempts into an application that must fail for it to count (0 to 1)")
	fs.Float64Var(&c.DetectDepsMinFailed, "detect-deps-min-failed", 5, "Failed connections into an application, in five minutes, before their share means anything")
	fs.StringVar(&c.DetectDepsExclude, "detect-deps-exclude", DefaultSilenceExclude, "Namespaces (a regular expression) whose applications are not judged for failing connections; empty = none")
	fs.StringVar(&c.NotifyAlertmanagerURL, "notify-alertmanager-url", "", "Alertmanager's address (such as http://alertmanager:9093): an incident the detector opens is posted to it, once, when it opens (empty = nobody is told)")
	fs.StringVar(&c.NotifyMinSeverity, "notify-min-severity", "warning", "The lowest severity that is told: critical, warning or info")
	fs.StringVar(&c.NotifyUIURL, "notify-ui-url", "", "The UI's address (such as https://telemetry.example), so each message links to the incident's page; empty = no link")
	fs.BoolVar(&c.Costs, "costs", false, "Serve the infrastructure cost report, and tell the UI to show its Costs page")
	fs.BoolVar(&c.CostsAzurePrices, "costs-azure-prices", true, "Price Azure nodes from Azure's public price list (the hub then needs to reach prices.azure.com); an override always wins")
	fs.Float64Var(&c.CostsVCPUGBRatio, "costs-vcpu-gb-ratio", 7.5, "How many GiB of memory cost as much as one core: splits a node's price between its CPU and its memory")
	fs.StringVar(&costPrices, "costs-prices", "", `JSON object of instance type to its price per hour, such as {"Standard_D4s_v6":{"hourly":0.25,"spotHourly":0.07}}; wins over the price list`)
	fs.BoolVar(&c.DetectRightsize, "detect-rightsize", false, "Also open an incident for a container that uses far less of its memory, CPU or volume than it was given (the rules decide how low)")
	fs.DurationVar(&c.DetectRightsizeEvery, "detect-rightsize-every", time.Hour, "How often to look for over-provisioned containers (at least 10m): each look reads days of metrics")
	fs.StringVar(&c.DetectRightsizeExclude, "detect-rightsize-exclude", DefaultSilenceExclude, "Namespaces (a regular expression) whose containers are not judged for right-sizing; empty = none")
	fs.StringVar(&rulesDefaults, "rules-defaults", "", `JSON object of rule name to the settings that differ from the built-in defaults, such as {"rightsize.memory":{"percent":40}}; a project's saved rules override them`)
	fs.StringVar(&analysisKeyFile, "analysis-key-file", "", "File holding an Anthropic API key: switches on 'analyse with Claude' for incidents (empty = off)")
	fs.StringVar(&c.AnalysisModel, "analysis-model", "claude-opus-5-5", "The Claude model that writes the analysis")
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

	if analysisKeyFile != "" {
		b, err := os.ReadFile(analysisKeyFile)
		if err != nil {
			return Config{}, fmt.Errorf("read analysis-key-file: %w", err)
		}
		if c.AnalysisKey = strings.TrimSpace(string(b)); c.AnalysisKey == "" {
			return Config{}, fmt.Errorf("analysis-key-file is empty")
		}
		if !modelName.MatchString(c.AnalysisModel) {
			return Config{}, fmt.Errorf("analysis-model %q must be a model id such as claude-opus-5-5", c.AnalysisModel)
		}
	}

	var err error
	if c.RulesDefaults, err = rules.ParseDefaults(rulesDefaults); err != nil {
		return Config{}, err
	}
	if c.Costs {
		if c.CostPrices, err = costs.ParseOverrides(costPrices); err != nil {
			return Config{}, err
		}
		if !(c.CostsVCPUGBRatio > 0 && c.CostsVCPUGBRatio <= 100) {
			return Config{}, fmt.Errorf("costs-vcpu-gb-ratio must be above 0 and at most 100")
		}
	}

	switch {
	case c.ProjectsFile == "":
		return Config{}, fmt.Errorf("projects-file is required")
	case len(c.ClickHouseAddrs) == 0:
		return Config{}, fmt.Errorf("clickhouse-addr must list at least one address")
	case !identifier(c.ClickHouseDatabase):
		return Config{}, fmt.Errorf("clickhouse-database %q must be letters, digits and underscores", c.ClickHouseDatabase)
	case c.TTLMetrics < time.Hour, c.TTLTraces < time.Hour, c.TTLLogs < time.Hour, c.TTLProfiles < time.Hour, c.TTLDatabases < time.Hour, c.TTLInventory < time.Hour:
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
	case c.DetectSilenceAfter < 0 || (c.DetectSilenceAfter > 0 && c.DetectSilenceAfter < time.Minute):
		return Config{}, fmt.Errorf("detect-silence-after must be 0 (off) or at least 1m")
	case c.DetectSilenceExclude != "" && !validRegexp(c.DetectSilenceExclude):
		return Config{}, fmt.Errorf("detect-silence-exclude is not a valid regular expression")
	case c.DetectDeps && !(c.DetectDepsShare > 0 && c.DetectDepsShare <= 1):
		return Config{}, fmt.Errorf("detect-deps-share must be above 0 and at most 1")
	case c.DetectDeps && !(c.DetectDepsMinFailed > 0):
		return Config{}, fmt.Errorf("detect-deps-min-failed must be positive")
	case c.DetectDeps && c.DetectDepsExclude != "" && !validRegexp(c.DetectDepsExclude):
		return Config{}, fmt.Errorf("detect-deps-exclude is not a valid regular expression")
	case c.NotifyAlertmanagerURL != "" && !webAddress(c.NotifyAlertmanagerURL):
		return Config{}, fmt.Errorf("notify-alertmanager-url must be an http or https address, such as http://alertmanager:9093")
	case c.NotifyAlertmanagerURL != "" && c.NotifyMinSeverity != "critical" && c.NotifyMinSeverity != "warning" && c.NotifyMinSeverity != "info":
		return Config{}, fmt.Errorf("notify-min-severity must be critical, warning or info")
	case c.NotifyAlertmanagerURL != "" && c.NotifyUIURL != "" && !webAddress(c.NotifyUIURL):
		return Config{}, fmt.Errorf("notify-ui-url must be an http or https address, such as https://telemetry.example")
	case c.DetectRightsize && c.DetectRightsizeEvery < 10*time.Minute:
		return Config{}, fmt.Errorf("detect-rightsize-every must be at least 10m: each look reads days of metrics")
	case c.DetectRightsize && c.DetectRightsizeExclude != "" && !validRegexp(c.DetectRightsizeExclude):
		return Config{}, fmt.Errorf("detect-rightsize-exclude is not a valid regular expression")
	case c.FlushInterval <= 0, c.ReloadEvery <= 0:
		return Config{}, fmt.Errorf("flush-interval and reload-interval must be positive")
	case c.MaxBodyBytes < 1<<10:
		return Config{}, fmt.Errorf("max-body-bytes must be at least 1024")
	}
	return c, nil
}

// identifier guards the one value that is spliced into SQL text.
// webAddress is an http or https address with a host.
func webAddress(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

var modelName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,63}$`)

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

func validRegexp(s string) bool {
	_, err := regexp.Compile(s)
	return err == nil
}
