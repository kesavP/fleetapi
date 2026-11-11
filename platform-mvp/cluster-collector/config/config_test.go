package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const key = "gke-ingest-key-supersecret-0001"

func env(vars map[string]string, files map[string]string) Environment {
	return Environment{
		Getenv: func(k string) string { return vars[k] },
		ReadFile: func(p string) ([]byte, error) {
			if v, ok := files[p]; ok {
				return []byte(v), nil
			}
			return nil, &errWrap{errors.New("no such file")}
		},
	}
}

type errWrap struct{ error }

func (e *errWrap) Unwrap() error { return e.error }

func TestAValidConfiguration(t *testing.T) {
	c, err := Parse([]string{"--endpoint", "http://hub:30081/", "--cluster", "gke", "--exclude-namespaces", "kube-system, istio-system,,"}, env(map[string]string{"API_KEY": " " + key + "\n"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.IngestURL() != "http://hub:30081/v1/inventory" || c.Key != key || c.Cluster != "gke" || c.Interval != 30*time.Second || c.Resync != 30*time.Minute || c.Events != "warning" || c.Listen != ":8082" {
		t.Fatalf("%+v", c)
	}
	if len(c.Exclude) != 2 || c.Exclude[0] != "kube-system" || c.Exclude[1] != "istio-system" {
		t.Fatalf("%v", c.Exclude)
	}
}

func TestTheKeyComesFromAFileOrTheEnvironmentAndTheFileWins(t *testing.T) {
	args := []string{"--endpoint", "https://hub.example.com", "--cluster", "gke", "--api-key-file", "/etc/key/apiKey"}
	c, err := Parse(args, env(map[string]string{"API_KEY": "from-the-environment-0000"}, map[string]string{"/etc/key/apiKey": key + "\n"}))
	if err != nil || c.Key != key {
		t.Fatalf("%v %q", err, c.Key)
	}
	if _, err := Parse(args, env(nil, nil)); err == nil || !strings.Contains(err.Error(), "key file") {
		t.Fatalf("a missing file: %v", err)
	}
	if _, err := Parse(args, env(nil, map[string]string{"/etc/key/apiKey": "  \n"})); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("an empty file: %v", err)
	}
	if _, err := Parse([]string{"--endpoint", "http://hub", "--cluster", "gke"}, env(nil, nil)); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("no key anywhere: %v", err)
	}
}

func TestTheKeyIsNotTakenOnTheCommandLine(t *testing.T) {
	for _, flag := range []string{"--api-key", "--key"} {
		_, err := Parse([]string{"--endpoint", "http://hub", "--cluster", "gke", flag, key}, env(nil, nil))
		if err == nil || !strings.Contains(err.Error(), "not taken on the command line") || strings.Contains(err.Error(), key) {
			t.Errorf("%s: %v", flag, err)
		}
	}
}

func TestRefusals(t *testing.T) {
	ok := []string{"--endpoint", "http://hub:30081", "--cluster", "gke"}
	with := func(extra ...string) []string { return append(append([]string{}, ok...), extra...) }
	e := env(map[string]string{"API_KEY": key}, nil)
	for name, args := range map[string][]string{
		"no cluster":                   {"--endpoint", "http://hub"},
		"a cluster in capitals":        {"--endpoint", "http://hub", "--cluster", "GKE"},
		"no endpoint":                  {"--cluster", "gke"},
		"an endpoint that is not http": with("--endpoint", "ftp://hub"),
		"an endpoint with a user":      {"--endpoint", "http://admin:" + key + "@hub", "--cluster", "gke"},
		"an endpoint with a query":     {"--endpoint", "http://hub?x=1", "--cluster", "gke"},
		"a short interval":             with("--interval", "1s"),
		"a resync under a minute":      with("--resync", "30s"),
		"a resync under the interval":  with("--interval", "2m", "--resync", "1m30s"),
		"unknown events":               with("--events", "some"),
		"an argument left over":        with("stray"),
		"an unknown flag":              with("--nope"),
	} {
		_, err := Parse(args, e)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), key) {
			t.Errorf("%s: the key is in the error: %v", name, err)
		}
	}
}

func TestStdoutAndCheckNeedNoEndpointOrKey(t *testing.T) {
	for _, flag := range []string{"--stdout", "--check"} {
		c, err := Parse([]string{flag, "--cluster", "gke"}, env(nil, nil))
		if err != nil || (!c.Stdout && !c.Check) {
			t.Errorf("%s: %v", flag, err)
		}
	}
	// --check may be run without a cluster name: it only looks
	if _, err := Parse([]string{"--check"}, env(nil, nil)); err != nil {
		t.Errorf("--check with no cluster: %v", err)
	}
}

func TestEventsModes(t *testing.T) {
	for _, m := range []string{"warning", "all", "none"} {
		if _, err := Parse([]string{"--endpoint", "http://hub", "--cluster", "gke", "--events", m}, env(map[string]string{"API_KEY": key}, nil)); err != nil {
			t.Errorf("%s: %v", m, err)
		}
	}
}
