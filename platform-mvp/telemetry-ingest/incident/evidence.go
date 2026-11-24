package incident

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Kinds of evidence. Each points at something to look at, in the terms the read API already has; none carries the data itself, so an
// incident does not copy telemetry out of its retention and a reference never goes stale in a way that misleads: it just finds less.
const (
	KindTrace       = "trace"       // one trace: trace_id
	KindLogs        = "logs"        // a log search: service and/or query, severity, and a time range
	KindProfile     = "profile"     // a profile: service, profile_type, and a time range
	KindMetric      = "metric"      // a metric: metric name, label matchers, and a time range
	KindApplication = "application" // an application page: service
	KindURL         = "url"         // somewhere else (a dashboard, a ticket, a runbook): an http(s) address
)

// EvidenceRef is the pointer. Which fields apply depends on Kind; Validate says which are needed and refuses what does not belong.
type EvidenceRef struct {
	Kind        string            `json:"kind"`
	TraceID     string            `json:"trace_id,omitempty"`
	Service     string            `json:"service,omitempty"`
	Severity    string            `json:"severity,omitempty"`
	Query       string            `json:"query,omitempty"`
	ProfileType string            `json:"profile_type,omitempty"`
	Metric      string            `json:"metric,omitempty"`
	Match       map[string]string `json:"match,omitempty"`
	From        *time.Time        `json:"from,omitempty"`
	To          *time.Time        `json:"to,omitempty"`
	URL         string            `json:"url,omitempty"`
	Note        string            `json:"note,omitempty"`
}

const (
	MaxEvidence    = 100
	maxMatchers    = 20
	maxURL         = 2000
	maxNote        = 500
	maxQuery       = 500
	maxService     = 300
	maxMatchValue  = 300
	maxMatchKeyLen = 100
)

var (
	traceIDRE     = regexp.MustCompile(`^[0-9a-f]{32}$`)
	metricRE      = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]{0,199}$`)
	labelRE       = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	profileTypeRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}:[a-z][a-z0-9_]{0,40}$`)
	severityRE    = regexp.MustCompile(`^(?i)(trace|debug|info|warn|warning|error|fatal)$`)
)

