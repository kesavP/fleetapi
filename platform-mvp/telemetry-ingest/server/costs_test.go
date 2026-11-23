package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/costs"
	"github.com/sojoner/kro-lab/platform-mvp/telemetry-ingest/project"
)

type fakeReporter struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeReporter) Report(_ context.Context, project string, window int) (costs.Report, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("%s/%d", project, window))
	if f.err != nil {
		return costs.Report{}, f.err
	}
	return costs.Report{Currency: "USD", WindowSeconds: window, Nodes: []costs.Node{{Name: "aks-n1"}}, Applications: []costs.App{}, Analysis: []costs.Finding{}, Warnings: []string{"w"}}, nil
}

func (f *fakeReporter) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

type costRig struct {
	*incRig
	rep *fakeReporter
	now *time.Time
	mu  sync.Mutex
}

func newCostRig(t *testing.T, enabled bool) *costRig {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "projects.json")
	data := `{"projects":[
	 {"name":"us","ingestKeys":["` + incUsIngest + `"],"readKeys":["` + incUsRead + `"],"manageKeys":["` + incUsManage + `"]},
	 {"name":"eu","readKeys":["` + incEuRead + `"],"manageKeys":["` + incEuManage + `"]}]}`
	if err := os.WriteFile(cfg, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := project.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	rg := &costRig{rep: &fakeReporter{}, now: &start}
	mux := http.NewServeMux()
	(&Costs{
		Projects: reg, Reporter: rg.rep, Enabled: enabled, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		now: func() time.Time { rg.mu.Lock(); defer rg.mu.Unlock(); return *rg.now },
	}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	rg.incRig = &incRig{t: t, srv: srv}
	return rg
}

func (r *costRig) advance(d time.Duration) { r.mu.Lock(); defer r.mu.Unlock(); *r.now = r.now.Add(d) }

const costsBase = "/api/v1/projects/us/costs"
const featuresBase = "/api/v1/projects/us/features"

func TestTheFeaturesSayWhetherCostsAreSwitchedOn(t *testing.T) {
	for _, on := range []bool{true, false} {
		r := newCostRig(t, on)
		got := r.do("GET", featuresBase, incUsRead, nil)
		if got.Status != 200 {
			t.Fatalf("%d %s", got.Status, got.Raw)
		}
		if d := got.Body["data"].(map[string]any); d["costs"] != on {
			t.Fatalf("features = %v, want costs %v", d, on)
		}
	}
}

func TestOnlyAReadOrManageKeyOfTheProjectSeesTheFeaturesOrTheCosts(t *testing.T) {
	r := newCostRig(t, true)
	for name, c := range map[string]struct {
		path, key string
		want      int
	}{
		"features without a key":      {featuresBase, "", 401},
		"features with an ingest key": {featuresBase, incUsIngest, 403},
		"features with another's key": {featuresBase, incEuRead, 403},
		"features with a read key":    {featuresBase, incUsRead, 200},
		"features with a manage key":  {featuresBase, incUsManage, 200},
		"costs without a key":         {costsBase, "", 401},
		"costs with an ingest key":    {costsBase, incUsIngest, 403},
		"costs with another's key":    {costsBase, incEuRead, 403},
		"costs with a read key":       {costsBase, incUsRead, 200},
		"costs with a manage key":     {costsBase, incUsManage, 200},
	} {
		if got := r.do("GET", c.path, c.key, nil); got.Status != c.want {
			t.Errorf("%s: %d %s", name, got.Status, got.Raw)
		}
	}
	if r.rep.count() != 1+0 && r.rep.count() != 2 {
		t.Fatalf("the reporter was called %d times: only the two allowed keys should have reached it", r.rep.count())
	}
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		if got := r.do(m, costsBase, incUsManage, nil); got.Status != 405 {
			t.Errorf("%s: %d", m, got.Status)
		}
	}
}

func TestTheCostsAreTheReportOfTheWindowAskedForAnHourByDefault(t *testing.T) {
	r := newCostRig(t, true)
	got := r.do("GET", costsBase, incUsRead, nil)
	d := got.Body["data"].(map[string]any)
	if got.Status != 200 || d["window_seconds"] != float64(3600) || d["currency"] != "USD" {
		t.Fatalf("%d %s", got.Status, got.Raw)
	}
	for _, key := range []string{"nodes", "applications", "analysis", "warnings"} {
		if _, ok := d[key].([]any); !ok {
			t.Fatalf("%s is %T: a list is [] and never null", key, d[key])
		}
	}
	if got := r.do("GET", costsBase+"?window=86400", incUsRead, nil); got.Body["data"].(map[string]any)["window_seconds"] != float64(86400) {
		t.Fatalf("%s", got.Raw)
	}
	if r.rep.calls[0] != "us/3600" || r.rep.calls[1] != "us/86400" {
		t.Fatalf("calls = %v", r.rep.calls)
	}
}

