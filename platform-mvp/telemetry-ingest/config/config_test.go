package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	c, err := Parse([]string{"--projects-file=/etc/p.json"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.GRPCListen != ":4317" || c.ClickHouseDatabase != "telemetry" || c.TTLMetrics != 14*24*time.Hour {
		t.Fatalf("%+v", c)
	}
}

func TestClusterInfoIsOffUnlessAskedFor(t *testing.T) {
	c, err := Parse([]string{"--projects-file=x"})
	if err != nil || c.ExposeClusterInfo {
		t.Fatalf("default must not share the platform's sizes and disks: %+v %v", c, err)
	}
	c, err = Parse([]string{"--projects-file=x", "--expose-cluster-info"})
	if err != nil || !c.ExposeClusterInfo {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestPasswordFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pw")
	_ = os.WriteFile(p, []byte("s3cret\n"), 0o600)
	c, err := Parse([]string{"--projects-file=x", "--clickhouse-password-file=" + p, "--clickhouse-addr=a:9000, b:9000"})
	if err != nil {
		t.Fatal(err)
	}
	if c.ClickHousePassword != "s3cret" || len(c.ClickHouseAddrs) != 2 || c.ClickHouseAddrs[1] != "b:9000" {
		t.Fatalf("%+v", c)
	}
}

func TestRejects(t *testing.T) {
	cases := map[string][]string{
		"no projects file": {},
		"bad database":     {"--projects-file=x", "--clickhouse-database=a;drop"},
		"short ttl":        {"--projects-file=x", "--ttl-logs=1m"},
		"buffer < batch":   {"--projects-file=x", "--batch-rows=100", "--max-buffered-rows=10"},
		"missing pw file":  {"--projects-file=x", "--clickhouse-password-file=/nope"},
		"no addresses":     {"--projects-file=x", "--clickhouse-addr= , "},
		"tiny body limit":  {"--projects-file=x", "--max-body-bytes=10"},
		"unknown flag":     {"--projects-file=x", "--nope"},
	}
	for name, args := range cases {
		if _, err := Parse(args); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Parse([]string{"--projects-file=x", "--clickhouse-database=a;drop"}); err == nil || !strings.Contains(err.Error(), "clickhouse-database") {
		t.Errorf("error should name the flag: %v", err)
	}
}

func TestTieredStorage(t *testing.T) {
	base := []string{"--projects-file=x"}
	c, err := Parse(base)
	if err != nil || c.StoragePolicy != "" || c.ColdVolume != "" || c.ColdAfter != 0 {
		t.Fatalf("tiering must be off by default: %+v %v", c, err)
	}
	c, err = Parse(append(base, "--storage-policy=tiered", "--cold-volume=cold", "--cold-after=48h"))
	if err != nil || c.StoragePolicy != "tiered" || c.ColdVolume != "cold" || c.ColdAfter != 48*time.Hour {
		t.Fatalf("%+v %v", c, err)
	}
	// a policy with no move is allowed: the tables just use it
	if _, err := Parse(append(base, "--storage-policy=tiered")); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"moves need a policy and a volume": {"--cold-after=48h"},
		"moves need a volume":              {"--storage-policy=tiered", "--cold-after=48h"},
		"a move before an hour":            {"--storage-policy=tiered", "--cold-volume=cold", "--cold-after=30m"},
		"a negative move":                  {"--storage-policy=tiered", "--cold-volume=cold", "--cold-after=-1h"},
		"moving as late as the deletion":   {"--storage-policy=tiered", "--cold-volume=cold", "--cold-after=168h", "--ttl-traces=168h"},
		"moving after the deletion":        {"--storage-policy=tiered", "--cold-volume=cold", "--cold-after=200h"},
		"a policy name with a quote":       {"--storage-policy=a'b"},
		"a volume name with a quote":       {"--storage-policy=p", "--cold-volume=a'b", "--cold-after=2h"},
	} {
		if _, err := Parse(append(append([]string{}, base...), args...)); err == nil {
			t.Errorf("%s: accepted %v", name, args)
		}
	}
}

func TestIncidentDetectionIsOffUnlessAskedFor(t *testing.T) {
	c, err := Parse([]string{"--projects-file", "p.json"})
	if err != nil {
		t.Fatal(err)
	}
	if c.DetectInterval != 0 {
		t.Fatalf("detection is on by default: %v", c.DetectInterval)
	}
	if c.DetectAvailability != 0.999 || c.DetectLatency != 0.999 || c.DetectLatencySeconds != 0.5 || c.DetectBurn != 14.4 {
		t.Fatalf("defaults = %+v", c)
	}
	c, err = Parse([]string{"--projects-file", "p.json", "--detect-interval", "1m", "--detect-burn", "6", "--detect-availability", "0.99"})
	if err != nil {
		t.Fatal(err)
	}
	if c.DetectInterval.Minutes() != 1 || c.DetectBurn != 6 || c.DetectAvailability != 0.99 {
		t.Fatalf("config = %+v", c)
	}
}

func TestSilenceDetectionSettings(t *testing.T) {
	c, err := Parse([]string{"--projects-file", "p.json"})
	if err != nil {
		t.Fatal(err)
	}
	if c.DetectSilenceAfter != 5*time.Minute || c.DetectSilenceExclude != DefaultSilenceExclude {
		t.Fatalf("defaults = %v %q", c.DetectSilenceAfter, c.DetectSilenceExclude)
	}
	c, err = Parse([]string{"--projects-file", "p.json", "--detect-silence-after", "0", "--detect-silence-exclude", ""})
	if err != nil || c.DetectSilenceAfter != 0 || c.DetectSilenceExclude != "" {
		t.Fatalf("off and no exclusion: %v %+v", err, c)
	}
	for name, args := range map[string][]string{
		"a delay under a minute": {"--detect-silence-after", "30s"},
		"a negative delay":       {"--detect-silence-after", "-5m"},
		"a bad expression":       {"--detect-silence-exclude", "(unclosed"},
	} {
		if _, err := Parse(append([]string{"--projects-file", "p.json"}, args...)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if !regexp.MustCompile(DefaultSilenceExclude).MatchString("kube-system") || regexp.MustCompile(DefaultSilenceExclude).MatchString("shop") || regexp.MustCompile(DefaultSilenceExclude).MatchString("monitoring") {
		t.Fatal("the default leaves out the platform's namespaces, not the applications' or the monitoring ones")
	}
}
