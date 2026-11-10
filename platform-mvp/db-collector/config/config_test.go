package config

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/db-collector/collector"
)

func env(vars map[string]string, files map[string]string) Environment {
	return Environment{
		Getenv: func(k string) string { return vars[k] },
		ReadFile: func(p string) ([]byte, error) {
			if v, ok := files[p]; ok {
				return []byte(v), nil
			}
			return nil, errors.New("open " + p + ": no such file or directory")
		},
	}
}

const good = `{
  "ingest": {"url": "http://telemetry-ingest/v1/db", "keyFile": "/etc/key"},
  "targets": [
    {"name": "orders-db", "engine": "postgres", "dsnEnv": "ORDERS_DSN",
     "statements": {"interval": "30s", "top": 100},
     "slowLog": {"path": "/var/log/postgresql/postgresql.log"}},
    {"name": "billing", "engine": "postgres", "dsnFile": "/etc/dsn/billing", "activity": {"enabled": false}}
  ]
}`

var goodEnv = env(map[string]string{"ORDERS_DSN": "postgres://u:orders-pw@h/db"}, map[string]string{"/etc/key": "ingest-key-0123456789\n", "/etc/dsn/billing": "postgres://u:billing-pw@h2/db\n"})

func TestAGoodFile(t *testing.T) {
	c, err := Parse([]byte(good), goodEnv)
	if err != nil {
		t.Fatal(err)
	}
	if c.IngestURL != "http://telemetry-ingest/v1/db" || c.IngestKey != "ingest-key-0123456789" || c.Listen != ":8081" {
		t.Fatalf("%+v", c)
	}
	if len(c.Targets) != 2 {
		t.Fatal(len(c.Targets))
	}
	o, b := c.Targets[0], c.Targets[1]
	if o.Name != "orders-db" || o.Engine != "postgres" || o.DSN != "postgres://u:orders-pw@h/db" {
		t.Fatalf("%+v", o)
	}
	if !o.Statements.Enabled || o.Statements.Interval != 30*time.Second || o.Statements.Top != 100 {
		t.Fatalf("%+v", o.Statements)
	}
	if !o.Activity.Enabled || o.Activity.Interval != 10*time.Second || o.Activity.Top != 20 {
		t.Fatalf("the defaults: %+v", o.Activity)
	}
	if !o.SlowLog.Enabled || o.SlowLog.Path != "/var/log/postgresql/postgresql.log" || o.SlowLog.FromStart {
		t.Fatalf("%+v", o.SlowLog)
	}
	if b.DSN != "postgres://u:billing-pw@h2/db" || b.Activity.Enabled || !b.Statements.Enabled || b.SlowLog.Enabled {
		t.Fatalf("%+v", b)
	}
}

func TestSecretsAreNotKeptInTheFile(t *testing.T) {
	for name, doc := range map[string]string{
		"a dsn":      `{"targets":[{"name":"a","engine":"postgres","dsn":"postgres://u:pw@h/db"}]}`,
		"a password": `{"targets":[{"name":"a","engine":"postgres","password":"x"}]}`,
		"a key":      `{"ingest":{"url":"http://x","key":"abc"},"targets":[]}`,
	} {
		_, err := Parse([]byte(doc), goodEnv)
		if err == nil || !strings.Contains(err.Error(), "secrets are not kept in this file") {
			t.Errorf("%s: %v", name, err)
		}
		if err != nil && (strings.Contains(err.Error(), "pw@h") || strings.Contains(err.Error(), "abc")) {
			t.Errorf("%s: the secret is in the error: %v", name, err)
		}
	}
}

