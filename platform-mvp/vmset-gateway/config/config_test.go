package config

import (
	"reflect"
	"testing"
)

func TestParse_Defaults(t *testing.T) {
	c, err := Parse(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Listen != ":8080" || c.Resource != "vmsets" || c.DefaultSize != "S" || c.MaxReplicas != 3 {
		t.Errorf("defaults = %+v", c)
	}
	if !reflect.DeepEqual(c.Sizes, []string{"S", "M"}) {
		t.Errorf("sizes = %v", c.Sizes)
	}
	if !c.SSHKeyFormat.MatchString("ssh-ed25519 AAAAC3Nza me@host") || c.SSHKeyFormat.MatchString("ssh-ed25519 A'; id") {
		t.Error("default key pattern should accept a normal key and reject shell metacharacters")
	}
}

func TestParse_Overrides(t *testing.T) {
	c, err := Parse([]string{"--listen=:9090", "--sizes=S, M ,L", "--default-size=M", "--max-replicas=5", "--vmset-resource=vmsets2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Listen != ":9090" || c.DefaultSize != "M" || c.MaxReplicas != 5 || c.Resource != "vmsets2" {
		t.Errorf("overrides = %+v", c)
	}
	if !reflect.DeepEqual(c.Sizes, []string{"S", "M", "L"}) {
		t.Errorf("sizes = %v, want whitespace trimmed", c.Sizes)
	}
}

func TestParse_RejectsInvalid(t *testing.T) {
	tests := map[string][]string{
		"default size not offered": {"--default-size=XL"},
		"no sizes":                 {"--sizes="},
		"zero replicas":            {"--max-replicas=0"},
		"bad pattern":              {"--ssh-key-pattern=("},
		"empty resource":           {"--vmset-resource="},
		"zero heartbeat":           {"--heartbeat=0s"},
		"unknown flag":             {"--nope"},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(args); err == nil {
				t.Error("expected an error")
			}
		})
	}
}
