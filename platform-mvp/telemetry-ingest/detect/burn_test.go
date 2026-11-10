package detect

import (
	"math"
	"testing"
)

const stepSec = 60

// flat is n steps of v requests per second.
func flat(n int, v float64) []float64 {
	a := make([]float64, n)
	for i := range a {
		a[i] = v
	}
	return a
}

// burst puts v into a[from:to).
func burst(a []float64, from, to int, v float64) []float64 {
	for i := from; i < to; i++ {
		a[i] = v
	}
	return a
}

func grid(n int) Grid { return Grid{Start: 1_700_000_000, Step: stepSec, N: n} }

// app serves 10 requests a second for n minutes; errors and slow are added by the test.
func app(n int) App {
	return App{ID: "/k8s/shop/web", Label: "shop/web", Total: flat(n, 10), Errors: make([]float64, n)}
}

func TestHealthyApplicationHasNoIncident(t *testing.T) {
	if got := FindBreaches(app(240), grid(240), Defaults()); len(got) != 0 {
		t.Fatalf("incidents = %+v", got)
	}
}

func TestBurstBecomesOneIncidentWithTheExactSteps(t *testing.T) {
	a := app(240)
	burst(a.Errors, 120, 160, 5) // half the requests fail for 40 minutes
	got := FindBreaches(a, grid(240), Defaults())
	if len(got) != 1 {
		t.Fatalf("incidents = %+v", got)
	}
	b := got[0]
	if b.First != 120 || b.Last != 159 {
		t.Fatalf("steps = %d..%d, want the first and last burning step 120..159", b.First, b.Last)
	}
	g := grid(240)
	if b.Start.Unix() != g.Start+120*60 || b.End.Unix() != g.Start+160*60 {
		t.Fatalf("time = %v .. %v", b.Start, b.End)
	}
	if b.Open {
		t.Fatal("it ended well before the last step")
	}
	if len(b.Kinds) != 1 || b.Kinds[0] != Availability {
		t.Fatalf("kinds = %v", b.Kinds)
	}
	if math.Abs(b.PeakBurn[Availability]-500) > 1e-6 { // 50% bad over a 0.1% budget
		t.Fatalf("peak burn = %v, want 500", b.PeakBurn[Availability])
	}
	if math.Abs(b.Compliance[Availability]-0.5) > 1e-9 {
		t.Fatalf("compliance = %v, want 0.5", b.Compliance[Availability])
	}
	if math.Abs(b.Requests-40*60*10) > 1e-6 {
		t.Fatalf("requests = %v", b.Requests)
	}
}

func TestIncidentStillBurningAtTheEndOfTheDataIsOpen(t *testing.T) {
	a := app(240)
	burst(a.Errors, 200, 240, 5)
	got := FindBreaches(a, grid(240), Defaults())
	if len(got) != 1 || !got[0].Open {
		t.Fatalf("incidents = %+v, want one open", got)
	}
}

func TestOneBadMinuteIsNoise(t *testing.T) {
	a := app(240)
	burst(a.Errors, 100, 101, 10) // every request of one minute fails
	if got := FindBreaches(a, grid(240), Defaults()); len(got) != 0 {
		t.Fatalf("incidents = %+v, a single minute is shorter than MinSeconds", got)
	}
}

func TestTooFewRequestsToJudge(t *testing.T) {
	a := App{ID: "x", Label: "x", Total: flat(240, 0.01), Errors: flat(240, 0.01)} // everything fails, but 0.6 requests a minute
	if got := FindBreaches(a, grid(240), Defaults()); len(got) != 0 {
		t.Fatalf("incidents = %+v, a share of three requests means nothing", got)
	}
}

func TestBreachesCloseTogetherJoinAndFarApartDoNot(t *testing.T) {
	near := app(240)
	burst(near.Errors, 100, 106, 5)
	burst(near.Errors, 108, 114, 5) // two minutes later
	if got := FindBreaches(near, grid(240), Defaults()); len(got) != 1 {
		t.Fatalf("near: %+v, want one incident", got)
	}
	far := app(300)
	burst(far.Errors, 60, 70, 5)
	burst(far.Errors, 200, 210, 5)
	got := FindBreaches(far, grid(300), Defaults())
	if len(got) != 2 {
		t.Fatalf("far: %+v, want two incidents", got)
	}
	if !got[0].Start.Before(got[1].Start) {
		t.Fatal("oldest first")
	}
}

func TestLatencyIsJudgedOnlyWhenThereIsAHistogram(t *testing.T) {
	a := app(240)
	burst(a.Errors, 0, 0, 0)
	a.Slow = burst(make([]float64, 240), 120, 160, 5)
	got := FindBreaches(a, grid(240), Defaults())
	if len(got) != 1 || len(got[0].Kinds) != 1 || got[0].Kinds[0] != LatencyKind {
		t.Fatalf("incidents = %+v, want one latency incident", got)
	}
	a.Slow = nil
	if got := FindBreaches(a, grid(240), Defaults()); len(got) != 0 {
		t.Fatalf("without a histogram nothing can be said about slow requests: %+v", got)
	}
}

