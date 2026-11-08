#!/usr/bin/env bash
# Runs the built telemetry-ui image (as the chart runs it: uid 101, read-only root, writable tmpfs for the
# directories nginx writes) in front of a stub API, and checks: the app and SPA routes are served; /api GETs are
# proxied with the X-API-Key header intact and the path unchanged; non-GET is refused; the agents' ingest paths
# and other /api paths are not reachable; the alerts path (and only it) goes to the alert service, with look-alike paths staying
# with the read API and a 502 when that service is absent; security headers and no-store are set; and the container starts
# although the API does not exist yet (the name is resolved per request).
# Needs Docker and network. Builds the image unless IMAGE is given.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
image="${IMAGE:-}"
net="uitest-$$"
fail() { echo "FAIL: $1" >&2; exit 1; }
cleanup() { docker rm -f "$net-ui" "$net-stub" "$net-alerts" >/dev/null 2>&1 || true; docker network rm "$net" >/dev/null 2>&1 || true; }
trap cleanup EXIT

if [ -z "$image" ]; then
  image="telemetry-ui:test-$$"
  echo "== building $image"
  docker build -q -t "$image" "$root/platform-mvp/telemetry-ui" >/dev/null
fi

work="$(mktemp -d)"; trap 'cleanup; rm -rf "$work"' EXIT
cat > "$work/stub.py" <<'PY'
import http.server, json, os
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(s):
        b = json.dumps({"who": os.environ.get("WHO"), "path": s.path, "key": s.headers.get("X-API-Key")}).encode()
        s.send_response(200); s.send_header("Content-Type", "application/json"); s.end_headers(); s.wfile.write(b)
    def log_message(*a): pass
http.server.HTTPServer(("", 80), H).serve_forever()
PY
cat > "$work/probe.sh" <<'SH'
U=http://ui:8080
code() { curl -s -o /dev/null -w "%{http_code}" "$@"; }
echo "proxy=$(curl -s -H 'X-API-Key: k1' "$U/api/v1/projects/gke/metrics?x=1")"
echo "health=$(code $U/healthz) index=$(code $U/) spa=$(code $U/some/route) ingest=$(code $U/v1/metrics)"
echo "otherapi=$(code $U/api/v1/other) post=$(code -X POST $U/api/v1/projects/gke/metrics) asset=$(code $U/assets/nope.js)"
echo "indexhdr=$(curl -sI $U/ | tr -d '\r' | tr 'A-Z' 'a-z' | grep -E '^(content-security-policy|x-frame-options|cache-control):' | cut -d: -f1 | sort | tr '\n' ,)"
echo "alerts=$(curl -s -H 'X-API-Key: k2' "$U/api/v1/projects/gke/alerts")"
echo "lookalikes=$(curl -s -H 'X-API-Key: k' "$U/api/v1/projects/gke/alerts/extra" | sed 's/.*"who": "\([a-z]*\)".*/\1/') $(curl -s -H 'X-API-Key: k' "$U/api/v1/projects/gke/alertsx" | sed 's/.*"who": "\([a-z]*\)".*/\1/') $(curl -s -H 'X-API-Key: k' "$U/api/v1/projects/alerts/metrics" | sed 's/.*"who": "\([a-z]*\)".*/\1/')"
echo "alertspost=$(code -X POST $U/api/v1/projects/gke/alerts)"
echo "apihdr=$(curl -sI -H 'X-API-Key: k' $U/api/v1/projects/gke/metrics | tr -d '\r' | tr 'A-Z' 'a-z' | grep -E '^(cache-control|x-content-type-options):' | cut -d: -f1 | sort | tr '\n' ,)"
SH

docker network create "$net" >/dev/null
opts="uid=101,gid=101,mode=1777"
run_ui() { docker run -d --name "$net-ui" --network "$net" --network-alias ui --read-only \
  --tmpfs /tmp:$opts --tmpfs /var/cache/nginx:$opts --tmpfs /etc/nginx/conf.d:$opts --user 101:101 "$@" "$image" >/dev/null; }

echo "== starts without the API (resolved per request, not at start-up)"
run_ui -e API_UPSTREAM=http://telemetry-ingest
sleep 3
[ "$(docker inspect -f '{{.State.Running}}' "$net-ui")" = true ] || { docker logs "$net-ui" >&2; fail "did not start without its API"; }
docker rm -f "$net-ui" >/dev/null

echo "== behaviour against a stub API"
docker run -d --name "$net-stub" --network "$net" --network-alias telemetry-ingest -e WHO=ingest -v "$work/stub.py":/s.py:ro python:3.12-alpine python /s.py >/dev/null
docker run -d --name "$net-alerts" --network "$net" --network-alias telemetry-alerts -e WHO=alerts -v "$work/stub.py":/s.py:ro python:3.12-alpine python /s.py >/dev/null
run_ui
sleep 3
out="$(docker run --rm --network "$net" -v "$work/probe.sh":/p.sh:ro --entrypoint sh curlimages/curl /p.sh)"
echo "$out" | sed 's/^/   /'
has() { grep -qF -- "$1" <<<"$out" || fail "expected: $1"; }
has 'proxy={"who": "ingest", "path": "/api/v1/projects/gke/metrics?x=1", "key": "k1"}'
has 'health=200 index=200 spa=200 ingest=404'
has 'otherapi=404 post=405 asset=404'
has 'indexhdr=cache-control,content-security-policy,x-frame-options,'
has 'apihdr=cache-control,x-content-type-options,'
has 'alerts={"who": "alerts", "path": "/api/v1/projects/gke/alerts", "key": "k2"}'
has 'lookalikes=ingest ingest ingest'
has 'alertspost=405'

echo "== the alerts path answers 502 when the alert service is not there (the page then checks for itself)"
docker rm -f "$net-alerts" >/dev/null
code="$(docker run --rm --network "$net" --entrypoint sh curlimages/curl -c 'curl -s -o /dev/null -w "%{http_code}" -H "X-API-Key: k" http://ui:8080/api/v1/projects/gke/alerts')"
[ "$code" = 502 ] || fail "expected 502 without the alert service, got $code"
code="$(docker run --rm --network "$net" --entrypoint sh curlimages/curl -c 'curl -s -o /dev/null -w "%{http_code}" -H "X-API-Key: k" http://ui:8080/api/v1/projects/gke/metrics')"
[ "$code" = 200 ] || fail "the rest of the API must still work, got $code"
echo "OK: telemetry-ui image"
