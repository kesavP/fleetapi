# Database collector

`platform-mvp/db-collector` reads what a database knows about its own workload and sends it to [telemetry-ingest](20-telemetry-ingest.md), which stores it with the project's metrics, traces, logs and profiles. The node-agent sees a database from outside (calls, latency, connections); the collector asks the database itself: **which statements cost the most, what the sessions are doing right now and who is blocking whom, and which statements were slow**.

There is one engine per kind of database: **`postgres`** (statements, sessions, slow log) and **`pgbouncer`** (connection pools, below). An engine implements a few small interfaces and says which signals it has (`Signals()`); everything else is shared. What is on by default follows from that: a PostgreSQL target has statements and activity, a PgBouncer target has pools, and asking an engine for something it does not have is a configuration error that says so.

## What it collects

| Signal | From | Every | What is sent |
|---|---|---|---|
| **Statements** | `pg_stat_statements` | 60 s (min 10 s) | per statement, **for the interval**: calls, total time, rows, buffer/WAL/temp counters; the busiest `top` (200) by name and one "other statements" row, so totals stay whole |
| **Activity** | `pg_stat_activity` | 10 s (min 1 s) | sessions by state, waits (`Lock:relation`), blocked sessions, longest query / transaction / idle-in-transaction, connections against `max_connections`, and the few sessions worth naming (the longest running, the idle in transaction, the blocked, and the **blockers**, even when quick) |
| **Slow statements** | the server log (`log_min_duration_statement`) | continuously | statements that took longer than the server's threshold: duration, time, pid, user, database |
| **Pools** | PgBouncer's console | 10 s (min 1 s) | see below |
| **Capabilities** | settings and catalog | 10 min | the server version, whether each signal can be collected, and **what to change when it cannot** |

Statistics are cumulative counters, so the collector turns them into per-interval figures (`collector/delta.go`): the first snapshot is only a baseline (days of work is not one interval's); a counter that goes down, a changed `stats_since` (PostgreSQL 17) or a changed `stats_reset` (14+) means the statistics restarted and the figures are what has run since; a new statement counts from zero; a failed scrape makes the next interval longer, not wrong.

## PgBouncer: is the pool too small?

A pooler in front of a database hides one failure from the database: when the pool is too small, or its servers are slow, clients queue at the pooler, the database sees a couple of busy connections, and the application sees latency that nothing in the database explains. The `pgbouncer` engine reads the pooler's own view, from its admin console (the virtual database `pgbouncer`), every 10 s:

| | |
|---|---|
| **Pools** (`SHOW POOLS`) | per database and user: clients active and **waiting** (`cl_active`, `cl_waiting`), server connections by state, the **oldest waiting client's wait** (`maxwait`), with the limits that apply: the pool size and reserve pool (the database's own, else the defaults), the mode, paused/disabled |
| **Totals** | clients in all pools, how many wait, the longest wait, against `max_client_conn` |
| **What went through** (`SHOW STATS`) | per database and interval: transactions, queries, bytes, time in transactions and queries, and **time clients spent waiting for a server connection**; from these, `avg_wait_ms` per transaction and `wait_share` (the part of the clients' time that went on waiting) |

```json
{"name": "pgb", "engine": "pgbouncer", "dsnEnv": "PGB_DSN", "pools": {"interval": "10s"}}
```
```ini
; pgbouncer.ini: a user that can only look. NOT an admin_user: see below.
stats_users = collector
```
`PGB_DSN=postgres://collector:...@pgbouncer:6432/pgbouncer?sslmode=...` (the database must be `pgbouncer`: Open refuses anything else, and says why). Tested on PgBouncer 1.22 and 1.26 in transaction mode; it works in every pool mode, because the console is the pooler's own and never goes through a pool.

**What it will and will not do to the pooler.** The console lets an admin `PAUSE`, `KILL` and `SHUTDOWN`, so the collector (1) sends only five fixed `SHOW` commands (`VERSION`, `POOLS`, `STATS`, `DATABASES`, `CONFIG`): anything else is refused in code before it is sent, and a test fails if the list grows; and (2) tells you, in `--check` and in the capabilities, if its user is in `admin_users`, because a read-only collector should not hold that power. It sends only `application_name` as a startup parameter (the console rejects the others) and never pings (`SELECT 1` and `;` are not valid there). The console's own pool (the collector's connection) is left out of what it reports.

