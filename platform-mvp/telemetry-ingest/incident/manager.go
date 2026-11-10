package incident

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Store is what the Manager needs from a database: an append-only log of events. Append must be durable when it returns.
type Store interface {
	Append(ctx context.Context, ev Event) error
	Events(ctx context.Context, project, incidentID string) ([]Event, error)
	// Recent returns the events of the `limit` most recently created incidents of the project, grouped by incident.
	Recent(ctx context.Context, project string, limit int) ([][]Event, error)
	// FindByDedupeKey returns the id of the incident created with the key, or "".
	FindByDedupeKey(ctx context.Context, project, key string) (string, error)
}

var (
	ErrNotFound = errors.New("no such incident")
	// ErrInvalid wraps what is wrong with an input; the message is for the caller.
	ErrInvalid = errors.New("invalid")
)

// ConflictError: the incident is not at the version the caller expected, because somebody else changed it first.
type ConflictError struct{ Current uint32 }

func (e *ConflictError) Error() string {
	return fmt.Sprintf("the incident has changed since it was read: it is now at version %d", e.Current)
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

const (
	MaxTitle      = 200
	MaxSummary    = 4000
	MaxComment    = 4000
	MaxNote       = 1000
	MaxApp        = 300
	MaxAssignee   = 64
	MaxReason     = 500
	MaxDedupeKey  = 128
	MaxList       = 500 // how many of the newest incidents a listing looks through
	skewAllowance = 5 * time.Minute
)

var (
	actorRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._@+-]{0,63}$`)
	assigneeRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._@+-]{0,63}$`)
	dedupeRE   = regexp.MustCompile(`^[A-Za-z0-9:._/@|=+-]{1,128}$`)
	idRE       = regexp.MustCompile(`^[a-z0-9]{8}$`)
)

// ValidActor says whether a name can be recorded as the actor of an event.
func ValidActor(s string) bool { return actorRE.MatchString(s) }

// ValidID says whether s has the shape of an incident id, so that anything else is refused before it reaches a query.
func ValidID(s string) bool { return idRE.MatchString(s) }

type Manager struct {
	store Store
	log   *slog.Logger
	now   func() time.Time
	newID func() string

	// mu serialises every change. The log is an append in a database with no transactions, so "read the incident, check the rule, append"
	// is made safe by never running two at once. That is enough for one replica of the service (the deployment's only shape: see the chart);
	// more than one would need the database to refuse a second event with the same sequence number.
	mu sync.Mutex
}

func NewManager(store Store, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{store: store, log: log, now: func() time.Time { return time.Now().UTC() }, newID: randomID}
}

func randomID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // the system's source of randomness is gone: nothing safe to do
	}
	out := make([]byte, 8)
	for i, x := range b {
		out[i] = alphabet[int(x)%len(alphabet)]
	}
	return string(out)
}

func eventID() string { return "e_" + randomID() + randomID() }

func text(name, s string, max int, newlines bool) (string, error) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > max {
		return "", invalid("%s is longer than %d characters", name, max)
	}
	if !utf8.ValidString(s) || hasControl(s, newlines) {
		return "", invalid("%s has characters that are not allowed", name)
	}
	return s, nil
}

type CreateInput struct {
	Title     string
	App       string
	Severity  Severity
	Source    Source
	DedupeKey string
	Summary   string
	StartedAt time.Time
}

// Create opens an incident. With a DedupeKey, creating again with the same key returns the incident that exists (created is false):
// whatever finds incidents can say "this one" as often as it likes and get one record.
func (m *Manager) Create(ctx context.Context, project, actor string, in CreateInput) (Incident, bool, error) {
	title, err := text("title", in.Title, MaxTitle, false)
	if err != nil {
		return Incident{}, false, err
	}
	if title == "" {
		return Incident{}, false, invalid("title is required")
	}
	app, err := text("app", in.App, MaxApp, false)
	if err != nil {
		return Incident{}, false, err
	}
	summary, err := text("summary", in.Summary, MaxSummary, true)
	if err != nil {
		return Incident{}, false, err
	}
	sev := in.Severity
	if sev == "" {
		sev = Warning
	}
	if !ValidSeverity(sev) {
		return Incident{}, false, invalid("severity must be critical, warning or info")
	}
	src := in.Source
	if src == "" {
		src = Manual
	}
	if src != Manual && src != Detected {
		return Incident{}, false, invalid("source must be manual or detected")
	}
	if in.DedupeKey != "" && !dedupeRE.MatchString(in.DedupeKey) {
		return Incident{}, false, invalid("dedupe_key must be 1 to %d characters of letters, digits and : . _ / @ | = + -", MaxDedupeKey)
	}
	now := m.now()
	started := in.StartedAt.UTC()
	switch {
	case in.StartedAt.IsZero():
		started = now
	case started.After(now.Add(skewAllowance)):
		return Incident{}, false, invalid("started_at is in the future")
	case started.Year() < 2000:
		return Incident{}, false, invalid("started_at is not a plausible time")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if in.DedupeKey != "" {
		id, err := m.store.FindByDedupeKey(ctx, project, in.DedupeKey)
		if err != nil {
			return Incident{}, false, err
		}
		if id != "" {
			inc, err := m.load(ctx, project, id)
			return inc, false, err
		}
	}
	id := m.newID()
	payload := Created{Title: title, App: app, Severity: sev, Source: src, DedupeKey: in.DedupeKey, Summary: summary, StartedAt: started}
	ev, err := m.event(project, id, 1, actor, EvCreated, payload, now)
	if err != nil {
		return Incident{}, false, err
	}
	ev.DedupeKey = in.DedupeKey
	if err := m.store.Append(ctx, ev); err != nil {
		return Incident{}, false, err
	}
	inc, err := Fold([]Event{ev})
	return inc, true, err
}

func (m *Manager) event(project, id string, seq uint32, actor string, t EventType, payload any, at time.Time) (Event, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return Event{}, err
	}
	return Event{Project: project, IncidentID: id, Seq: seq, ID: eventID(), Time: at, Type: t, Actor: actor, Data: data}, nil
}

