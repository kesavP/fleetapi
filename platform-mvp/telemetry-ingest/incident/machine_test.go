package incident

import (
	"errors"
	"strings"
	"testing"
)

// The whole lifecycle, written out: every pair of states, and whether the move is allowed.
func TestTransitionMatrix(t *testing.T) {
	allowed := map[State][]State{
		Open:          {Acknowledged, Investigating, Mitigated, Resolved},
		Acknowledged:  {Investigating, Mitigated, Resolved},
		Investigating: {Mitigated, Resolved},
		Mitigated:     {Investigating, Resolved},
		Resolved:      {Closed, Open},
		Closed:        {Open},
	}
	for _, from := range States {
		for _, to := range States {
			want := false
			for _, a := range allowed[from] {
				if a == to {
					want = true
				}
			}
			if got := CanMove(from, to); got != want {
				t.Errorf("%s -> %s: allowed %v, want %v", from, to, got, want)
			}
		}
	}
}

func TestEveryStateCanBeLeftExceptByTheRules(t *testing.T) {
	// no state is a dead end, and Closed is reached only from Resolved
	for _, s := range States {
		if len(Allowed(s)) == 0 {
			t.Errorf("%s has nowhere to go", s)
		}
	}
	for _, from := range States {
		if CanMove(from, Closed) != (from == Resolved) {
			t.Errorf("%s -> closed", from)
		}
	}
}

func TestAllowedIsACopy(t *testing.T) {
	a := Allowed(Open)
	a[0] = Closed
	if Allowed(Open)[0] == Closed {
		t.Fatal("changing the result changed the lifecycle")
	}
	if len(Allowed("nonsense")) != 0 {
		t.Fatal("an unknown state has no moves")
	}
}

func TestCheckMoveRules(t *testing.T) {
	cases := []struct {
		name     string
		from, to State
		note     string
		res      Resolution
		wantErr  string
	}{
		{"plain move", Open, Acknowledged, "", "", ""},
		{"a note is welcome", Open, Investigating, "looking", "", ""},
		{"resolve with a resolution", Investigating, Resolved, "", Fixed, ""},
		{"resolve without one", Investigating, Resolved, "", "", "needs a resolution"},
		{"resolve with a made-up one", Investigating, Resolved, "", "magic", "needs a resolution"},
		{"a resolution when not resolving", Open, Acknowledged, "", Fixed, "only given when resolving"},
		{"close", Resolved, Closed, "", "", ""},
		{"reopen with a note", Resolved, Open, "it came back", "", ""},
		{"reopen with a blank note", Closed, Open, "   ", "", "needs a note"},
		{"reopen with none", Closed, Open, "", "", "needs a note"},
		{"same state", Open, Open, "x", "", "already open"},
		{"a move the lifecycle lacks", Open, Closed, "", "", "cannot move from open to closed"},
		{"back to acknowledged", Investigating, Acknowledged, "", "", "cannot move"},
		{"to something that is no state", Open, "exploded", "", "", "is not a state"},
	}
	for _, c := range cases {
		err := CheckMove(c.from, c.to, c.note, c.res)
		if c.wantErr == "" {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		var te *TransitionError
		if !errors.As(err, &te) || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: %v, want an error with %q", c.name, err, c.wantErr)
			continue
		}
		if te.From != c.from || te.To != c.to {
			t.Errorf("%s: error names %s -> %s", c.name, te.From, te.To)
		}
	}
}

func TestRefusalSaysWhatWouldBeAllowed(t *testing.T) {
	err := CheckMove(Open, Closed, "", "")
	var te *TransitionError
	if !errors.As(err, &te) {
		t.Fatal(err)
	}
	if len(te.Allowed) != 4 || !strings.Contains(err.Error(), "acknowledged, investigating, mitigated, resolved") {
		t.Fatalf("%v %v", te.Allowed, err)
	}
}