**How the figures behave.** PgBouncer counts a transaction and its queries when they **arrive** and the time they took when they **finish**, so in one 10 s sample the count and the time of the same transaction can fall in different samples (a 2.5 s transaction that arrives at the end of one interval is counted there and timed in the next). Over a window they agree, which is why the read API sums over the window. A counter that goes down means the pooler restarted, and the interval after a restart can read low. A pooler with nothing connected lists no pool: the API then shows the last sample that had pools, with its time.

**Reading it:** `GET .../databases/{target}/pools`: the **peak** of clients and of those waiting in each bucket (a queue of a few seconds is not averaged away), the pools in the latest sample, and per database over the window the transactions, the time in them and the time waiting. Clients waiting while `sv_active` equals the pool size is a pool that is too small (raise `default_pool_size`, or find what holds the connections: sessions idle in transaction on the database behind it are in the PostgreSQL target's activity); waiting with servers idle or logging in is the database being slow to answer.

## Privacy: nothing leaves with a literal in it

Statement text can hold anything an application put in it: an e-mail in a `WHERE`, a password in `CREATE ROLE ... PASSWORD '...'`. **Every query text passes through `collector.Normalize` before it goes anywhere** (the statements, the sessions' current queries, the slow log): strings, numbers, dollar-quoted bodies and bit/hex strings become `?`, comments are dropped (they carry trace and user ids), `IN (?, ?, ?)` becomes `IN (?)` and repeated `VALUES` tuples become one. Text cut off in the middle of a literal has the rest removed. The log's `DETAIL: parameters:` lines (where the bound values are) are never read. It follows PostgreSQL's own lexer, and a property test inserts secrets into every kind of literal and checks none survives.

Two more guards. The collector's own queries start with `/* db-collector */` and are left out of everything it reports (statistics, sessions, log). And telemetry-ingest refuses a batch whose query text still has a quoted literal (a tripwire for a collector that does not normalize; it cannot catch a bare number, so normalizing stays the collector's job and the ingest key is a trust boundary).

## Setting up PostgreSQL

```sql
-- postgresql.conf (then restart): shared_preload_libraries = 'pg_stat_statements'
--                                 log_min_duration_statement = 250ms     -- for the slow log; -1 turns it off
CREATE ROLE db_collector LOGIN PASSWORD '...';
GRANT pg_monitor TO db_collector;            -- see query text and other users' sessions (pg_read_all_stats is enough)
-- in the database the collector connects to:
CREATE EXTENSION pg_stat_statements;
```

The collector's session is read-only (`default_transaction_read_only`), has a 5 s statement timeout, a 1 s lock timeout and an idle-in-transaction timeout, and uses at most two connections. It works behind a pooler in session mode; in transaction mode the startup parameters may need `ignore_startup_parameters`.

**`pg_stat_statements` changes between versions** (`total_time` became `total_exec_time` in 13, `toplevel` came in 14, `blk_read_time` became `shared_blk_read_time` and `stats_since` appeared in 17), so the query is built from the columns the server actually has, and one name is reported for the same counter across versions. Tested against 13, 16 and 17.

## Configuration and running

```json
{
  "listen": ":8081",
  "ingest": {"url": "http://telemetry-ingest/v1/db", "keyFile": "/etc/db-collector/ingest-key"},
  "targets": [
    {"name": "orders-db", "engine": "postgres", "dsnEnv": "ORDERS_DSN",
     "statements": {"interval": "60s", "top": 200},
     "activity":   {"interval": "10s", "top": 20},
     "slowLog":    {"path": "/var/log/postgresql/postgresql.log"}}
  ]
}
```

**No secret is in the file**: the connection string comes from the environment variable (`dsnEnv`) or file (`dsnFile`) it names, and so does the ingest key (`keyEnv`, `keyFile`); a file with a `dsn` or `key` in it is refused with the way to do it properly. Errors never carry a password or the key.

```sh
db-collector --config c.json --check     # what each database allows, and what to change; exit 1 if what is asked for cannot be collected
db-collector --config c.json --stdout    # print what would be sent, one JSON batch a line
db-collector --config c.json             # collect and send; /healthz and /metrics on "listen"
```

`--check` answers things like "the extension is not created in this database: run CREATE EXTENSION pg_stat_statements; in it", "log_min_duration_statement is off", "the role cannot see other users' sessions: GRANT pg_read_all_stats TO db_collector". Image: `make build-db-collector-image push-db-collector DOCKER_REGISTRY=... IMAGE_TAG=...`.

**The slow log must be a file the collector can read** (`logging_collector = on`, the stderr text format, a volume shared with the collector, or the log directory of a VM). It is followed like `tail -F` (from its end at start, across rotation and truncation). The time in a line is read only when the server logs in UTC or a numeric offset; another zone's name ("CEST") does not say its offset, so such lines get the time they were read.

**Failure behaviour.** A database that is down is a log line (once, not once a poll), a counter and a gap in the data; it is retried, and the other targets and signals carry on. Batches wait in a bounded queue with retries; if telemetry-ingest is down for longer than the queue holds, the oldest are dropped and counted (`db_collector_batches_dropped_total`). Metrics: `db_collector_target_up`, `_scrapes_total`, `_errors_total`, `_batches_sent_total`, `_slow_queries_total`, `_statement_resets_total`.

## What telemetry-ingest serves

Ingest: `POST /v1/db` with the project's **ingest key**, JSON, at most 4 MiB, checked before storing (`dbtelemetry`). Read, with a **read key** (see [20-telemetry-ingest.md](20-telemetry-ingest.md)):

| Endpoint | |
|---|---|
| `GET .../databases` | the targets, their engine and version, when they last reported, and what each allows |
| `GET .../databases/{target}/statements` | `order` (`total_ms` `calls` `mean_ms` `rows`), `limit`, `database`, `user`, `from`/`to`: summed over the window, with the window's total calls and time so a statement's share can be told |
| `GET .../databases/{target}/activity` | the **peak** of each figure in each bucket (a pile-up of a few seconds is not averaged away), and the sessions named in the latest sample |
| `GET .../databases/{target}/slow` | `min_ms`, `order` (`time` or `duration`), `limit` |
| `GET .../databases/{target}/pools` | a pooler: `step`, `from`/`to`: the peak of clients and of those waiting per bucket, the pools in the latest sample, and per-database totals with `avg_wait_ms` and `wait_share` over the window |

Kept for `--ttl-databases` (default 7 days) in seven tables, `db_targets`, `db_statements`, `db_activity`, `db_slow_queries`, `db_pool_samples`, `db_pools`, `db_pool_stats`; they appear as "databases" in the ClickHouse tab's Storage Usage.

## What is not done

- **No chart for the collector yet** (a Deployment next to each database, with the role's connection string in a Secret and the log directory on a shared volume): the image, the config and `--check` are there.
- **No UI** for any of it: the data is in the read API.
- **Managed databases** (RDS, Cloud SQL) keep their logs in the provider's service, not in a file. The statistics and activity work with a role that has the grants; the slow log would need another source (the provider's API, or the platform's own logs store, since the node-agent already ships container logs that carry these lines). The interface for it is `SlowLogSource`.
- **Only the stderr text log format**: not `csvlog` or `jsonlog`. `auto_explain` plans are ignored.
- **Restarts lose what happened while the collector was down** (the log is followed from its end, and the statement counters resume from a new baseline).
- **Two engines: PostgreSQL and PgBouncer.** Adding one means implementing `collector.Engine` (and `StatementSource`, `ActivitySource`, `PoolSource`, `SlowLogSource` for what it has) and `Normalize` rules for its dialect. Other poolers (pgcat, Odyssey, RDS Proxy) are not read.
- **PgBouncer's per-client and per-server detail** (`SHOW CLIENTS`, `SHOW SERVERS`: which client holds which server connection) is deliberately not read: it carries client addresses and is the part of the console that is most sensitive. Pool totals answer "is it exhausted"; finding who holds the connections is the PostgreSQL target's job.

## Tests

`make test-dbc-pgbouncer` (real PgBouncer 1.22 and the latest in transaction mode with a pool of 2: eight clients hold eight 2.5 s transactions, and the test checks that six are seen waiting, the oldest for about 1.5 s, and that the waiting appears in the counters; and that an admin user is warned and a user outside stats_users is refused without its password), `make test-dbc` (unit, including the secret-leak property test, the counter-reset cases and the log follower across rotation), `make test-dbc-integration` (real PostgreSQL 13, 16 and 17: capabilities, a statement delta with a statistics reset, a blocked update against an idle-in-transaction session, and the server's own log with simple and extended-protocol statements), `make test-dbc-e2e` (PostgreSQL, a PgBouncer in front of it, the collector image, the ingest image and ClickHouse together: the data of both targets arrives, a saturated pool shows its waiting clients, and no secret does).