func (m *Manager) load(ctx context.Context, project, id string) (Incident, error) {
	if !ValidID(id) {
		return Incident{}, ErrNotFound
	}
	evs, err := m.store.Events(ctx, project, id)
	if err != nil {
		return Incident{}, err
	}
	if len(evs) == 0 {
		return Incident{}, ErrNotFound
	}
	return Fold(evs)
}

// Get returns the incident as it stands.
func (m *Manager) Get(ctx context.Context, project, id string) (Incident, error) {
	return m.load(ctx, project, id)
}

type Filter struct {
	States   []State
	App      string
	Severity Severity
	Limit    int
}

// List returns the project's incidents, newest first, from the MaxList most recently created. `skipped` counts incidents whose record
// would not fold: they are left out of the list (and logged) rather than failing it, so one bad record cannot hide the rest.
func (m *Manager) List(ctx context.Context, project string, f Filter) ([]Incident, int, error) {
	groups, err := m.store.Recent(ctx, project, MaxList)
	if err != nil {
		return nil, 0, err
	}
	want := map[State]bool{}
	for _, s := range f.States {
		want[s] = true
	}
	var out []Incident
	skipped := 0
	for _, evs := range groups {
		inc, err := Fold(evs)
		if err != nil {
			skipped++
			m.log.Warn("incident record does not fold; left out of the list", "project", project, "error", err)
			continue
		}
		if (len(want) > 0 && !want[inc.State]) || (f.App != "" && inc.App != f.App) || (f.Severity != "" && inc.Severity != f.Severity) {
			continue
		}
		out = append(out, inc)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, skipped, nil
}

// Events returns the incident's timeline after `after` (a sequence number; 0 for all), at most `limit` of them (1 to 500).
func (m *Manager) Events(ctx context.Context, project, id string, after uint32, limit int) ([]Event, error) {
	if !ValidID(id) {
		return nil, ErrNotFound
	}
	evs, err := m.store.Events(ctx, project, id)
	if err != nil {
		return nil, err
	}
	if len(evs) == 0 {
		return nil, ErrNotFound
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].Seq < evs[j].Seq })
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	out := []Event{}
	for _, e := range evs {
		if e.Seq > after {
			out = append(out, e)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

// change is the one way an existing incident is altered: under the lock, read it, let `build` check the rule and say what to append, append
// that as the next event, and return the incident as it is after. `expected`, when set, is the version the caller read.
func (m *Manager) change(ctx context.Context, project, id, actor string, expected *uint32, build func(cur Incident) (EventType, any, error)) (Incident, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !ValidID(id) {
		return Incident{}, ErrNotFound
	}
	evs, err := m.store.Events(ctx, project, id)
	if err != nil {
		return Incident{}, err
	}
	if len(evs) == 0 {
		return Incident{}, ErrNotFound
	}
	cur, err := Fold(evs)
	if err != nil {
		return Incident{}, err
	}
	if expected != nil && *expected != cur.Version {
		return Incident{}, &ConflictError{Current: cur.Version}
	}
	t, payload, err := build(cur)
	if err != nil {
		return Incident{}, err
	}
	at := m.now()
	if at.Before(cur.UpdatedAt) {
		at = cur.UpdatedAt // a clock that went backwards must not put an event before the one it follows
	}
	ev, err := m.event(project, id, cur.Version+1, actor, t, payload, at)
	if err != nil {
		return Incident{}, err
	}
	if err := m.store.Append(ctx, ev); err != nil {
		return Incident{}, err
	}
	return Fold(append(evs, ev))
}

type UpdateInput struct {
	Title    *string
	Severity *Severity
	Summary  *string
	Assignee *string
}

// Update changes the fields given. Nothing to change, or a change that changes nothing, is refused: the timeline records what happened.
func (m *Manager) Update(ctx context.Context, project, id, actor string, expected *uint32, in UpdateInput) (Incident, error) {
	return m.change(ctx, project, id, actor, expected, func(cur Incident) (EventType, any, error) {
		var u Updated
		if in.Title != nil {
			t, err := text("title", *in.Title, MaxTitle, false)
			if err != nil {
				return "", nil, err
			}
			if t == "" {
				return "", nil, invalid("title cannot be empty")
			}
			if t != cur.Title {
				u.Title = &t
			}
		}
		if in.Severity != nil {
			if !ValidSeverity(*in.Severity) {
				return "", nil, invalid("severity must be critical, warning or info")
			}
			if *in.Severity != cur.Severity {
				u.Severity = in.Severity
			}
		}
		if in.Summary != nil {
			s, err := text("summary", *in.Summary, MaxSummary, true)
			if err != nil {
				return "", nil, err
			}
			if s != cur.Summary {
				u.Summary = &s
			}
		}
		if in.Assignee != nil {
			a := strings.TrimSpace(*in.Assignee)
			if a != "" && !assigneeRE.MatchString(a) {
				return "", nil, invalid("assignee must be a name of up to %d letters, digits, spaces and . _ @ + -", MaxAssignee)
			}
			if a != cur.Assignee {
				u.Assignee = &a
			}
		}
		if u.Title == nil && u.Severity == nil && u.Summary == nil && u.Assignee == nil {
			return "", nil, invalid("nothing to change")
		}
		return EvUpdated, u, nil
	})
}

type TransitionInput struct {
	To         State
	Note       string
	Resolution Resolution
}

// Transition moves the incident to another state if the lifecycle allows it. A refusal is a *TransitionError.
func (m *Manager) Transition(ctx context.Context, project, id, actor string, expected *uint32, in TransitionInput) (Incident, error) {
	return m.change(ctx, project, id, actor, expected, func(cur Incident) (EventType, any, error) {
		note, err := text("note", in.Note, MaxNote, true)
		if err != nil {
			return "", nil, err
		}
		if err := CheckMove(cur.State, in.To, note, in.Resolution); err != nil {
			return "", nil, err
		}
		return EvStateChanged, StateChanged{From: cur.State, To: in.To, Note: note, Resolution: in.Resolution}, nil
	})
}

// Comment adds a note to the timeline.
func (m *Manager) Comment(ctx context.Context, project, id, actor string, expected *uint32, body string) (Incident, error) {
	return m.change(ctx, project, id, actor, expected, func(Incident) (EventType, any, error) {
		t, err := text("text", body, MaxComment, true)
		if err != nil {
			return "", nil, err
		}
		if t == "" {
			return "", nil, invalid("text is required")
		}
		return EvCommented, Commented{Text: t}, nil
	})
}

// AddEvidence attaches a reference. The same reference twice is one: `added` is false and nothing is appended the second time.
func (m *Manager) AddEvidence(ctx context.Context, project, id, actor string, expected *uint32, ref EvidenceRef) (Incident, Evidence, bool, error) {
	ref = ref.Normalize()
	if err := ref.Validate(); err != nil {
		return Incident{}, Evidence{}, false, invalid("%v", err)
	}
	evID := EvidenceID(ref)
	added := false
	inc, err := m.change(ctx, project, id, actor, expected, func(cur Incident) (EventType, any, error) {
		if hasEvidence(cur.Evidence, evID) {
			return "", nil, errAlready
		}
		if len(cur.Evidence) >= MaxEvidence {
			return "", nil, invalid("an incident holds at most %d pieces of evidence", MaxEvidence)
		}
		added = true
		return EvEvidenceAdded, EvidenceAdded{ID: evID, Ref: ref}, nil
	})
	if errors.Is(err, errAlready) {
		cur, err := m.load(ctx, project, id)
		if err != nil {
			return Incident{}, Evidence{}, false, err
		}
		for _, e := range cur.Evidence {
			if e.ID == evID {
				return cur, e, false, nil
			}
		}
	}
	if err != nil {
		return Incident{}, Evidence{}, false, err
	}
	for _, e := range inc.Evidence {
		if e.ID == evID {
			return inc, e, added, nil
		}
	}
	return inc, Evidence{}, added, nil
}

var errAlready = errors.New("already attached")

// RemoveEvidence detaches a reference, with an optional reason. A reference that is not attached is ErrNotFound.
func (m *Manager) RemoveEvidence(ctx context.Context, project, id, actor string, expected *uint32, evidenceID, reason string) (Incident, error) {
	return m.change(ctx, project, id, actor, expected, func(cur Incident) (EventType, any, error) {
		if !hasEvidence(cur.Evidence, evidenceID) {
			return "", nil, ErrNotFound
		}
		r, err := text("reason", reason, MaxReason, false)
		if err != nil {
			return "", nil, err
		}
		return EvEvidenceRemoved, EvidenceRemoved{ID: evidenceID, Reason: r}, nil
	})
}
