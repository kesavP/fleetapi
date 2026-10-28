// Package config turns command-line flags into a validated Config. Helm
// values are the source of truth; the flag defaults only keep a bare
// `vmset-gateway` runnable.
package config

import (
	"flag"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

type Config struct {
	Listen        string
	HubKubeconfig string // empty = in-cluster
	Resource      string // plural of the VmSet resource
	Sizes         []string
	DefaultSize   string
	MaxReplicas   int
	SSHKeyFormat  *regexp.Regexp
	Heartbeat     time.Duration
}

// The key format matches vmSet.sshPublicKeyPattern in the hub chart: key types
// and characters that cannot break out of the generated cloud-init line.
const defaultSSHKeyFormat = `^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp256) [A-Za-z0-9+/=]+( [A-Za-z0-9@._-]+)?$`

func Parse(args []string) (Config, error) {
	fs := flag.NewFlagSet("vmset-gateway", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var c Config
	var sizes, keyFormat string
	fs.StringVar(&c.Listen, "listen", ":8080", "Address to serve the API on")
	fs.StringVar(&c.HubKubeconfig, "hub-kubeconfig", "", "Path to hub kubeconfig, used for the API server address and CA only (empty = in-cluster)")
	fs.StringVar(&c.Resource, "vmset-resource", "vmsets", "Plural resource name of the Kro-generated VmSet API")
	fs.StringVar(&sizes, "sizes", "S,M", "Comma-separated VM tiers accepted on create (match vmSet.tiers)")
	fs.StringVar(&c.DefaultSize, "default-size", "S", "Tier used when a request omits size")
	fs.IntVar(&c.MaxReplicas, "max-replicas", 3, "Largest replica count accepted on create (match vmSet.maxReplicas)")
	fs.StringVar(&keyFormat, "ssh-key-pattern", defaultSSHKeyFormat, "Regular expression an sshPublicKey must match")
	fs.DurationVar(&c.Heartbeat, "heartbeat", 15*time.Second, "Keep-alive period on event streams")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	for _, s := range strings.Split(sizes, ",") {
		if s = strings.TrimSpace(s); s != "" {
			c.Sizes = append(c.Sizes, s)
		}
	}
	var err error
	if c.SSHKeyFormat, err = regexp.Compile(keyFormat); err != nil {
		return Config{}, fmt.Errorf("invalid ssh-key-pattern: %w", err)
	}
	switch {
	case c.Resource == "":
		return Config{}, fmt.Errorf("vmset-resource must not be empty")
	case len(c.Sizes) == 0:
		return Config{}, fmt.Errorf("sizes must list at least one tier")
	case !contains(c.Sizes, c.DefaultSize):
		return Config{}, fmt.Errorf("default-size %q is not one of the sizes %v", c.DefaultSize, c.Sizes)
	case c.MaxReplicas < 1:
		return Config{}, fmt.Errorf("max-replicas must be at least 1")
	case c.Heartbeat <= 0:
		return Config{}, fmt.Errorf("heartbeat must be positive")
	}
	return c, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
