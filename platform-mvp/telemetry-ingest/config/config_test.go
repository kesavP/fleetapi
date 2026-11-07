package config

import (
	"os"
	"path/filepath"
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
