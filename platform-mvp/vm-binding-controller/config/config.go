// Package config turns command-line flags into a validated Config. Helm
// values are the source of truth; the flag defaults only keep a bare
// `vm-binding-controller` runnable.
package config

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

type Config struct {
	HubKubeconfig    string
	ClusterSelector  map[string]string // ClusterProfile labels a spoke must carry to be engaged
	RunStrategy      string            // KubeVirt runStrategy for created VMs
	RequeueInterval  time.Duration     // drift-correction and Unschedulable retry period
	PollInterval     time.Duration     // how often the provider re-lists ClusterProfiles
	DefaultNamespace string            // spoke namespace when a request has no tenant label
	ProfileNamespace string            // hub namespace holding the ClusterProfiles
}

var runStrategies = map[string]bool{"Always": true, "Halted": true, "Manual": true, "RerunOnFailure": true, "Once": true}

func Parse(args []string) (Config, error) {
	fs := flag.NewFlagSet("vm-binding-controller", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var c Config
	var selector string
	fs.StringVar(&c.HubKubeconfig, "hub-kubeconfig", "", "Path to hub kubeconfig (empty = in-cluster)")
	fs.StringVar(&selector, "cluster-selector", "platform.example.com/offers-vms=true", "Comma-separated key=value ClusterProfile labels; empty engages every cluster")
	fs.StringVar(&c.RunStrategy, "run-strategy", "Always", "KubeVirt runStrategy for created VMs")
	fs.DurationVar(&c.RequeueInterval, "requeue-interval", 30*time.Second, "Requeue period for drift correction and unschedulable requests")
	fs.DurationVar(&c.PollInterval, "poll-interval", 10*time.Second, "ClusterProfile discovery period")
	fs.StringVar(&c.DefaultNamespace, "default-namespace", "default", "Spoke namespace for requests without a tenant label")
	fs.StringVar(&c.ProfileNamespace, "cluster-profile-namespace", "default", "Hub namespace holding the ClusterProfiles, for the status cluster block")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	var err error
	if c.ClusterSelector, err = parseSelector(selector); err != nil {
		return Config{}, err
	}
	switch {
	case !runStrategies[c.RunStrategy]:
		return Config{}, fmt.Errorf("unknown run strategy %q", c.RunStrategy)
	case c.RequeueInterval <= 0:
		return Config{}, fmt.Errorf("requeue-interval must be positive")
	case c.PollInterval <= 0:
		return Config{}, fmt.Errorf("poll-interval must be positive")
	case c.DefaultNamespace == "":
		return Config{}, fmt.Errorf("default-namespace must not be empty")
	case c.ProfileNamespace == "":
		return Config{}, fmt.Errorf("cluster-profile-namespace must not be empty")
	}
	return c, nil
}

func parseSelector(s string) (map[string]string, error) {
	selector := map[string]string{}
	if s == "" {
		return selector, nil
	}
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid selector entry %q, want key=value", pair)
		}
		selector[k] = v
	}
	return selector, nil
}