func TestAvailabilityAndLatencyThatOverlapAreOneIncident(t *testing.T) {
	a := app(240)
	burst(a.Errors, 120, 150, 5)
	a.Slow = burst(make([]float64, 240), 130, 160, 5)
	got := FindBreaches(a, grid(240), Defaults())
	if len(got) != 1 {
		t.Fatalf("incidents = %+v", got)
	}
	if len(got[0].Kinds) != 2 || got[0].First != 120 || got[0].Last != 159 {
		t.Fatalf("incident = %+v, want both kinds over 120..159", got[0])
	}
}

func TestConfigControlsTheThreshold(t *testing.T) {
	a := app(240)
	burst(a.Errors, 100, 160, 0.2) // 2% fail for an hour: a burn rate of 20 in both windows
	if got := FindBreaches(a, grid(240), Defaults()); len(got) != 1 {
		t.Fatalf("at 14.4: %+v", got)
	}
	c := Defaults()
	c.BurnThreshold = 30
	if got := FindBreaches(a, grid(240), c); len(got) != 0 {
		t.Fatalf("at 30: %+v, a burn of 20 is below it", got)
	}
}

func TestEmptyGridFindsNothing(t *testing.T) {
	if got := FindBreaches(app(0), Grid{Start: 1, Step: 60}, Defaults()); got != nil {
		t.Fatalf("got %+v", got)
	}
}

func TestBurnRate(t *testing.T) {
	total, bad := flat(10, 10), flat(10, 1) // 10% bad
	if got := BurnRate(total, bad, 9, 5, 60, 0.99, 0); math.Abs(got-10) > 1e-9 {
		t.Fatalf("10%% bad over a 1%% budget = %v, want 10", got)
	}
	if got := BurnRate(total, bad, 9, 5, 60, 0.99, 1e9); got != 0 {
		t.Fatalf("too few requests must give 0, got %v", got)
	}
	if got := BurnRate(make([]float64, 5), make([]float64, 5), 4, 5, 60, 0.99, 0); got != 0 {
		t.Fatalf("no requests must give 0 (not NaN), got %v", got)
	}
	if got := BurnRate(total, bad, 1, 5, 60, 0.99, 0); math.Abs(got-10) > 1e-9 {
		t.Fatalf("a window reaching before the start uses the steps there are, got %v", got)
	}
}

func TestConfigValidate(t *testing.T) {
	if err := Defaults().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Config){
		"availability 1":    func(c *Config) { c.Availability = 1 },
		"availability 0":    func(c *Config) { c.Availability = 0 },
		"latency above 1":   func(c *Config) { c.Latency = 1.5 },
		"no latency limit":  func(c *Config) { c.LatencySeconds = 0 },
		"no burn":           func(c *Config) { c.BurnThreshold = 0 },
		"short above long":  func(c *Config) { c.ShortSeconds = 7200 },
		"negative requests": func(c *Config) { c.MinRequests = -1 },
		"NaN objective":     func(c *Config) { c.Availability = math.NaN() },
	} {
		c := Defaults()
		mut(&c)
		if c.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTwoBurstsHalfAnHourApartAreTwoIncidents(t *testing.T) {
	// The long window stays hot for most of an hour after a burst, so only the short window ends the first incident before the second.
	a := app(240)
	burst(a.Errors, 60, 70, 5)
	burst(a.Errors, 100, 110, 5)
	got := FindBreaches(a, grid(240), Defaults())
	if len(got) != 2 {
		t.Fatalf("incidents = %+v, want two", got)
	}
	if got[0].Last >= got[1].First {
		t.Fatalf("they overlap: %+v", got)
	}
}

func TestABriefGapBetweenBurstsIsBridged(t *testing.T) {
	// Bursts 100..106 and 113..119: the windows go cool for two steps between them, which the join (five minutes) bridges.
	a := app(240)
	burst(a.Errors, 100, 107, 5)
	burst(a.Errors, 113, 120, 5)
	got := FindBreaches(a, grid(240), Defaults())
	if len(got) != 1 || got[0].First != 100 || got[0].Last != 119 {
		t.Fatalf("incidents = %+v, want one over 100..119", got)
	}
	c := Defaults()
	c.JoinSeconds = 0
	if got := FindBreaches(a, grid(240), c); len(got) != 2 {
		t.Fatalf("with no join: %+v, want two", got)
	}
}

func TestAvailabilityAndLatencyJustApartAreOneIncident(t *testing.T) {
	a := app(240)
	burst(a.Errors, 100, 110, 5)
	a.Slow = burst(make([]float64, 240), 115, 125, 5) // starts four steps after the availability burst ends
	got := FindBreaches(a, grid(240), Defaults())
	if len(got) != 1 || len(got[0].Kinds) != 2 || got[0].First != 100 || got[0].Last != 124 {
		t.Fatalf("incidents = %+v, want one with both kinds over 100..124", got)
	}
}
