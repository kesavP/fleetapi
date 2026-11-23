// Package rules holds the settings that decide when the hub's detectors speak, so that a person can change them without a deploy.
//
// A rule is a name and a few settings. Each project has its own: a save is a new VERSION (the history is kept, with who made it and when) and the
// newest version is the one in force. What nothing has been saved for is the DEFAULT, which the service is started with (see ParseDefaults) and which
// is what the detector uses until someone saves a rule. Reading the rules takes a read key and changing one takes a manage key (see the server package).
package rules

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
)

// The rules there are. Each says when a pod counts as OVER-PROVISIONED: its use of a resource stayed under Percent of what it was given.
const (
	RightsizeMemory  = "rightsize.memory"
	RightsizeCPU     = "rightsize.cpu"
	RightsizeStorage = "rightsize.storage"
)

// Names is every rule, in the order they are listed.
var Names = []string{RightsizeMemory, RightsizeCPU, RightsizeStorage}

// MaxDays is the longest window a rule may name. The metrics are kept for 14 days by default, so a longer window sees less than it asks for, and
// the detector then leaves the workload alone (it does not judge what it has not watched for the whole window).
const MaxDays = 30

var (
	// ErrInvalid wraps what is wrong with an input; the message is for the caller.
	ErrInvalid = errors.New("invalid")
	// ErrUnknownRule is a name that is not one of Names.
	ErrUnknownRule = errors.New("no such rule")
)

// Settings is one rule.
type Settings struct {
	Enabled bool `json:"enabled"`
	// Percent: a pod is over-provisioned when its use stayed BELOW this share of its limit (above 0, at most 100).
	Percent float64 `json:"percent"`
	// Days is how far back to look (1 to MaxDays).
	Days int `json:"days"`
	// Percentile is which of the highest uses to judge by: 95 means "the use that was exceeded only 5% of the time"; 100 is the highest there was
	// (50 to 100).
	Percentile float64 `json:"percentile"`
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Validate says what is unusable in s, or nil.
func (s Settings) Validate() error {
	switch {
	case math.IsNaN(s.Percent) || !(s.Percent > 0 && s.Percent <= 100):
		return invalid("percent must be above 0 and at most 100")
	case s.Days < 1 || s.Days > MaxDays:
		return invalid("days must be from 1 to %d", MaxDays)
	case math.IsNaN(s.Percentile) || !(s.Percentile >= 50 && s.Percentile <= 100):
		return invalid("percentile must be from 50 to 100")
	}
	return nil
}

// Defaults are the settings of every rule that nothing has been saved for: memory and CPU on, at 50% over 7 days judged by the 95th percentile of use;
// storage off (a volume cannot usually be made smaller, and one that is filling up looks over-provisioned until it has filled), judged by its highest use.
func Defaults() map[string]Settings {
	return map[string]Settings{
		RightsizeMemory:  {Enabled: true, Percent: 50, Days: 7, Percentile: 95},
		RightsizeCPU:     {Enabled: true, Percent: 50, Days: 7, Percentile: 95},
		RightsizeStorage: {Enabled: false, Percent: 50, Days: 7, Percentile: 100},
	}
}

func known(name string) bool {
	for _, n := range Names {
		if n == name {
			return true
		}
	}
	return false
}

// partial is a rule as a person writes it for the defaults: only what differs.
type partial struct {
	Enabled    *bool    `json:"enabled"`
	Percent    *float64 `json:"percent"`
	Days       *int     `json:"days"`
	Percentile *float64 `json:"percentile"`
}

// ParseDefaults is Defaults with what raw (a JSON object of rule name to the settings that differ) changes. Empty raw is the built-in defaults. An
// unknown rule or setting, a value of the wrong kind, or one out of range is an error, so a typo cannot quietly leave a rule as it was.
func ParseDefaults(raw string) (map[string]Settings, error) {
	out := Defaults()
	if len(bytes.TrimSpace([]byte(raw))) == 0 {
		return out, nil
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	var given map[string]partial
	if err := dec.Decode(&given); err != nil {
		return nil, fmt.Errorf("rules defaults: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("rules defaults: something after the JSON object")
	}
	for name, p := range given {
		if !known(name) {
			return nil, fmt.Errorf("rules defaults: %q is not a rule (they are %v)", name, Names)
		}
		s := out[name]
		if p.Enabled != nil {
			s.Enabled = *p.Enabled
		}
		if p.Percent != nil {
			s.Percent = *p.Percent
		}
		if p.Days != nil {
			s.Days = *p.Days
		}
		if p.Percentile != nil {
			s.Percentile = *p.Percentile
		}
		if err := s.Validate(); err != nil {
			return nil, fmt.Errorf("rules defaults: %s: %w", name, err)
		}
		out[name] = s
	}
	return out, nil
}
