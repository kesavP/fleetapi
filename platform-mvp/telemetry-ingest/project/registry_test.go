package project

import (
	"crypto/sha256"
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

func TestManageKeysAreTheirOwnKind(t *testing.T) {
	data := `{"projects":[{"name":"us","ingestKeys":["us-ingest-key-0000001"],"readKeys":["us-read-key-00000001"],"manageKeys":["us-manage-key-000001"]},
 {"name":"eu","manageKeys":["eu-manage-key-000001"]}]}`
	p := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := r.ManageProject("us-manage-key-000001"); !ok || n != "us" {
		t.Fatalf("%q %v", n, ok)
	}
	// each kind of key opens only its own kind of access
	if _, ok := r.ReadProject("us-manage-key-000001"); ok {
		t.Fatal("a manage key was accepted as a read key: reading is the server's decision, not the registry's")
	}
	if _, ok := r.IngestProject("us-manage-key-000001"); ok {
		t.Fatal("a manage key may ingest")
	}
	if _, ok := r.ManageProject("us-read-key-00000001"); ok {
		t.Fatal("a read key may manage: a browser's key could change incidents")
	}
	if _, ok := r.ManageProject("us-ingest-key-0000001"); ok {
		t.Fatal("an ingest key may manage")
	}
	if _, ok := r.ManageProject(""); ok {
		t.Fatal("an empty key was accepted")
	}
	if n, ok := r.ManageProject("eu-manage-key-000001"); !ok || n != "eu" {
		t.Fatalf("a project with only a manage key: %q %v", n, ok)
	}
}

func TestManageKeysAreCheckedLikeTheOthers(t *testing.T) {
	for name, data := range map[string]string{
		"short":     `{"projects":[{"name":"us","manageKeys":["short"]}]}`,
		"shared":    `{"projects":[{"name":"us","readKeys":["shared-key-0000000001"],"manageKeys":["shared-key-0000000001"]}]}`,
		"two teams": `{"projects":[{"name":"us","manageKeys":["shared-key-0000000001"]},{"name":"eu","manageKeys":["shared-key-0000000001"]}]}`,
	} {
		if _, err := Parse([]byte(data)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestProjectsListsEveryProjectOnceInOrder(t *testing.T) {
	p := filepath.Join(t.TempDir(), "p.json")
	// "eu" has only an ingest key, "ops" only a manage key, "us" all three: each must be listed, once.
	data := `{"projects":[
 {"name":"us","ingestKeys":["us-ingest-key-0000001"],"readKeys":["us-read-key-00000001"],"manageKeys":["us-manage-key-000001"]},
 {"name":"ops","manageKeys":["ops-manage-key-00001"]},
 {"name":"eu","ingestKeys":["eu-ingest-key-0000001"]}]}`
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	got := r.Projects()
	if strings.Join(got, ",") != "eu,ops,us" {
		t.Fatalf("projects = %v", got)
	}
}

func TestIssuedIngestKeysAreAcceptedForIngestOnlyAndOnlyForAProjectTheFileLists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(p, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	issued := sha256.Sum256([]byte("ing_issued-key-for-us-0000000000000000000000000"))
	gone := sha256.Sum256([]byte("ing_issued-key-for-a-removed-project-00000000000"))
	if _, ok := r.IngestProject("ing_issued-key-for-us-0000000000000000000000000"); ok {
		t.Fatal("a key that was not issued is accepted")
	}
	in := map[[32]byte]string{issued: "us", gone: "removed"}
	r.SetIssuedIngest(in)
	if n, ok := r.IngestProject("ing_issued-key-for-us-0000000000000000000000000"); !ok || n != "us" {
		t.Fatalf("%q %v", n, ok)
	}
	if _, ok := r.ReadProject("ing_issued-key-for-us-0000000000000000000000000"); ok {
		t.Fatal("an issued ingest key reads")
	}
	if _, ok := r.ManageProject("ing_issued-key-for-us-0000000000000000000000000"); ok {
		t.Fatal("an issued ingest key manages")
	}
	if _, ok := r.IngestProject("ing_issued-key-for-a-removed-project-00000000000"); ok {
		t.Fatal("a key of a project the file does not list is accepted")
	}
	// the file's own keys are unaffected, and a copy was taken
	if n, ok := r.IngestProject("us-ingest-key-0000001"); !ok || n != "us" {
		t.Fatal("the file's key")
	}
	delete(in, issued)
	if _, ok := r.IngestProject("ing_issued-key-for-us-0000000000000000000000000"); !ok {
		t.Fatal("the registry kept the caller's map instead of a copy")
	}
	// replaced: what is not in the new set is gone (a revoked key)
	r.SetIssuedIngest(map[[32]byte]string{})
	if _, ok := r.IngestProject("ing_issued-key-for-us-0000000000000000000000000"); ok {
		t.Fatal("a key that was dropped is still accepted")
	}
	// a project the reload took out of the file takes its issued keys with it
	r.SetIssuedIngest(map[[32]byte]string{issued: "us"})
	if err := os.WriteFile(p, []byte(`{"projects":[{"name":"eu","ingestKeys":["eu-ingest-key-0000001"]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reload(); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.IngestProject("ing_issued-key-for-us-0000000000000000000000000"); ok {
		t.Fatal("an issued key outlived its project")
	}
}
