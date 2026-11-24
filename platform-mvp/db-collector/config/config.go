// Package config reads the collector's configuration file.
//
// The file lists the databases to collect from and where to send what is collected. It holds NO secrets: a connection string (which carries a
// password) is read from an environment variable or a file the file names, and so is the ingest key, so the configuration can be kept in a
// ConfigMap or a repository without a credential in it. A file that has a "dsn" or a "key" in it is refused, with the way to do it properly,
// rather than accepted and leaked.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/db-collector/collector"
)

type duration struct{ time.Duration }

func (d *duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New(`a duration is a string such as "60s" or "5m"`)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("%q is not a duration such as \"60s\"", s)
	}
	d.Duration = v
	return nil
}

type signal struct {
	Enabled  *bool    `json:"enabled"`
	Interval duration `json:"interval"`
	Top      int      `json:"top"`
}

type slowLog struct {
	Enabled   *bool  `json:"enabled"`
	Path      string `json:"path"`
	FromStart bool   `json:"fromStart"`
}

type target struct {
	Name       string   `json:"name"`
	Engine     string   `json:"engine"`
	DSNEnv     string   `json:"dsnEnv"`
	DSNFile    string   `json:"dsnFile"`
	Statements *signal  `json:"statements"`
	Activity   *signal  `json:"activity"`
	Pools      *signal  `json:"pools"`
	SlowLog    *slowLog `json:"slowLog"`
}

type ingest struct {
	URL       string `json:"url"`
	KeyEnv    string `json:"keyEnv"`
	KeyFile   string `json:"keyFile"`
	QueueSize int    `json:"queueSize"`
}

type file struct {
	Listen  string   `json:"listen"`
	Ingest  ingest   `json:"ingest"`
	Targets []target `json:"targets"`
}

