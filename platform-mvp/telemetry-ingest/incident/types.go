package incident

import (
	"encoding/json"
	"time"
)

type State string

const (
	Open          State = "open"
	Acknowledged  State = "acknowledged"
	Investigating State = "investigating"
	Mitigated     State = "mitigated"
	Resolved      State = "resolved"
	Closed        State = "closed"
)

var States = []State{Open, Acknowledged, Investigating, Mitigated, Resolved, Closed}

type Severity string

const (
	Critical Severity = "critical"
	Warning  Severity = "warning"
	Info     Severity = "info"
)

type Resolution string

const (
	Fixed          Resolution = "fixed"
	Workaround     Resolution = "workaround"
	FalsePositive  Resolution = "false_positive"
	Duplicate      Resolution = "duplicate"
	NotReproducing Resolution = "not_reproducible"
)

// Source says how an incident came about: found by detection (and then created by whatever ran it), or opened by a person.
type Source string

const (
	Detected Source = "detected"
	Manual   Source = "manual"
)

type EventType string

const (
	EvCreated         EventType = "created"
	EvStateChanged    EventType = "state_changed"
	EvUpdated         EventType = "updated"
	EvCommented       EventType = "commented"
	EvEvidenceAdded   EventType = "evidence_added"
	EvEvidenceRemoved EventType = "evidence_removed"
)

// Event is one entry of an incident's timeline. Seq is 1 for the creation and goes up by one for each event after it; it is what orders
// the log and what a caller's expected version is compared with. Data is the payload of the event's type (below), as JSON.
type Event struct {
	Project    string          `json:"-"`
	IncidentID string          `json:"incident_id"`
	Seq        uint32          `json:"seq"`
	ID         string          `json:"id"`
	Time       time.Time       `json:"time"`
	Type       EventType       `json:"type"`
	Actor      string          `json:"actor"`
	Data       json.RawMessage `json:"data"`
	// DedupeKey is carried on the creation event only: it is what lets a caller create "the same" incident twice and get one.
	DedupeKey string `json:"-"`
}

// Payloads, one per event type.
type Created struct {
	Title     string    `json:"title"`
	App       string    `json:"app,omitempty"`
	Severity  Severity  `json:"severity"`
	Source    Source    `json:"source"`
	DedupeKey string    `json:"dedupe_key,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

type StateChanged struct {
	From       State      `json:"from"`
	To         State      `json:"to"`
	Note       string     `json:"note,omitempty"`
	Resolution Resolution `json:"resolution,omitempty"`
}

// Updated carries only what changed: a nil field is left alone, and a non-nil empty Assignee clears it.
type Updated struct {
	Title    *string   `json:"title,omitempty"`
	Severity *Severity `json:"severity,omitempty"`
	Summary  *string   `json:"summary,omitempty"`
	Assignee *string   `json:"assignee,omitempty"`
}

type Commented struct {
	Text string `json:"text"`
}

type EvidenceAdded struct {
	ID  string      `json:"id"`
	Ref EvidenceRef `json:"ref"`
}

type EvidenceRemoved struct {
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
}

// Evidence is a reference to something that supports the incident: a pointer to where to look, never a copy of the data.
type Evidence struct {
	ID      string      `json:"id"`
	Ref     EvidenceRef `json:"ref"`
	AddedBy string      `json:"added_by"`
	AddedAt time.Time   `json:"added_at"`
}

// Incident is the fold of an incident's events.
type Incident struct {
	ID         string     `json:"id"`
	Project    string     `json:"project"`
	Title      string     `json:"title"`
	App        string     `json:"app,omitempty"`
	Severity   Severity   `json:"severity"`
	State      State      `json:"state"`
	Source     Source     `json:"source"`
	DedupeKey  string     `json:"dedupe_key,omitempty"`
	Summary    string     `json:"summary,omitempty"`
	Assignee   string     `json:"assignee,omitempty"`
	Resolution Resolution `json:"resolution,omitempty"`

	StartedAt time.Time `json:"started_at"` // when the trouble began, as far as it is known
	CreatedAt time.Time `json:"created_at"` // when the incident was opened here
	UpdatedAt time.Time `json:"updated_at"`
	// AcknowledgedAt is when someone first took it on (acknowledged, or went straight to investigating); MitigatedAt when it was first
	// mitigated. ResolvedAt and ClosedAt are the latest, and are cleared when it is reopened.
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
	MitigatedAt    *time.Time `json:"mitigated_at,omitempty"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
	ClosedAt       *time.Time `json:"closed_at,omitempty"`
	Reopened       int        `json:"reopened"`

	// Version is the Seq of the last event. A caller that read version N and changes the incident with expected_version N is refused if
	// somebody else changed it meanwhile.
	Version            uint32     `json:"version"`
	Evidence           []Evidence `json:"evidence"`
	AllowedTransitions []State    `json:"allowed_transitions"`
}
