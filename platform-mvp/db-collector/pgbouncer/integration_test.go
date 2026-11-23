//go:build integration

// Runs the PgBouncer engine against a real PgBouncer in TRANSACTION mode with a pool of 2, in front of a real PostgreSQL.
// hack/platform-mvp/test-db-collector-pgbouncer.sh starts them, one PgBouncer version after another, and sets:
//
//	PGB_COLLECTOR_DSN   the console, as a user in stats_users
//	PGB_ADMIN_DSN       the console, as a user in admin_users
//	PGB_APP_DSN         the application's connection, through the pooler
//	PGB_PLAIN_DSN       the console, as a user that is in neither (not allowed in)
//	PGB_VERSION         the version the container runs, such as 1.26
package pgbouncer_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sojoner/kro-lab/platform-mvp/db-collector/collector"
	_ "github.com/sojoner/kro-lab/platform-mvp/db-collector/pgbouncer"
)

func env(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Skipf("%s not set; run hack/platform-mvp/test-db-collector-pgbouncer.sh", k)
	}
	return v
}

var ctx = context.Background()

func open(t *testing.T, dsnVar string) collector.Source {
	t.Helper()
	e, err := collector.Lookup("pgbouncer")
	if err != nil {
		t.Fatal(err)
	}
	src, err := e.Open(ctx, collector.TargetConfig{Name: "pgb", Engine: "pgbouncer", DSN: env(t, dsnVar)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(src.Close)
	return src
}

func TestCapabilitiesAsAStatsUser(t *testing.T) {
	caps, err := open(t, "PGB_COLLECTOR_DSN").Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", caps)
	if caps.Engine != "pgbouncer" || !strings.HasPrefix(caps.Version, env(t, "PGB_VERSION")) {
		t.Fatalf("%q, want %s: %+v", caps.Version, os.Getenv("PGB_VERSION"), caps)
	}
	if !caps.Pools.Available || len(caps.Pools.Problems) != 0 {
		t.Fatalf("%+v", caps.Pools)
	}
	if caps.Settings["pool_mode"] != "transaction" || caps.Settings["default_pool_size"] != "2" || caps.Settings["max_client_conn"] != "100" {
		t.Fatalf("%v", caps.Settings)
	}
	if caps.Statements.Available || caps.Activity.Available || caps.SlowLog.Available {
		t.Fatalf("%+v", caps)
	}
}

func TestAnAdminUserIsWarnedAboutBecauseItCouldPauseAndShutDown(t *testing.T) {
	caps, err := open(t, "PGB_ADMIN_DSN").Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !caps.Pools.Available || len(caps.Pools.Problems) != 1 || !strings.Contains(caps.Pools.Problems[0], "admin_users") {
		t.Fatalf("%+v", caps.Pools)
	}
}

func TestAUserThatMayNotUseTheConsoleGetsAnErrorWithoutItsPassword(t *testing.T) {
	_, err := open(t, "PGB_PLAIN_DSN").Capabilities(ctx)
	if err == nil {
		t.Fatal("a user in neither stats_users nor admin_users got in")
	}
	if strings.Contains(err.Error(), "app-pw") {
		t.Fatalf("the password is in: %v", err)
	}
	t.Log(err)
}

// The failure this engine exists to show: more clients than the pool has servers, so the rest queue at the pooler while the database
// sees two busy connections.
func TestAPoolThatIsTooSmallShowsClientsWaiting(t *testing.T) {
	src := open(t, "PGB_COLLECTOR_DSN").(collector.PoolSource)
	var d collector.PoolDiffer

	base, err := src.PoolSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	quiet, _ := d.Diff(base)
	for _, p := range quiet.Pools {
		if p.Database == "pgbouncer" {
			t.Fatalf("the console's own pool (the collector's connection) was reported: %+v", p)
		}
	}

	// eight clients, each holding a transaction open for 2.5 s: two get a server connection and six wait
	var wg sync.WaitGroup
	started := make(chan struct{}, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := pgx.Connect(ctx, env(t, "PGB_APP_DSN"))
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close(ctx)
			started <- struct{}{}
			if _, err := c.Exec(ctx, "BEGIN; SELECT pg_sleep(2.5); COMMIT"); err != nil {
				t.Error(err)
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-started
	}
	time.Sleep(1500 * time.Millisecond)

	snap, err := src.PoolSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	during, _ := d.Diff(snap)
	t.Logf("pools while loaded: %+v", during.Pools)
	var shop *collector.Pool
	for i := range during.Pools {
		if during.Pools[i].Database == "shop" && during.Pools[i].User == "app" {
			shop = &during.Pools[i]
		}
	}
	if shop == nil {
		t.Fatalf("no pool for shop/app: %+v", during.Pools)
	}
	if shop.Mode != "transaction" || shop.PoolSize != 2 || shop.ReservePool != 1 {
		t.Fatalf("the limits that apply: %+v", *shop)
	}
	if shop.SvActive != 2 || shop.ClActive != 2 {
		t.Fatalf("two server connections serve two clients: %+v", *shop)
	}
	if shop.ClWaiting != 6 {
		t.Fatalf("six clients wait: %+v", *shop)
	}
	if shop.MaxWaitMs < 1000 || shop.MaxWaitMs > 2500 {
		t.Fatalf("the oldest waiting client has waited about 1.5 s: %v ms", shop.MaxWaitMs)
	}
	if during.Waiting != 6 || during.Clients != 8 || during.MaxWaitMs != shop.MaxWaitMs || during.MaxClientConn != 100 {
		t.Fatalf("the totals: %+v", during)
	}
	if during.Pools[0].Database != "shop" {
		t.Fatalf("the pool where clients wait is listed first: %+v", during.Pools)
	}

	wg.Wait()
	// what went through while they ran, and the time the clients spent waiting for a server connection
	after, err := src.PoolSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	end, _ := d.Diff(after)
	// PgBouncer counts a transaction and its queries when they ARRIVE, and the time they took when they FINISH, so the count lands in the
	// interval of the first sample taken under load and the times in the one after: the two have to be added to be compared.
	var xacts, queries uint64
	var waitMs, xactMs, queryMs float64
	for _, p := range []collector.Pools{during, end} {
		for _, st := range p.Stats {
			if st.Database == "shop" {
				xacts, queries, waitMs, xactMs, queryMs = xacts+st.Xacts, queries+st.Queries, waitMs+st.WaitMs, xactMs+st.XactMs, queryMs+st.QueryMs
			}
		}
	}
	t.Logf("during: %+v; after: %+v", during.Stats, end.Stats)
	if xacts != 8 || queries < 8 {
		t.Fatalf("eight transactions went through: %d, %d queries", xacts, queries)
	}
	// six clients waited for about 2.5 s, two for about 5 s: tens of seconds of waiting in all
	if waitMs < 10_000 {
		t.Fatalf("the clients' waiting is in the counters: %v ms", waitMs)
	}
	if xactMs < 8*2000 || queryMs < 8*2000 {
		t.Fatalf("each transaction took 2.5 s: %v ms in transactions, %v ms in queries", xactMs, queryMs)
	}
	if end.Waiting != 0 {
		t.Fatalf("nobody waits now: %+v", end)
	}
	for _, p := range end.Pools {
		if p.Database == "shop" && (p.ClWaiting != 0 || p.SvActive != 0) {
			t.Fatalf("%+v", p)
		}
	}
}
