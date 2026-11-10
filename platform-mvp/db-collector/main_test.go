package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runWith(t *testing.T, config string, args ...string) (int, string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.json")
	if config != "" {
		if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out, errb bytes.Buffer
	code := run(append([]string{"--config", path}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

func TestExitCodes(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(nil, &out, &errb); code != 2 || !strings.Contains(errb.String(), "--config is required") {
		t.Fatalf("%d %s", code, errb.String())
	}
	if code := run([]string{"--nonsense"}, &out, &errb); code != 2 {
		t.Fatalf("%d", code)
	}
	if code, _, e := runWith(t, ""); code != 1 || !strings.Contains(e, "read the configuration") {
		t.Fatalf("%d %s", code, e)
	}
	if code, _, e := runWith(t, `{"targets":[]}`); code != 1 || !strings.Contains(e, "no targets") {
		t.Fatalf("%d %s", code, e)
	}
}

func TestCheckSaysWhyATargetCannotBeUsed(t *testing.T) {
	t.Setenv("DSN", "postgres://u:never-shown-pw@127.0.0.1:1/db?sslmode=disable&connect_timeout=1")
	cfg := `{"targets":[{"name":"down","engine":"postgres","dsnEnv":"DSN"},{"name":"odd","engine":"oracle","dsnEnv":"DSN"}]}`
	code, _, e := runWith(t, cfg, "--check")
	if code != 1 {
		t.Fatalf("%d", code)
	}
	if !strings.Contains(e, "target down: cannot be asked") || !strings.Contains(e, `unknown database engine "oracle"`) || !strings.Contains(e, "known: pgbouncer, postgres") {
		t.Fatalf("%s", e)
	}
	if strings.Contains(e, "never-shown-pw") {
		t.Fatalf("the password is in what was printed: %s", e)
	}
}

func TestCollectingWithNowhereToSendRefusesToStart(t *testing.T) {
	t.Setenv("DSN", "postgres://u:pw@127.0.0.1:1/db")
	code, _, e := runWith(t, `{"targets":[{"name":"a","engine":"postgres","dsnEnv":"DSN"}]}`)
	if code != 1 || !strings.Contains(e, "ingest.url is not set") {
		t.Fatalf("%d %s", code, e)
	}
}
