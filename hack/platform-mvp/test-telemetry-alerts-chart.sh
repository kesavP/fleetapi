#!/usr/bin/env bash
# Checks the hub chart's telemetry-alerts template: nothing renders unless enabled; the pod is locked down and mounts only the
# projects file of the keys Secret; settings reach the container as the service reads them; persistence swaps the state volume
# and the rollout strategy; a bad namespace is refused; and the UI's proxy points its alerts path at the service.
# Rendering only (needs Docker and network).
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
chart="$root/deploy/platform-mvp/chart"
helm_image="${HELM_IMAGE:-alpine/helm:3.16.4}"

fail() { echo "FAIL: $1" >&2; exit 1; }
render() { # <template> <values...>
  local t="$1"; shift
  docker run --rm -v "$chart":/chart:ro "$helm_image" template h /chart/hub-services -s "templates/$t.yaml" "$@"
}
docs() { render telemetry-alerts --set telemetryAlerts.enabled=true "$@" | python3 -c '
import sys, yaml, json
print(json.dumps([d for d in yaml.safe_load_all(sys.stdin) if d]))'; }
check() { python3 -c "
import sys, json
docs = {d['kind']: d for d in json.load(sys.stdin)}
dep, svc = docs['Deployment'], docs['Service']
pod = dep['spec']['template']['spec']
c = pod['containers'][0]
env = {e['name']: e.get('value') for e in c['env']}
$1"; }

echo "== disabled by default: nothing renders"
out="$(render telemetry-alerts 2>&1 || true)"
grep -q "could not find template" <<<"$out" || fail "rendered while disabled: $out"

echo "== defaults"
docs | check '
assert set(docs) == {"Service", "Deployment"}, set(docs)
assert svc["spec"]["type"] == "ClusterIP" and svc["spec"]["ports"][0]["targetPort"] == "http"
assert dep["spec"]["replicas"] == 1
assert "strategy" not in dep["spec"]
sc = c["securityContext"]
assert sc["readOnlyRootFilesystem"] is True and sc["runAsNonRoot"] is True and sc["allowPrivilegeEscalation"] is False
assert sc["capabilities"]["drop"] == ["ALL"] and sc["runAsUser"] == 1000
assert pod["automountServiceAccountToken"] is False and pod["securityContext"]["fsGroup"] == 1000
assert env["READ_API"] == "http://telemetry-ingest.monitoring.svc.cluster.local", env
assert env["INTERVAL_SECONDS"] == "60" and env["PORT"] == "8081"
assert "ALERTMANAGER_URL" not in env and "RULES_JSON" not in env
assert c["readinessProbe"]["httpGet"]["path"] == "/readyz" and c["livenessProbe"]["httpGet"]["path"] == "/healthz"
mounts = {m["mountPath"]: m.get("readOnly", False) for m in c["volumeMounts"]}
assert mounts == {"/etc/telemetry/projects": True, "/var/lib/telemetry-alerts": False, "/tmp": False}, mounts
vols = {v["name"]: v for v in pod["volumes"]}
# only projects.json of the keys Secret is mounted, never the whole Secret (it holds every ingest key too)
assert vols["projects"]["secret"]["secretName"] == "telemetry-ingest-keys"
assert vols["projects"]["secret"]["items"] == [{"key": "projects.json", "path": "projects.json"}]
assert "emptyDir" in vols["state"]
' || fail "defaults"

echo "== no Secret is rendered (it reads the existing keys, it makes none)"
render telemetry-alerts --set telemetryAlerts.enabled=true | grep -qE '^kind: Secret' && fail "a Secret was rendered"

echo "== settings reach the container"
docs --set telemetryAlerts.intervalSeconds=30 --set telemetryAlerts.alertmanagerUrl=http://am.monitoring.svc:9093 \
     --set telemetryAlerts.readApi=http://api:9000 --set telemetryAlerts.thresholds.errorShare=10 --set telemetryAlerts.thresholds.baselineSeconds=7200 | check '
assert env["INTERVAL_SECONDS"] == "30"
assert env["ALERTMANAGER_URL"] == "http://am.monitoring.svc:9093"
assert env["READ_API"] == "http://api:9000"
assert json.loads(env["RULES_JSON"]) == {"errorShare": 10, "baselineSeconds": 7200}, env["RULES_JSON"]
' || fail "settings"

echo "== persistence: a volume, and Recreate because a ReadWriteOnce volume cannot be mounted twice"
render telemetry-alerts --set telemetryAlerts.enabled=true --set telemetryAlerts.persistence.enabled=true --set telemetryAlerts.persistence.size=1Gi \
  --set telemetryAlerts.persistence.storageClassName=standard | python3 -c '
import sys, yaml
d = {x["kind"]: x for x in yaml.safe_load_all(sys.stdin) if x}
pvc, dep = d["PersistentVolumeClaim"], d["Deployment"]
assert pvc["spec"]["resources"]["requests"]["storage"] == "1Gi" and pvc["spec"]["storageClassName"] == "standard"
assert pvc["spec"]["accessModes"] == ["ReadWriteOnce"]
assert dep["spec"]["strategy"] == {"type": "Recreate"}
vols = {v["name"]: v for v in dep["spec"]["template"]["spec"]["volumes"]}
assert vols["state"]["persistentVolumeClaim"]["claimName"] == "telemetry-alerts-state"
' || fail "persistence"

echo "== ServiceMonitor only when asked"
render telemetry-alerts --set telemetryAlerts.enabled=true | grep -q "kind: ServiceMonitor" && fail "ServiceMonitor rendered by default"
render telemetry-alerts --set telemetryAlerts.enabled=true --set telemetryAlerts.serviceMonitor.enabled=true | grep -q "kind: ServiceMonitor" || fail "ServiceMonitor missing"

echo "== a namespace other than the keys Secret's is refused"
out="$(render telemetry-alerts --set telemetryAlerts.enabled=true --set telemetryAlerts.namespace=elsewhere 2>&1 || true)"
grep -q "must equal telemetryIngest.namespace" <<<"$out" || fail "namespace mismatch accepted: $out"

echo "== the UI's proxy sends the alerts path to this service"
render telemetry-ui --set telemetryUi.enabled=true | python3 -c '
import sys, yaml
d = {x["kind"]: x for x in yaml.safe_load_all(sys.stdin) if x}
env = {e["name"]: e["value"] for e in d["Deployment"]["spec"]["template"]["spec"]["containers"][0]["env"]}
assert env["ALERTS_UPSTREAM"] == "http://telemetry-alerts.monitoring.svc.cluster.local", env
' || fail "UI default"
render telemetry-ui --set telemetryUi.enabled=true --set telemetryUi.alertsUpstream=http://x:1 | grep -q 'http://x:1' || fail "alertsUpstream override"

echo "OK: telemetry-alerts chart"
