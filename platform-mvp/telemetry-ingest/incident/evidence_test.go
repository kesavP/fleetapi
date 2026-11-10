package incident

import (
	"strings"
	"testing"
	"time"
)

var (
	t0 = time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Hour)
)

func TestEvidenceValid(t *testing.T) {
	good := []EvidenceRef{
		{Kind: "trace", TraceID: strings.Repeat("a", 32)},
		{Kind: "logs", Service: "/k8s/shop/cart", Severity: "ERROR", From: &t0, To: &t1},
		{Kind: "logs", Query: "timeout"},
		{Kind: "profile", Service: "/k8s/shop/cart", ProfileType: "cpu:nanoseconds", From: &t0, To: &t1},
		{Kind: "profile", Service: "x", ProfileType: "alloc_space:bytes"},
		{Kind: "metric", Metric: "container_net_latency_seconds", Match: map[string]string{"container_id": "/k8s/a/b/c"}},
		{Kind: "application", Service: "/k8s/shop/frontend"},
		{Kind: "url", URL: "https://grafana.example.com/d/abc?from=now-1h"},
		{Kind: "url", URL: "http://10.0.0.5:3000/dashboard", Note: "the db dashboard"},
	}
	for _, r := range good {
		if err := r.Validate(); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
}

func TestEvidenceInvalid(t *testing.T) {
	bad := map[string]EvidenceRef{
		"unknown kind":                {Kind: "screenshot"},
		"no kind":                     {},
		"trace id too short":          {Kind: "trace", TraceID: "abc"},
		"trace id not hex":            {Kind: "trace", TraceID: strings.Repeat("g", 32)},
		"trace with a service":        {Kind: "trace", TraceID: strings.Repeat("a", 32), Service: "x"},
		"logs with nothing to search": {Kind: "logs"},
		"logs with a bad severity":    {Kind: "logs", Service: "x", Severity: "loud"},
		"logs backwards in time":      {Kind: "logs", Service: "x", From: &t1, To: &t0},
		"logs with equal times":       {Kind: "logs", Service: "x", From: &t0, To: &t0},
		"logs with a profile type":    {Kind: "logs", Service: "x", ProfileType: "cpu:nanoseconds"},
		"profile without a service":   {Kind: "profile", ProfileType: "cpu:nanoseconds"},
		"profile type that is not":    {Kind: "profile", Service: "x", ProfileType: "cpu"},
		"profile type with a quote":   {Kind: "profile", Service: "x", ProfileType: "cpu':nanoseconds"},
		"metric that is not a name":   {Kind: "metric", Metric: "a b"},
		"metric with a bad label":     {Kind: "metric", Metric: "up", Match: map[string]string{"a-b": "x"}},
		"metric with a newline":       {Kind: "metric", Metric: "up", Match: map[string]string{"a": "x\ny"}},
		"application without service": {Kind: "application"},
		"url that is javascript":      {Kind: "url", URL: "javascript:alert(1)"},
		"url that is data":            {Kind: "url", URL: "data:text/html,<script>alert(1)</script>"},
		"url that is file":            {Kind: "url", URL: "file:///etc/passwd"},
		"url that is relative":        {Kind: "url", URL: "/etc/passwd"},
		"url with no host":            {Kind: "url", URL: "https://"},
		"url with credentials":        {Kind: "url", URL: "https://user:pass@example.com/"},
		"url with a space":            {Kind: "url", URL: "https://example.com/a b"},
		"url with a backslash":        {Kind: "url", URL: `https://example.com\@evil.com/`},
		"url with a control char":     {Kind: "url", URL: "https://example.com/\x00"},
		"url too long":                {Kind: "url", URL: "https://example.com/" + strings.Repeat("a", 2100)},
		"url with a trace id":         {Kind: "url", URL: "https://example.com/", TraceID: strings.Repeat("a", 32)},
		"note too long":               {Kind: "application", Service: "x", Note: strings.Repeat("n", 501)},
		"note with a control char":    {Kind: "application", Service: "x", Note: "a\x07b"},
		"too many labels":             {Kind: "metric", Metric: "up", Match: manyLabels(21)},
	}
	for name, r := range bad {
		if err := r.Validate(); err == nil {
			t.Errorf("%s: accepted %+v", name, r)
		}
	}
}

func manyLabels(n int) map[string]string {
	m := map[string]string{}
	for i := 0; i < n; i++ {
		m["l"+strings.Repeat("x", i)] = "v"
	}
	return m
}

func TestEvidenceKindAndTraceIDAreForgiving(t *testing.T) {
	r := EvidenceRef{Kind: " Trace ", TraceID: strings.ToUpper(strings.Repeat("ab", 16))}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	if n := r.Normalize(); n.Kind != "trace" || n.TraceID != strings.Repeat("ab", 16) {
		t.Fatalf("%+v", n)
	}
}

func TestEvidenceIDIsStableForTheSameReference(t *testing.T) {
	a := EvidenceRef{Kind: "logs", Service: "x", Severity: "error", From: &t0}
	b := EvidenceRef{Kind: "LOGS", Service: " x ", Severity: "ERROR", From: &t0}
	if EvidenceID(a) != EvidenceID(b) {
		t.Fatal("the same reference typed two ways has two ids")
	}
	if EvidenceID(a) == EvidenceID(EvidenceRef{Kind: "logs", Service: "y", Severity: "error", From: &t0}) {
		t.Fatal("different references share an id")
	}
	// the note is part of the reference: the same trace with another note is another piece of evidence
	tr := EvidenceRef{Kind: "trace", TraceID: strings.Repeat("a", 32)}
	if EvidenceID(tr) == EvidenceID(EvidenceRef{Kind: "trace", TraceID: strings.Repeat("a", 32), Note: "n"}) {
		t.Fatal("a note did not change the id")
	}
	// maps are written in a fixed order, so the order they were typed in does not matter
	m1 := EvidenceRef{Kind: "metric", Metric: "up", Match: map[string]string{"a": "1", "b": "2", "c": "3"}}
	m2 := EvidenceRef{Kind: "metric", Metric: "up", Match: map[string]string{"c": "3", "b": "2", "a": "1"}}
	if EvidenceID(m1) != EvidenceID(m2) {
		t.Fatal("label order changed the id")
	}
	// the same instant in another zone is the same reference
	zone := time.FixedZone("x", 5*3600)
	z := t0.In(zone)
	if EvidenceID(EvidenceRef{Kind: "logs", Service: "x", From: &z}) != EvidenceID(EvidenceRef{Kind: "logs", Service: "x", From: &t0}) {
		t.Fatal("a time zone changed the id")
	}
	if !strings.HasPrefix(EvidenceID(a), "ev_") || len(EvidenceID(a)) != 13 {
		t.Fatal(EvidenceID(a))
	}
}
