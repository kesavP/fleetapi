package enroll

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var bg = context.Background()

type rig struct {
	m     *Manager
	s     *MemStore
	clock *time.Time
	keys  *atomic.Int32
}

func newRig() *rig {
	r := &rig{s: NewMemStore(), keys: &atomic.Int32{}}
	r.m = NewManager(r.s)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	r.clock = &now
	r.m.now = func() time.Time { return *r.clock }
	r.m.OnKeys = func() { r.keys.Add(1) }
	return r
}
func (r *rig) tick(d time.Duration) { *r.clock = r.clock.Add(d) }

func (r *rig) create(t *testing.T, cluster string) (Enrollment, string) {
	t.Helper()
	e, tok, err := r.m.Create(bg, "gke", "ana", cluster, 0)
	if err != nil {
		t.Fatal(err)
	}
	return e, tok
}

func TestCreateMakesAPendingEnrollmentAndAToken(t *testing.T) {
	r := newRig()
	e, tok := r.create(t, "gke")
	if !tokenRE.MatchString(tok) || !strings.HasPrefix(tok, TokenPrefix) {
		t.Fatalf("token %q", tok)
	}
	if e.Status != Pending || e.Cluster != "gke" || e.Project != "gke" || e.CreatedBy != "ana" || !ValidID(e.ID) {
		t.Fatalf("%+v", e)
	}
	if got := e.ExpiresAt.Sub(e.CreatedAt); got != DefaultTTL {
		t.Fatalf("lifetime %v, want the default %v", got, DefaultTTL)
	}
	_, other := r.create(t, "gke")
	if other == tok {
		t.Fatal("two tokens are the same")
	}
}

// The token is shown once and what is kept is its hash: nothing in the log may be able to redeem it.
func TestTheTokenIsNeverStored(t *testing.T) {
	r := newRig()
	_, tok := r.create(t, "gke")
	iss, err := r.m.Redeem(bg, tok)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range r.s.Events() {
		for _, field := range []string{e.Project, e.ID, e.Actor, e.Cluster, e.TokenHash, e.KeyHash} {
			if strings.Contains(field, tok) || strings.Contains(field, iss.Key) || strings.Contains(field, strings.TrimPrefix(tok, TokenPrefix)) || strings.Contains(field, strings.TrimPrefix(iss.Key, KeyPrefix)) {
				t.Fatalf("a secret is in the log: %+v", e)
			}
		}
	}
	h := Hash(tok)
	if r.s.Events()[0].TokenHash != hex.EncodeToString(h[:]) {
		t.Fatal("the stored value is not the token's hash")
	}
}