func TestSecretsComeFromWhereTheyAreNamed(t *testing.T) {
	cases := map[string]struct {
		doc  string
		env  Environment
		want string
	}{
		"both an env and a file": {`{"targets":[{"name":"a","engine":"postgres","dsnEnv":"X","dsnFile":"/f"}]}`, goodEnv, "not both"},
		"neither":                {`{"targets":[{"name":"a","engine":"postgres"}]}`, goodEnv, "name where to read it from"},
		"an unset variable":      {`{"targets":[{"name":"a","engine":"postgres","dsnEnv":"NOPE"}]}`, goodEnv, "NOPE is not set"},
		"an empty variable":      {`{"targets":[{"name":"a","engine":"postgres","dsnEnv":"E"}]}`, env(map[string]string{"E": "  "}, nil), "E is not set or is empty"},
		"a missing file":         {`{"targets":[{"name":"a","engine":"postgres","dsnFile":"/nope"}]}`, goodEnv, "cannot read the file /nope"},
		"an empty file":          {`{"targets":[{"name":"a","engine":"postgres","dsnFile":"/e"}]}`, env(nil, map[string]string{"/e": "\n"}), "is empty"},
		"no ingest key":          {`{"ingest":{"url":"http://x"},"targets":[{"name":"a","engine":"postgres","dsnEnv":"ORDERS_DSN"}]}`, goodEnv, "the ingest key"},
	}
	for name, c := range cases {
		_, err := Parse([]byte(c.doc), c.env)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestTheSecretIsNeverInAnError(t *testing.T) {
	e := env(map[string]string{"D": "postgres://u:s3cret-pw@h/db"}, nil)
	// every kind of failure after the secret has been read
	for _, doc := range []string{
		`{"targets":[{"name":"BAD NAME","engine":"postgres","dsnEnv":"D"}]}`,
		`{"targets":[{"name":"a","engine":"","dsnEnv":"D"}]}`,
		`{"targets":[{"name":"a","engine":"postgres","dsnEnv":"D","statements":{"interval":"1ms"}}]}`,
		`{"targets":[{"name":"a","engine":"postgres","dsnEnv":"D","statements":{"enabled":false},"activity":{"enabled":false}}]}`,
	} {
		_, err := Parse([]byte(doc), e)
		if err == nil || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%v", err)
		}
	}
}

func TestValidation(t *testing.T) {
	tgt := func(extra string) string {
		return `{"targets":[{"name":"a","engine":"postgres","dsnEnv":"ORDERS_DSN"` + extra + `}]}`
	}
	cases := map[string]struct{ doc, want string }{
		"no targets":                      {`{"targets":[]}`, "no targets"},
		"no file":                         {``, "not valid"},
		"not json":                        {`{`, "not valid"},
		"trailing":                        {tgt("") + ` {}`, "after the JSON object"},
		"unknown field":                   {tgt(`,"frequency":"1m"`), `unknown field "frequency"`},
		"a name with a capital":           {`{"targets":[{"name":"Orders","engine":"postgres","dsnEnv":"ORDERS_DSN"}]}`, "name must be"},
		"a name with a space":             {`{"targets":[{"name":"a b","engine":"postgres","dsnEnv":"ORDERS_DSN"}]}`, "name must be"},
		"no name":                         {`{"targets":[{"engine":"postgres","dsnEnv":"ORDERS_DSN"}]}`, "name must be"},
		"no engine":                       {`{"targets":[{"name":"a","dsnEnv":"ORDERS_DSN"}]}`, "engine is required"},
		"a repeated name":                 {`{"targets":[{"name":"a","engine":"postgres","dsnEnv":"ORDERS_DSN"},{"name":"a","engine":"postgres","dsnEnv":"ORDERS_DSN"}]}`, "listed twice"},
		"an interval that is not":         {tgt(`,"statements":{"interval":"soon"}`), "not a duration"},
		"a number for a duration":         {tgt(`,"statements":{"interval":60}`), "a duration is a string"},
		"a statements interval too short": {tgt(`,"statements":{"interval":"2s"}`), "between 10s and 24h"},
		"an activity interval too short":  {tgt(`,"activity":{"interval":"100ms"}`), "between 1s and 24h"},
		"an interval over a day":          {tgt(`,"statements":{"interval":"48h"}`), "between 10s and 24h"},
		"a top that is too big":           {tgt(`,"statements":{"top":100000}`), "top must be between"},
		"a negative top":                  {tgt(`,"statements":{"top":-1}`), "top must be between"},
		"a slow log with no path":         {tgt(`,"slowLog":{}`), "path is required"},
		"nothing enabled":                 {tgt(`,"statements":{"enabled":false},"activity":{"enabled":false}`), "nothing is enabled"},
		"an ingest url that is not":       {`{"ingest":{"url":"ftp://x","keyFile":"/etc/key"},"targets":[{"name":"a","engine":"postgres","dsnEnv":"ORDERS_DSN"}]}`, "http or https"},
		"an ingest url with a password":   {`{"ingest":{"url":"http://u:p@x/","keyFile":"/etc/key"},"targets":[{"name":"a","engine":"postgres","dsnEnv":"ORDERS_DSN"}]}`, "must not carry a user name"},
		"a queue that is too big":         {`{"ingest":{"url":"http://x","keyFile":"/etc/key","queueSize":9999999},"targets":[{"name":"a","engine":"postgres","dsnEnv":"ORDERS_DSN"}]}`, "queueSize"},
	}
	for name, c := range cases {
		_, err := Parse([]byte(c.doc), goodEnv)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v (want %q)", name, err, c.want)
		}
	}
}

func TestWithNoIngestTheCollectorCanStillBeChecked(t *testing.T) {
	c, err := Parse([]byte(`{"targets":[{"name":"a","engine":"postgres","dsnEnv":"ORDERS_DSN"}]}`), goodEnv)
	if err != nil || c.IngestURL != "" || c.IngestKey != "" {
		t.Fatalf("%v %+v", err, c)
	}
}

