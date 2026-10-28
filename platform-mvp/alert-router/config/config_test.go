package config

import (
	"testing"
	"time"
)

func TestParse_Defaults(t *testing.T) {
	c, err := Parse([]string{"--token-file=/t"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Config{Listen: ":8080", TokenFile: "/t", DedupeTTL: 10 * time.Minute, SinkTimeout: 5 * time.Second}
	if c != want {
		t.Errorf("defaults = %+v, want %+v", c, want)
	}
}

func TestParse_Overrides(t *testing.T) {
	c, err := Parse([]string{"--token-file=/t", "--listen=:9090", "--dedupe-interval=0", "--sink-timeout=1s"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Listen != ":9090" || c.DedupeTTL != 0 || c.SinkTimeout != time.Second {
		t.Errorf("overrides = %+v", c)
	}
}

func TestParse_RejectsInvalid(t *testing.T) {
	tests := map[string][]string{
		"no token file":     {},
		"negative dedupe":   {"--token-file=/t", "--dedupe-interval=-1m"},
		"zero sink timeout": {"--token-file=/t", "--sink-timeout=0s"},
		"unknown flag":      {"--token-file=/t", "--nope"},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(args); err == nil {
				t.Error("expected an error")
			}
		})
	}
}
