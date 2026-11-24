// Package enroll is how a cluster gets its ingest key without anyone copying it: a person with a MANAGE key asks for an enrollment, which is a
// one-time token that lasts minutes; the cluster redeems it once, over the hub's ingest address, and is given a key of its own.
//
// What this keeps from the ways of doing it that were worse:
//   - The token is not the key. It does nothing on its own but be redeemed, once, within its time. It is shown to the person once, when it is made,
//     and what is stored is its hash.
//   - Each cluster gets its OWN ingest key, made when the token is redeemed and shown once to the cluster; what is stored is its hash. A cluster
//     can be cut off (its key revoked) without touching the others, and the key the projects file holds is not handed out at all.
//   - The hub never reaches into the cluster: the cluster comes to the hub with the token. Nothing here knows how to write to a cluster.
//
// Everything is an event in a log, as for incidents: created, redeemed, revoked. What an enrollment is now is the fold of its events.
//
// One replica: "check that it is still unused, then record that it was used" is made one step by this package doing one redemption at a time,
// which is correct for the single replica the service runs as and not for two.
package enroll

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	TokenPrefix = "enr_"
	KeyPrefix   = "ing_"
	DefaultTTL  = 15 * time.Minute
	MinTTL      = time.Minute
	MaxTTL      = 24 * time.Hour
	// MaxPending bounds the unredeemed tokens a project may have at once: a forgotten page must not be able to fill the log.
	MaxPending = 20
	// MaxList is how many enrollments a listing returns, newest first.
	MaxList = 200

	secretBytes = 32
)

var (
	ErrInvalid      = errors.New("invalid")
	ErrNotFound     = errors.New("not found")
	ErrTooMany      = errors.New("too many pending")
	ErrInvalidToken = errors.New("the enrollment token is invalid, expired or already used") // one answer for every reason: which it was is not the caller's to learn

	clusterRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$`)
	actorRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._@+-]{0,63}$`)
	idRE      = regexp.MustCompile(`^[0-9a-f]{12}$`)
	// a token is its prefix and 43 characters of base64url (32 bytes): anything else is not one, and is turned away without asking the store
	tokenRE = regexp.MustCompile(`^enr_[A-Za-z0-9_-]{43}$`)
)

// ValidID reports whether s looks like an enrollment id.
func ValidID(s string) bool { return idRE.MatchString(s) }

// ValidCluster reports whether s is a usable cluster name.
func ValidCluster(s string) bool { return clusterRE.MatchString(s) }

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

type EventType string

const (
	EvCreated  EventType = "created"
	EvRedeemed EventType = "redeemed"
	EvRevoked  EventType = "revoked"
)

// Event is one entry of the log.
type Event struct {
	Project string
	ID      string
	Time    time.Time
	Type    EventType
	Actor   string
	// created
	Cluster   string
	TokenHash string // hex of the SHA-256 of the token
	ExpiresAt time.Time
	// redeemed
	KeyHash string // hex of the SHA-256 of the key that was issued
}

type Status string

const (
	Pending  Status = "pending"  // made, not yet redeemed, not expired
	Expired  Status = "expired"  // made, never redeemed, and its time has passed
	Redeemed Status = "redeemed" // the cluster has its key, and the key works
	Revoked  Status = "revoked"  // cut off: a token that can no longer be redeemed, or a key that no longer works
)

