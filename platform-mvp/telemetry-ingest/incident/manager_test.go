package incident

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type rig struct {
	m     *Manager
	s     *MemStore
	clock *time.Time
	n     int
}

func newRig() *rig {
	r := &rig{s: NewMemStore()}
	r.m = NewManager(r.s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := t0.Add(24 * time.Hour)
	r.clock = &now
	r.m.now = func() time.Time { return *r.clock }
	r.m.newID = func() string { r.n++; return fmt.Sprintf("id%06d", r.n) }
	return r
}
func (r *rig) tick(d time.Duration) { *r.clock = r.clock.Add(d) }

var ctx = context.Background()

func (r *rig) create(t *testing.T, in CreateInput) Incident {
	t.Helper()
	if in.Title == "" {
		in.Title = "frontend errors"
	}
	inc, _, err := r.m.Create(ctx, "gke", "ana", in)
	if err != nil {
		t.Fatal(err)
	}
	return inc
}

func TestCreate(t *testing.T) {
	r := newRig()
	inc, created, err := r.m.Create(ctx, "gke", "ana", CreateInput{Title: "  frontend errors ", App: "/k8s/shop/frontend", Severity: Critical, Summary: "5xx"})
	if err != nil || !created {
		t.Fatal(err, created)
	}
	if inc.ID != "id000001" || inc.State != Open || inc.Title != "frontend errors" || inc.Severity != Critical || inc.Source != Manual || inc.Version != 1 {
		t.Fatalf("%+v", inc)
	}
	if !inc.StartedAt.Equal(*r.clock) || !inc.CreatedAt.Equal(*r.clock) {
		t.Fatal("with no start given it began now")
	}
	got, err := r.m.Get(ctx, "gke", inc.ID)
	if err != nil || got.Title != "frontend errors" {
		t.Fatal(err, got)
	}
}

func TestCreateDefaults(t *testing.T) {
	r := newRig()
	inc := r.create(t, CreateInput{})
	if inc.Severity != Warning || inc.Source != Manual {
		t.Fatalf("%+v", inc)
	}
}

func TestCreateRefusesWhatIsInvalid(t *testing.T) {
	r := newRig()
	future := r.clock.Add(time.Hour)
	bad := map[string]CreateInput{
		"no title":          {Title: "  "},
		"a long title":      {Title: strings.Repeat("t", 201)},
		"a title with \\n":  {Title: "a\nb"},
		"a long summary":    {Title: "x", Summary: strings.Repeat("s", 4001)},
		"a bad severity":    {Title: "x", Severity: "meh"},
		"a bad source":      {Title: "x", Source: "robot"},
		"a bad dedupe key":  {Title: "x", DedupeKey: "has space"},
		"a long dedupe key": {Title: "x", DedupeKey: strings.Repeat("k", 129)},
		"a start in future": {Title: "x", StartedAt: future},
		"a start long ago":  {Title: "x", StartedAt: time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)},
		"a control in app":  {Title: "x", App: "a\x00b"},
		"invalid utf-8":     {Title: "x\xff"},
	}
	for name, in := range bad {
		if _, _, err := r.m.Create(ctx, "gke", "ana", in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(r.s.events) != 0 {
		t.Fatal("something was written for an invalid input")
	}
}

func TestCreateAllowsAStartInThePastAndSomeClockSkew(t *testing.T) {
	r := newRig()
	if _, _, err := r.m.Create(ctx, "gke", "ana", CreateInput{Title: "x", StartedAt: r.clock.Add(-6 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.m.Create(ctx, "gke", "ana", CreateInput{Title: "x", StartedAt: r.clock.Add(2 * time.Minute)}); err != nil {
		t.Fatal("a clock a little ahead is not a future incident:", err)
	}
}

func TestCreateWithADedupeKeyIsIdempotent(t *testing.T) {
	r := newRig()
	a, created, _ := r.m.Create(ctx, "gke", "ana", CreateInput{Title: "one", DedupeKey: "/k8s/shop/frontend|1800000000", Source: Detected})
	if !created {
		t.Fatal("first time should create")
	}
	b, created, err := r.m.Create(ctx, "gke", "bo", CreateInput{Title: "different", DedupeKey: "/k8s/shop/frontend|1800000000", Source: Detected})
	if err != nil || created || b.ID != a.ID || b.Title != "one" {
		t.Fatalf("%v %v %+v", err, created, b)
	}
	if len(r.s.events) != 1 {
		t.Fatal("a second record was made")
	}
	// the same key in another project is another incident
	c, created, _ := r.m.Create(ctx, "dc1", "ana", CreateInput{Title: "one", DedupeKey: "/k8s/shop/frontend|1800000000"})
	if !created || c.ID == a.ID {
		t.Fatal("projects share keys")
	}
	// the existing one comes back as it now is
	if _, err := r.m.Transition(ctx, "gke", a.ID, "ana", nil, TransitionInput{To: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	again, _, _ := r.m.Create(ctx, "gke", "ana", CreateInput{Title: "x", DedupeKey: "/k8s/shop/frontend|1800000000"})
	if again.State != Acknowledged {
		t.Fatalf("%+v", again)
	}
}

func TestTheLifecycle(t *testing.T) {
	r := newRig()
	inc := r.create(t, CreateInput{})
	step := func(in TransitionInput) Incident {
		t.Helper()
		r.tick(time.Minute)
		out, err := r.m.Transition(ctx, "gke", inc.ID, "ana", nil, in)
		if err != nil {
			t.Fatalf("%+v: %v", in, err)
		}
		return out
	}
	got := step(TransitionInput{To: Acknowledged})
	if got.State != Acknowledged || got.AcknowledgedAt == nil || got.Version != 2 {
		t.Fatalf("%+v", got)
	}
	step(TransitionInput{To: Investigating, Note: "looking at redis"})
	got = step(TransitionInput{To: Mitigated, Note: "rolled back"})
	if got.MitigatedAt == nil {
		t.Fatal("no mitigation time")
	}
	got = step(TransitionInput{To: Resolved, Resolution: Fixed, Note: "root cause: network"})
	if got.State != Resolved || got.Resolution != Fixed || got.ResolvedAt == nil || len(got.AllowedTransitions) != 2 {
		t.Fatalf("%+v", got)
	}
	got = step(TransitionInput{To: Closed})
	if got.State != Closed || got.ClosedAt == nil {
		t.Fatalf("%+v", got)
	}
	got = step(TransitionInput{To: Open, Note: "it is back"})
	if got.State != Open || got.Reopened != 1 || got.ResolvedAt != nil {
		t.Fatalf("%+v", got)
	}
	evs, err := r.m.Events(ctx, "gke", inc.ID, 0, 0)
	if err != nil || len(evs) != 7 {
		t.Fatal(err, len(evs))
	}
	for i, e := range evs {
		if e.Seq != uint32(i+1) || e.Actor != "ana" {
			t.Fatalf("%d: %+v", i, e)
		}
	}
}

func TestTransitionsAreRefusedWithWhatIsAllowed(t *testing.T) {
	r := newRig()
	inc := r.create(t, CreateInput{})
	_, err := r.m.Transition(ctx, "gke", inc.ID, "ana", nil, TransitionInput{To: Closed})
	var te *TransitionError
	if !errors.As(err, &te) || len(te.Allowed) != 4 {
		t.Fatalf("%v", err)
	}
	if _, err := r.m.Transition(ctx, "gke", inc.ID, "ana", nil, TransitionInput{To: Resolved}); !errors.As(err, &te) {
		t.Fatal("resolving without a resolution was allowed")
	}
	if _, err := r.m.Transition(ctx, "gke", inc.ID, "ana", nil, TransitionInput{To: "nope"}); !errors.As(err, &te) {
		t.Fatal(err)
	}
	if _, err := r.m.Transition(ctx, "gke", inc.ID, "ana", nil, TransitionInput{To: Acknowledged, Note: strings.Repeat("n", 1001)}); !errors.Is(err, ErrInvalid) {
		t.Fatal("a note of any length was allowed:", err)
	}
	got, _ := r.m.Get(ctx, "gke", inc.ID)
	if got.Version != 1 || got.State != Open {
		t.Fatalf("a refused move changed the incident: %+v", got)
	}
}

func TestExpectedVersion(t *testing.T) {
	r := newRig()
	inc := r.create(t, CreateInput{})
	v1, v9 := uint32(1), uint32(9)
	if _, err := r.m.Transition(ctx, "gke", inc.ID, "ana", &v9, TransitionInput{To: Acknowledged}); err == nil {
		t.Fatal("a wrong version was accepted")
	} else {
		var ce *ConflictError
		if !errors.As(err, &ce) || ce.Current != 1 {
			t.Fatal(err)
		}
	}
	if _, err := r.m.Transition(ctx, "gke", inc.ID, "ana", &v1, TransitionInput{To: Acknowledged}); err != nil {
		t.Fatal(err)
	}
	// the same request again is stale now
	var ce *ConflictError
	if _, err := r.m.Transition(ctx, "gke", inc.ID, "ana", &v1, TransitionInput{To: Investigating}); !errors.As(err, &ce) || ce.Current != 2 {
		t.Fatal(err)
	}
	// and the check applies to every kind of change
	if _, err := r.m.Comment(ctx, "gke", inc.ID, "ana", &v1, "hi"); !errors.As(err, &ce) {
		t.Fatal(err)
	}
	title := "t"
	if _, err := r.m.Update(ctx, "gke", inc.ID, "ana", &v1, UpdateInput{Title: &title}); !errors.As(err, &ce) {
		t.Fatal(err)
	}
	if _, _, _, err := r.m.AddEvidence(ctx, "gke", inc.ID, "ana", &v1, EvidenceRef{Kind: "application", Service: "x"}); !errors.As(err, &ce) {
		t.Fatal(err)
	}
	if _, err := r.m.RemoveEvidence(ctx, "gke", inc.ID, "ana", &v1, "ev_x", ""); !errors.As(err, &ce) {
		t.Fatal(err)
	}
}

// Two people acting on the same incident at once, each having read version 1: exactly one wins.
func TestRacingWritersOnOneVersion(t *testing.T) {
	r := newRig()
	inc := r.create(t, CreateInput{})
	const n = 40
	var won, conflicted atomic.Int32
	var wg sync.WaitGroup
	v := uint32(1)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := r.m.Comment(ctx, "gke", inc.ID, fmt.Sprintf("u%d", i), &v, "me first")
			var ce *ConflictError
			switch {
			case err == nil:
				won.Add(1)
			case errors.As(err, &ce):
				conflicted.Add(1)
			default:
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if won.Load() != 1 || conflicted.Load() != n-1 {
		t.Fatalf("won %d, conflicted %d", won.Load(), conflicted.Load())
	}
	got, err := r.m.Get(ctx, "gke", inc.ID)
	if err != nil || got.Version != 2 {
		t.Fatal(err, got.Version)
	}
}

// Writers that do not care about the version all succeed, each in its own place in the sequence: no event is lost or written twice.
func TestRacingWritersWithoutAVersion(t *testing.T) {
	r := newRig()
	inc := r.create(t, CreateInput{})
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := r.m.Comment(ctx, "gke", inc.ID, "ana", nil, fmt.Sprintf("note %d", i)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	evs, _ := r.m.Events(ctx, "gke", inc.ID, 0, 500)
	if len(evs) != n+1 {
		t.Fatalf("%d events", len(evs))
	}
	if _, err := r.m.Get(ctx, "gke", inc.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRacingTransitionsLeaveOneStateThatTheRulesAllow(t *testing.T) {
	r := newRig()
	inc := r.create(t, CreateInput{})
	var wg sync.WaitGroup
	var ok atomic.Int32
	for _, to := range []State{Acknowledged, Investigating, Mitigated, Resolved, Closed, Acknowledged, Closed, Investigating} {
		wg.Add(1)
		go func(to State) {
			defer wg.Done()
			in := TransitionInput{To: to}
			if to == Resolved {
				in.Resolution = Fixed
			}
			if _, err := r.m.Transition(ctx, "gke", inc.ID, "ana", nil, in); err == nil {
				ok.Add(1)
			}
		}(to)
	}
	wg.Wait()
	if _, err := r.m.Get(ctx, "gke", inc.ID); err != nil { // every event that was kept was legal when it was kept: it folds
		t.Fatal(err)
	}
	if ok.Load() < 1 {
		t.Fatal("none succeeded")
	}
}

func TestUpdate(t *testing.T) {
	r := newRig()
	inc := r.create(t, CreateInput{Title: "old", Severity: Warning})
	title, sev, sum, who := "new", Critical, "now with a summary", "bo"
	got, err := r.m.Update(ctx, "gke", inc.ID, "ana", nil, UpdateInput{Title: &title, Severity: &sev, Summary: &sum, Assignee: &who})
	if err != nil || got.Title != "new" || got.Severity != Critical || got.Summary != sum || got.Assignee != "bo" || got.Version != 2 {
		t.Fatalf("%v %+v", err, got)
	}
	empty := ""
	got, err = r.m.Update(ctx, "gke", inc.ID, "ana", nil, UpdateInput{Assignee: &empty})
	if err != nil || got.Assignee != "" {
		t.Fatalf("%v %+v", err, got)
	}
}

func TestUpdateRefusals(t *testing.T) {
	r := newRig()
	inc := r.create(t, CreateInput{Title: "t", Severity: Warning})
	same, blank, badSev, bad, long := "t", "  ", Severity("meh"), "a;b", strings.Repeat("a", 65)
	sameSev := Warning
	for name, in := range map[string]UpdateInput{
		"nothing":           {},
		"the same title":    {Title: &same},
		"the same severity": {Severity: &sameSev},
		"a blank title":     {Title: &blank},
		"a bad severity":    {Severity: &badSev},
		"a bad assignee":    {Assignee: &bad},
		"a long assignee":   {Assignee: &long},
	} {
		if _, err := r.m.Update(ctx, "gke", inc.ID, "ana", nil, in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	got, _ := r.m.Get(ctx, "gke", inc.ID)
	if got.Version != 1 {
		t.Fatal("a refused update was recorded")
	}
}

func TestComment(t *testing.T) {
	r := newRig()
	inc := r.create(t, CreateInput{})
	if _, err := r.m.Comment(ctx, "gke", inc.ID, "ana", nil, "  checked the logs\nnothing  "); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "   ", strings.Repeat("c", 4001), "a\x00b"} {
		if _, err := r.m.Comment(ctx, "gke", inc.ID, "ana", nil, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	evs, _ := r.m.Events(ctx, "gke", inc.ID, 1, 0)
	if len(evs) != 1 || evs[0].Type != EvCommented || !strings.Contains(string(evs[0].Data), "checked the logs") {
		t.Fatalf("%+v", evs)
	}
}

func TestEvidence(t *testing.T) {
	r := newRig()
	inc := r.create(t, CreateInput{})
	ref := EvidenceRef{Kind: "trace", TraceID: strings.Repeat("A", 32), Note: "the failing one"}
	got, ev, added, err := r.m.AddEvidence(ctx, "gke", inc.ID, "ana", nil, ref)
	if err != nil || !added || ev.ID != EvidenceID(ref) || len(got.Evidence) != 1 || got.Evidence[0].Ref.TraceID != strings.Repeat("a", 32) || ev.AddedBy != "ana" {
		t.Fatalf("%v %v %+v %+v", err, added, ev, got)
	}
	// the same again changes nothing, and says so
	got2, ev2, added, err := r.m.AddEvidence(ctx, "gke", inc.ID, "bo", nil, EvidenceRef{Kind: "TRACE", TraceID: strings.Repeat("a", 32), Note: "the failing one"})
	if err != nil || added || ev2.ID != ev.ID || got2.Version != got.Version || ev2.AddedBy != "ana" {
		t.Fatalf("%v %v %+v", err, added, got2)
	}
	// another kind, then one is removed
	_, ev3, _, err := r.m.AddEvidence(ctx, "gke", inc.ID, "ana", nil, EvidenceRef{Kind: "logs", Service: "/k8s/shop/cart", Severity: "error", From: &t0, To: &t1})
	if err != nil {
		t.Fatal(err)
	}
	got, err = r.m.RemoveEvidence(ctx, "gke", inc.ID, "ana", nil, ev.ID, "wrong trace")
	if err != nil || len(got.Evidence) != 1 || got.Evidence[0].ID != ev3.ID {
		t.Fatalf("%v %+v", err, got.Evidence)
	}
	if _, err := r.m.RemoveEvidence(ctx, "gke", inc.ID, "ana", nil, ev.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Fatal("removing it twice:", err)
	}
	// removed, it can be added again
	if _, _, added, err := r.m.AddEvidence(ctx, "gke", inc.ID, "ana", nil, ref); err != nil || !added {
		t.Fatal(err, added)
	}
}

func TestEvidenceRefusals(t *testing.T) {
	r := newRig()
	inc := r.create(t, CreateInput{})
	for name, ref := range map[string]EvidenceRef{
		"a bad kind": {Kind: "x"},
		"a bad url":  {Kind: "url", URL: "javascript:alert(1)"},
		"bad trace":  {Kind: "trace", TraceID: "z"},
	} {
		if _, _, _, err := r.m.AddEvidence(ctx, "gke", inc.ID, "ana", nil, ref); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	got, _ := r.m.Get(ctx, "gke", inc.ID)
	if got.Version != 1 {
		t.Fatal("a refused reference was recorded")
	}
	if _, err := r.m.RemoveEvidence(ctx, "gke", inc.ID, "ana", nil, "ev_nope", strings.Repeat("r", 501)); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestEvidenceIsCapped(t *testing.T) {
	r := newRig()
	inc := r.create(t, CreateInput{})
	for i := 0; i < MaxEvidence; i++ {
		if _, _, _, err := r.m.AddEvidence(ctx, "gke", inc.ID, "ana", nil, EvidenceRef{Kind: "application", Service: fmt.Sprintf("svc%d", i)}); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, _, _, err := r.m.AddEvidence(ctx, "gke", inc.ID, "ana", nil, EvidenceRef{Kind: "application", Service: "one-more"}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	// what is already there can still be "added" again: it is not a new piece
	if _, _, added, err := r.m.AddEvidence(ctx, "gke", inc.ID, "ana", nil, EvidenceRef{Kind: "application", Service: "svc0"}); err != nil || added {
		t.Fatal(err, added)
	}
}

func TestNotFoundAndProjects(t *testing.T) {
	r := newRig()
	inc := r.create(t, CreateInput{})
	for _, id := range []string{"nope0000", "", "../etc", "ID000001", strings.Repeat("a", 100), "id00000'"} {
		if _, err := r.m.Get(ctx, "gke", id); !errors.Is(err, ErrNotFound) {
			t.Errorf("%q: %v", id, err)
		}
		if _, err := r.m.Transition(ctx, "gke", id, "ana", nil, TransitionInput{To: Acknowledged}); !errors.Is(err, ErrNotFound) {
			t.Errorf("%q: %v", id, err)
		}
		if _, err := r.m.Events(ctx, "gke", id, 0, 0); !errors.Is(err, ErrNotFound) {
			t.Errorf("%q: %v", id, err)
		}
	}
	// an incident of one project does not exist in another
	if _, err := r.m.Get(ctx, "dc1", inc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := r.m.Comment(ctx, "dc1", inc.ID, "ana", nil, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatal("another project's incident could be changed:", err)
	}
}

func TestList(t *testing.T) {
	r := newRig()
	mk := func(title, app string, sev Severity) Incident {
		r.tick(time.Minute)
		return r.create(t, CreateInput{Title: title, App: app, Severity: sev})
	}
	a := mk("a", "/k8s/shop/frontend", Critical)
	b := mk("b", "/k8s/shop/cart", Warning)
	c := mk("c", "/k8s/shop/frontend", Warning)
	_, _ = r.m.Transition(ctx, "gke", b.ID, "ana", nil, TransitionInput{To: Resolved, Resolution: Fixed})
	r.m.Create(ctx, "dc1", "ana", CreateInput{Title: "other project"})

	ids := func(f Filter) string {
		list, skipped, err := r.m.List(ctx, "gke", f)
		if err != nil || skipped != 0 {
			t.Fatal(err, skipped)
		}
		var out []string
		for _, i := range list {
			out = append(out, i.Title)
		}
		return strings.Join(out, "")
	}
	if got := ids(Filter{}); got != "cba" {
		t.Errorf("newest first, this project only: %q", got)
	}
	if got := ids(Filter{States: []State{Open}}); got != "ca" {
		t.Errorf("open: %q", got)
	}
	if got := ids(Filter{States: []State{Resolved, Closed}}); got != "b" {
		t.Errorf("resolved or closed: %q", got)
	}
	if got := ids(Filter{App: "/k8s/shop/frontend"}); got != "ca" {
		t.Errorf("app: %q", got)
	}
	if got := ids(Filter{Severity: Critical}); got != "a" {
		t.Errorf("severity: %q", got)
	}
	if got := ids(Filter{Limit: 2}); got != "cb" {
		t.Errorf("limit: %q", got)
	}
	_, _ = a, c
}

func TestListSkipsARecordThatDoesNotFoldAndSaysSo(t *testing.T) {
	r := newRig()
	r.create(t, CreateInput{Title: "good"})
	// a record with a hole in its sequence, as damage might leave
	bad := r.create(t, CreateInput{Title: "bad"})
	r.s.mu.Lock()
	k := "gke|" + bad.ID
	r.s.events[k] = append(r.s.events[k], Event{Project: "gke", IncidentID: bad.ID, Seq: 5, Type: EvCommented, Data: []byte(`{"text":"x"}`), Time: t0})
	r.s.mu.Unlock()
	list, skipped, err := r.m.List(ctx, "gke", Filter{})
	if err != nil || skipped != 1 || len(list) != 1 || list[0].Title != "good" {
		t.Fatalf("%v %d %+v", err, skipped, list)
	}
	if _, err := r.m.Get(ctx, "gke", bad.ID); !errors.Is(err, ErrCorrupt) {
		t.Fatal("asking for the damaged one should say so:", err)
	}
	if _, err := r.m.Comment(ctx, "gke", bad.ID, "ana", nil, "x"); !errors.Is(err, ErrCorrupt) {
		t.Fatal("a damaged incident must not be written to:", err)
	}
}

func TestEvents(t *testing.T) {
	r := newRig()
	inc := r.create(t, CreateInput{})
	for i := 0; i < 5; i++ {
		_, _ = r.m.Comment(ctx, "gke", inc.ID, "ana", nil, fmt.Sprintf("c%d", i))
	}
	all, _ := r.m.Events(ctx, "gke", inc.ID, 0, 0)
	if len(all) != 6 || all[0].Type != EvCreated {
		t.Fatalf("%d", len(all))
	}
	page, _ := r.m.Events(ctx, "gke", inc.ID, 2, 2)
	if len(page) != 2 || page[0].Seq != 3 || page[1].Seq != 4 {
		t.Fatalf("%+v", page)
	}
	none, err := r.m.Events(ctx, "gke", inc.ID, 6, 0)
	if err != nil || none == nil || len(none) != 0 {
		t.Fatalf("an empty page is an empty list, not null: %v %v", err, none)
	}
}

func TestAFailingStoreIsAnErrorAndChangesNothing(t *testing.T) {
	r := newRig()
	inc := r.create(t, CreateInput{})
	r.s.SetFailing(true)
	if _, err := r.m.Transition(ctx, "gke", inc.ID, "ana", nil, TransitionInput{To: Acknowledged}); err == nil || errors.Is(err, ErrInvalid) || errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if _, _, err := r.m.Create(ctx, "gke", "ana", CreateInput{Title: "x"}); err == nil {
		t.Fatal("created with a failing store")
	}
	r.s.SetFailing(false)
	got, _ := r.m.Get(ctx, "gke", inc.ID)
	if got.Version != 1 || got.State != Open {
		t.Fatalf("%+v", got)
	}
}

func TestAnEventNeverPrecedesTheOneBeforeIt(t *testing.T) {
	r := newRig()
	inc := r.create(t, CreateInput{})
	*r.clock = r.clock.Add(-time.Hour) // the clock goes back
	got, err := r.m.Comment(ctx, "gke", inc.ID, "ana", nil, "x")
	if err != nil {
		t.Fatal(err)
	}
	if got.UpdatedAt.Before(got.CreatedAt) {
		t.Fatal("the timeline runs backwards")
	}
}

func TestActorAndIDShapes(t *testing.T) {
	for _, ok := range []string{"ana", "ana.b@example.com", "Ana B", "a", "svc-alerts_1"} {
		if !ValidActor(ok) {
			t.Errorf("%q", ok)
		}
	}
	for _, bad := range []string{"", " ", " ana", "a\nb", "a;b", strings.Repeat("a", 65), "<script>", "a\x00"} {
		if ValidActor(bad) {
			t.Errorf("%q", bad)
		}
	}
	if !ValidID("abcd1234") || ValidID("ABCD1234") || ValidID("abc") || ValidID("abcd123!") {
		t.Fatal("id shape")
	}
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		id := randomID()
		if !ValidID(id) || seen[id] {
			t.Fatalf("%q", id)
		}
		seen[id] = true
	}
}