func TestSignalsCanBeTurnedOffAndTheRestKeepTheirDefaults(t *testing.T) {
	c, err := Parse([]byte(`{"targets":[{"name":"a","engine":"postgres","dsnEnv":"ORDERS_DSN","statements":{"enabled":false}}]}`), goodEnv)
	if err != nil {
		t.Fatal(err)
	}
	if c.Targets[0].Statements.Enabled || !c.Targets[0].Activity.Enabled {
		t.Fatalf("%+v", c.Targets[0])
	}
}

// ---- engines with different signals ----

type fakeEngine struct {
	name string
	sig  collector.Signals
}

func (e fakeEngine) Name() string               { return e.name }
func (e fakeEngine) Signals() collector.Signals { return e.sig }
func (fakeEngine) Open(context.Context, collector.TargetConfig) (collector.Source, error) {
	return nil, nil
}
func (fakeEngine) SlowLog(collector.TargetConfig) (collector.SlowLogSource, error) { return nil, nil }

func init() {
	collector.Register(fakeEngine{"testpooler", collector.Signals{Pools: true}})
	collector.Register(fakeEngine{"testdb", collector.Signals{Statements: true, Activity: true, SlowLog: true}})
}

func TestAPoolerHasPoolsAndNothingElseByDefault(t *testing.T) {
	c, err := Parse([]byte(`{"targets":[{"name":"pgb","engine":"testpooler","dsnEnv":"ORDERS_DSN"}]}`), goodEnv)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Targets[0]
	if !p.Pools.Enabled || p.Pools.Interval != 10*time.Second || p.Statements.Enabled || p.Activity.Enabled || p.SlowLog.Enabled {
		t.Fatalf("%+v", p)
	}
	c, err = Parse([]byte(`{"targets":[{"name":"pgb","engine":"testpooler","dsnEnv":"ORDERS_DSN","pools":{"interval":"2s"}}]}`), goodEnv)
	if err != nil || c.Targets[0].Pools.Interval != 2*time.Second {
		t.Fatalf("%v %+v", err, c.Targets[0].Pools)
	}
}

func TestADatabaseHasNoPoolsByDefault(t *testing.T) {
	c, err := Parse([]byte(`{"targets":[{"name":"db","engine":"testdb","dsnEnv":"ORDERS_DSN"}]}`), goodEnv)
	if err != nil {
		t.Fatal(err)
	}
	d := c.Targets[0]
	if d.Pools.Enabled || !d.Statements.Enabled || !d.Activity.Enabled {
		t.Fatalf("%+v", d)
	}
}

func TestAskingAnEngineForWhatItDoesNotHaveIsRefusedAndSaysWhich(t *testing.T) {
	for name, c := range map[string]struct{ doc, want string }{
		"statements of a pooler": {`{"targets":[{"name":"pgb","engine":"testpooler","dsnEnv":"ORDERS_DSN","statements":{}}]}`, "statements: the testpooler engine does not have this"},
		"activity of a pooler":   {`{"targets":[{"name":"pgb","engine":"testpooler","dsnEnv":"ORDERS_DSN","activity":{"interval":"5s"}}]}`, "activity: the testpooler engine does not have this"},
		"a slow log of a pooler": {`{"targets":[{"name":"pgb","engine":"testpooler","dsnEnv":"ORDERS_DSN","slowLog":{"path":"/x"}}]}`, "testpooler engine has no slow log"},
		"pools of a database":    {`{"targets":[{"name":"db","engine":"testdb","dsnEnv":"ORDERS_DSN","pools":{}}]}`, "pools: the testdb engine does not have this"},
	} {
		_, err := Parse([]byte(c.doc), goodEnv)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v (want %q)", name, err, c.want)
		}
	}
	// turned off, it is not asking
	if _, err := Parse([]byte(`{"targets":[{"name":"pgb","engine":"testpooler","dsnEnv":"ORDERS_DSN","statements":{"enabled":false}}]}`), goodEnv); err != nil {
		t.Fatal(err)
	}
}

func TestAPoolerWithPoolsTurnedOffHasNothingEnabled(t *testing.T) {
	_, err := Parse([]byte(`{"targets":[{"name":"pgb","engine":"testpooler","dsnEnv":"ORDERS_DSN","pools":{"enabled":false}}]}`), goodEnv)
	if err == nil || !strings.Contains(err.Error(), "nothing is enabled") {
		t.Fatal(err)
	}
}

func TestPoolsIntervalsAreChecked(t *testing.T) {
	for _, iv := range []string{"100ms", "48h"} {
		_, err := Parse([]byte(`{"targets":[{"name":"pgb","engine":"testpooler","dsnEnv":"ORDERS_DSN","pools":{"interval":"`+iv+`"}}]}`), goodEnv)
		if err == nil || !strings.Contains(err.Error(), "between 1s and 24h") {
			t.Errorf("%s: %v", iv, err)
		}
	}
}
