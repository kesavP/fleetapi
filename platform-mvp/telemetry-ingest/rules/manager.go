package rules

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Rule is one saved version of a rule: what was saved, who saved it and when.
type Rule struct {
	Name      string
	Settings  Settings
	Version   uint32
	UpdatedAt time.Time
	UpdatedBy string
}

// Store is where saved rules are kept: the ClickHouse table in service, MemStore in a test.
type Store interface {
	// Append keeps a new version. A version that is already kept is refused: the Manager writes one change at a time, so that is a second writer.
	Append(ctx context.Context, project string, r Rule) error
	// Latest is the newest version of each rule that has been saved for the project.
	Latest(ctx context.Context, project string) ([]Rule, error)
}

// View is a rule as it is in force for a project: the settings in use, and whether they were saved or are the default.
type View struct {
	Name      string
	Settings  Settings // in force
	Default   Settings // what it is when nothing is saved
	Saved     bool
	Version   uint32 // 0 when nothing is saved
	UpdatedAt time.Time
	UpdatedBy string
}

// ConflictError says a change was based on a version that is no longer the newest.
type ConflictError struct{ Current uint32 }

func (e *ConflictError) Error() string {
	return fmt.Sprintf("the rule has changed since it was read: it is now at version %d", e.Current)
}

// Manager reads and changes the rules of every project.
type Manager struct {
	store    Store
	defaults map[string]Settings
	now      func() time.Time
	mu       sync.Mutex // one change at a time: a version is read, then the next one is written
}

// NewManager uses `defaults` for a rule nothing has been saved for; nil is the built-in Defaults.
func NewManager(store Store, defaults map[string]Settings) *Manager {
	if defaults == nil {
		defaults = Defaults()
	}
	return &Manager{store: store, defaults: defaults, now: func() time.Time { return time.Now().UTC() }}
}

func (m *Manager) views(ctx context.Context, project string) ([]View, error) {
	saved, err := m.store.Latest(ctx, project)
	if err != nil {
		return nil, err
	}
	byName := map[string]Rule{}
	for _, r := range saved {
		if cur, ok := byName[r.Name]; !ok || r.Version > cur.Version {
			byName[r.Name] = r
		}
	}
	out := make([]View, 0, len(Names))
	for _, name := range Names {
		v := View{Name: name, Settings: m.defaults[name], Default: m.defaults[name]}
		if r, ok := byName[name]; ok {
			v.Settings, v.Saved, v.Version, v.UpdatedAt, v.UpdatedBy = r.Settings, true, r.Version, r.UpdatedAt, r.UpdatedBy
		}
		out = append(out, v)
	}
	return out, nil
}

// List is every rule as it is in force for the project, in the order of Names.
func (m *Manager) List(ctx context.Context, project string) ([]View, error) {
	return m.views(ctx, project)
}

// Effective is the settings in force, by rule name. A failure of the store is an error and never the defaults: a rule someone tightened must not
// quietly loosen because the database was slow.
func (m *Manager) Effective(ctx context.Context, project string) (map[string]Settings, error) {
	vs, err := m.views(ctx, project)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Settings, len(vs))
	for _, v := range vs {
		out[v.Name] = v.Settings
	}
	return out, nil
}

// Put saves a new version of a rule. expected, when given, is the version the caller read (0 for "nothing saved"): if the rule has moved on, the
// change is refused with a *ConflictError and the caller looks again.
func (m *Manager) Put(ctx context.Context, project, name string, s Settings, actor string, expected *uint32) (View, error) {
	if !known(name) {
		return View{}, fmt.Errorf("%w: %q (they are %v)", ErrUnknownRule, name, Names)
	}
	if err := s.Validate(); err != nil {
		return View{}, err
	}
	actor = strings.TrimSpace(actor)
	if actor == "" || utf8.RuneCountInString(actor) > 64 {
		return View{}, invalid("who is making the change is required (1 to 64 characters)")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	vs, err := m.views(ctx, project)
	if err != nil {
		return View{}, err
	}
	var cur View
	for _, v := range vs {
		if v.Name == name {
			cur = v
		}
	}
	if expected != nil && *expected != cur.Version {
		return View{}, &ConflictError{Current: cur.Version}
	}
	r := Rule{Name: name, Settings: s, Version: cur.Version + 1, UpdatedAt: m.now(), UpdatedBy: actor}
	if err := m.store.Append(ctx, project, r); err != nil {
		return View{}, err
	}
	return View{Name: name, Settings: s, Default: cur.Default, Saved: true, Version: r.Version, UpdatedAt: r.UpdatedAt, UpdatedBy: actor}, nil
}

// MemStore is Store in memory, with a way to make it fail: for tests, and for trying the API with no database. It is safe for concurrent use.
type MemStore struct {
	mu      sync.Mutex
	rows    map[string][]Rule // project
	failing atomic.Bool
}

func NewMemStore() *MemStore { return &MemStore{rows: map[string][]Rule{}} }

// SetFailing makes every call fail (true) or work again (false).
func (s *MemStore) SetFailing(on bool) { s.failing.Store(on) }

// Count is how many versions are kept, across projects.
func (s *MemStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.rows {
		n += len(r)
	}
	return n
}

var errMemFailing = errors.New("disk full")

func (s *MemStore) Append(_ context.Context, project string, r Rule) error {
	if s.failing.Load() {
		return errMemFailing
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.rows[project] {
		if e.Name == r.Name && e.Version == r.Version {
			return fmt.Errorf("version %d of %s is already kept", r.Version, r.Name)
		}
	}
	s.rows[project] = append(s.rows[project], r)
	return nil
}

func (s *MemStore) Latest(_ context.Context, project string) ([]Rule, error) {
	if s.failing.Load() {
		return nil, errMemFailing
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	best := map[string]Rule{}
	for _, r := range s.rows[project] {
		if cur, ok := best[r.Name]; !ok || r.Version > cur.Version {
			best[r.Name] = r
		}
	}
	out := make([]Rule, 0, len(best))
	for _, r := range best {
		out = append(out, r)
	}
	return out, nil
}
