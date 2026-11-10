package collector

import (
	"fmt"
	"testing"
)

func c(key string, calls uint64, totalMs float64, rows uint64) Cumulative {
	return Cumulative{Key: key, QueryID: key, Database: "shop", User: "app", Query: "SELECT " + key, Calls: calls, TotalMs: totalMs, Rows: rows}
}

func snap(epoch string, stats ...Cumulative) Snapshot { return Snapshot{Stats: stats, Epoch: epoch} }

func byID(sts []Statement) map[string]Statement {
	m := map[string]Statement{}
	for _, s := range sts {
		m[s.QueryID] = s
	}
	return m
}

func TestTheFirstSnapshotIsOnlyABaseline(t *testing.T) {
	var d Differ
	out, info := d.Diff(snap("e", c("a", 1_000_000, 5e9, 9e9)))
	if len(out) != 0 || !info.Primed {
		t.Fatalf("days of work reported as one interval: %+v %+v", out, info)
	}
	out, info = d.Diff(snap("e", c("a", 1_000_010, 5e9+30, 9e9+10)))
	if info.Primed || len(out) != 1 || out[0].Calls != 10 || out[0].TotalMs != 30 || out[0].Rows != 10 {
		t.Fatalf("%+v %+v", out, info)
	}
}

func TestOnlyWhatChangedIsReported(t *testing.T) {
	var d Differ
	d.Diff(snap("e", c("a", 10, 100, 10), c("b", 5, 50, 5), c("c", 1, 1, 1)))
	out, info := d.Diff(snap("e", c("a", 10, 100, 10), c("b", 8, 80, 9), c("c", 1, 1, 1)))
	if len(out) != 1 || out[0].QueryID != "b" || out[0].Calls != 3 || out[0].TotalMs != 30 || out[0].Rows != 4 {
		t.Fatalf("%+v", out)
	}
	if info.Seen != 3 || info.Changed != 1 {
		t.Fatalf("%+v", info)
	}
	// nothing ran: nothing to say
	out, _ = d.Diff(snap("e", c("a", 10, 100, 10), c("b", 8, 80, 9), c("c", 1, 1, 1)))
	if len(out) != 0 {
		t.Fatalf("%+v", out)
	}
}

func TestANewStatementIsWhatItHasCounted(t *testing.T) {
	var d Differ
	d.Diff(snap("e", c("a", 10, 100, 10)))
	out, _ := d.Diff(snap("e", c("a", 10, 100, 10), c("new", 4, 40, 4)))
	if len(out) != 1 || out[0].QueryID != "new" || out[0].Calls != 4 || out[0].TotalMs != 40 {
		t.Fatalf("%+v", out)
	}
}

func TestACounterThatGoesDownStartedAgain(t *testing.T) {
	var d Differ
	d.Diff(snap("e", c("a", 1000, 9000, 1000), c("b", 10, 10, 10)))
	// "a" was evicted and came back: it has run 7 times since
	out, info := d.Diff(snap("e", c("a", 7, 70, 7), c("b", 12, 12, 12)))
	m := byID(out)
	if m["a"].Calls != 7 || m["a"].TotalMs != 70 || m["b"].Calls != 2 || info.Resets != 1 {
		t.Fatalf("%+v %+v", out, info)
	}
}

func TestAResetTokenStartsAStatementAgain(t *testing.T) {
	// the calls went up as well as the token changing: only the token says the counters restarted
	var d Differ
	a := c("a", 10, 100, 10)
	a.Reset = "t1"
	d.Diff(snap("e", a))
	a2 := c("a", 500, 900, 500)
	a2.Reset = "t2"
	out, info := d.Diff(snap("e", a2))
	if len(out) != 1 || out[0].Calls != 500 || info.Resets != 1 {
		t.Fatalf("%+v %+v", out, info)
	}
}

func TestAResetOfEverythingStartsEverythingAgain(t *testing.T) {
	var d Differ
	d.Diff(snap("before", c("a", 10, 100, 10), c("b", 20, 200, 20)))
	// after a reset, both have counted past where they were: by the numbers alone it would look like an ordinary interval
	out, info := d.Diff(snap("after", c("a", 50, 500, 50), c("b", 60, 600, 60)))
	m := byID(out)
	if m["a"].Calls != 50 || m["b"].Calls != 60 || info.Resets != 2 {
		t.Fatalf("%+v %+v", out, info)
	}
	// and the next interval is an ordinary one again
	out, _ = d.Diff(snap("after", c("a", 55, 550, 55), c("b", 60, 600, 60)))
	if len(out) != 1 || out[0].Calls != 5 {
		t.Fatalf("%+v", out)
	}
}

func TestTimeThatWouldGoNegativeIsZero(t *testing.T) {
	var d Differ
	d.Diff(snap("e", c("a", 10, 100.0000001, 10)))
	out, _ := d.Diff(snap("e", c("a", 11, 100, 11)))
	if len(out) != 1 || out[0].TotalMs != 0 {
		t.Fatalf("%+v", out)
	}
}

