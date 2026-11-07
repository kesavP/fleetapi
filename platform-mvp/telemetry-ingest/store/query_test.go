package store

import (
	"math"
	"testing"
)

func TestRate(t *testing.T) {
	// 0 -> 10s: +20 = 2/s. 10 -> 20s: 30 down to 5 is a counter reset, so the
	// increase is 5 = 0.5/s. The second reading at 20s has dt = 0 and is skipped.
	// 20s -> 30s is measured from the reading just before it, (20s, 9): +6 = 0.6/s.
	got := Rate([]Point{{0, 10}, {10_000, 30}, {20_000, 5}, {20_000, 9}, {30_000, 15}})
	want := []Point{{10_000, 2}, {20_000, 0.5}, {30_000, 0.6}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i].T != want[i].T || math.Abs(got[i].V-want[i].V) > 1e-9 {
			t.Fatalf("point %d: got %v, want %v", i, got[i], want[i])
		}
	}
	if Rate([]Point{{0, 1}}) != nil {
		t.Fatal("a single reading has no rate")
	}
}

func TestFiniteDropsNaNAndInf(t *testing.T) {
	got := finite([]Point{{1, 1}, {2, math.NaN()}, {3, math.Inf(1)}, {4, 4}})
	if len(got) != 2 || got[0].T != 1 || got[1].T != 4 {
		t.Fatalf("%v", got)
	}
}

func TestInList(t *testing.T) {
	if got := inList([]uint64{1, 18446744073709551615}); got != "1,18446744073709551615" {
		t.Fatal(got)
	}
	if got := inList(nil); got != "" {
		t.Fatal(got)
	}
}

func TestRateFromZero(t *testing.T) {
	// a counter born with value 3 within a 15 s step, then 3 -> 5: 3/15 then 2/15 per second
	got := RateFromZero([]Point{{15_000, 3}, {30_000, 5}}, 15_000)
	want := []Point{{15_000, 3.0 / 15}, {30_000, 2.0 / 15}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i].T != want[i].T || math.Abs(got[i].V-want[i].V) > 1e-9 {
			t.Fatalf("point %d: got %v, want %v", i, got[i], want[i])
		}
	}
	// a single reading is no longer dropped: this is the case it exists for
	if one := RateFromZero([]Point{{15_000, 1}}, 15_000); len(one) != 1 || math.Abs(one[0].V-1.0/15) > 1e-9 {
		t.Fatalf("a lone first reading: %v", one)
	}
	// born at zero stays zero; a later drop is still a reset; no step or no points falls back to Rate
	if z := RateFromZero([]Point{{15_000, 0}, {30_000, 0}}, 15_000); len(z) != 2 || z[0].V != 0 || z[1].V != 0 {
		t.Fatalf("zero: %v", z)
	}
	if r := RateFromZero([]Point{{15_000, 4}, {30_000, 1}}, 15_000); len(r) != 2 || math.Abs(r[1].V-1.0/15) > 1e-9 {
		t.Fatalf("reset after birth: %v", r)
	}
	if RateFromZero(nil, 15_000) != nil || RateFromZero([]Point{{1, 1}}, 0) != nil {
		t.Fatal("no points, or no step, must fall back to Rate")
	}
}
