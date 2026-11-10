package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sojoner/kro-lab/platform-mvp/db-collector/collector"
)

func TestAnErrorNeverCarriesThePassword(t *testing.T) {
	const pw = "s3cr3t-Pw!9"
	for name, dsn := range map[string]string{
		"a url":              "postgres://app:" + pw + "@db.internal:5432/shop?sslmode=require",
		"a url, encoded":     "postgres://app:s3cr3t-Pw%219@db.internal:5432/shop",
		"key and value form": "host=db.internal user=app password='" + pw + "' dbname=shop",
	} {
		// the two ways of scrubbing, each on its own: the secrets read from the parsed connection, and the connection string itself
		bySecret := &source{cfg: collector.TargetConfig{Name: "t"}, secrets: []string{pw}}
		if err := bySecret.wrap(errors.New("failed to connect with password " + pw + " refused")); strings.Contains(err.Error(), pw) {
			t.Errorf("%s: the secret list did not scrub: %v", name, err)
		}
		byDSN := &source{cfg: collector.TargetConfig{Name: "t", DSN: dsn}}
		if err := byDSN.wrap(errors.New("bad config " + dsn + " (password " + pw + ")")); strings.Contains(err.Error(), pw) || strings.Contains(err.Error(), dsn) {
			t.Errorf("%s: the connection string did not scrub: %v", name, err)
		}
	}
}

func TestACauseIsKeptButItsTextIsClean(t *testing.T) {
	cause := context.Canceled
	s := &source{cfg: collector.TargetConfig{DSN: "postgres://a:pw12345678@h/db"}, secrets: []string{"pw12345678"}}
	err := s.wrap(cause)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("wrapping lost what the error was: a cancelled context must still be recognised as one")
	}
	if s.wrap(nil) != nil {
		t.Fatal("no error is no error")
	}
}

func TestAnInvalidConnectionStringIsRefusedWithoutItsPassword(t *testing.T) {
	for _, dsn := range []string{
		"postgres://app:s3cr3t-Pw@db.internal:notaport/shop",
		"postgres://app:s3cr3t-Pw@db.internal:5432/shop?sslmode=bogus",
		"host=db user=app password=s3cr3t-Pw sslmode=bogus",
		"postgres://app:s3cr3t-Pw@%zz/shop",
	} {
		_, err := Engine{}.Open(context.Background(), collector.TargetConfig{Name: "orders", DSN: dsn})
		if err == nil {
			t.Errorf("%q was accepted", dsn)
			continue
		}
		if strings.Contains(err.Error(), "s3cr3t-Pw") {
			t.Errorf("the password is in: %v", err)
		}
		if !strings.Contains(err.Error(), "orders") {
			t.Errorf("the error does not say which target: %v", err)
		}
	}
}

func TestAnEngineThatIsGivenNoPathHasNoSlowLog(t *testing.T) {
	if _, err := (Engine{}).SlowLog(collector.TargetConfig{}); err == nil || !strings.Contains(err.Error(), "slowLog.path") {
		t.Fatal(err)
	}
	f, err := (Engine{}).SlowLog(collector.TargetConfig{SlowLog: collector.SlowLogConfig{Path: "/x", FromStart: true}})
	if err != nil {
		t.Fatal(err)
	}
	tl := f.(*Tailer)
	if tl.Path != "/x" || !tl.FromStart {
		t.Fatalf("%+v", tl)
	}
}

func TestEngineIsRegistered(t *testing.T) {
	e, err := collector.Lookup("postgres")
	if err != nil || e.Name() != "postgres" {
		t.Fatal(err)
	}
	if _, err := collector.Lookup("oracle"); err == nil || !strings.Contains(err.Error(), "known: postgres") {
		t.Fatal(err)
	}
}