func TestExtraCountersAreDifferencedToo(t *testing.T) {
	var d Differ
	a := c("a", 10, 100, 10)
	a.Extra = map[string]float64{"shared_blks_read": 100, "shared_blks_hit": 1000, "wal_bytes": 5}
	d.Diff(snap("e", a))
	a2 := c("a", 12, 120, 12)
	a2.Extra = map[string]float64{"shared_blks_read": 130, "shared_blks_hit": 1000, "wal_bytes": 5, "temp_blks_written": 8}
	out, _ := d.Diff(snap("e", a2))
	got := out[0].Extra
	if len(got) != 2 || got["shared_blks_read"] != 30 || got["temp_blks_written"] != 8 {
		t.Fatalf("what did not change is left out, what is new is as it is: %v", got)
	}
}

func TestTheBusiestAreNamedAndTheRestAreOneRowThatKeepsTheTotals(t *testing.T) {
	d := Differ{Top: 3}
	var before, after []Cumulative
	var wantCalls uint64
	var wantMs float64
	for i := 0; i < 10; i++ {
		k := fmt.Sprintf("q%02d", i)
		before = append(before, c(k, 100, 1000, 100))
		calls := uint64(i + 1)
		after = append(after, c(k, 100+calls, 1000+float64(i)*10+5, 100+calls))
		wantCalls += calls
		wantMs += float64(i)*10 + 5
	}
	d.Diff(snap("e", before...))
	out, info := d.Diff(snap("e", after...))
	if len(out) != 4 || info.Folded != 7 {
		t.Fatalf("%d rows, %+v", len(out), info)
	}
	if out[0].QueryID != "q09" || out[1].QueryID != "q08" || out[2].QueryID != "q07" {
		t.Fatalf("not the busiest first: %v %v %v", out[0].QueryID, out[1].QueryID, out[2].QueryID)
	}
	other := out[3]
	if !other.Other || other.Query != "(other statements)" || other.QueryID != "" {
		t.Fatalf("%+v", other)
	}
	var calls uint64
	var ms float64
	for _, s := range out {
		calls += s.Calls
		ms += s.TotalMs
	}
	if calls != wantCalls || ms != wantMs {
		t.Fatalf("the totals changed by folding: %d %v, want %d %v", calls, ms, wantCalls, wantMs)
	}
}

func TestNothingIsFoldedWhenThereIsRoom(t *testing.T) {
	d := Differ{Top: 5}
	d.Diff(snap("e", c("a", 1, 1, 1), c("b", 1, 1, 1)))
	out, info := d.Diff(snap("e", c("a", 2, 2, 2), c("b", 2, 2, 2)))
	if len(out) != 2 || info.Folded != 0 || out[0].Other || out[1].Other {
		t.Fatalf("%+v", out)
	}
}

func TestTheOrderDoesNotDependOnTheOrderOfTheInput(t *testing.T) {
	mk := func(order []int) []Statement {
		var d Differ
		var b, a []Cumulative
		for _, i := range order {
			k := fmt.Sprintf("q%d", i)
			b = append(b, c(k, 1, 1, 1))
			a = append(a, c(k, 2, 11, 2)) // every statement the same: only the key can break the tie
		}
		d.Diff(snap("e", b...))
		out, _ := d.Diff(snap("e", a...))
		return out
	}
	x, y := mk([]int{1, 2, 3, 4}), mk([]int{4, 2, 1, 3})
	for i := range x {
		if x[i].QueryID != y[i].QueryID {
			t.Fatalf("%v vs %v", x, y)
		}
	}
}

func TestAStatementThatLeftAndCameBackStartsFromWhatItHas(t *testing.T) {
	var d Differ
	d.Diff(snap("e", c("a", 100, 100, 100), c("b", 1, 1, 1)))
	d.Diff(snap("e", c("b", 2, 2, 2))) // "a" is gone from the view
	out, _ := d.Diff(snap("e", c("a", 3, 3, 3), c("b", 2, 2, 2)))
	if len(out) != 1 || out[0].QueryID != "a" || out[0].Calls != 3 {
		t.Fatalf("it is new again: %+v", out)
	}
}

func TestTheDifferDoesNotShareMapsWithItsInput(t *testing.T) {
	var d Differ
	a := c("a", 1, 1, 1)
	a.Extra = map[string]float64{"x": 1}
	d.Diff(snap("e", a))
	a2 := c("a", 2, 2, 2)
	a2.Extra = map[string]float64{"x": 5}
	out, _ := d.Diff(snap("e", a2))
	out[0].Extra["x"] = 999
	if a2.Extra["x"] != 5 {
		t.Fatal("changing a result changed the engine's data")
	}
}

func TestTopIsCapped(t *testing.T) {
	d := Differ{Top: 100000}
	var b, a []Cumulative
	for i := 0; i < MaxStatements+50; i++ {
		k := fmt.Sprintf("q%04d", i)
		b = append(b, c(k, 1, 1, 1))
		a = append(a, c(k, 2, 2, 2))
	}
	d.Diff(snap("e", b...))
	out, info := d.Diff(snap("e", a...))
	if len(out) != MaxStatements+1 || info.Folded != 50 {
		t.Fatalf("%d rows, %+v", len(out), info)
	}
}
