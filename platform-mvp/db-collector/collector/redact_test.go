package collector

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestScrubTakesTheConnectionStringAndItsPasswordOut(t *testing.T) {
	dsn := "postgres://app:s3cr3t%21Pw@db:5432/x"
	got := Scrub("could not connect to "+dsn+" with s3cr3t!Pw", dsn)
	if strings.Contains(got, "s3cr3t") || strings.Contains(got, dsn) {
		t.Fatal(got)
	}
	if Scrub("plain message", "") != "plain message" {
		t.Fatal("nothing to take out")
	}
	if Scrub("a message about pw", "postgres://u@h/db") != "a message about pw" {
		t.Fatal("a connection string with no password has none to take out")
	}
}

func TestRedactKeepsWhatTheErrorWas(t *testing.T) {
	err := Redact(context.DeadlineExceeded, "postgres://u:pw1234567@h/db", "pw1234567")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("redacting must not hide that it was a deadline")
	}
	if Redact(nil, "x") != nil {
		t.Fatal("no error is no error")
	}
	got := Redact(errors.New("auth failed for pw1234567"), "", "pw1234567", "")
	if strings.Contains(got.Error(), "pw1234567") {
		t.Fatal(got)
	}
}