// Enrollment is the fold of an enrollment's events.
type Enrollment struct {
	ID         string     `json:"id"`
	Project    string     `json:"project"`
	Cluster    string     `json:"cluster"`
	Status     Status     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	CreatedBy  string     `json:"created_by"`
	ExpiresAt  time.Time  `json:"expires_at"`
	RedeemedAt *time.Time `json:"redeemed_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	RevokedBy  string     `json:"revoked_by,omitempty"`
}

// Key is an ingest key that has been issued and not revoked: its hash, and whose it is.
type Key struct {
	Project, ID, Cluster string
	Hash                 [32]byte
}

// Store keeps the log.
type Store interface {
	Append(ctx context.Context, e Event) error
	// EventsOf is the events of one enrollment, oldest first.
	EventsOf(ctx context.Context, project, id string) ([]Event, error)
	// Project is every event of a project, oldest first.
	Project(ctx context.Context, project string) ([]Event, error)
	// FindByToken is the enrollment a token hash was made for, if any.
	FindByToken(ctx context.Context, tokenHash string) (project, id string, found bool, err error)
	// All is every event of every project, oldest first: what the issued keys are worked out from.
	All(ctx context.Context) ([]Event, error)
}

// Fold is what an enrollment is at `now`, from its events (oldest first). false when there is no creation in them.
func Fold(events []Event, now time.Time) (Enrollment, bool) {
	sorted := append([]Event(nil), events...)
	rank := map[EventType]int{EvCreated: 0, EvRedeemed: 1, EvRevoked: 2}
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].Time.Equal(sorted[j].Time) {
			return sorted[i].Time.Before(sorted[j].Time)
		}
		return rank[sorted[i].Type] < rank[sorted[j].Type] // in the same instant, a redemption cannot come before its creation
	})
	var e Enrollment
	made := false
	for _, ev := range sorted {
		switch ev.Type {
		case EvCreated:
			if made {
				continue
			}
			made = true
			e = Enrollment{ID: ev.ID, Project: ev.Project, Cluster: ev.Cluster, CreatedAt: ev.Time, CreatedBy: ev.Actor, ExpiresAt: ev.ExpiresAt, Status: Pending}
		case EvRedeemed:
			if made && e.RedeemedAt == nil && e.RevokedAt == nil {
				t := ev.Time
				e.RedeemedAt = &t
			}
		case EvRevoked:
			if made && e.RevokedAt == nil {
				t := ev.Time
				e.RevokedAt, e.RevokedBy = &t, ev.Actor
			}
		}
	}
	if !made {
		return Enrollment{}, false
	}
	switch {
	case e.RevokedAt != nil:
		e.Status = Revoked
	case e.RedeemedAt != nil:
		e.Status = Redeemed
	case !now.Before(e.ExpiresAt):
		e.Status = Expired
	}
	return e, true
}

// Hash is the SHA-256 of a token or key, which is all of either that is ever stored.
func Hash(secret string) [32]byte { return sha256.Sum256([]byte(secret)) }

func hashHex(secret string) string { h := Hash(secret); return hex.EncodeToString(h[:]) }

// Manager makes, redeems and revokes enrollments.
type Manager struct {
	store Store
	now   func() time.Time
	rand  io.Reader
	// OnKeys is called after the set of issued keys changed (one was issued, or a redeemed one revoked), so that whatever authenticates requests
	// can take them up at once instead of at its next refresh.
	OnKeys func()

	mu sync.Mutex
}

func NewManager(store Store) *Manager {
	return &Manager{store: store, now: time.Now, rand: rand.Reader}
}

func (m *Manager) secret(prefix string) (string, error) {
	b := make([]byte, secretBytes)
	if _, err := io.ReadFull(m.rand, b); err != nil {
		return "", fmt.Errorf("random: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

func (m *Manager) newID() (string, error) {
	b := make([]byte, 6)
	if _, err := io.ReadFull(m.rand, b); err != nil {
		return "", fmt.Errorf("random: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// ValidActor reports whether s may be recorded as who did something.
func ValidActor(s string) bool { return actorRE.MatchString(s) }

// Create makes an enrollment for a cluster and returns it with its token, which is not kept: this is the only time it can be read.
func (m *Manager) Create(ctx context.Context, project, actor, cluster string, ttl time.Duration) (Enrollment, string, error) {
	if !clusterRE.MatchString(cluster) {
		return Enrollment{}, "", invalid("cluster must be lower-case letters, digits and . _ -, starting and ending with a letter or digit, at most 64 characters")
	}
	if !actorRE.MatchString(actor) {
		return Enrollment{}, "", invalid("who is asking must be 1 to 64 letters, digits, spaces and . _ @ + -")
	}
	if ttl == 0 {
		ttl = DefaultTTL
	}
	if ttl < MinTTL || ttl > MaxTTL {
		return Enrollment{}, "", invalid("the token's lifetime must be between %v and %v", MinTTL, MaxTTL)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	events, err := m.store.Project(ctx, project)
	if err != nil {
		return Enrollment{}, "", err
	}
	pending := 0
	for _, list := range groupByID(events) {
		if e, ok := Fold(list, now); ok && e.Status == Pending {
			pending++
		}
	}
	if pending >= MaxPending {
		return Enrollment{}, "", fmt.Errorf("%w: a project may have %d unredeemed tokens at once; revoke some or wait for them to expire", ErrTooMany, MaxPending)
	}
	token, err := m.secret(TokenPrefix)
	if err != nil {
		return Enrollment{}, "", err
	}
	id, err := m.newID()
	if err != nil {
		return Enrollment{}, "", err
	}
	ev := Event{Project: project, ID: id, Time: now, Type: EvCreated, Actor: actor, Cluster: cluster, TokenHash: hashHex(token), ExpiresAt: now.Add(ttl)}
	if err := m.store.Append(ctx, ev); err != nil {
		return Enrollment{}, "", err
	}
	e, _ := Fold([]Event{ev}, now)
	return e, token, nil
}

func groupByID(events []Event) map[string][]Event {
	out := map[string][]Event{}
	for _, e := range events {
		out[e.ID] = append(out[e.ID], e)
	}
	return out
}

// Issued is what a cluster is given for a token.
type Issued struct {
	Project, Cluster, ID string
	Key                  string // shown once: only its hash is kept
}

// Redeem turns a token into a key. A token works once: the key is made and the redemption recorded before the key is returned, so a token whose
// answer was lost is spent, and the cluster enrolls again with a new one. Whatever is wrong with a token (never made, used, revoked, expired,
// not a token at all) is the same error.
func (m *Manager) Redeem(ctx context.Context, token string) (Issued, error) {
	if !tokenRE.MatchString(token) {
		return Issued{}, ErrInvalidToken // not asked of the store: garbage must not cost a query
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	project, id, found, err := m.store.FindByToken(ctx, hashHex(token))
	if err != nil {
		return Issued{}, err
	}
	if !found {
		return Issued{}, ErrInvalidToken
	}
	events, err := m.store.EventsOf(ctx, project, id)
	if err != nil {
		return Issued{}, err
	}
	e, ok := Fold(events, now)
	if !ok || e.Status != Pending {
		return Issued{}, ErrInvalidToken
	}
	key, err := m.secret(KeyPrefix)
	if err != nil {
		return Issued{}, err
	}
	if err := m.store.Append(ctx, Event{Project: project, ID: id, Time: now, Type: EvRedeemed, Actor: "cluster:" + e.Cluster, KeyHash: hashHex(key)}); err != nil {
		return Issued{}, err // nothing was spent: the token is as it was
	}
	if m.OnKeys != nil {
		m.OnKeys()
	}
	return Issued{Project: project, Cluster: e.Cluster, ID: id, Key: key}, nil
}

// List is a project's enrollments, newest first, at most MaxList.
func (m *Manager) List(ctx context.Context, project string) ([]Enrollment, error) {
	events, err := m.store.Project(ctx, project)
	if err != nil {
		return nil, err
	}
	now := m.now().UTC()
	out := []Enrollment{}
	for _, list := range groupByID(events) {
		if e, ok := Fold(list, now); ok {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > MaxList {
		out = out[:MaxList]
	}
	return out, nil
}

// Revoke cuts an enrollment off: a token that has not been redeemed can no longer be, and a key that was issued stops working. Revoking what is
// revoked already changes nothing.
func (m *Manager) Revoke(ctx context.Context, project, id, actor string) (Enrollment, error) {
	if !idRE.MatchString(id) {
		return Enrollment{}, ErrNotFound
	}
	if !actorRE.MatchString(actor) {
		return Enrollment{}, invalid("who is asking must be 1 to 64 letters, digits, spaces and . _ @ + -")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	events, err := m.store.EventsOf(ctx, project, id)
	if err != nil {
		return Enrollment{}, err
	}
	e, ok := Fold(events, now)
	if !ok {
		return Enrollment{}, ErrNotFound
	}
	if e.Status == Revoked {
		return e, nil
	}
	if err := m.store.Append(ctx, Event{Project: project, ID: id, Time: now, Type: EvRevoked, Actor: actor}); err != nil {
		return Enrollment{}, err
	}
	wasIssued := e.Status == Redeemed
	e, _ = Fold(append(events, Event{Project: project, ID: id, Time: now, Type: EvRevoked, Actor: actor}), now)
	if wasIssued && m.OnKeys != nil {
		m.OnKeys()
	}
	return e, nil
}

// Keys is every ingest key that has been issued and not revoked.
func (m *Manager) Keys(ctx context.Context) ([]Key, error) {
	events, err := m.store.All(ctx)
	if err != nil {
		return nil, err
	}
	var out []Key
	for id, list := range groupByID(events) {
		_ = id
		e, ok := Fold(list, time.Time{})
		if !ok || e.Status != Redeemed {
			continue
		}
		var hash string
		for _, ev := range list {
			if ev.Type == EvRedeemed {
				hash = ev.KeyHash
			}
		}
		raw, err := hex.DecodeString(strings.TrimSpace(hash))
		if err != nil || len(raw) != 32 {
			continue
		}
		k := Key{Project: e.Project, ID: e.ID, Cluster: e.Cluster}
		copy(k.Hash[:], raw)
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
