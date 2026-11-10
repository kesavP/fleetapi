package incident

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func ev(seq uint32, typ EventType, payload any, at time.Time) Event {
	data, _ := json.Marshal(payload)
	return Event{Project: "gke", IncidentID: "abcd1234", Seq: seq, ID: "e_" + strings.Repeat("x", 16), Time: at, Type: typ, Actor: "ana", Data: data}
}

func created(at time.Time) Event {
	return ev(1, EvCreated, Created{Title: "frontend errors", App: "/k8s/shop/frontend", Severity: Critical, Source: Detected, StartedAt: at.Add(-5 * time.Minute)}, at)
}

func TestFoldACreation(t *testing.T) {
	inc, err := Fold([]Event{created(t0)})
	if err != nil {
		t.Fatal(err)
	}
	if inc.State != Open || inc.Title != "frontend errors" || inc.Severity != Critical || inc.Version != 1 || inc.ID != "abcd1234" || inc.Project != "gke" {
		t.Fatalf("%+v", inc)
	}
	if !inc.CreatedAt.Equal(t0) || !inc.StartedAt.Equal(t0.Add(-5*time.Minute)) || !inc.UpdatedAt.Equal(t0) {
		t.Fatalf("%+v", inc)
	}
	if inc.Evidence == nil || len(inc.Evidence) != 0 {
		t.Fatal("an incident with no evidence has an empty list, not null")
	}
	if len(inc.AllowedTransitions) != 4 {
		t.Fatalf("%v", inc.AllowedTransitions)
	}
}

