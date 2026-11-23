package enroll

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
)

// MemStore is the log in memory, with a way to make it fail: for tests, and for trying the API with no database.
type MemStore struct {
	mu      sync.Mutex
	events  []Event
	failing atomic.Bool
	// Lookups counts the questions asked by token, so a test can tell that garbage was not asked about.
	Lookups atomic.Int64
}

func NewMemStore() *MemStore { return &MemStore{} }

func (s *MemStore) SetFailing(on bool) { s.failing.Store(on) }

func (s *MemStore) Append(_ context.Context, e Event) error {
	if s.failing.Load() {
		return errors.New("disk full")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return nil
}

func (s *MemStore) pick(keep func(Event) bool) []Event {
	var out []Event
	for _, e := range s.events {
		if keep(e) {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

func (s *MemStore) EventsOf(_ context.Context, project, id string) ([]Event, error) {
	if s.failing.Load() {
		return nil, errors.New("disk gone")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pick(func(e Event) bool { return e.Project == project && e.ID == id }), nil
}

func (s *MemStore) Project(_ context.Context, project string) ([]Event, error) {
	if s.failing.Load() {
		return nil, errors.New("disk gone")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pick(func(e Event) bool { return e.Project == project }), nil
}

func (s *MemStore) All(_ context.Context) ([]Event, error) {
	if s.failing.Load() {
		return nil, errors.New("disk gone")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pick(func(Event) bool { return true }), nil
}

func (s *MemStore) FindByToken(_ context.Context, tokenHash string) (string, string, bool, error) {
	s.Lookups.Add(1)
	if s.failing.Load() {
		return "", "", false, errors.New("disk gone")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.Type == EvCreated && e.TokenHash == tokenHash {
			return e.Project, e.ID, true, nil
		}
	}
	return "", "", false, nil
}

// Events is everything stored, for a test that looks for what must not be there.
func (s *MemStore) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}
