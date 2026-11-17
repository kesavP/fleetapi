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

func TestDependencyDetectionSettings(t *testing.T) {
	c, err := Parse([]string{"--projects-file", "p.json"})
	if err != nil {
		t.Fatal(err)
	}
	if c.DetectDeps || c.DetectDepsShare != 0.2 || c.DetectDepsMinFailed != 5 || c.DetectDepsExclude != DefaultSilenceExclude {
		t.Fatalf("defaults = %v %v %v %q: it must be off unless asked for", c.DetectDeps, c.DetectDepsShare, c.DetectDepsMinFailed, c.DetectDepsExclude)
	}
	c, err = Parse([]string{"--projects-file", "p.json", "--detect-deps", "--detect-deps-share", "0.5", "--detect-deps-min-failed", "20", "--detect-deps-exclude", ""})
	if err != nil || !c.DetectDeps || c.DetectDepsShare != 0.5 || c.DetectDepsMinFailed != 20 || c.DetectDepsExclude != "" {
		t.Fatalf("set: %v %+v", err, c)
	}
	for name, args := range map[string][]string{
		"a share of zero":       {"--detect-deps", "--detect-deps-share", "0"},
		"a share over one":      {"--detect-deps", "--detect-deps-share", "1.5"},
		"no failed connections": {"--detect-deps", "--detect-deps-min-failed", "0"},
		"a bad expression":      {"--detect-deps", "--detect-deps-exclude", "(unclosed"},
	} {
		if _, err := Parse(append([]string{"--projects-file", "p.json"}, args...)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// settings that are not used are not checked, so an old value left in a chart cannot stop the service
	if _, err := Parse([]string{"--projects-file", "p.json", "--detect-deps-share", "0"}); err != nil {
		t.Errorf("a setting of a switched-off check was refused: %v", err)
	}
}

func TestAnalysisSettings(t *testing.T) {
	c, err := Parse([]string{"--projects-file", "p.json"})
	if err != nil {
		t.Fatal(err)
	}
	if c.AnalysisKey != "" || c.AnalysisModel != "claude-opus-5-5" {
		t.Fatalf("defaults = %q %q: it must be off unless a key is given", c.AnalysisKey, c.AnalysisModel)
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	if err := os.WriteFile(key, []byte("  sk-ant-test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = Parse([]string{"--projects-file", "p.json", "--analysis-key-file", key, "--analysis-model", "claude-sonnet-5-5"})
	if err != nil || c.AnalysisKey != "sk-ant-test-key" || c.AnalysisModel != "claude-sonnet-5-5" {
		t.Fatalf("set: %v %+v", err, c)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"a key file that is not there":  {"--analysis-key-file", filepath.Join(dir, "nope")},
		"a key file with nothing in it": {"--analysis-key-file", empty},
		"a model with a space":          {"--analysis-key-file", key, "--analysis-model", "claude opus"},
		"no model":                      {"--analysis-key-file", key, "--analysis-model", ""},
		"a model that is a path":        {"--analysis-key-file", key, "--analysis-model", "../x"},
	} {
		if _, err := Parse(append([]string{"--projects-file", "p.json"}, args...)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// a model is not looked at while analysis is off, so a value left in a chart cannot stop the service
	if _, err := Parse([]string{"--projects-file", "p.json", "--analysis-model", "bad model"}); err != nil {
		t.Errorf("a setting of a switched-off feature was refused: %v", err)
	}
}

func TestRightsizeAndRulesSettings(t *testing.T) {
	c, err := Parse([]string{"--projects-file", "p.json"})
	if err != nil {
		t.Fatal(err)
	}
	if c.DetectRightsize || c.DetectRightsizeEvery != time.Hour || c.DetectRightsizeExclude != DefaultSilenceExclude {
		t.Fatalf("defaults = %v %v %q: it must be off unless asked for", c.DetectRightsize, c.DetectRightsizeEvery, c.DetectRightsizeExclude)
	}
	if got := c.RulesDefaults["rightsize.memory"]; got.Percent != 50 || got.Days != 7 || !got.Enabled || got.Percentile != 95 {
		t.Fatalf("rule defaults = %+v: the built-in ones when none are given", c.RulesDefaults)
	}
	c, err = Parse([]string{"--projects-file", "p.json", "--detect-rightsize", "--detect-rightsize-every", "30m", "--detect-rightsize-exclude", "",
		"--rules-defaults", `{"rightsize.memory":{"percent":30},"rightsize.storage":{"enabled":true}}`})
	if err != nil || !c.DetectRightsize || c.DetectRightsizeEvery != 30*time.Minute || c.DetectRightsizeExclude != "" {
		t.Fatalf("set: %v %+v", err, c)
	}
	if m := c.RulesDefaults["rightsize.memory"]; m.Percent != 30 || m.Days != 7 {
		t.Fatalf("memory = %+v", m)
	}
	if s := c.RulesDefaults["rightsize.storage"]; !s.Enabled || s.Percentile != 100 {
		t.Fatalf("storage = %+v", s)
	}
	for name, args := range map[string][]string{
		"a look every five minutes":  {"--detect-rightsize", "--detect-rightsize-every", "5m"},
		"a bad expression":           {"--detect-rightsize", "--detect-rightsize-exclude", "(unclosed"},
		"defaults that are not JSON": {"--rules-defaults", "{"},
		"a rule that does not exist": {"--rules-defaults", `{"rightsize.network":{"percent":30}}`},
		"a default out of range":     {"--rules-defaults", `{"rightsize.cpu":{"percent":500}}`},
	} {
		if _, err := Parse(append([]string{"--projects-file", "p.json"}, args...)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// settings of a switched-off check are not checked, so a value left in a chart cannot stop the service
	if _, err := Parse([]string{"--projects-file", "p.json", "--detect-rightsize-every", "1m", "--detect-rightsize-exclude", "(unclosed"}); err != nil {
		t.Errorf("a setting of a switched-off check was refused: %v", err)
	}
}
