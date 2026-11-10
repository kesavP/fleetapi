package store

import (
	"strings"
	"testing"
	"time"
)

func TestTTLStatement(t *testing.T) {
	ttl := TTLs{Metrics: 336 * time.Hour, Traces: 168 * time.Hour, Logs: 168 * time.Hour, Profiles: 168 * time.Hour}
	var logs, series ttlRule
	for _, r := range ttlRules {
		switch r.table {
		case "logs_records":
			logs = r
		case "metrics_series":
			series = r
		}
	}
	// no tiering: delete only, as before
	if got, want := ttlStatement(logs, ttl, Tiering{}), "ALTER TABLE logs_records MODIFY TTL toDateTime(Timestamp) + INTERVAL 604800 SECOND DELETE"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	// tiering: move first, then delete
	tier := Tiering{Policy: "tiered", Volume: "cold", After: 48 * time.Hour}
	want := "ALTER TABLE logs_records MODIFY TTL toDateTime(Timestamp) + INTERVAL 172800 SECOND TO VOLUME 'cold', toDateTime(Timestamp) + INTERVAL 604800 SECOND DELETE"
	if got := ttlStatement(logs, ttl, tier); got != want {
		t.Errorf("got %q want %q", got, want)
	}
	// a small lookup table never moves
	if got := ttlStatement(series, ttl, tier); strings.Contains(got, "VOLUME") {
		t.Errorf("a small table must stay on the default disk: %q", got)
	}
	// a policy without a move (After 0) does not move either
	if got := ttlStatement(logs, ttl, Tiering{Policy: "tiered", Volume: "cold"}); strings.Contains(got, "VOLUME") {
		t.Errorf("no move without a time: %q", got)
	}
}

func TestOnlyTheBigTablesAreTiered(t *testing.T) {
	big := map[string]bool{}
	for _, r := range ttlRules {
		big[r.table] = r.big
	}
	for _, name := range []string{"metrics_samples", "traces_spans", "logs_records", "profiling_samples"} {
		if !big[name] {
			t.Errorf("%s should be tiered", name)
		}
	}
	for _, name := range []string{"metrics_series", "profiling_stacks", "profiling_services"} {
		if big[name] {
			t.Errorf("%s is small and read often; it should stay hot", name)
		}
	}
}
