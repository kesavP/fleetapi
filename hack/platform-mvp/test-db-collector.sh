#!/usr/bin/env bash
# Runs the db-collector's PostgreSQL integration tests against real servers, one per version (default 13, 16 and 17), each started with
# pg_stat_statements loaded and log_min_duration_statement at 100ms. Needs Docker (about 1 minute per version).
#   PG_VERSIONS="16" ./hack/platform-mvp/test-db-collector.sh     # one version
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
mod="$root/platform-mvp/db-collector"
go_image="${GO_IMAGE:-golang:1.23}"
versions="${PG_VERSIONS:-13 16 17}"
suffix="$$"
net="dbc-it-net-$suffix"
work="$(mktemp -d)"
containers=()

cleanup() {
  for c in "${containers[@]:-}"; do [ -n "$c" ] && docker rm -f "$c" >/dev/null 2>&1 || true; done
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
docker network create "$net" >/dev/null

gotest() { # <env...> -- <go test args>
  local envs=()
  while [ "$1" != "--" ]; do envs+=(-e "$1"); shift; done; shift
  docker run --rm --network "$net" -v "$mod":/src -v gomodcache:/go/pkg/mod -v gobuildcache:/root/.cache/go-build -w /src \
    -e GOTOOLCHAIN=local -e GOFLAGS=-mod=mod -e CGO_ENABLED=0 "${envs[@]}" "$go_image" go test -tags integration ./postgres/ -count=1 -v "$@"
}

for v in $versions; do
  name="dbc-it-pg$v-$suffix"
  containers+=("$name")
  echo "==> PostgreSQL $v"
  docker run -d --name "$name" --network "$net" -e POSTGRES_PASSWORD=admin-pw "postgres:$v" \
    -c shared_preload_libraries=pg_stat_statements -c log_min_duration_statement=100ms -c "pg_stat_statements.track=all" >/dev/null
  for _ in $(seq 1 60); do
    # the entrypoint restarts the server once after initialising: wait for the real one, which has the library loaded
    if docker exec "$name" psql -U postgres -qAt -c "SHOW shared_preload_libraries" 2>/dev/null | grep -q pg_stat_statements; then ready=1; break; fi
    sleep 1
  done
  [ "${ready:-}" = 1 ] || { echo "PostgreSQL $v did not start" >&2; docker logs "$name" | tail -20 >&2; exit 1; }
  sleep 2
  docker exec -i "$name" psql -U postgres -v ON_ERROR_STOP=1 -q <<'SQL'
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
CREATE ROLE db_collector LOGIN PASSWORD 'collector-pw';
GRANT pg_monitor TO db_collector;
SQL
  unset ready
  dsn="postgres://db_collector:collector-pw@$name:5432/postgres?sslmode=disable"
  admin="postgres://postgres:admin-pw@$name:5432/postgres?sslmode=disable"

  gotest "PG_DSN=$dsn" "PG_ADMIN_DSN=$admin" -- || { echo "PostgreSQL $v: the tests failed" >&2; exit 1; }
  # the server's own log, which the second phase reads
  docker logs "$name" >"$work/pg$v.log" 2>&1
  docker run --rm --network "$net" -v "$mod":/src -v "$work/pg$v.log":/log/pg.log:ro -v gomodcache:/go/pkg/mod -v gobuildcache:/root/.cache/go-build -w /src \
    -e GOTOOLCHAIN=local -e GOFLAGS=-mod=mod -e CGO_ENABLED=0 -e PG_DSN="$dsn" -e PG_ADMIN_DSN="$admin" -e PG_LOG=/log/pg.log \
    "$go_image" go test -tags integration ./postgres/ -count=1 -v -run 'TestReadTheServersOwnLog' || { echo "PostgreSQL $v: reading the log failed" >&2; exit 1; }
  echo "==> PostgreSQL $v: ok"
done
echo PASS
