package incident

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

// MemStore is the event log in memory, with a way to make it fail: for tests, and for trying the API with no database. It is safe for
// concurrent use.
type MemStore struct {
	mu      sync.Mutex
	events  map[string][]Event // project|id
	order   []string           // creation order of the keys
	failing atomic.Bool
}

func NewMemStore() *MemStore { return &MemStore{events: map[string][]Event{}} }

// SetFailing makes every call fail (true) or work again (false).
func (s *MemStore) SetFailing(on bool) { s.failing.Store(on) }

func (s *MemStore) Append(_ context.Context, ev Event) error {
	if s.failing.Load() {
		return errors.New("disk full")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := ev.Project + "|" + ev.IncidentID
	for _, e := range s.events[k] {
		if e.Seq == ev.Seq {
			return fmt.Errorf("sequence %d of %s written twice", ev.Seq, k) // what a unique key would say
		}
	}
	if ev.Seq == 1 {
		s.order = append(s.order, k)
	}
	s.events[k] = append(s.events[k], ev)
	return nil
}

func (s *MemStore) Events(_ context.Context, project, id string) ([]Event, error) {
	if s.failing.Load() {
		return nil, errors.New("disk gone")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events[project+"|"+id]...), nil
}

func (s *MemStore) Recent(_ context.Context, project string, limit int) ([][]Event, error) {
	if s.failing.Load() {
		return nil, errors.New("disk gone")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][]Event
	for i := len(s.order) - 1; i >= 0 && len(out) < limit; i-- {
		if strings.HasPrefix(s.order[i], project+"|") {
			out = append(out, append([]Event(nil), s.events[s.order[i]]...))
		}
	}
	return out, nil
}

func (s *MemStore) FindByDedupeKey(_ context.Context, project, key string) (string, error) {
	if s.failing.Load() {
		return "", errors.New("disk gone")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, evs := range s.events {
		if strings.HasPrefix(k, project+"|") && evs[0].DedupeKey == key {
			return evs[0].IncidentID, nil
		}
	}
	return "", nil
}
