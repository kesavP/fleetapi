#!/usr/bin/env bash
# Runs the built telemetry-alerts image, as the chart runs it (read-only root, non-root user), against a stub read API that
# doubles as Alertmanager, and checks the whole life of an alert:
#   - it starts on plain Node with the settings it is given and is ready after its first check;
#   - a service whose calls fail goes pending, then firing, and is served to the project's read key (401 without a key, 403 with
#     a wrong one) and exported as a metric;
#   - Alertmanager is told, with the right labels;
#   - the alert survives a restart of the container with its original start time;
#   - when the calls recover it resolves, and Alertmanager is told the end;
#   - no key appears in its logs.
# Needs Docker and network (it pulls node:24-alpine). Builds the image unless IMAGE is given. About 2 minutes.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
pkg="$root/platform-mvp/telemetry-ui"
image="${IMAGE:-}"
id="alerts-test-$$"
KEY="gke-read-key-0123456789abcdef"
fail() { echo "FAIL: $1" >&2; echo "--- alerts logs:" >&2; docker logs "$id-svc" 2>&1 | tail -15 >&2 || true; exit 1; }
work="$(mktemp -d)"
cleanup() { docker rm -f "$id-svc" "$id-stub" >/dev/null 2>&1 || true; docker network rm "$id" >/dev/null 2>&1 || true; chmod -R u+rwX "$work" 2>/dev/null || true; rm -rf "$work"; }
trap cleanup EXIT

if [ -z "$image" ]; then
  image="telemetry-alerts:test-$$"
  echo "== building $image"
  docker build -q -f "$pkg/Dockerfile.alerts" -t "$image" "$pkg" >/dev/null
fi

mkdir -p "$work/projects" "$work/state"
chmod 777 "$work/state"   # the image's user is not the owner of a bind mount; a volume does this for it in the chart
printf '{"projects":[{"name":"gke","ingestKeys":["INGEST-KEY-NOT-FOR-READING"],"readKeys":["%s"]}]}' "$KEY" > "$work/projects/projects.json"

docker network create "$id" >/dev/null
echo "== starting the stub read API (also Alertmanager)"
docker run -d --name "$id-stub" --network "$id" --network-alias telemetry-ingest --network-alias alertmanager \
  -e KEY="$KEY" -e PORT=8080 -v "$pkg":/app:ro -w /app node:24-alpine node server/testing/stub-read-api.js >/dev/null

echo "== starting telemetry-alerts"
docker run -d --name "$id-svc" --network "$id" --network-alias alerts --read-only --tmpfs /tmp \
  -v "$work/projects":/etc/telemetry/projects:ro -v "$work/state":/var/lib/telemetry-alerts \
  -e READ_API=http://telemetry-ingest:8080 -e ALERTMANAGER_URL=http://alertmanager:8080 -e INTERVAL_SECONDS=10 \
  -e STATE_FILE=/var/lib/telemetry-alerts/state.json "$image" >/dev/null

# Requests are made from the stub's container (it has wget and is on the network).
http() { docker exec "$id-stub" wget -qO- "$@" 2>/dev/null; }
status() { docker exec "$id-stub" wget -S -qO /dev/null "$@" 2>&1 | awk '/HTTP\//{print $2; exit}'; }
alerts() { http --header "X-API-Key: $KEY" http://alerts:8081/api/v1/projects/gke/alerts; }
# <description> <seconds> <condition>: the condition is a string, evaluated afresh on every try (a command substitution passed as
# an argument would be evaluated once, before waiting began).
wait_for() {
  local what="$1" secs="$2" cond="$3"
  for _ in $(seq "$secs"); do if eval "$cond" >/dev/null 2>&1; then return 0; fi; sleep 1; done
  fail "timed out waiting for: $what"
}
has_status() { alerts | grep -q "\"status\":\"$1\""; }

wait_for "the service to be ready" 30 '[ "$(status http://alerts:8081/readyz)" = 200 ]'
echo "== ready"
wait_for "a pending or firing alert" 40 'alerts | grep -q "\"rule\":\"errors\""'
wait_for "the alert to fire" 60 'has_status firing'
out="$(alerts)"
grep -q '"service":"/k8s/shop/cartservice"' <<<"$out" || fail "the alert is not on cartservice: $out"
grep -q '"severity":"critical"' <<<"$out" || fail "not critical: $out"
grep -q '"evaluatedBy":"server"' <<<"$out" || fail "not marked as evaluated by the server"
echo "== firing"

echo "== auth"
[ "$(status http://alerts:8081/api/v1/projects/gke/alerts)" = 401 ] || fail "no key should be 401"
[ "$(status --header 'X-API-Key: wrong' http://alerts:8081/api/v1/projects/gke/alerts)" = 403 ] || fail "a wrong key should be 403"
[ "$(status --header "X-API-Key: $KEY" http://alerts:8081/api/v1/projects/other/alerts)" = 403 ] || fail "another project should be 403"
[ "$(status --header 'X-API-Key: INGEST-KEY-NOT-FOR-READING' http://alerts:8081/api/v1/projects/gke/alerts)" = 403 ] || fail "the ingest key must not read alerts"

echo "== metrics"
metrics="$(http http://alerts:8081/metrics)"
grep -q 'telemetry_alerts_firing{project="gke",rule="errors",severity="critical"} 1' <<<"$metrics" || fail "the firing alert is not in /metrics: $metrics"
grep -q 'telemetry_alerts_last_check_ok{project="gke"} 1' <<<"$metrics" || fail "check ok is not 1"

echo "== Alertmanager was told"
posts="$(http http://alertmanager:8080/__posts)"
grep -q '"alertname":"ServiceCallsFailing"' <<<"$posts" || fail "no ServiceCallsFailing post: $posts"
grep -q '"project":"gke"' <<<"$posts" || fail "no project label"

echo "== survives a restart, with its original start"
first="$(alerts | sed -n 's/.*"firstSeen":\([0-9]*\).*/\1/p' | head -1)"
[ -n "$first" ] || fail "no firstSeen"
[ -s "$work/state/state.json" ] || fail "no state file was written"
docker restart "$id-svc" >/dev/null
wait_for "the service to come back" 30 '[ "$(status http://alerts:8081/healthz)" = 200 ]'
after="$(alerts | sed -n 's/.*"firstSeen":\([0-9]*\).*/\1/p' | head -1)"
[ "$after" = "$first" ] || fail "firstSeen changed across the restart: $first -> $after"
has_status firing || fail "the alert is not firing after the restart"

echo "== resolves when the calls recover"
docker exec "$id-stub" wget -qO- --post-data= 'http://alertmanager:8080/__control?broken=false' >/dev/null
wait_for "the alert to resolve" 90 'has_status resolved'
posts="$(http http://alertmanager:8080/__posts)"
[ "$(grep -o '"alertname":"ServiceCallsFailing"' <<<"$posts" | wc -l)" -ge 2 ] || fail "Alertmanager was not re-sent the alert"

echo "== no key in the logs"
logs="$(docker logs "$id-svc" 2>&1)"
for secret in "$KEY" "INGEST-KEY-NOT-FOR-READING"; do
  ! grep -qF "$secret" <<<"$logs" || fail "a key is in the logs"
done
echo "OK: telemetry-alerts image"
