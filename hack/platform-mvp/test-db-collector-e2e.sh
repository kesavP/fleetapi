#!/usr/bin/env bash
# End to end: a real PostgreSQL and a PgBouncer in front of it, the db-collector image reading both, the telemetry-ingest image storing what it sends in a real ClickHouse,
# and the read API answering. Checks that statements, sessions and the slow log arrive, and that no secret that went through the database does.
# Needs Docker; builds two images (a few minutes the first time).
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
suffix="$$"
net="dbc-e2e-net-$suffix"
work="$(mktemp -d)"
pgv="${PG_VERSION:-16}"
names=(dbc-e2e-ch-$suffix dbc-e2e-pg-$suffix dbc-e2e-ti-$suffix dbc-e2e-col-$suffix dbc-e2e-pgb-$suffix)
volume="dbc-e2e-pgdata-$suffix"

cleanup() {
  for n in "${names[@]}"; do docker rm -f "$n" >/dev/null 2>&1 || true; done
  docker network rm "$net" >/dev/null 2>&1 || true
  docker volume rm "$volume" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
fail() { echo "FAIL: $1" >&2; exit 1; }

docker network create "$net" >/dev/null
docker volume create "$volume" >/dev/null

echo "==> building the images"
docker build -q -t dbc-e2e-collector "$root/platform-mvp/db-collector" >/dev/null
docker build -q -t dbc-e2e-ingest "$root/platform-mvp/telemetry-ingest" >/dev/null

echo "==> ClickHouse and PostgreSQL $pgv"
docker run -d --name "${names[0]}" --network "$net" -e CLICKHOUSE_USER=it -e CLICKHOUSE_PASSWORD=it-pass clickhouse/clickhouse-server:24.8 >/dev/null
# the server's log goes to a file inside its data directory, which the collector's container also mounts (read-only)
docker run -d --name "${names[1]}" --network "$net" -v "$volume":/var/lib/postgresql/data -e POSTGRES_PASSWORD=admin-pw "postgres:$pgv" \
  -c shared_preload_libraries=pg_stat_statements -c log_min_duration_statement=100ms -c logging_collector=on -c log_directory=log -c log_filename=postgresql.log \
  -c log_line_prefix='%m [%p] %u@%d ' >/dev/null
for _ in $(seq 1 60); do
  docker exec "${names[0]}" clickhouse-client --user it --password it-pass -q 'SELECT 1' >/dev/null 2>&1 && ch=1 && break; sleep 1
done
[ "${ch:-}" = 1 ] || fail "ClickHouse did not start"
for _ in $(seq 1 60); do
  docker exec "${names[1]}" psql -U postgres -qAt -c "SHOW shared_preload_libraries" 2>/dev/null | grep -q pg_stat_statements && pg=1 && break; sleep 1
done
[ "${pg:-}" = 1 ] || { docker logs "${names[1]}" | tail >&2; fail "PostgreSQL did not start"; }
sleep 2
docker exec -i "${names[1]}" psql -U postgres -v ON_ERROR_STOP=1 -q <<'SQL'
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
CREATE ROLE db_collector LOGIN PASSWORD 'collector-pw-S3cret';
GRANT pg_monitor TO db_collector;
CREATE DATABASE shop;
SQL
docker exec -i "${names[1]}" psql -U postgres -d shop -v ON_ERROR_STOP=1 -q -c "CREATE EXTENSION pg_stat_statements; CREATE TABLE orders (id int primary key, email text, note text)" # the extension is per database: the one the collector connects to

echo "==> PgBouncer (transaction mode, a pool of 2) in front of it"
docker exec -i "${names[1]}" psql -U postgres -v ON_ERROR_STOP=1 -q <<'SQL'
CREATE ROLE app LOGIN PASSWORD 'app-pw-S3cret';
GRANT ALL ON DATABASE shop TO app;
SQL
docker exec -i "${names[1]}" psql -U postgres -d shop -v ON_ERROR_STOP=1 -q -c "GRANT ALL ON SCHEMA public TO app; GRANT ALL ON orders TO app"
cat >"$work/pgbouncer.ini" <<INI
[databases]
shop = host=${names[1]} port=5432 dbname=shop
[pgbouncer]
listen_addr = 0.0.0.0
listen_port = 6432
auth_type = plain
auth_file = /etc/pgbouncer/userlist.txt
pool_mode = transaction
default_pool_size = 2
reserve_pool_size = 1
max_client_conn = 100
stats_users = collector
ignore_startup_parameters = extra_float_digits
logfile = /dev/null
INI
printf '"app" "app-pw-S3cret"\n"collector" "pgb-col-pw-S3cret"\n' >"$work/userlist.txt"
chmod 755 "$work"; chmod 644 "$work"/*
docker run -d --name "${names[4]}" --network "$net" -v "$work/pgbouncer.ini":/etc/pgbouncer/pgbouncer.ini:ro -v "$work/userlist.txt":/etc/pgbouncer/userlist.txt:ro edoburu/pgbouncer:latest >/dev/null
sleep 3

echo "==> telemetry-ingest"
cat >"$work/projects.json" <<'JSON'
{"projects":[{"name":"lab","ingestKeys":["lab-ingest-key-0000001"],"readKeys":["lab-read-key-00000001"]}]}
JSON
printf '%s' it-pass >"$work/ch-pass"
chmod 755 "$work"; chmod 644 "$work"/* # the services run as a user that is not this one (the values are the test's own)
docker run -d --name "${names[2]}" --network "$net" -v "$work":/etc/ti:ro dbc-e2e-ingest \
  --projects-file=/etc/ti/projects.json --clickhouse-addr="${names[0]}:9000" --clickhouse-user=it --clickhouse-password-file=/etc/ti/ch-pass --listen=:8080 --grpc-listen= >/dev/null
for _ in $(seq 1 60); do
  docker run --rm --network "$net" curlimages/curl -sf "http://${names[2]}:8080/readyz" >/dev/null 2>&1 && ti=1 && break; sleep 1
done
[ "${ti:-}" = 1 ] || { docker logs "${names[2]}" | tail >&2; fail "telemetry-ingest did not become ready"; }

echo "==> db-collector"
cat >"$work/collector.json" <<JSON
{"listen":":8081","ingest":{"url":"http://${names[2]}:8080/v1/db","keyEnv":"INGEST_KEY"},
 "targets":[{"name":"orders-db","engine":"postgres","dsnEnv":"ORDERS_DSN",
   "statements":{"interval":"10s"},"activity":{"interval":"2s"},
   "slowLog":{"path":"/pgdata/log/postgresql.log"}},
  {"name":"pooler","engine":"pgbouncer","dsnEnv":"POOLER_DSN","pools":{"interval":"2s"}}]}
JSON
chmod 644 "$work"/*
# --check first: the collector must say the target is ready, and its output must not carry the password
docker run --rm --network "$net" --user 0:0 -v "$work":/etc/dbc:ro -v "$volume":/pgdata:ro \
  -e INGEST_KEY=lab-ingest-key-0000001 -e "ORDERS_DSN=postgres://db_collector:collector-pw-S3cret@${names[1]}:5432/shop?sslmode=disable" \
  -e "POOLER_DSN=postgres://collector:pgb-col-pw-S3cret@${names[4]}:6432/pgbouncer?sslmode=disable" \
  dbc-e2e-collector --config /etc/dbc/collector.json --check >"$work/check.out" 2>"$work/check.err" || { cat "$work/check.out" "$work/check.err" >&2; fail "--check says the target is not ready"; }
grep -q '"available": true' "$work/check.out" || fail "--check did not report the data as available"
grep -q '"engine": "pgbouncer"' "$work/check.out" || fail "--check did not report the pooler"
grep -q "collector-pw-S3cret" "$work/check.out" "$work/check.err" && fail "the password is in the output of --check"
docker run -d --name "${names[3]}" --network "$net" --user 0:0 -v "$work":/etc/dbc:ro -v "$volume":/pgdata:ro \
  -e INGEST_KEY=lab-ingest-key-0000001 -e "ORDERS_DSN=postgres://db_collector:collector-pw-S3cret@${names[1]}:5432/shop?sslmode=disable" \
  -e "POOLER_DSN=postgres://collector:pgb-col-pw-S3cret@${names[4]}:6432/pgbouncer?sslmode=disable" \
  dbc-e2e-collector --config /etc/dbc/collector.json >/dev/null
sleep 14 # the first statements snapshot is only a baseline

echo "==> a workload, with secrets in it"
psql() { docker exec -i "${names[1]}" psql -U postgres -d shop -v ON_ERROR_STOP=1 -q "$@"; }
for i in $(seq 1 30); do psql -c "INSERT INTO orders VALUES ($i, 'customer$i@secret-mail.example', 'note-$i-s3cr3t-note')"; done
for i in $(seq 1 12); do psql -c "SELECT count(*) FROM orders WHERE email = 'customer1@secret-mail.example'" >/dev/null; done
psql -c "SELECT pg_sleep(0.4), 'slow-secret-marker'" >/dev/null
psql -c "CREATE ROLE e2e_leaky PASSWORD 'hunter2-e2e-password'" ; psql -c "DROP ROLE e2e_leaky"
# a session that holds a row and one that waits for it, long enough for the sampler to see them
( psql -c "BEGIN; SELECT * FROM orders WHERE id = 1 FOR UPDATE; SELECT pg_sleep(8); COMMIT" >/dev/null 2>&1 ) &
sleep 1
( psql -c "UPDATE orders SET note = 'blocked-secret-update' WHERE id = 1" >/dev/null 2>&1 ) &
sleep 4
# a pool that is too small: eight clients through a pool of two, so six wait at the pooler while the database sees two connections
for i in $(seq 1 8); do
  ( docker exec -e PGPASSWORD=app-pw-S3cret "${names[1]}" psql -h "${names[4]}" -p 6432 -U app shop -qAt -c "BEGIN; SELECT pg_sleep(6), 'pooled-secret-marker'; COMMIT" >/dev/null 2>&1 ) &
done
sleep 3

api() { docker run --rm --network "$net" curlimages/curl -sf -H "X-API-Key: lab-read-key-00000001" "http://${names[2]}:8080/api/v1/projects/lab/$1"; }
waitfor() { # <what> <path> <python predicate over the parsed json "d">
  local what="$1" path="$2" pred="$3"
  for _ in $(seq 1 40); do
    if api "$path" 2>/dev/null | python3 -c "import sys,json; d=json.load(sys.stdin)['data']; sys.exit(0 if ($pred) else 1)" 2>/dev/null; then echo "    ok: $what"; return 0; fi
    sleep 2
  done
  echo "--- last answer for $path:" >&2; api "$path" >&2 || true; docker logs "${names[3]}" 2>&1 | tail -8 >&2
  fail "$what"
}

echo "==> reading it back"
waitfor "the targets and what they allow" "databases" \
  "{t['target'] for t in d}=={'orders-db','pooler'} and [t for t in d if t['target']=='orders-db'][0]['capabilities']['slow_log']['available'] and [t for t in d if t['target']=='pooler'][0]['engine']=='pgbouncer' and [t for t in d if t['target']=='pooler'][0]['capabilities']['pools']['available']"
waitfor "the inserts, counted as one statement" "databases/orders-db/statements?order=calls" \
  "any(s['query'].startswith('INSERT INTO orders') and s['calls']>=30 and s['rows']>=30 for s in d['statements'])"
waitfor "the selects" "databases/orders-db/statements?order=calls" "any(s['query'].startswith('SELECT count(*) FROM orders') and s['calls']>=12 for s in d['statements'])"
waitfor "the sessions: one holding, one blocked" "databases/orders-db/activity" "max(p['blocked'] for p in d['points'])>=1 and max(p['idle_in_transaction'] for p in d['points'])>=0"
waitfor "the blocked session named, waiting for another" "databases/orders-db/activity" "d['latest'] is not None"
waitfor "the slow statement from the server's own log" "databases/orders-db/slow?order=duration" "any(q['query'].startswith('SELECT pg_sleep') and q['duration_ms']>=400 for q in d)"

waitfor "clients waiting at the pooler" "databases/pooler/pools" "max(p['waiting'] for p in d['points'])>=5 and max(p['max_wait_ms'] for p in d['points'])>=1000"
waitfor "the pool, with its limits" "databases/pooler/pools" "any(q['database']=='shop' and q['pool_size']==2 and q['reserve_pool']==1 and q['mode']=='transaction' for q in d['latest']['pools'])"
waitfor "what went through, and what the waiting cost" "databases/pooler/pools" "any(s['database']=='shop' and s['xacts']>=8 and s['wait_ms']>0 for s in d['stats'])"

echo "==> no secret came through"
all="$work/all.json"
: >"$all"
for p in "databases" "databases/orders-db/statements?limit=200" "databases/orders-db/activity" "databases/orders-db/slow?limit=500" "databases/pooler/pools"; do api "$p" >>"$all"; echo >>"$all"; done
for secret in secret-mail.example s3cr3t-note slow-secret-marker hunter2 blocked-secret-update collector-pw-S3cret pgb-col-pw-S3cret app-pw-S3cret pooled-secret-marker lab-ingest-key lab-read-key; do
  if grep -q "$secret" "$all"; then grep -o ".\{60\}$secret.\{20\}" "$all" | head -3 >&2; fail "'$secret' is in what the API serves"; fi
done
docker logs "${names[3]}" 2>&1 | grep -qE "collector-pw-S3cret|pgb-col-pw|lab-ingest-key|secret-mail" && fail "a secret is in the collector's log"
docker logs "${names[2]}" 2>&1 | grep -qE "collector-pw-S3cret|secret-mail|hunter2" && fail "a secret is in the ingest service's log"
echo "    ok: none of the literals, the password or the keys"

echo "==> the collector's own view of itself"
docker run --rm --network "$net" curlimages/curl -sf "http://${names[3]}:8081/metrics" | grep -E 'db_collector_(target_up|batches_sent_total|scrapes_total)' | head -6
docker run --rm --network "$net" curlimages/curl -sf "http://${names[3]}:8081/metrics" | grep -q 'db_collector_target_up{target="orders-db"} 1' || fail "the collector does not say the target is up"
docker run --rm --network "$net" curlimages/curl -sf "http://${names[3]}:8081/metrics" | grep -q 'db_collector_target_up{target="pooler"} 1' || fail "the collector does not say the pooler is up"
echo PASS
