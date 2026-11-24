#!/usr/bin/env bash
# Runs the db-collector's PgBouncer integration tests against a real PgBouncer in transaction mode (pool of 2) in front of a real PostgreSQL,
# for each PgBouncer version in PGB_TAGS (default: 1.22.1-p0 and the latest). Needs Docker (about a minute per version).
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
mod="$root/platform-mvp/db-collector"
go_image="${GO_IMAGE:-golang:1.23}"
tags="${PGB_TAGS:-1.22.1-p0 latest}"
suffix="$$"
net="dbc-pgb-net-$suffix"
work="$(mktemp -d)"
pg="dbc-pgb-pg-$suffix"
containers=("$pg")

cleanup() {
  for c in "${containers[@]}"; do docker rm -f "$c" >/dev/null 2>&1 || true; done
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

docker network create "$net" >/dev/null
docker run -d --name "$pg" --network "$net" -e POSTGRES_PASSWORD=pgpw postgres:16 >/dev/null
for _ in $(seq 1 60); do docker exec "$pg" pg_isready -U postgres >/dev/null 2>&1 && ready=1 && break; sleep 1; done
[ "${ready:-}" = 1 ] || { echo "PostgreSQL did not start" >&2; exit 1; }
sleep 3
docker exec -i "$pg" psql -U postgres -v ON_ERROR_STOP=1 -q <<'SQL'
CREATE ROLE app LOGIN PASSWORD 'app-pw';
CREATE DATABASE shop OWNER app;
SQL

cat >"$work/userlist.txt" <<'TXT'
"app" "app-pw"
"pgbadmin" "adm-pw"
"collector" "col-pw"
TXT
for tag in $tags; do
  name="dbc-pgb-$(echo "$tag" | tr -c 'a-z0-9\n' '-')-$suffix"
  containers+=("$name")
  cat >"$work/pgbouncer.ini" <<INI
[databases]
shop = host=$pg port=5432 dbname=shop
[pgbouncer]
listen_addr = 0.0.0.0
listen_port = 6432
auth_type = plain
auth_file = /etc/pgbouncer/userlist.txt
pool_mode = transaction
default_pool_size = 2
reserve_pool_size = 1
max_client_conn = 100
admin_users = pgbadmin
stats_users = collector
ignore_startup_parameters = extra_float_digits
logfile = /dev/null
INI
  chmod 644 "$work"/*
  echo "==> PgBouncer $tag"
  docker run -d --name "$name" --network "$net" -v "$work/pgbouncer.ini":/etc/pgbouncer/pgbouncer.ini:ro -v "$work/userlist.txt":/etc/pgbouncer/userlist.txt:ro "edoburu/pgbouncer:$tag" >/dev/null
  version=""
  for _ in $(seq 1 30); do
    version="$(docker logs "$name" 2>&1 | sed -n 's/.*process up: PgBouncer \([0-9]*\.[0-9]*\).*/\1/p' | head -1)"
    [ -n "$version" ] && break; sleep 1
  done
  [ -n "$version" ] || { docker logs "$name" | tail >&2; echo "PgBouncer $tag did not start" >&2; exit 1; }
  echo "    PgBouncer $version"
  docker run --rm --network "$net" -v "$mod":/src -v gomodcache:/go/pkg/mod -v gobuildcache:/root/.cache/go-build -w /src \
    -e GOTOOLCHAIN=local -e GOFLAGS=-mod=mod -e CGO_ENABLED=0 -e PGB_VERSION="$version" \
    -e "PGB_COLLECTOR_DSN=postgres://collector:col-pw@$name:6432/pgbouncer?sslmode=disable" \
    -e "PGB_ADMIN_DSN=postgres://pgbadmin:adm-pw@$name:6432/pgbouncer?sslmode=disable" \
    -e "PGB_PLAIN_DSN=postgres://app:app-pw@$name:6432/pgbouncer?sslmode=disable" \
    -e "PGB_APP_DSN=postgres://app:app-pw@$name:6432/shop?sslmode=disable&default_query_exec_mode=simple_protocol" \
    "$go_image" go test -tags integration ./pgbouncer/ -count=1 -v || { echo "PgBouncer $tag: the tests failed" >&2; exit 1; }
  echo "==> PgBouncer $version: ok"
done
echo PASS
