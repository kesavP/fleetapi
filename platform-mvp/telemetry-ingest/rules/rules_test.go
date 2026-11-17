package rules

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

var bg = context.Background()

func TestTheDefaultsAreWhatTheDocsSayAndAreValid(t *testing.T) {
	d := Defaults()
	if len(d) != 3 {
		t.Fatalf("defaults = %v", d)
	}
	want := map[string]Settings{
		RightsizeMemory:  {Enabled: true, Percent: 50, Days: 7, Percentile: 95},
		RightsizeCPU:     {Enabled: true, Percent: 50, Days: 7, Percentile: 95},
		RightsizeStorage: {Enabled: false, Percent: 50, Days: 7, Percentile: 100},
	}
	for name, w := range want {
		if d[name] != w {
			t.Errorf("%s = %+v, want %+v", name, d[name], w)
		}
		if err := d[name].Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	d[RightsizeMemory] = Settings{} // a caller changing its copy does not change the defaults
	if Defaults()[RightsizeMemory].Percent != 50 {
		t.Fatal("Defaults hands out the one map")
	}
}

func TestSettingsAreCheckedAtTheirLimits(t *testing.T) {
	ok := Settings{Enabled: true, Percent: 50, Days: 7, Percentile: 95}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*Settings){
		"a percent of zero":                 func(s *Settings) { s.Percent = 0 },
		"a negative percent":                func(s *Settings) { s.Percent = -5 },
		"a percent over 100":                func(s *Settings) { s.Percent = 100.5 },
		"a percent that is not a number":    func(s *Settings) { s.Percent = nan() },
		"no days":                           func(s *Settings) { s.Days = 0 },
		"too many days":                     func(s *Settings) { s.Days = MaxDays + 1 },
		"a percentile under 50":             func(s *Settings) { s.Percentile = 49 },
		"a percentile over 100":             func(s *Settings) { s.Percentile = 101 },
		"a percentile that is not a number": func(s *Settings) { s.Percentile = nan() },
	} {
		s := ok
		mod(&s)
		if err := s.Validate(); err == nil || !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	for name, mod := range map[string]func(*Settings){
		"a percent of 100":   func(s *Settings) { s.Percent = 100 },
		"a percent of 1":     func(s *Settings) { s.Percent = 1 },
		"one day":            func(s *Settings) { s.Days = 1 },
		"the most days":      func(s *Settings) { s.Days = MaxDays },
		"a percentile of 50": func(s *Settings) { s.Percentile = 50 },
		"the maximum":        func(s *Settings) { s.Percentile = 100 },
	} {
		s := ok
		mod(&s)
		if err := s.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func nan() float64 { var z float64; return z / z }

// ---- the manager ----

func newManager(t *testing.T) (*Manager, *MemStore, *time.Time) {
	t.Helper()
	st := NewMemStore()
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	m := NewManager(st, nil)
	m.now = func() time.Time { return now }
	return m, st, &now
}

func TestWithNothingSavedEveryRuleIsItsDefault(t *testing.T) {
	m, _, _ := newManager(t)
	got, err := m.List(bg, "us")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Name != RightsizeMemory || got[1].Name != RightsizeCPU || got[2].Name != RightsizeStorage {
		t.Fatalf("rules = %+v: all three, in the order the docs list them", got)
	}
	for _, v := range got {
		if v.Saved || v.Version != 0 || v.UpdatedBy != "" || !v.UpdatedAt.IsZero() || v.Settings != Defaults()[v.Name] || v.Default != v.Settings {
			t.Errorf("%s = %+v", v.Name, v)
		}
	}
	eff, err := m.Effective(bg, "us")
	if err != nil || eff[RightsizeMemory] != Defaults()[RightsizeMemory] {
		t.Fatalf("effective = %+v, %v", eff, err)
	}
}

func TestASavedRuleOverridesTheDefaultForItsProjectOnlyAndKeepsWhoAndWhen(t *testing.T) {
	m, _, now := newManager(t)
	saved := Settings{Enabled: true, Percent: 40, Days: 14, Percentile: 99}
	v, err := m.Put(bg, "us", RightsizeMemory, saved, "sam", nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.Version != 1 || !v.Saved || v.Settings != saved || v.UpdatedBy != "sam" || !v.UpdatedAt.Equal(*now) || v.Default != Defaults()[RightsizeMemory] {
		t.Fatalf("put = %+v", v)
	}
	us, _ := m.Effective(bg, "us")
	eu, _ := m.Effective(bg, "eu")
	if us[RightsizeMemory] != saved || eu[RightsizeMemory] != Defaults()[RightsizeMemory] {
		t.Fatalf("us = %+v, eu = %+v: one project's rule must not reach another", us[RightsizeMemory], eu[RightsizeMemory])
	}
	if us[RightsizeCPU] != Defaults()[RightsizeCPU] {
		t.Fatal("saving one rule changed another")
	}
	list, _ := m.List(bg, "us")
	if !list[0].Saved || list[0].Version != 1 || list[1].Saved {
		t.Fatalf("list = %+v", list)
	}
}

func TestEachSaveIsANewVersionAndANewestOneWins(t *testing.T) {
	m, _, now := newManager(t)
	for i, pct := range []float64{40, 30, 20} {
		*now = now.Add(time.Minute)
		v, err := m.Put(bg, "us", RightsizeCPU, Settings{Enabled: true, Percent: pct, Days: 7, Percentile: 95}, "sam", nil)
		if err != nil || v.Version != uint32(i+1) {
			t.Fatalf("save %d: %+v, %v", i, v, err)
		}
	}
	eff, _ := m.Effective(bg, "us")
	if eff[RightsizeCPU].Percent != 20 {
		t.Fatalf("effective = %+v", eff[RightsizeCPU])
	}
}

func TestAChangeBasedOnAnOlderReadIsRefusedWithTheCurrentVersion(t *testing.T) {
	m, _, _ := newManager(t)
	s := Settings{Enabled: true, Percent: 40, Days: 7, Percentile: 95}
	zero, one := uint32(0), uint32(1)
	if _, err := m.Put(bg, "us", RightsizeMemory, s, "sam", &zero); err != nil {
		t.Fatalf("a first save that says it read the default: %v", err)
	}
	if _, err := m.Put(bg, "us", RightsizeMemory, s, "ana", &zero); err == nil {
		t.Fatal("a second save based on the default was accepted")
	} else {
		var ce *ConflictError
		if !errors.As(err, &ce) || ce.Current != 1 {
			t.Fatalf("err = %v", err)
		}
	}
	if _, err := m.Put(bg, "us", RightsizeMemory, s, "ana", &one); err != nil {
		t.Fatalf("a save based on the current version: %v", err)
	}
	if _, err := m.Put(bg, "us", RightsizeMemory, s, "ana", nil); err != nil {
		t.Fatalf("a save that does not say what it read is allowed: %v", err)
	}
}

func TestAnUnknownRuleAnInvalidSettingAndAMissingActorAreRefusedAndNothingIsSaved(t *testing.T) {
	m, st, _ := newManager(t)
	good := Settings{Enabled: true, Percent: 40, Days: 7, Percentile: 95}
	if _, err := m.Put(bg, "us", "rightsize.network", good, "sam", nil); !errors.Is(err, ErrUnknownRule) {
		t.Fatalf("an unknown rule: %v", err)
	}
	if _, err := m.Put(bg, "us", RightsizeMemory, Settings{Percent: 0, Days: 7, Percentile: 95}, "sam", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an invalid setting: %v", err)
	}
	for _, actor := range []string{"", "   ", strings.Repeat("a", 65)} {
		if _, err := m.Put(bg, "us", RightsizeMemory, good, actor, nil); !errors.Is(err, ErrInvalid) {
			t.Fatalf("actor %q: %v", actor, err)
		}
	}
	if n := st.Count(); n != 0 {
		t.Fatalf("%d rows were written by refused changes", n)
	}
}

func TestADefaultTheServiceWasStartedWithShowsUntilAProjectSavesItsOwn(t *testing.T) {
	st := NewMemStore()
	d := Defaults()
	d[RightsizeMemory] = Settings{Enabled: true, Percent: 30, Days: 10, Percentile: 90}
	m := NewManager(st, d)
	got, _ := m.List(bg, "us")
	if got[0].Settings.Percent != 30 || got[0].Default.Percent != 30 || got[0].Saved {
		t.Fatalf("list = %+v", got[0])
	}
	eff, _ := m.Effective(bg, "us")
	if eff[RightsizeMemory].Days != 10 {
		t.Fatalf("effective = %+v", eff[RightsizeMemory])
	}
}

func TestAStoreThatFailsIsAnErrorAndNeverTheDefault(t *testing.T) {
	m, st, _ := newManager(t)
	st.SetFailing(true)
	if _, err := m.Effective(bg, "us"); err == nil {
		t.Fatal("Effective hid a failure of the store behind the defaults: a rule someone tightened would silently loosen")
	}
	if _, err := m.List(bg, "us"); err == nil {
		t.Fatal("List hid a failure")
	}
	if _, err := m.Put(bg, "us", RightsizeMemory, Defaults()[RightsizeMemory], "sam", nil); err == nil {
		t.Fatal("Put hid a failure")
	}
}

func TestSavesAtTheSameTimeAreOneAfterTheOther(t *testing.T) {
	m, _, _ := newManager(t)
	var wg sync.WaitGroup
	versions := make([]uint32, 20)
	for i := range versions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := m.Put(bg, "us", RightsizeMemory, Settings{Enabled: true, Percent: float64(10 + i), Days: 7, Percentile: 95}, "sam", nil)
			if err != nil {
				t.Error(err)
			}
			versions[i] = v.Version
		}(i)
	}
	wg.Wait()
	seen := map[uint32]bool{}
	for _, v := range versions {
		seen[v] = true
	}
	if len(seen) != 20 {
		t.Fatalf("versions = %v: each save must get its own", versions)
	}
}

// ---- the defaults the service is started with ----

func TestParseDefaultsOverlaysWhatIsGivenOnTheBuiltIns(t *testing.T) {
	got, err := ParseDefaults(`{"rightsize.memory":{"percent":30},"rightsize.storage":{"enabled":true,"days":14}}`)
	if err != nil {
		t.Fatal(err)
	}
	if m := got[RightsizeMemory]; m.Percent != 30 || m.Days != 7 || m.Percentile != 95 || !m.Enabled {
		t.Fatalf("memory = %+v: only the percent was named", m)
	}
	if s := got[RightsizeStorage]; !s.Enabled || s.Days != 14 || s.Percent != 50 || s.Percentile != 100 {
		t.Fatalf("storage = %+v", s)
	}
	if c := got[RightsizeCPU]; c != Defaults()[RightsizeCPU] {
		t.Fatalf("cpu = %+v: not named, so unchanged", c)
	}
	for _, empty := range []string{"", "  ", "{}"} {
		if got, err := ParseDefaults(empty); err != nil || got[RightsizeMemory] != Defaults()[RightsizeMemory] {
			t.Errorf("%q: %v", empty, err)
		}
	}
}

func TestParseDefaultsRefusesWhatItCannotUse(t *testing.T) {
	for name, raw := range map[string]string{
		"not JSON":                  `{`,
		"an array":                  `[]`,
		"an unknown rule":           `{"rightsize.network":{"percent":30}}`,
		"an unknown setting":        `{"rightsize.memory":{"precent":30}}`,
		"a value of the wrong kind": `{"rightsize.memory":{"percent":"thirty"}}`,
		"a percent out of range":    `{"rightsize.memory":{"percent":150}}`,
		"days out of range":         `{"rightsize.cpu":{"days":365}}`,
		"something after the JSON":  `{} {}`,
	} {
		if _, err := ParseDefaults(raw); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
