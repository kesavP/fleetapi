package incident

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// ErrCorrupt means a log of events does not describe a valid incident: a gap in the sequence, an event of another incident, a move the
// lifecycle does not allow. It should not happen (events are only appended through the Manager, which checks them), so it is reported
// as a fault of the record and never papered over.
var ErrCorrupt = errors.New("the incident's events are inconsistent")

func decode(ev Event, into any) error {
	dec := json.NewDecoder(bytes.NewReader(ev.Data))
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("%w: event %d (%s): %v", ErrCorrupt, ev.Seq, ev.Type, err)
	}
	return nil
}

// Fold rebuilds an incident from its events, in any order. The events must be 1..n with no gap, the first must be the creation, and every
// state change must be one the lifecycle allows from where the incident then was.
func Fold(events []Event) (Incident, error) {
	if len(events) == 0 {
		return Incident{}, fmt.Errorf("%w: no events", ErrCorrupt)
	}
	evs := append([]Event(nil), events...)
	sort.Slice(evs, func(i, j int) bool { return evs[i].Seq < evs[j].Seq })
	var inc Incident
	for i, ev := range evs {
		if ev.Seq != uint32(i+1) {
			return Incident{}, fmt.Errorf("%w: expected event %d, found %d", ErrCorrupt, i+1, ev.Seq)
		}
		if i == 0 && ev.Type != EvCreated {
			return Incident{}, fmt.Errorf("%w: the first event is %s, not the creation", ErrCorrupt, ev.Type)
		}
		if i > 0 && (ev.IncidentID != inc.ID || ev.Project != inc.Project) {
			return Incident{}, fmt.Errorf("%w: event %d belongs to another incident", ErrCorrupt, ev.Seq)
		}
		at := ev.Time.UTC()
		switch ev.Type {
		case EvCreated:
			if i != 0 {
				return Incident{}, fmt.Errorf("%w: a second creation at event %d", ErrCorrupt, ev.Seq)
			}
			var c Created
			if err := decode(ev, &c); err != nil {
				return Incident{}, err
			}
			inc = Incident{
				ID: ev.IncidentID, Project: ev.Project, Title: c.Title, App: c.App, Severity: c.Severity, State: Open, Source: c.Source,
				DedupeKey: c.DedupeKey, Summary: c.Summary, StartedAt: c.StartedAt.UTC(), CreatedAt: at, Evidence: []Evidence{},
			}
		case EvStateChanged:
			var s StateChanged
			if err := decode(ev, &s); err != nil {
				return Incident{}, err
			}
			if s.From != inc.State {
				return Incident{}, fmt.Errorf("%w: event %d moves from %s but the incident was %s", ErrCorrupt, ev.Seq, s.From, inc.State)
			}
			if err := CheckMove(s.From, s.To, s.Note, s.Resolution); err != nil {
				return Incident{}, fmt.Errorf("%w: event %d: %v", ErrCorrupt, ev.Seq, err)
			}
			apply(&inc, s, at)
		case EvUpdated:
			var u Updated
			if err := decode(ev, &u); err != nil {
				return Incident{}, err
			}
			if u.Title != nil {
				inc.Title = *u.Title
			}
			if u.Severity != nil {
				inc.Severity = *u.Severity
			}
			if u.Summary != nil {
				inc.Summary = *u.Summary
			}
			if u.Assignee != nil {
				inc.Assignee = *u.Assignee
			}
		case EvCommented:
			var c Commented
			if err := decode(ev, &c); err != nil {
				return Incident{}, err
			}
		case EvEvidenceAdded:
			var a EvidenceAdded
			if err := decode(ev, &a); err != nil {
				return Incident{}, err
			}
			if !hasEvidence(inc.Evidence, a.ID) {
				inc.Evidence = append(inc.Evidence, Evidence{ID: a.ID, Ref: a.Ref, AddedBy: ev.Actor, AddedAt: at})
			}
		case EvEvidenceRemoved:
			var r EvidenceRemoved
			if err := decode(ev, &r); err != nil {
				return Incident{}, err
			}
			kept := inc.Evidence[:0:0]
			for _, e := range inc.Evidence {
				if e.ID != r.ID {
					kept = append(kept, e)
				}
			}
			inc.Evidence = kept
		default:
			return Incident{}, fmt.Errorf("%w: event %d has the unknown type %q", ErrCorrupt, ev.Seq, ev.Type)
		}
		inc.UpdatedAt = at
		inc.Version = ev.Seq
	}
	inc.AllowedTransitions = Allowed(inc.State)
	return inc, nil
}

func hasEvidence(list []Evidence, id string) bool {
	for _, e := range list {
		if e.ID == id {
			return true
		}
	}
	return false
}

// apply moves the incident and keeps the milestones: when it was first taken on and first mitigated, and when it was last resolved and
// closed (which a reopening clears, as the incident is open again).
func apply(inc *Incident, s StateChanged, at time.Time) {
	t := at
	inc.State = s.To
	switch s.To {
	case Acknowledged, Investigating:
		if inc.AcknowledgedAt == nil {
			inc.AcknowledgedAt = &t
		}
	case Mitigated:
		if inc.AcknowledgedAt == nil {
			inc.AcknowledgedAt = &t
		}
		if inc.MitigatedAt == nil {
			inc.MitigatedAt = &t
		}
	case Resolved:
		if inc.AcknowledgedAt == nil {
			inc.AcknowledgedAt = &t
		}
		inc.ResolvedAt = &t
		inc.Resolution = s.Resolution
	case Closed:
		inc.ClosedAt = &t
	case Open:
		inc.ResolvedAt, inc.ClosedAt, inc.Resolution = nil, nil, ""
		inc.Reopened++
	}
}
