package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const good = `{"projects":[
 {"name":"us","ingestKeys":["us-ingest-key-0000001"],"readKeys":["us-read-key-00000001"]},
 {"name":"eu","ingestKeys":["eu-ingest-key-0000001"]}]}`

func TestLookup(t *testing.T) {
	p := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(p, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := r.IngestProject("us-ingest-key-0000001"); !ok || n != "us" {
		t.Fatalf("ingest key: %q %v", n, ok)
	}
	if _, ok := r.ReadProject("us-ingest-key-0000001"); ok {
		t.Fatal("an ingest key must not read")
	}
	if _, ok := r.IngestProject("us-read-key-00000001"); ok {
		t.Fatal("a read key must not ingest")
	}
	if _, ok := r.IngestProject(""); ok {
		t.Fatal("empty key accepted")
	}
	if _, ok := r.ReadProject("eu-ingest-key-0000001"); ok {
		t.Fatal("eu has no read keys")
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"empty":        `{"projects":[]}`,
		"bad name":     `{"projects":[{"name":"US_East","ingestKeys":["0123456789abcdef"]}]}`,
		"short key":    `{"projects":[{"name":"us","ingestKeys":["short"]}]}`,
		"no keys":      `{"projects":[{"name":"us"}]}`,
		"dup project":  `{"projects":[{"name":"us","ingestKeys":["0123456789abcdef"]},{"name":"us","ingestKeys":["fedcba9876543210"]}]}`,
		"shared key":   `{"projects":[{"name":"us","ingestKeys":["0123456789abcdef"]},{"name":"eu","ingestKeys":["0123456789abcdef"]}]}`,
		"ingest==read": `{"projects":[{"name":"us","ingestKeys":["0123456789abcdef"],"readKeys":["0123456789abcdef"]}]}`,
		"not json":     `nope`,
	}
	for name, in := range cases {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestReloadKeepsLastGood(t *testing.T) {
	p := filepath.Join(t.TempDir(), "p.json")
	_ = os.WriteFile(p, []byte(good), 0o600)
	r, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(p, []byte(`{"projects":[`), 0o600)
	if changed, err := r.reload(); err == nil || changed {
		t.Fatalf("bad file: changed=%v err=%v", changed, err)
	}
	if _, ok := r.IngestProject("us-ingest-key-0000001"); !ok {
		t.Fatal("last good config was lost")
	}
	_ = os.WriteFile(p, []byte(strings.Replace(good, "us-ingest-key-0000001", "us-ingest-key-0000002", 1)), 0o600)
	if changed, err := r.reload(); err != nil || !changed {
		t.Fatalf("good file: changed=%v err=%v", changed, err)
	}
	if _, ok := r.IngestProject("us-ingest-key-0000001"); ok {
		t.Fatal("rotated key still valid")
	}
}

func TestLoadFailsOnBadFirstFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "p.json")
	_ = os.WriteFile(p, []byte(`{}`), 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error")
	}
}
