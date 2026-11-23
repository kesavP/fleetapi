// Package config reads the collector's flags and environment.
//
// There is no flag for the ingest key, on purpose: a key given on the command line is visible to every process on the machine and in the
// pod's spec. It is read from a file (--api-key-file, for a mounted Secret) or from API_KEY.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

type Config struct {
	Endpoint      string // the hub's telemetry-ingest, without a path
	Key           string
	Cluster       string
	Interval      time.Duration
	Resync        time.Duration
	Events        string
	Exclude       []string
	Kubeconfig    string
	Listen        string
	CAFile        string
	TLSSkipVerify bool
	Stdout        bool // print the batches instead of sending them
	Check         bool // look once, say what was found, and stop
}

// IngestURL is where batches are posted.
func (c Config) IngestURL() string { return c.Endpoint + "/v1/inventory" }

// Environment reads the environment and files; a test gives it fakes.
type Environment struct {
	Getenv   func(string) string
	ReadFile func(string) ([]byte, error)
}

// OSEnvironment is the process's own.
func OSEnvironment() Environment { return Environment{Getenv: os.Getenv, ReadFile: os.ReadFile} }

var clusterRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$`)

// Parse reads the flags and, for the key, the file or variable they point to.
func Parse(args []string, env Environment) (Config, error) {
	fs := flag.NewFlagSet("cluster-collector", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var c Config
	var keyFile, exclude string
	fs.StringVar(&c.Endpoint, "endpoint", "", "The hub's telemetry-ingest address, such as http://hub:30081 (the path is added)")
	fs.StringVar(&keyFile, "api-key-file", "", "A file holding this cluster's ingest key (or set API_KEY)")
	fs.StringVar(&c.Cluster, "cluster", "", "This cluster's name, as it is listed in the hub")
	fs.DurationVar(&c.Interval, "interval", 30*time.Second, "How often to look at the cluster")
	fs.DurationVar(&c.Resync, "resync", 30*time.Minute, "How often to send everything again, which also repairs a report that was lost")
	fs.StringVar(&c.Events, "events", "warning", "Which events to report: warning, all or none")
	fs.StringVar(&exclude, "exclude-namespaces", "", "Comma-separated namespaces to leave out")
	fs.StringVar(&c.Kubeconfig, "kubeconfig", "", "A kubeconfig, to run outside the cluster (the default is the pod's service account)")
	fs.StringVar(&c.Listen, "listen", ":8082", "Address for /healthz and /readyz (empty = none)")
	fs.StringVar(&c.CAFile, "ca-file", "", "A CA certificate for an https hub with a private CA")
	fs.BoolVar(&c.TLSSkipVerify, "tls-skip-verify", false, "Do not verify the hub's certificate (a lab only)")
	fs.BoolVar(&c.Stdout, "stdout", false, "Print the batches as JSON instead of sending them")
	fs.BoolVar(&c.Check, "check", false, "Look once, say what was found, and stop")
	if err := fs.Parse(args); err != nil {
		if strings.Contains(err.Error(), "flag provided but not defined: -api-key") || strings.Contains(err.Error(), "flag provided but not defined: -key") {
			return Config{}, errors.New("the ingest key is not taken on the command line: put it in a file and give --api-key-file, or set API_KEY")
		}
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	if !clusterRE.MatchString(c.Cluster) && !(c.Check && c.Cluster == "") {
		return Config{}, errors.New("--cluster is required: lower-case letters, digits and . _ -, at most 64 characters")
	}
	if c.Interval < 5*time.Second {
		return Config{}, errors.New("--interval must be at least 5s")
	}
	if c.Resync < time.Minute || c.Resync < c.Interval {
		return Config{}, errors.New("--resync must be at least 1m and no shorter than --interval")
	}
	switch c.Events {
	case "warning", "all", "none":
	default:
		return Config{}, errors.New("--events must be warning, all or none")
	}
	for _, n := range strings.Split(exclude, ",") {
		if n = strings.TrimSpace(n); n != "" {
			c.Exclude = append(c.Exclude, n)
		}
	}

	if c.Stdout || c.Check {
		return c, nil
	}
	u, err := url.Parse(strings.TrimSpace(c.Endpoint))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Config{}, errors.New("--endpoint must be an http or https address, such as http://hub:30081")
	}
	if u.User != nil {
		return Config{}, errors.New("--endpoint must not carry a user name or password: the ingest key is read from --api-key-file or API_KEY")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return Config{}, errors.New("--endpoint must not have a query or a fragment")
	}
	c.Endpoint = strings.TrimRight(u.String(), "/")
	switch {
	case keyFile != "":
		raw, err := env.ReadFile(keyFile)
		if err != nil {
			return Config{}, fmt.Errorf("read the ingest key file: %w", errors.Unwrap(err))
		}
		c.Key = strings.TrimSpace(string(raw))
	default:
		c.Key = strings.TrimSpace(env.Getenv("API_KEY"))
	}
	if c.Key == "" {
		return Config{}, errors.New("the ingest key is missing: give --api-key-file or set API_KEY")
	}
	return c, nil
}
