// Package project maps API keys to projects. A project is one fleet cluster
// (or any other telemetry source); its ingest keys let an agent write, its
// read keys let a caller query. A key is valid for exactly one project, and a
// key that is unknown is rejected: there is no default project.
package project

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sync"
	"time"
)

const minKeyLen = 16

var nameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

type Entry struct {
	Name       string   `json:"name"`
	IngestKeys []string `json:"ingestKeys"`
	ReadKeys   []string `json:"readKeys"`
}

type file struct {
	Projects []Entry `json:"projects"`
}

type snapshot struct {
	ingest map[[32]byte]string
	read   map[[32]byte]string
}

type Registry struct {
	path string
	mu   sync.RWMutex
	cur  *snapshot
	raw  [32]byte
}

// Load reads and validates path. It fails when the file is unusable, so a
// bad first config stops the service instead of running with no projects.
func Load(path string) (*Registry, error) {
	r := &Registry{path: path}
	if _, err := r.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Registry) reload() (bool, error) {
	data, err := os.ReadFile(r.path)
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(data)
	r.mu.RLock()
	same := r.cur != nil && sum == r.raw
	r.mu.RUnlock()
	if same {
		return false, nil
	}
	s, err := Parse(data)
	if err != nil {
		return false, err
	}
	r.mu.Lock()
	r.cur, r.raw = s, sum
	r.mu.Unlock()
	return true, nil
}

// Watch re-reads the file until ctx ends. A file that no longer validates is
// logged and the last good one stays in force.
func (r *Registry) Watch(ctx context.Context, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			changed, err := r.reload()
			if err != nil {
				log.Error("projects file not reloaded; keeping the previous one", "error", err)
			} else if changed {
				log.Info("projects file reloaded")
			}
		}
	}
}

func Parse(data []byte) (*snapshot, error) {
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse projects: %w", err)
	}
	if len(f.Projects) == 0 {
		return nil, fmt.Errorf("projects file lists no projects")
	}
	s := &snapshot{ingest: map[[32]byte]string{}, read: map[[32]byte]string{}}
	names := map[string]bool{}
	used := map[[32]byte]string{}
	add := func(dst map[[32]byte]string, name, kind, key string) error {
		if len(key) < minKeyLen {
			return fmt.Errorf("project %q: a %s key is shorter than %d characters", name, kind, minKeyLen)
		}
		h := sha256.Sum256([]byte(key))
		if other, dup := used[h]; dup {
			return fmt.Errorf("project %q: a %s key is already used by project %q", name, kind, other)
		}
		used[h] = name
		dst[h] = name
		return nil
	}
	for _, e := range f.Projects {
		if !nameRE.MatchString(e.Name) {
			return nil, fmt.Errorf("project name %q is not a DNS label", e.Name)
		}
		if names[e.Name] {
			return nil, fmt.Errorf("project %q is listed twice", e.Name)
		}
		names[e.Name] = true
		if len(e.IngestKeys)+len(e.ReadKeys) == 0 {
			return nil, fmt.Errorf("project %q has no keys", e.Name)
		}
		for _, k := range e.IngestKeys {
			if err := add(s.ingest, e.Name, "ingest", k); err != nil {
				return nil, err
			}
		}
		for _, k := range e.ReadKeys {
			if err := add(s.read, e.Name, "read", k); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

func (r *Registry) IngestProject(key string) (string, bool) { return r.lookup(key, true) }
func (r *Registry) ReadProject(key string) (string, bool)   { return r.lookup(key, false) }

func (r *Registry) lookup(key string, ingest bool) (string, bool) {
	if key == "" {
		return "", false
	}
	h := sha256.Sum256([]byte(key))
	r.mu.RLock()
	defer r.mu.RUnlock()
	m := r.cur.read
	if ingest {
		m = r.cur.ingest
	}
	name, ok := m[h]
	return name, ok
}
