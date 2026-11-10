package store

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

var testTTL = TTLs{Metrics: 14 * 24 * time.Hour, Traces: 7 * 24 * time.Hour, Logs: 7 * 24 * time.Hour, Profiles: 3 * 24 * time.Hour, Databases: 5 * 24 * time.Hour}

func kind(t *testing.T, rows []ChStorage, name string) ChStorage {
	t.Helper()
	for _, r := range rows {
		if r.Type == name {
			return r
		}
	}
	t.Fatalf("no %q in %+v", name, rows)
	return ChStorage{}
}

func TestBuildStorageListsEveryKindInOrderWithItsRetention(t *testing.T) {
	rows := buildStorage(testTTL, map[string]chTable{}, map[string]chMine{}, false)
	var names []string
	for _, r := range rows {
		names = append(names, r.Type)
	}
	if strings.Join(names, ",") != "metrics,traces,logs,profiles,databases" {
		t.Fatalf("%v", names)
	}
	if kind(t, rows, "metrics").TTLSeconds != 14*86400 || kind(t, rows, "profiles").TTLSeconds != 3*86400 || kind(t, rows, "databases").TTLSeconds != 5*86400 {
		t.Fatalf("%+v", rows)
	}
}

func TestBuildStorageEstimatesTheProjectsShareByItsShareOfTheRows(t *testing.T) {
	parts := map[string]chTable{"logs_records": {rows: 1000, compressed: 10_000, uncompressed: 110_000}}
	mine := map[string]chMine{"logs_records": {rows: 250, oldest: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}}
	l := kind(t, buildStorage(testTTL, parts, mine, false), "logs")
	if l.Rows != 250 {
		t.Errorf("rows %d", l.Rows)
	}
	if l.EstimatedBytes == nil || *l.EstimatedBytes != 2500 { // a quarter of the rows, a quarter of the bytes
		t.Errorf("estimate %v", l.EstimatedBytes)
	}
	if l.Compression != 11 {
		t.Errorf("compression %v", l.Compression)
	}
	if l.Oldest == nil || !l.Oldest.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("oldest %v", l.Oldest)
	}
}

func TestBuildStorageAddsUpTheTablesOfAKindAndTakesRowsFromItsMainTable(t *testing.T) {
	parts := map[string]chTable{
		"metrics_samples": {rows: 1000, compressed: 8000, uncompressed: 16000},
		"metrics_series":  {rows: 100, compressed: 2000, uncompressed: 12000},
	}
	mine := map[string]chMine{"metrics_samples": {rows: 500, oldest: time.Unix(1_700_000_000, 0)}, "metrics_series": {rows: 10}}
	m := kind(t, buildStorage(testTTL, parts, mine, false), "metrics")
	if m.Rows != 500 {
		t.Errorf("rows are the main table's, not a sum of tables: %d", m.Rows)
	}
	// half of the samples' 8000 and a tenth of the series' 2000
	if m.EstimatedBytes == nil || *m.EstimatedBytes != 4200 {
		t.Errorf("estimate %v", m.EstimatedBytes)
	}
	if m.Compression != 2.8 { // 28000 uncompressed over 10000 compressed
		t.Errorf("compression %v", m.Compression)
	}
}

func TestBuildStorageDoesNotInventDataForAProjectWithNone(t *testing.T) {
	parts := map[string]chTable{"traces_spans": {rows: 5000, compressed: 50_000, uncompressed: 500_000}}
	tr := kind(t, buildStorage(testTTL, parts, map[string]chMine{"traces_spans": {rows: 0}}, false), "traces")
	if tr.Rows != 0 || tr.Oldest != nil || tr.EstimatedBytes == nil || *tr.EstimatedBytes != 0 {
		t.Fatalf("%+v", tr)
	}
}

func TestBuildStorageSurvivesAnEmptyTable(t *testing.T) {
	// nothing stored at all: no division by zero, no estimate out of thin air
	l := kind(t, buildStorage(testTTL, map[string]chTable{"logs_records": {}}, map[string]chMine{"logs_records": {}}, true), "logs")
	if l.Compression != 0 || l.EstimatedBytes == nil || *l.EstimatedBytes != 0 {
		t.Fatalf("%+v", l)
	}
}

func TestBuildStorageLeavesOutWhatItCouldNotRead(t *testing.T) {
	l := kind(t, buildStorage(testTTL, nil, map[string]chMine{"logs_records": {rows: 7, oldest: time.Unix(1, 0)}}, true), "logs")
	if l.Rows != 7 {
		t.Errorf("the project's own rows do not need the system tables: %d", l.Rows)
	}
	if l.EstimatedBytes != nil || l.Compression != 0 || l.TableBytes != nil || l.TableRows != nil {
		t.Fatalf("sizes were not read, so none may be claimed: %+v", l)
	}
}

func TestBuildStorageSharesWholeTableSizesOnlyWhenAsked(t *testing.T) {
	parts := map[string]chTable{"logs_records": {rows: 1000, compressed: 10_000, uncompressed: 110_000}}
	mine := map[string]chMine{"logs_records": {rows: 1, oldest: time.Unix(1, 0)}}
	private := kind(t, buildStorage(testTTL, parts, mine, false), "logs")
	if private.TableBytes != nil || private.TableRows != nil {
		t.Fatalf("another project's volume must not be visible by default: %+v", private)
	}
	shared := kind(t, buildStorage(testTTL, parts, mine, true), "logs")
	if shared.TableBytes == nil || *shared.TableBytes != 10_000 || shared.TableRows == nil || *shared.TableRows != 1000 {
		t.Fatalf("%+v", shared)
	}
	b, _ := json.Marshal(private)
	if strings.Contains(string(b), "table_bytes") || strings.Contains(string(b), "table_rows") {
		t.Fatalf("the keys must be absent, not null: %s", b)
	}
}

func TestInfoTypesCannotCarryTheAddressOrCredentials(t *testing.T) {
	b, _ := json.Marshal(ClickHouseInfo{Connection: ChConnection{Protocol: "native", Database: "telemetry"}, Storage: []ChStorage{}, Warnings: []string{}})
	for _, forbidden := range []string{"addr", "user", "pass", "secret"} {
		if strings.Contains(strings.ToLower(string(b)), forbidden) {
			t.Fatalf("%q appears in %s", forbidden, b)
		}
	}
}

func TestDiskSize(t *testing.T) {
	if f, tot, u := diskSize(61, 100); f != 61 || tot != 100 || u {
		t.Errorf("%d %d %v", f, tot, u)
	}
	// a bucket: ClickHouse says 2^64-1 for both, which would otherwise become -1
	if f, tot, u := diskSize(math.MaxUint64, math.MaxUint64); f != 0 || tot != 0 || !u {
		t.Errorf("a bucket has no size: %d %d %v", f, tot, u)
	}
	// more free than the whole is a clock-skew style inconsistency; never report a negative used share
	if f, tot, _ := diskSize(150, 100); f != 100 || tot != 100 {
		t.Errorf("%d %d", f, tot)
	}
}