func TestFoldTheWholeLifecycleKeepsItsMilestones(t *testing.T) {
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	inc, err := Fold([]Event{
		created(at(0)),
		ev(2, EvStateChanged, StateChanged{From: Open, To: Acknowledged}, at(2)),
		ev(3, EvStateChanged, StateChanged{From: Acknowledged, To: Investigating}, at(3)),
		ev(4, EvStateChanged, StateChanged{From: Investigating, To: Mitigated, Note: "rolled back"}, at(10)),
		ev(5, EvStateChanged, StateChanged{From: Mitigated, To: Investigating, Note: "came back"}, at(12)),
		ev(6, EvStateChanged, StateChanged{From: Investigating, To: Resolved, Resolution: Fixed}, at(30)),
		ev(7, EvStateChanged, StateChanged{From: Resolved, To: Closed}, at(40)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if inc.State != Closed || inc.Version != 7 || inc.Resolution != Fixed {
		t.Fatalf("%+v", inc)
	}
	if inc.AcknowledgedAt == nil || !inc.AcknowledgedAt.Equal(at(2)) {
		t.Fatalf("acknowledged %v", inc.AcknowledgedAt)
	}
	if inc.MitigatedAt == nil || !inc.MitigatedAt.Equal(at(10)) {
		t.Fatalf("mitigated %v (the first time, not the last)", inc.MitigatedAt)
	}
	if inc.ResolvedAt == nil || !inc.ResolvedAt.Equal(at(30)) || inc.ClosedAt == nil || !inc.ClosedAt.Equal(at(40)) {
		t.Fatalf("%v %v", inc.ResolvedAt, inc.ClosedAt)
	}
	if !inc.UpdatedAt.Equal(at(40)) {
		t.Fatal(inc.UpdatedAt)
	}
}

func TestGoingStraightToInvestigatingOrResolvedStillCountsAsTakenOn(t *testing.T) {
	for _, to := range []State{Investigating, Mitigated, Resolved} {
		sc := StateChanged{From: Open, To: to}
		if to == Resolved {
			sc.Resolution = Fixed
		}
		inc, err := Fold([]Event{created(t0), ev(2, EvStateChanged, sc, t0.Add(time.Minute))})
		if err != nil {
			t.Fatal(err)
		}
		if inc.AcknowledgedAt == nil {
			t.Errorf("open -> %s left it unacknowledged", to)
		}
	}
}

func TestReopeningClearsWhatNoLongerHolds(t *testing.T) {
	inc, err := Fold([]Event{
		created(t0),
		ev(2, EvStateChanged, StateChanged{From: Open, To: Resolved, Resolution: FalsePositive}, t0.Add(time.Minute)),
		ev(3, EvStateChanged, StateChanged{From: Resolved, To: Closed}, t0.Add(2*time.Minute)),
		ev(4, EvStateChanged, StateChanged{From: Closed, To: Open, Note: "it is back"}, t0.Add(3*time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if inc.State != Open || inc.Reopened != 1 || inc.ResolvedAt != nil || inc.ClosedAt != nil || inc.Resolution != "" {
		t.Fatalf("%+v", inc)
	}
	if inc.AcknowledgedAt == nil {
		t.Fatal("it was taken on once; reopening does not undo that")
	}
}

func TestFoldAppliesUpdatesCommentsAndEvidence(t *testing.T) {
	trace := EvidenceRef{Kind: "trace", TraceID: strings.Repeat("a", 32)}
	title, sev, assignee := "new title", Info, "bo"
	inc, err := Fold([]Event{
		created(t0),
		ev(2, EvUpdated, Updated{Title: &title, Severity: &sev, Assignee: &assignee}, t0.Add(time.Minute)),
		ev(3, EvCommented, Commented{Text: "looking at it"}, t0.Add(2*time.Minute)),
		ev(4, EvEvidenceAdded, EvidenceAdded{ID: "ev_1", Ref: trace}, t0.Add(3*time.Minute)),
		ev(5, EvEvidenceAdded, EvidenceAdded{ID: "ev_1", Ref: trace}, t0.Add(4*time.Minute)),
		ev(6, EvEvidenceAdded, EvidenceAdded{ID: "ev_2", Ref: EvidenceRef{Kind: "application", Service: "x"}}, t0.Add(5*time.Minute)),
		ev(7, EvEvidenceRemoved, EvidenceRemoved{ID: "ev_1"}, t0.Add(6*time.Minute)),
		ev(8, EvUpdated, Updated{Assignee: new(string)}, t0.Add(7*time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if inc.Title != "new title" || inc.Severity != Info || inc.Assignee != "" {
		t.Fatalf("%+v", inc)
	}
	if len(inc.Evidence) != 1 || inc.Evidence[0].ID != "ev_2" || inc.Evidence[0].AddedBy != "ana" || !inc.Evidence[0].AddedAt.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("%+v", inc.Evidence)
	}
	if inc.Version != 8 || inc.State != Open {
		t.Fatalf("%+v", inc)
	}
}

func TestFoldDoesNotCareWhatOrderTheEventsComeIn(t *testing.T) {
	evs := []Event{
		created(t0),
		ev(2, EvStateChanged, StateChanged{From: Open, To: Acknowledged}, t0.Add(time.Minute)),
		ev(3, EvStateChanged, StateChanged{From: Acknowledged, To: Investigating}, t0.Add(2*time.Minute)),
	}
	inc, err := Fold([]Event{evs[2], evs[0], evs[1]})
	if err != nil || inc.State != Investigating {
		t.Fatalf("%v %+v", err, inc)
	}
}

func TestFoldRefusesALogThatBreaksTheRules(t *testing.T) {
	at := t0.Add(time.Minute)
	ok := created(t0)
	cases := map[string][]Event{
		"no events":              nil,
		"a gap":                  {ok, ev(3, EvCommented, Commented{Text: "x"}, at)},
		"a repeated sequence":    {ok, ev(2, EvCommented, Commented{Text: "x"}, at), ev(2, EvCommented, Commented{Text: "y"}, at)},
		"not starting at one":    {ev(2, EvCreated, Created{Title: "x", StartedAt: t0}, t0)},
		"a second creation":      {ok, ev(2, EvCreated, Created{Title: "x", StartedAt: t0}, at)},
		"first is not creation":  {ev(1, EvCommented, Commented{Text: "x"}, t0)},
		"another incident":       {ok, withID(ev(2, EvCommented, Commented{Text: "x"}, at), "zzzz9999")},
		"another project":        {ok, withProject(ev(2, EvCommented, Commented{Text: "x"}, at), "dc1")},
		"a move that is barred":  {ok, ev(2, EvStateChanged, StateChanged{From: Open, To: Closed}, at)},
		"a move from the wrong":  {ok, ev(2, EvStateChanged, StateChanged{From: Investigating, To: Resolved, Resolution: Fixed}, at)},
		"resolving without why":  {ok, ev(2, EvStateChanged, StateChanged{From: Open, To: Resolved}, at)},
		"reopening without why":  {ok, ev(2, EvStateChanged, StateChanged{From: Open, To: Resolved, Resolution: Fixed}, at), ev(3, EvStateChanged, StateChanged{From: Resolved, To: Open}, at)},
		"an unknown type":        {ok, ev(2, "exploded", Commented{Text: "x"}, at)},
		"a payload that is junk": {ok, {Project: "gke", IncidentID: "abcd1234", Seq: 2, Type: EvCommented, Data: json.RawMessage(`{"text":`), Time: at}},
	}
	for name, evs := range cases {
		_, err := Fold(evs)
		if !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func withID(e Event, id string) Event     { e.IncidentID = id; return e }
func withProject(e Event, p string) Event { e.Project = p; return e }

func TestFoldTimesAreUTC(t *testing.T) {
	zone := time.FixedZone("x", 5*3600)
	c := created(t0.In(zone))
	inc, err := Fold([]Event{c})
	if err != nil {
		t.Fatal(err)
	}
	if inc.CreatedAt.Location() != time.UTC || inc.StartedAt.Location() != time.UTC {
		t.Fatal("times are not UTC")
	}
}