func TestAWindowThatIsNotANumberOrNotAllowedIsRefused(t *testing.T) {
	r := newCostRig(t, true)
	for _, w := range []string{"abc", "-5", "0", "10", "1.5", "99999999999", "3600;x", ""} {
		path := costsBase + "?window=" + w
		got := r.do("GET", path, incUsRead, nil)
		if w == "" {
			if got.Status != 200 {
				t.Errorf("an empty window is the default: %d", got.Status)
			}
			continue
		}
		if got.Status != 400 {
			t.Errorf("window %q: %d %s", w, got.Status, got.Raw)
		}
	}
}

func TestSwitchedOffTheCostsAreNotFoundAndNothingIsRead(t *testing.T) {
	r := newCostRig(t, false)
	got := r.do("GET", costsBase, incUsRead, nil)
	if got.Status != 404 || !strings.Contains(got.Raw, "not switched on") {
		t.Fatalf("%d %s", got.Status, got.Raw)
	}
	if r.rep.count() != 0 {
		t.Fatal("the reporter was called while the feature is off")
	}
	// the key rules still apply: the answer does not tell a stranger anything
	if got := r.do("GET", costsBase, "", nil); got.Status != 401 {
		t.Fatalf("%d", got.Status)
	}
}

func TestAFailureIsOneSentenceAndAWindowTheCollectorRefusesIsABadRequest(t *testing.T) {
	r := newCostRig(t, true)
	r.rep.err = errors.New("clickhouse: connection refused sk-ant-LEAK")
	got := r.do("GET", costsBase, incUsRead, nil)
	if got.Status != 500 || strings.Contains(got.Raw, "clickhouse") || strings.Contains(got.Raw, "LEAK") {
		t.Fatalf("%d %s", got.Status, got.Raw)
	}
	r.rep.err = costs.ErrWindow
	if got := r.do("GET", costsBase+"?window=7200", incUsRead, nil); got.Status != 400 {
		t.Fatalf("%d %s", got.Status, got.Raw)
	}
}

func TestAReportIsKeptForHalfAMinuteSoARefreshingPageDoesNotReadDaysOfMetricsEveryTime(t *testing.T) {
	r := newCostRig(t, true)
	for i := 0; i < 3; i++ {
		r.do("GET", costsBase, incUsRead, nil)
	}
	if r.rep.count() != 1 {
		t.Fatalf("read %d times", r.rep.count())
	}
	r.do("GET", costsBase+"?window=86400", incUsRead, nil) // another window is another report
	if r.rep.count() != 2 {
		t.Fatalf("read %d times", r.rep.count())
	}
	r.do("GET", "/api/v1/projects/eu/costs", incEuRead, nil) // another project's is its own
	if r.rep.count() != 3 {
		t.Fatalf("read %d times", r.rep.count())
	}
	r.advance(29 * time.Second)
	r.do("GET", costsBase, incUsRead, nil)
	if r.rep.count() != 3 {
		t.Fatalf("read %d times within half a minute", r.rep.count())
	}
	r.advance(2 * time.Second)
	r.do("GET", costsBase, incUsRead, nil)
	if r.rep.count() != 4 {
		t.Fatalf("read %d times after half a minute", r.rep.count())
	}
}

func TestAFailureIsNotKept(t *testing.T) {
	r := newCostRig(t, true)
	r.rep.err = errors.New("busy")
	r.do("GET", costsBase, incUsRead, nil)
	r.rep.mu.Lock()
	r.rep.err = nil
	r.rep.mu.Unlock()
	if got := r.do("GET", costsBase, incUsRead, nil); got.Status != 200 {
		t.Fatalf("%d: a failed report was served again", got.Status)
	}
}

func TestEveryCostAnswerSaysNotToKeepIt(t *testing.T) {
	r := newCostRig(t, true)
	for _, p := range []string{costsBase, featuresBase} {
		if got := r.do("GET", p, incUsRead, nil); got.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control = %q", p, got.Header.Get("Cache-Control"))
		}
	}
}
