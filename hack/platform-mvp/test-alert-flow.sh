#!/usr/bin/env bash
# End-to-end check of the alert path: a real Alertmanager, configured from the
# infrastructure chart values (only timers and the webhook address changed),
# sending to a real alert-router built from its Dockerfile. Needs Docker.
# ROUTER_ARGS passes extra flags to the router, to see a check fail on purpose.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
am_image="${AM_IMAGE:-prom/alertmanager:v0.27.0}"
curl_image="${CURL_IMAGE:-curlimages/curl:8.11.1}"
router_image="alert-router:flowtest"
net="alertflow-$$"

work="$(mktemp -d)"
cleanup() {
  docker rm -f "$net-router" "$net-am" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
chmod 755 "$work"

docker build -q -t "$router_image" "$root/platform-mvp/alert-router" >/dev/null
docker network create "$net" >/dev/null

mkdir -p "$work/secrets/alert-router-token" "$work/config"
head -c 24 /dev/urandom | base64 | tr -d '=+/\n' > "$work/secrets/alert-router-token/token"

python3 - "$root/deploy/platform-mvp/chart/infrastructure/values.yaml" "$work/alertmanager.yml" "$net-router" <<'PY'
import sys, yaml
v = yaml.safe_load(open(sys.argv[1]))
cfg = v["kube-prometheus-stack"]["alertmanager"]["config"]
cfg["route"]["group_wait"], cfg["route"]["group_interval"] = "1s", "3s"
hook = next(r for r in cfg["receivers"] if r["name"] == "alert-router")["webhook_configs"][0]
hook["url"] = f"http://{sys.argv[3]}:8080/alerts"
hook["http_config"]["authorization"]["credentials_file"] = "/t/secrets/alert-router-token/token"
cfg["templates"] = []
yaml.safe_dump(cfg, open(sys.argv[2], "w"))
PY
chmod -R a+rX "$work"

docker run -d --name "$net-router" --network "$net" -v "$work/secrets/alert-router-token":/tok:ro \
  "$router_image" --token-file=/tok/token ${ROUTER_ARGS:-} >/dev/null
docker run -d --name "$net-am" --network "$net" -v "$work":/t:ro "$am_image" \
  --config.file=/t/alertmanager.yml --storage.path=/tmp/am >/dev/null

http() { docker run --rm --network "$net" "$curl_image" -s "$@"; }

for _ in $(seq 1 30); do
  http -o /dev/null -f "http://$net-am:9093/-/ready" && break
  sleep 1
done

now() { date -u +%Y-%m-%dT%H:%M:%SZ; }
# post <alertname> <region> [resolved]: a resolving post ends the alert in the
# past, so it also has to start earlier.
post() {
  local starts ends=""
  starts="$(now)"
  if [ "${3:-}" = resolved ]; then
    starts="$(date -u -d '-5 minutes' +%Y-%m-%dT%H:%M:%SZ)"
    ends=",\"endsAt\":\"$(date -u -d '-1 minute' +%Y-%m-%dT%H:%M:%SZ)\""
  fi
  http -f -X POST -H 'Content-Type: application/json' "http://$net-am:9093/api/v2/alerts" \
    -d "[{\"labels\":{\"alertname\":\"$1\",\"severity\":\"warning\",\"tenant\":\"acme-corp\",\"vmset\":\"web\",\"region\":\"$2\"},\"annotations\":{\"summary\":\"$1 in $2\"},\"startsAt\":\"$starts\"$ends}]" >/dev/null
}
# post_cluster <alertname> <cluster> <severity>: an alert about a cluster, as
# the spoke API probes raise them.
post_cluster() {
  http -f -X POST -H 'Content-Type: application/json' "http://$net-am:9093/api/v2/alerts" \
    -d "[{\"labels\":{\"alertname\":\"$1\",\"severity\":\"$3\",\"cluster\":\"$2\"},\"annotations\":{\"summary\":\"$1 for $2\"},\"startsAt\":\"$(now)\"}]" >/dev/null
}
logs() { docker logs "$net-router" 2>&1; }
count() { logs | grep -c "$1" || true; }
fail() { echo "FAIL: $1" >&2; echo "--- router log:" >&2; logs >&2; exit 1; }
expect_count() { [ "$(count "$1")" -eq "$2" ] || fail "$3 (wanted $2 lines matching '$1', got $(count "$1"))"; echo "ok: $3"; }

echo "== firing alerts: one cause plus its symptom, and the always-firing Watchdog"
post VmSetNotReady us
post VmSetPodsNotRunning us
http -f -X POST -H 'Content-Type: application/json' "http://$net-am:9093/api/v2/alerts" \
  -d "[{\"labels\":{\"alertname\":\"Watchdog\",\"severity\":\"none\"},\"startsAt\":\"$(now)\"}]" >/dev/null
sleep 9
expect_count 'alert=VmSetPodsNotRunning status=firing.*region=us' 1 "the cause is logged once"
expect_count 'alert=VmSetNotReady' 0 "the symptom is inhibited by the cause"
expect_count 'alert=Watchdog' 0 "Watchdog goes to the null receiver"

echo "== a second region joins the same group: the unchanged first alert is a repeat"
post VmSetPodsNotRunning eu
sleep 9
expect_count 'alert=VmSetPodsNotRunning status=firing.*region=us' 1 "the unchanged us alert is not logged again"
expect_count 'alert=VmSetPodsNotRunning status=firing.*region=eu' 1 "the new eu alert is logged"
http "http://$net-router:8080/metrics" | grep -Eq '^alert_router_alerts_deduped_total [1-9]' || fail "deduped counter did not increase"
echo "ok: dedupe is counted in metrics"

echo "== resolving passes through even though the router saw the alert recently"
post VmSetPodsNotRunning us resolved
sleep 9
expect_count 'alert=VmSetPodsNotRunning status=resolved.*region=us' 1 "the resolution is logged"

echo "== spoke API probe alerts: an unreachable API server also looks slow"
post_cluster SpokeAPIUnreachable us critical
post_cluster SpokeAPISlow us warning
sleep 9
expect_count 'alert=SpokeAPIUnreachable status=firing severity=critical.*cluster=us' 1 "the unreachable alert is logged with its cluster"
expect_count 'alert=SpokeAPISlow' 0 "the slow alert is inhibited while the cluster is unreachable"
post_cluster SpokeAPISlow eu warning
sleep 9
expect_count 'alert=SpokeAPISlow status=firing severity=warning.*cluster=eu' 1 "a slow API on another cluster is not inhibited"

echo "== the router refuses callers without the token"
code="$(http -o /dev/null -w '%{http_code}' -X POST "http://$net-router:8080/alerts" -d '{}')"
[ "$code" = 401 ] || fail "unauthenticated request got $code, want 401"
echo "ok: 401 without a token"

echo "PASS"
