package incident

import (
	"fmt"
	"strings"
)

var transitions = map[State][]State{
	Open:          {Acknowledged, Investigating, Mitigated, Resolved},
	Acknowledged:  {Investigating, Mitigated, Resolved},
	Investigating: {Mitigated, Resolved},
	Mitigated:     {Investigating, Resolved},
	Resolved:      {Closed, Open},
	Closed:        {Open},
}

// Allowed lists the states an incident in `from` can move to (a copy: callers may keep it).
func Allowed(from State) []State { return append([]State(nil), transitions[from]...) }

// CanMove reports whether from -> to is a move the lifecycle allows.
func CanMove(from, to State) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

func ValidState(s State) bool {
	for _, x := range States {
		if x == s {
			return true
		}
	}
	return false
}

func ValidSeverity(s Severity) bool { return s == Critical || s == Warning || s == Info }

func ValidResolution(r Resolution) bool {
	switch r {
	case Fixed, Workaround, FalsePositive, Duplicate, NotReproducing:
		return true
	}
	return false
}

// TransitionError is a refused move, with what would have been allowed.
type TransitionError struct {
	From, To State
	Reason   string
	Allowed  []State
}

func (e *TransitionError) Error() string {
	names := make([]string, len(e.Allowed))
	for i, s := range e.Allowed {
		names[i] = string(s)
	}
	allowed := "none"
	if len(names) > 0 {
		allowed = strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s: an incident that is %s can move to: %s", e.Reason, e.From, allowed)
}

// CheckMove is the one place the rules of a move live: the states, and what a move into a state needs (a resolution to resolve; a note to
// reopen). It is used when a move is asked for and again when a log is folded, so a log that breaks the rules does not fold.
func CheckMove(from, to State, note string, res Resolution) error {
	deny := func(reason string) error {
		return &TransitionError{From: from, To: to, Reason: reason, Allowed: Allowed(from)}
	}
	switch {
	case !ValidState(to):
		return deny(fmt.Sprintf("%q is not a state", to))
	case from == to:
		return deny("the incident is already " + string(to))
	case !CanMove(from, to):
		return deny(fmt.Sprintf("cannot move from %s to %s", from, to))
	case to == Resolved && !ValidResolution(res):
		return deny("resolving needs a resolution: fixed, workaround, false_positive, duplicate or not_reproducible")
	case to != Resolved && res != "":
		return deny("a resolution is only given when resolving")
	case to == Open && strings.TrimSpace(note) == "":
		return deny("reopening needs a note saying why")
	}
	return nil
}