func TestCreateRefusals(t *testing.T) {
	r := newRig()
	for name, c := range map[string]struct {
		cluster, actor string
		ttl            time.Duration
	}{
		"capitals":         {"GKE", "ana", 0},
		"a space":          {"my cluster", "ana", 0},
		"empty":            {"", "ana", 0},
		"too long":         {strings.Repeat("a", 65), "ana", 0},
		"a bad actor":      {"gke", "a;b", 0},
		"no actor":         {"gke", "", 0},
		"a short lifetime": {"gke", "ana", time.Second},
		"a long lifetime":  {"gke", "ana", 48 * time.Hour},
	} {
		if _, _, err := r.m.Create(bg, "gke", c.actor, c.cluster, c.ttl); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(r.s.Events()) != 0 {
		t.Fatal("something was stored for a refused request")
	}
	if _, _, err := r.m.Create(bg, "gke", "ana", "gke", MaxTTL); err != nil {
		t.Fatalf("the longest lifetime: %v", err)
	}
	if _, _, err := r.m.Create(bg, "gke", "ana", "gke", MinTTL); err != nil {
		t.Fatalf("the shortest lifetime: %v", err)
	}
}

func TestAProjectMayHaveOnlySomePendingTokens(t *testing.T) {
	r := newRig()
	for i := 0; i < MaxPending; i++ {
		r.create(t, "c")
	}
	if _, _, err := r.m.Create(bg, "gke", "ana", "c", 0); !errors.Is(err, ErrTooMany) {
		t.Fatalf("%v", err)
	}
	// another project is not held back
	if _, _, err := r.m.Create(bg, "eu", "ana", "c", 0); err != nil {
		t.Fatal(err)
	}
	// redeeming or letting them expire makes room
	r.tick(DefaultTTL + time.Second)
	if _, _, err := r.m.Create(bg, "gke", "ana", "c", 0); err != nil {
		t.Fatalf("after they expired: %v", err)
	}
}

func TestRedeemGivesTheClusterAKeyOfItsOwnOnce(t *testing.T) {
	r := newRig()
	e, tok := r.create(t, "gke")
	iss, err := r.m.Redeem(bg, tok)
	if err != nil {
		t.Fatal(err)
	}
	if iss.Project != "gke" || iss.Cluster != "gke" || iss.ID != e.ID || !strings.HasPrefix(iss.Key, KeyPrefix) || len(iss.Key) != len(KeyPrefix)+43 {
		t.Fatalf("%+v", iss)
	}
	if r.keys.Load() != 1 {
		t.Fatalf("the key was not announced to what authenticates: %d", r.keys.Load())
	}
	keys, err := r.m.Keys(bg)
	h := Hash(iss.Key)
	if err != nil || len(keys) != 1 || keys[0].Hash != h || keys[0].Project != "gke" || keys[0].Cluster != "gke" || keys[0].ID != e.ID {
		t.Fatalf("%+v %v", keys, err)
	}
	list, _ := r.m.List(bg, "gke")
	if list[0].Status != Redeemed || list[0].RedeemedAt == nil {
		t.Fatalf("%+v", list[0])
	}
	// once
	if _, err := r.m.Redeem(bg, tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("a second redemption: %v", err)
	}
	if keys, _ := r.m.Keys(bg); len(keys) != 1 {
		t.Fatalf("a second key was made: %d", len(keys))
	}
}

func TestATokenThatIsNotGoodIsTheSameErrorWhateverIsWrong(t *testing.T) {
	r := newRig()
	_, expired := r.create(t, "gke")
	r.tick(DefaultTTL)
	_, fresh := r.create(t, "gke")
	revokedE, revoked := r.create(t, "gke")
	if _, err := r.m.Revoke(bg, "gke", revokedE.ID, "ana"); err != nil {
		t.Fatal(err)
	}
	_, used := r.create(t, "gke")
	if _, err := r.m.Redeem(bg, used); err != nil {
		t.Fatal(err)
	}
	unknown := TokenPrefix + strings.Repeat("A", 43)
	for name, tok := range map[string]string{
		"expired": expired, "revoked": revoked, "used": used, "never made": unknown,
		"not a token": "hello", "empty": "", "a key": "ing_" + strings.Repeat("A", 43), "too short": TokenPrefix + "abc", "too long": fresh + "x",
		"a space": TokenPrefix + strings.Repeat("A", 42) + " ", "a newline": fresh + "\n",
	} {
		if _, err := r.m.Redeem(bg, tok); err != ErrInvalidToken {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := r.m.Redeem(bg, fresh); err != nil {
		t.Fatalf("the good one: %v", err)
	}
}

func TestGarbageDoesNotCostAQuery(t *testing.T) {
	r := newRig()
	for _, g := range []string{"", "x", "enr_short", "ing_" + strings.Repeat("A", 43), strings.Repeat("A", 47), "enr_" + strings.Repeat("!", 43)} {
		r.m.Redeem(bg, g)
	}
	if n := r.s.Lookups.Load(); n != 0 {
		t.Fatalf("%d lookups for what is not a token", n)
	}
	r.m.Redeem(bg, TokenPrefix+strings.Repeat("A", 43))
	if r.s.Lookups.Load() != 1 {
		t.Fatal("a well-formed token is looked up")
	}
}

// The token is the one thing that must work once. Fifty clusters presenting it at once get one key between them.
func TestOnlyOneOfManyAtOnceRedeemsAToken(t *testing.T) {
	r := newRig()
	_, tok := r.create(t, "gke")
	// the log is slow between "is it unused" and "it was used", as a database is: without one redemption at a time, they would all pass the check
	r.m.store = &slowEvents{MemStore: r.s}
	var wg sync.WaitGroup
	var won atomic.Int32
	keys := make(chan string, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if iss, err := r.m.Redeem(bg, tok); err == nil {
				won.Add(1)
				keys <- iss.Key
			} else if err != ErrInvalidToken {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	close(keys)
	if won.Load() != 1 {
		t.Fatalf("%d redeemed it", won.Load())
	}
	if k, _ := r.m.Keys(bg); len(k) != 1 {
		t.Fatalf("%d keys", len(k))
	}
}

func TestAFailedWriteSpendsNothing(t *testing.T) {
	r := newRig()
	_, tok := r.create(t, "gke")
	// the log fails after the token was found: the key must not be handed out, and the token must still be good
	fs := &failAppend{MemStore: r.s}
	r.m.store = fs
	fs.fail.Store(true)
	if iss, err := r.m.Redeem(bg, tok); err == nil || iss.Key != "" || errors.Is(err, ErrInvalidToken) {
		t.Fatalf("%+v %v", iss, err)
	}
	if r.keys.Load() != 0 {
		t.Fatal("a key was announced that was never recorded")
	}
	fs.fail.Store(false)
	if _, err := r.m.Redeem(bg, tok); err != nil {
		t.Fatalf("the token was spent by a failure: %v", err)
	}
}

type slowEvents struct{ *MemStore }

func (s *slowEvents) EventsOf(ctx context.Context, project, id string) ([]Event, error) {
	ev, err := s.MemStore.EventsOf(ctx, project, id)
	time.Sleep(5 * time.Millisecond)
	return ev, err
}

type failAppend struct {
	*MemStore
	fail atomic.Bool
}

func (f *failAppend) Append(ctx context.Context, e Event) error {
	if f.fail.Load() && e.Type == EvRedeemed {
		return errors.New("disk full")
	}
	return f.MemStore.Append(ctx, e)
}

func TestStoreErrorsAreNotTheInvalidTokenError(t *testing.T) {
	r := newRig()
	_, tok := r.create(t, "gke")
	r.s.SetFailing(true)
	if _, err := r.m.Redeem(bg, tok); err == nil || errors.Is(err, ErrInvalidToken) {
		t.Fatalf("a failing store told the cluster its token is bad: %v", err)
	}
}

func TestRevoke(t *testing.T) {
	r := newRig()
	// a token not yet redeemed can no longer be
	p, ptok := r.create(t, "a")
	got, err := r.m.Revoke(bg, "gke", p.ID, "ana")
	if err != nil || got.Status != Revoked || got.RevokedBy != "ana" || got.RevokedAt == nil {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := r.m.Redeem(bg, ptok); err != ErrInvalidToken {
		t.Fatalf("a revoked token was redeemed: %v", err)
	}
	if r.keys.Load() != 0 {
		t.Fatal("revoking a token that was never redeemed changes no key")
	}

	// a key that was issued stops being one
	e, tok := r.create(t, "b")
	iss, _ := r.m.Redeem(bg, tok)
	before := r.keys.Load()
	if k, _ := r.m.Keys(bg); len(k) != 1 || k[0].Hash != Hash(iss.Key) {
		t.Fatalf("%+v", k)
	}
	if got, err := r.m.Revoke(bg, "gke", e.ID, "ana"); err != nil || got.Status != Revoked {
		t.Fatalf("%+v %v", got, err)
	}
	if k, _ := r.m.Keys(bg); len(k) != 0 {
		t.Fatalf("a revoked key is still issued: %+v", k)
	}
	if r.keys.Load() != before+1 {
		t.Fatal("revoking an issued key was not announced")
	}
	// again: nothing changes
	n := len(r.s.Events())
	if got, err := r.m.Revoke(bg, "gke", e.ID, "bo"); err != nil || got.Status != Revoked || got.RevokedBy != "ana" || len(r.s.Events()) != n {
		t.Fatalf("a second revoke: %+v %v", got, err)
	}
}

func TestRevokeOfWhatIsNotYoursOrNotThere(t *testing.T) {
	r := newRig()
	e, _ := r.create(t, "gke")
	for name, c := range map[string]struct{ project, id string }{
		"another project": {"eu", e.ID}, "unknown id": {"gke", "000000000000"}, "not an id": {"gke", "../x"}, "empty": {"gke", ""},
	} {
		if _, err := r.m.Revoke(bg, c.project, c.id, "ana"); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := r.m.Revoke(bg, "gke", e.ID, "a;b"); !errors.Is(err, ErrInvalid) {
		t.Errorf("a bad actor: %v", err)
	}
	if got, _ := r.m.List(bg, "gke"); got[0].Status != Pending {
		t.Fatalf("a refused revoke changed it: %+v", got[0])
	}
}

func TestAnExpiredEnrollmentIsExpiredAndItsKeyIsNot(t *testing.T) {
	r := newRig()
	_, old := r.create(t, "a")
	_, tok := r.create(t, "b")
	r.m.Redeem(bg, tok)
	r.tick(MaxTTL * 2)
	list, _ := r.m.List(bg, "gke")
	byCluster := map[string]Status{}
	for _, e := range list {
		byCluster[e.Cluster] = e.Status
	}
	if byCluster["a"] != Expired || byCluster["b"] != Redeemed {
		t.Fatalf("%v: a key that was issued does not expire with its token", byCluster)
	}
	if _, err := r.m.Redeem(bg, old); err != ErrInvalidToken {
		t.Fatal(err)
	}
	if k, _ := r.m.Keys(bg); len(k) != 1 {
		t.Fatalf("keys: %d", len(k))
	}
}

func TestTheLastSecondOfATokenIsStillGoodAndTheNextIsNot(t *testing.T) {
	r := newRig()
	_, tok := r.create(t, "gke")
	r.tick(DefaultTTL - time.Nanosecond)
	if _, err := r.m.Redeem(bg, tok); err != nil {
		t.Fatalf("just before it expires: %v", err)
	}
	_, tok = r.create(t, "gke")
	r.tick(DefaultTTL)
	if _, err := r.m.Redeem(bg, tok); err != ErrInvalidToken {
		t.Fatalf("at the moment it expires: %v", err)
	}
}

func TestListIsNewestFirstAndOnlyTheProjects(t *testing.T) {
	r := newRig()
	r.create(t, "one")
	r.tick(time.Minute)
	r.create(t, "two")
	r.tick(time.Minute)
	r.m.Create(bg, "eu", "ana", "other", 0)
	r.create(t, "three")
	got, _ := r.m.List(bg, "gke")
	if len(got) != 3 || got[0].Cluster != "three" || got[1].Cluster != "two" || got[2].Cluster != "one" {
		t.Fatalf("%+v", got)
	}
	if eu, _ := r.m.List(bg, "eu"); len(eu) != 1 || eu[0].Cluster != "other" {
		t.Fatalf("%+v", eu)
	}
	if none, err := r.m.List(bg, "nobody"); err != nil || none == nil || len(none) != 0 {
		t.Fatalf("%v %v", none, err)
	}
}

func TestEachClusterGetsItsOwnKeyAndASecondEnrollmentDoesNotReplaceTheFirst(t *testing.T) {
	r := newRig()
	_, t1 := r.create(t, "gke")
	_, t2 := r.create(t, "gke") // the same cluster enrolled again, as when a key is rotated
	_, t3 := r.create(t, "aks")
	a, _ := r.m.Redeem(bg, t1)
	b, _ := r.m.Redeem(bg, t2)
	c, _ := r.m.Redeem(bg, t3)
	if a.Key == b.Key || b.Key == c.Key || a.Key == c.Key {
		t.Fatal("two clusters got the same key")
	}
	if k, _ := r.m.Keys(bg); len(k) != 3 {
		t.Fatalf("%d keys: the old key stays good until it is revoked", len(k))
	}
}

func TestFoldIgnoresWhatDoesNotBelong(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	if _, ok := Fold(nil, now); ok {
		t.Fatal("nothing is not an enrollment")
	}
	if _, ok := Fold([]Event{{Type: EvRedeemed}}, now); ok {
		t.Fatal("a redemption with no creation is not an enrollment")
	}
	created := Event{Project: "p", ID: "a1", Time: now, Type: EvCreated, Cluster: "c", ExpiresAt: now.Add(time.Hour)}
	// a redemption after a revocation changes nothing
	e, _ := Fold([]Event{created, {Type: EvRevoked, Time: now.Add(time.Minute), Actor: "ana"}, {Type: EvRedeemed, Time: now.Add(2 * time.Minute)}}, now.Add(3*time.Minute))
	if e.Status != Revoked || e.RedeemedAt != nil {
		t.Fatalf("%+v", e)
	}
	// events handed over out of order are folded in time order
	e, _ = Fold([]Event{{Type: EvRedeemed, Time: now.Add(time.Minute)}, created}, now.Add(2*time.Minute))
	if e.Status != Redeemed {
		t.Fatalf("%+v", e)
	}
	// a second creation of the same id is ignored
	e, _ = Fold([]Event{created, {Type: EvCreated, Time: now.Add(time.Second), Cluster: "evil", ExpiresAt: now.Add(100 * time.Hour)}}, now)
	if e.Cluster != "c" {
		t.Fatalf("%+v", e)
	}
}

func TestKeysSkipsARecordWhoseHashIsDamaged(t *testing.T) {
	r := newRig()
	_, tok := r.create(t, "gke")
	r.m.Redeem(bg, tok)
	r.s.mu.Lock()
	for i := range r.s.events {
		if r.s.events[i].Type == EvRedeemed {
			r.s.events[i].KeyHash = "not hex"
		}
	}
	r.s.mu.Unlock()
	if k, err := r.m.Keys(bg); err != nil || len(k) != 0 {
		t.Fatalf("%+v %v", k, err)
	}
}

func TestEntropy(t *testing.T) {
	r := newRig()
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		s, err := r.m.secret(TokenPrefix)
		if err != nil || seen[s] {
			t.Fatalf("%v %v", err, seen[s])
		}
		seen[s] = true
	}
	r.m.rand = bytes.NewReader([]byte("short"))
	if _, err := r.m.secret(KeyPrefix); err == nil {
		t.Fatal("a source of randomness that ran out was accepted")
	}
}