// Config is the configuration with its secrets read: what main needs.
type Config struct {
	Listen    string
	IngestURL string
	IngestKey string
	QueueSize int
	Targets   []collector.TargetConfig
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$`)

// Environment reads variables and files; a test gives it fakes.
type Environment struct {
	Getenv   func(string) string
	ReadFile func(string) ([]byte, error)
}

func osEnv() Environment { return Environment{Getenv: os.Getenv, ReadFile: os.ReadFile} }

// Load reads the file at path, with secrets from the process's environment and files.
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read the configuration: %w", err)
	}
	return Parse(raw, osEnv())
}

func on(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

// Parse validates the file's contents and reads the secrets it points to.
func Parse(raw []byte, env Environment) (Config, error) {
	var f file
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		msg := err.Error()
		if strings.Contains(msg, `unknown field "dsn"`) || strings.Contains(msg, `unknown field "password"`) || strings.Contains(msg, `unknown field "key"`) {
			return Config{}, errors.New(msg + `: secrets are not kept in this file; name an environment variable (dsnEnv, keyEnv) or a file (dsnFile, keyFile) that holds it`)
		}
		return Config{}, fmt.Errorf("the configuration is not valid: %s", msg)
	}
	if dec.More() {
		return Config{}, errors.New("the configuration has something after the JSON object")
	}
	c := Config{Listen: f.Listen, IngestURL: strings.TrimSpace(f.Ingest.URL), QueueSize: f.Ingest.QueueSize}
	if c.Listen == "" {
		c.Listen = ":8081"
	}
	if c.QueueSize < 0 || c.QueueSize > 100000 {
		return Config{}, errors.New("ingest.queueSize must be between 0 and 100000")
	}
	if c.IngestURL != "" {
		u, err := url.Parse(c.IngestURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Config{}, errors.New("ingest.url must be an http or https address")
		}
		if u.User != nil {
			return Config{}, errors.New("ingest.url must not carry a user name or password: the ingest key is read from keyEnv or keyFile")
		}
		key, err := secret(env, "the ingest key", f.Ingest.KeyEnv, f.Ingest.KeyFile)
		if err != nil {
			return Config{}, err
		}
		c.IngestKey = key
	}
	if len(f.Targets) == 0 {
		return Config{}, errors.New("no targets: list at least one database in targets")
	}
	seen := map[string]bool{}
	for i, t := range f.Targets {
		label := fmt.Sprintf("targets[%d]", i)
		if t.Name != "" {
			label = "target " + t.Name
		}
		if !nameRE.MatchString(t.Name) {
			return Config{}, fmt.Errorf("%s: name must be lower-case letters, digits and . _ -, starting and ending with a letter or digit", label)
		}
		if seen[t.Name] {
			return Config{}, fmt.Errorf("%s is listed twice", label)
		}
		seen[t.Name] = true
		if t.Engine == "" {
			return Config{}, fmt.Errorf("%s: engine is required (for example postgres)", label)
		}
		dsn, err := secret(env, label+": the connection string", t.DSNEnv, t.DSNFile)
		if err != nil {
			return Config{}, err
		}
		tc := collector.TargetConfig{Name: t.Name, Engine: t.Engine, DSN: dsn}
		// what an engine can give decides what is on by default, and asking for what it cannot give is a mistake worth saying so. An engine this
		// process does not know (it was not imported) is taken to be a database with statements and sessions, as before there were poolers.
		sup, known := collector.SupportedSignals(t.Engine)
		if !known {
			sup = collector.Signals{Statements: true, Activity: true, SlowLog: true}
		}
		if tc.Statements, err = signalOf(label+" statements", t.Engine, t.Statements, collector.DefaultStatementsEvery, 200, 10*time.Second, sup.Statements, known); err != nil {
			return Config{}, err
		}
		if tc.Activity, err = signalOf(label+" activity", t.Engine, t.Activity, collector.DefaultActivityEvery, 20, time.Second, sup.Activity, known); err != nil {
			return Config{}, err
		}
		if tc.Pools, err = signalOf(label+" pools", t.Engine, t.Pools, collector.DefaultPoolsEvery, 100, time.Second, sup.Pools, known); err != nil {
			return Config{}, err
		}
		if t.SlowLog != nil {
			tc.SlowLog = collector.SlowLogConfig{Enabled: on(t.SlowLog.Enabled, true), Path: t.SlowLog.Path, FromStart: t.SlowLog.FromStart}
			if tc.SlowLog.Enabled && known && !sup.SlowLog {
				return Config{}, fmt.Errorf("%s: the %s engine has no slow log", label, t.Engine)
			}
			if tc.SlowLog.Enabled && t.SlowLog.Path == "" {
				return Config{}, fmt.Errorf("%s slowLog: path is required: the file PostgreSQL writes its log to", label)
			}
		}
		if !tc.Statements.Enabled && !tc.Activity.Enabled && !tc.Pools.Enabled && !tc.SlowLog.Enabled {
			return Config{}, fmt.Errorf("%s: nothing is enabled: turn on statements, activity, pools or slowLog", label)
		}
		c.Targets = append(c.Targets, tc)
	}
	return c, nil
}

func signalOf(label, engine string, s *signal, def time.Duration, defTop int, min time.Duration, supported, known bool) (collector.SignalConfig, error) {
	out := collector.SignalConfig{Enabled: supported, Interval: def, Top: defTop}
	if s == nil {
		return out, nil
	}
	out.Enabled = on(s.Enabled, true)
	if out.Enabled && known && !supported {
		return out, fmt.Errorf("%s: the %s engine does not have this: leave it out", label, engine)
	}
	if s.Interval.Duration != 0 {
		if s.Interval.Duration < min || s.Interval.Duration > 24*time.Hour {
			return out, fmt.Errorf("%s: interval must be between %s and 24h", label, min)
		}
		out.Interval = s.Interval.Duration
	}
	if s.Top != 0 {
		if s.Top < 1 || s.Top > collector.MaxStatements {
			return out, fmt.Errorf("%s: top must be between 1 and %d", label, collector.MaxStatements)
		}
		out.Top = s.Top
	}
	return out, nil
}

// secret reads a value from the environment variable or the file named, one of which is required. The value is trimmed of white space; it is
// never part of an error.
func secret(env Environment, what, envName, fileName string) (string, error) {
	if envName == "" && fileName == "" {
		return "", fmt.Errorf("%s: name where to read it from: an environment variable (…Env) or a file (…File)", what)
	}
	if envName != "" && fileName != "" {
		return "", fmt.Errorf("%s: give an environment variable or a file, not both", what)
	}
	var v string
	if envName != "" {
		v = strings.TrimSpace(env.Getenv(envName))
		if v == "" {
			return "", fmt.Errorf("%s: the environment variable %s is not set or is empty", what, envName)
		}
		return v, nil
	}
	b, err := env.ReadFile(fileName)
	if err != nil {
		return "", fmt.Errorf("%s: cannot read the file %s: %s", what, fileName, errText(err))
	}
	v = strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("%s: the file %s is empty", what, fileName)
	}
	return v, nil
}

func errText(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}