// hasControl reports whether s has a control character other than tab and newline (when newlines are allowed).
func hasControl(s string, newlines bool) bool {
	for _, r := range s {
		if r == '\t' || (newlines && r == '\n') {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// Normalize trims the fields a person types, so that the same reference typed twice is the same reference.
func (r EvidenceRef) Normalize() EvidenceRef {
	r.Kind = strings.ToLower(strings.TrimSpace(r.Kind))
	r.TraceID = strings.ToLower(strings.TrimSpace(r.TraceID))
	r.Service = strings.TrimSpace(r.Service)
	r.Severity = strings.ToLower(strings.TrimSpace(r.Severity))
	r.Query = strings.TrimSpace(r.Query)
	r.ProfileType = strings.TrimSpace(r.ProfileType)
	r.Metric = strings.TrimSpace(r.Metric)
	r.URL = strings.TrimSpace(r.URL)
	r.Note = strings.TrimSpace(r.Note)
	if len(r.Match) == 0 {
		r.Match = nil
	}
	if r.From != nil {
		t := r.From.UTC()
		r.From = &t
	}
	if r.To != nil {
		t := r.To.UTC()
		r.To = &t
	}
	return r
}

// Validate says what is wrong with a reference, or nil. A field that does not belong to the kind is refused, not ignored: a reference that
// quietly drops half of what was sent would point somewhere other than where the sender meant.
func (r EvidenceRef) Validate() error {
	r = r.Normalize()
	has := map[string]bool{
		"trace_id": r.TraceID != "", "service": r.Service != "", "severity": r.Severity != "", "query": r.Query != "",
		"profile_type": r.ProfileType != "", "metric": r.Metric != "", "match": len(r.Match) > 0, "from": r.From != nil, "to": r.To != nil, "url": r.URL != "",
	}
	allow := map[string][]string{
		KindTrace:       {"trace_id"},
		KindLogs:        {"service", "severity", "query", "from", "to"},
		KindProfile:     {"service", "profile_type", "from", "to"},
		KindMetric:      {"metric", "match", "from", "to"},
		KindApplication: {"service"},
		KindURL:         {"url"},
	}
	ok, known := allow[r.Kind]
	if !known {
		return fmt.Errorf("evidence kind %q is not one of trace, logs, profile, metric, application, url", r.Kind)
	}
	allowed := map[string]bool{}
	for _, f := range ok {
		allowed[f] = true
	}
	var extra []string
	for f, set := range has {
		if set && !allowed[f] {
			extra = append(extra, f)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return fmt.Errorf("a %s reference does not take %s", r.Kind, strings.Join(extra, ", "))
	}
	need := func(field string, set bool) error {
		if !set {
			return fmt.Errorf("a %s reference needs %s", r.Kind, field)
		}
		return nil
	}
	if len(r.Note) > maxNote || hasControl(r.Note, true) {
		return fmt.Errorf("note: at most %d characters, with no control characters", maxNote)
	}
	if len(r.Service) > maxService || hasControl(r.Service, false) {
		return fmt.Errorf("service: at most %d characters, with no control characters", maxService)
	}
	if r.From != nil && r.To != nil && !r.From.Before(*r.To) {
		return fmt.Errorf("from must be before to")
	}
	switch r.Kind {
	case KindTrace:
		if !traceIDRE.MatchString(r.TraceID) {
			return fmt.Errorf("trace_id must be 32 hexadecimal characters")
		}
	case KindLogs:
		if err := need("a service or a query", r.Service != "" || r.Query != ""); err != nil {
			return err
		}
		if len(r.Query) > maxQuery || hasControl(r.Query, false) {
			return fmt.Errorf("query: at most %d characters, with no control characters", maxQuery)
		}
		if r.Severity != "" && !severityRE.MatchString(r.Severity) {
			return fmt.Errorf("severity must be one of trace, debug, info, warn, error, fatal")
		}
	case KindProfile:
		if err := need("a service", r.Service != ""); err != nil {
			return err
		}
		if !profileTypeRE.MatchString(r.ProfileType) {
			return fmt.Errorf("profile_type must look like cpu:nanoseconds")
		}
	case KindMetric:
		if !metricRE.MatchString(r.Metric) {
			return fmt.Errorf("metric is not a metric name")
		}
		if len(r.Match) > maxMatchers {
			return fmt.Errorf("match: at most %d labels", maxMatchers)
		}
		for k, v := range r.Match {
			if !labelRE.MatchString(k) || len(k) > maxMatchKeyLen {
				return fmt.Errorf("match: %q is not a label name", k)
			}
			if len(v) > maxMatchValue || hasControl(v, false) {
				return fmt.Errorf("match: the value of %q is too long or has control characters", k)
			}
		}
	case KindApplication:
		if err := need("a service", r.Service != ""); err != nil {
			return err
		}
	case KindURL:
		return validateURL(r.URL)
	}
	return nil
}

// validateURL accepts an absolute http or https address with a host and no credentials. Anything else (javascript:, data:, file:, a relative
// path) is refused: this address will be shown as a link to whoever opens the incident.
func validateURL(raw string) error {
	if raw == "" || len(raw) > maxURL || hasControl(raw, false) || strings.ContainsAny(raw, " \\") {
		return fmt.Errorf("url must be an http or https address of at most %d characters", maxURL)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("url must be an http or https address")
	}
	if u.User != nil {
		return fmt.Errorf("url must not carry a user name or password")
	}
	return nil
}

// EvidenceID is the same for the same reference, so adding one twice is adding it once.
func EvidenceID(r EvidenceRef) string {
	b, _ := json.Marshal(r.Normalize())
	sum := sha256.Sum256(b)
	return "ev_" + hex.EncodeToString(sum[:])[:10]
}
