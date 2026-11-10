#!/usr/bin/env bash
# Runs telemetry-ingest's integration test against a real ClickHouse (needs
# Docker, ~1 min). Starts a throwaway ClickHouse on a private network, runs
# the test in a Go container, then removes both.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
mod="$root/platform-mvp/telemetry-ingest"
ch_image="${CLICKHOUSE_IMAGE:-clickhouse/clickhouse-server:24.8}"
go_image="${GO_IMAGE:-golang:1.23}"
suffix="$$"
net="ti-it-net-$suffix"
ch="ti-it-clickhouse-$suffix"

cleanup() { docker rm -f "$ch" >/dev/null 2>&1 || true; docker network rm "$net" >/dev/null 2>&1 || true; docker volume rm ti-it-cold-"$suffix" >/dev/null 2>&1 || true; }
trap cleanup EXIT

docker network create "$net" >/dev/null
# The tiered-storage test needs a storage policy; the second disk is a plain directory owned by the server's user.
docker run -d --name "$ch" --network "$net" \
  -v "$mod/integration/testdata/tiered-storage.xml":/etc/clickhouse-server/config.d/tiered-storage.xml:ro \
  -v ti-it-cold-"$suffix":/var/lib/clickhouse-cold \
  -e CLICKHOUSE_USER=it -e CLICKHOUSE_PASSWORD=it-pass -e CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1 \
  "$ch_image" >/dev/null

echo "==> waiting for ClickHouse"
for _ in $(seq 1 60); do
  if docker exec "$ch" clickhouse-client --user it --password it-pass -q 'SELECT 1' >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
[ "${ready:-}" = 1 ] || { echo "ClickHouse did not start" >&2; docker logs "$ch" | tail -20 >&2; exit 1; }

echo "==> running the integration test"
docker run --rm --network "$net" -v "$mod":/src -v gomodcache:/go/pkg/mod -v gobuildcache:/root/.cache/go-build -w /src \
  -e GOTOOLCHAIN=local -e GOFLAGS=-mod=mod -e CGO_ENABLED=0 \
  -e CLICKHOUSE_TIERED=1 -e CLICKHOUSE_ADDR="$ch:9000" -e CLICKHOUSE_USER=it -e CLICKHOUSE_PASSWORD=it-pass \
  "$go_image" go test -tags integration ./integration/... -count=1 -v
