package config

import (
	"reflect"
	"testing"
	"time"
)

func TestParse_Defaults(t *testing.T) {
	c, err := Parse(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Config{
		HubKubeconfig:    "",
		ClusterSelector:  map[string]string{"platform.example.com/offers-vms": "true"},
		RunStrategy:      "Always",
		RequeueInterval:  30 * time.Second,
		PollInterval:     10 * time.Second,
		DefaultNamespace: "default",
		ProfileNamespace: "default",
	}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("defaults =\n%+v\nwant\n%+v", c, want)
	}
}

func TestParse_Overrides(t *testing.T) {
	c, err := Parse([]string{
		"--hub-kubeconfig=/k", "--cluster-selector=a=b,c=d", "--run-strategy=Halted",
		"--requeue-interval=5s", "--poll-interval=2s", "--default-namespace=tenants",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.HubKubeconfig != "/k" || c.RunStrategy != "Halted" || c.DefaultNamespace != "tenants" ||
		c.RequeueInterval != 5*time.Second || c.PollInterval != 2*time.Second ||
		!reflect.DeepEqual(c.ClusterSelector, map[string]string{"a": "b", "c": "d"}) {
		t.Errorf("overrides not applied: %+v", c)
	}
}

func TestParse_EmptySelectorMeansAllClusters(t *testing.T) {
	c, err := Parse([]string{"--cluster-selector="})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c.ClusterSelector) != 0 {
		t.Errorf("expected empty selector, got %v", c.ClusterSelector)
	}
}

func TestParse_Rejects(t *testing.T) {
	tests := map[string][]string{
		"selector without value":  {"--cluster-selector=novalue"},
		"selector empty key":      {"--cluster-selector==x"},
		"unknown run strategy":    {"--run-strategy=Sometimes"},
		"zero requeue interval":   {"--requeue-interval=0s"},
		"negative poll interval":  {"--poll-interval=-1s"},
		"empty default namespace": {"--default-namespace="},
		"empty profile namespace": {"--cluster-profile-namespace="},
		"unknown flag":            {"--bogus=1"},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(args); err == nil {
				t.Errorf("expected an error for %v", args)
			}
		})
	}
}
