#!/usr/bin/env bash
# Checks the hub chart's telemetry-ui template: nothing renders unless enabled; the Service type and node port
# are validated; the pod is locked down (non-root, read-only root, no token, writable emptyDirs for nginx);
# the proxy target defaults to the telemetry-ingest Service and can be overridden; and no Secret is rendered
# (the UI holds no key). Rendering only (needs Docker and network).
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
chart="$root/deploy/platform-mvp/chart"
helm_image="${HELM_IMAGE:-alpine/helm:3.16.4}"

fail() { echo "FAIL: $1" >&2; exit 1; }
render() { docker run --rm -v "$chart":/chart:ro "$helm_image" template h /chart/hub-services -s templates/telemetry-ui.yaml "$@"; }
# <values...>: prints "<kind>/<name>" lines then a JSON document of the Deployment and Service for python checks.
docs() { render --set telemetryUi.enabled=true "$@" | python3 -c '
import sys, yaml, json
print(json.dumps([d for d in yaml.safe_load_all(sys.stdin) if d]))'; }
check() { python3 -c "
import sys, json
docs = {d['kind']: d for d in json.load(sys.stdin)}
dep, svc = docs['Deployment'], docs['Service']
c = dep['spec']['template']['spec']['containers'][0]
$1" ; }

echo "== disabled by default: nothing renders"
out="$(render 2>&1 || true)"
grep -q "could not find template" <<<"$out" || fail "rendered while disabled: $out"

echo "== defaults: ClusterIP, locked-down pod, proxy to telemetry-ingest"
docs | check '
assert svc["spec"]["type"] == "ClusterIP", svc["spec"]
assert "nodePort" not in svc["spec"]["ports"][0]
sc = c["securityContext"]
assert sc["readOnlyRootFilesystem"] is True and sc["runAsNonRoot"] is True and sc["allowPrivilegeEscalation"] is False
assert sc["capabilities"]["drop"] == ["ALL"]
ps = dep["spec"]["template"]["spec"]
assert ps["automountServiceAccountToken"] is False
assert ps["securityContext"]["fsGroup"] == sc["runAsUser"] == 101
mounts = {m["mountPath"] for m in c["volumeMounts"]}
assert mounts == {"/etc/nginx/conf.d", "/tmp", "/var/cache/nginx"}, mounts
env = {e["name"]: e["value"] for e in c["env"]}
assert env["API_UPSTREAM"] == "http://telemetry-ingest.monitoring.svc.cluster.local", env
assert c["readinessProbe"]["httpGet"]["path"] == "/healthz"
' || fail "defaults"

echo "== no Secret or key is rendered"
render --set telemetryUi.enabled=true | grep -qE '^kind: Secret' && fail "a Secret was rendered"

echo "== apiUpstream and namespaces are honoured"
docs --set telemetryUi.apiUpstream=http://api.example:9000 | check '
assert {e["name"]: e["value"] for e in c["env"]}["API_UPSTREAM"] == "http://api.example:9000"' || fail "apiUpstream"
docs --set telemetryIngest.namespace=obs --set telemetryUi.namespace=ui | check '
assert svc["metadata"]["namespace"] == "ui"
assert {e["name"]: e["value"] for e in c["env"]}["API_UPSTREAM"] == "http://telemetry-ingest.obs.svc.cluster.local"' || fail "namespaces"

echo "== NodePort and LoadBalancer"
docs --set telemetryUi.service.type=NodePort | check '
assert svc["spec"]["type"] == "NodePort" and svc["spec"]["ports"][0]["nodePort"] == 30082' || fail "NodePort"
docs --set telemetryUi.service.type=LoadBalancer \
     --set 'telemetryUi.service.loadBalancerSourceRanges={10.0.0.0/8}' \
     --set-string 'telemetryUi.service.annotations.networking\.gke\.io/load-balancer-type=Internal' | check '
assert svc["spec"]["type"] == "LoadBalancer"
assert svc["spec"]["loadBalancerSourceRanges"] == ["10.0.0.0/8"]
assert svc["metadata"]["annotations"]["networking.gke.io/load-balancer-type"] == "Internal"' || fail "LoadBalancer"

echo "== bad values are refused"
out="$(render --set telemetryUi.enabled=true --set telemetryUi.service.type=Bogus 2>&1 || true)"
grep -q "must be ClusterIP, NodePort or LoadBalancer" <<<"$out" || fail "bad type accepted: $out"
out="$(render --set telemetryUi.enabled=true --set telemetryUi.service.type=NodePort --set telemetryUi.service.nodePort=80 2>&1 || true)"
grep -q "outside the NodePort range" <<<"$out" || fail "bad nodePort accepted: $out"

echo "OK: telemetry-ui chart"
