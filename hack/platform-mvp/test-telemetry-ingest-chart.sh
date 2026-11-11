#!/usr/bin/env bash
# Checks the hub chart's telemetry-ingest template, in particular how spokes
# reach it: nothing renders unless enabled; the Service is ClusterIP by default,
# a NodePort (both ports, in range, distinct) for the kind lab, or a
# LoadBalancer with the provider's annotation and source ranges; bad values are
# refused; and the generated projects file agrees with the per-cluster keys.
# Rendering only (needs Docker and network); a cluster is not required.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
chart="$root/deploy/platform-mvp/chart"
helm_image="${HELM_IMAGE:-alpine/helm:3.16.4}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() { echo "FAIL: $1" >&2; exit 1; }
render() { docker run --rm -v "$chart":/chart:ro "$helm_image" template h /chart/hub-services -s templates/telemetry-ingest.yaml "$@"; }

echo "== disabled by default: nothing renders"
out="$(render 2>&1 || true)"
grep -q "could not find template" <<<"$out" || fail "the template rendered while disabled: $out"

service() { # <values...>: print the telemetry-ingest Service as JSON
  render --set telemetryIngest.enabled=true "$@" | python3 -c '
import sys, yaml, json
for d in yaml.safe_load_all(sys.stdin):
    if d and d["kind"] == "Service" and d["metadata"]["name"] == "telemetry-ingest":
        print(json.dumps(d))
'
}

echo "== default: ClusterIP, in-cluster only"
service > "$work/default.json"
python3 - "$work/default.json" <<'PY'
import sys, json
d = json.load(open(sys.argv[1]))
assert d["spec"]["type"] == "ClusterIP", d["spec"]["type"]
assert all("nodePort" not in p for p in d["spec"]["ports"]), "a nodePort was rendered for ClusterIP"
assert "loadBalancerSourceRanges" not in d["spec"]
assert [p["port"] for p in d["spec"]["ports"]] == [80, 4317]
print("ok: ClusterIP, ports 80 and 4317, no node ports")
PY

echo "== NodePort: both ports pinned, so the spokes' URL is stable"
service --set telemetryIngest.service.type=NodePort > "$work/np.json"
python3 - "$work/np.json" <<'PY'
import sys, json
d = json.load(open(sys.argv[1]))
assert d["spec"]["type"] == "NodePort"
ports = {p["name"]: p for p in d["spec"]["ports"]}
assert ports["http"]["nodePort"] == 30081 and ports["http"]["port"] == 80
assert ports["otlp-grpc"]["nodePort"] == 30318 and ports["otlp-grpc"]["port"] == 4317
taken = {30080, 30317, 30909}  # Coroot HTTP and gRPC, metrics-ingest
assert not taken & {30081, 30318}
print("ok: http 30081, otlp-grpc 30318, clear of Coroot (30080, 30317) and metrics-ingest (30909)")
PY

echo "== LoadBalancer: provider annotation and source ranges pass through"
service --set telemetryIngest.service.type=LoadBalancer \
  --set 'telemetryIngest.service.annotations.networking\.gke\.io/load-balancer-type=Internal' \
  --set 'telemetryIngest.service.loadBalancerSourceRanges={10.0.0.0/8,192.168.0.0/16}' > "$work/lb.json"
python3 - "$work/lb.json" <<'PY'
import sys, json
d = json.load(open(sys.argv[1]))
assert d["spec"]["type"] == "LoadBalancer"
assert d["metadata"]["annotations"] == {"networking.gke.io/load-balancer-type": "Internal"}, d["metadata"].get("annotations")
assert d["spec"]["loadBalancerSourceRanges"] == ["10.0.0.0/8", "192.168.0.0/16"]
assert all("nodePort" not in p for p in d["spec"]["ports"]), "pinned node ports must not be rendered for a LoadBalancer"
print("ok: internal load balancer annotation and source ranges rendered")
PY

expect_error() { # <description> <expected message> <values...>
  local desc="$1" want="$2" out
  shift 2
  out="$(render --set telemetryIngest.enabled=true "$@" 2>&1 || true)"
  grep -qF -- "$want" <<<"$out" || fail "$desc: expected an error containing '$want', got: $out"
  echo "ok: $desc"
}
echo "== bad values are refused"
expect_error "unknown type" "must be ClusterIP, NodePort or LoadBalancer" --set telemetryIngest.service.type=ExternalName
expect_error "node port below the range" "outside the NodePort range" --set telemetryIngest.service.type=NodePort --set telemetryIngest.service.nodePort=8080
expect_error "node port above the range" "outside the NodePort range" --set telemetryIngest.service.type=NodePort --set telemetryIngest.service.grpcNodePort=40000
expect_error "same node port twice" "must differ" --set telemetryIngest.service.type=NodePort --set telemetryIngest.service.grpcNodePort=30081

echo "== serviceMonitor.enabled=false drops the Prometheus Operator object (plain GKE has no such CRD)"
render --set telemetryIngest.enabled=true > "$work/with-sm.yaml"
render --set telemetryIngest.enabled=true --set telemetryIngest.serviceMonitor.enabled=false > "$work/no-sm.yaml"
grep -q "kind: ServiceMonitor" "$work/with-sm.yaml" || fail "no ServiceMonitor by default"
if grep -q "kind: ServiceMonitor" "$work/no-sm.yaml"; then fail "a ServiceMonitor was rendered while disabled"; fi
grep -q "kind: StatefulSet" "$work/no-sm.yaml" && grep -q "kind: Deployment" "$work/no-sm.yaml" || fail "the rest of the template went missing"
echo "ok"

echo "== a single project named for this cluster"
render --set telemetryIngest.enabled=true --set telemetryIngest.serviceMonitor.enabled=false --set 'fleet.clusters[0].name=gke' > "$work/gke.yaml"
python3 - "$work/gke.yaml" <<'PY'
import sys, yaml, json
secret = next(d for d in yaml.safe_load_all(open(sys.argv[1])) if d and d["kind"] == "Secret" and d["metadata"]["name"] == "telemetry-ingest-keys")
data = secret["stringData"]
assert sorted(k for k in data if k != "projects.json") == ["gke.ingest", "gke.manage", "gke.read"], sorted(data)
assert [p["name"] for p in json.loads(data["projects.json"])["projects"]] == ["gke"]
print("ok: only gke.ingest, gke.read and gke.manage, and one project")
PY

echo "== the projects file matches the generated keys"
render --set telemetryIngest.enabled=true > "$work/all.yaml"
python3 - "$work/all.yaml" <<'PY'
import sys, yaml, json
secret = next(d for d in yaml.safe_load_all(open(sys.argv[1])) if d and d["kind"] == "Secret" and d["metadata"]["name"] == "telemetry-ingest-keys")
data = secret["stringData"]
projects = json.loads(data["projects.json"])["projects"]
assert [p["name"] for p in projects] == ["us", "eu"], projects
for p in projects:
    assert data[p["name"] + ".ingest"] == p["ingestKeys"][0] and data[p["name"] + ".read"] == p["readKeys"][0]
    assert data[p["name"] + ".manage"] == p["manageKeys"][0], "the projects file and the Secret disagree about the manage key"
    keys = [p["ingestKeys"][0], p["readKeys"][0], p["manageKeys"][0]]
    assert all(len(k) >= 16 for k in keys) and len(set(keys)) == 3, "the three kinds of key must differ (a key is valid for one kind of access)"
print("ok: one ingest and one read key per fleet cluster, matching the projects file")
PY
echo "== cluster details are shared only when asked for"
render --set telemetryIngest.enabled=true | grep -q -- "--expose-cluster-info" && fail "--expose-cluster-info rendered by default"
render --set telemetryIngest.enabled=true --set telemetryIngest.exposeClusterInfo=true | grep -q -- "--expose-cluster-info" || fail "--expose-cluster-info missing when exposeClusterInfo=true"

echo "== incident detection: off by default, and its settings reach the service when on"
render --set telemetryIngest.enabled=true | grep -q -- "--detect-" && fail "a --detect- flag rendered by default"
det="$(render --set telemetryIngest.enabled=true --set telemetryIngest.detection.enabled=true --set telemetryIngest.detection.interval=30s --set telemetryIngest.detection.burn=6 --set telemetryIngest.detection.availability=0.99)"
for want in "--detect-interval=30s" "--detect-burn=6" "--detect-availability=0.99" "--detect-lookback=3h" "--detect-latency=0.999" "--detect-latency-seconds=0.5" "--detect-max-age=30m"; do
  grep -q -- "$want" <<<"$det" || fail "$want missing with detection on"
done
for want in "--detect-silence-after=5m" "--detect-silence-exclude=^(kube-|gke-"; do
  grep -qF -- "$want" <<<"$det" || fail "$want missing with detection on"
done
sil="$(render --set telemetryIngest.enabled=true --set telemetryIngest.detection.enabled=true --set telemetryIngest.detection.silence.after=0 --set telemetryIngest.detection.silence.exclude=)"
grep -qF -- "--detect-silence-after=0" <<<"$sil" || fail "silence cannot be switched off"
grep -qF -- '"--detect-silence-exclude="' <<<"$sil" || fail "an empty exclusion (none) was not passed on: $(grep detect-silence <<<"$sil")"
echo "ok: no detection flags by default; interval, burn, objectives, lookback and the silence settings pass through when enabled"

echo "== tiered storage: off by default, and a bucket plus a Secret when on"
off="$(render --set telemetryIngest.enabled=true)"
for needle in "storage-policy" "cold-after" "GCS_ACCESS_KEY_ID" "telemetry-clickhouse-storage" "storage.googleapis.com"; do
  grep -q -- "$needle" <<<"$off" && fail "'$needle' rendered with tiered storage off"
done
cold=(--set telemetryIngest.enabled=true --set telemetryIngest.clickhouse.coldStorage.enabled=true)
for missing in "bucket" "secret"; do
  case "$missing" in
    bucket) args=("${cold[@]}" --set telemetryIngest.clickhouse.coldStorage.existingSecret=gcs) ;;
    secret) args=("${cold[@]}" --set telemetryIngest.clickhouse.coldStorage.bucket=b) ;;
  esac
  out="$(render "${args[@]}" 2>&1 || true)"
  grep -q "required when coldStorage.enabled" <<<"$out" || fail "a missing $missing was not refused: $out"
done
out="$(render "${cold[@]}" --set telemetryIngest.clickhouse.coldStorage.bucket='b</endpoint><x>' --set telemetryIngest.clickhouse.coldStorage.existingSecret=gcs 2>&1 || true)"
grep -q "is not a bucket name" <<<"$out" || fail "a bucket name that breaks out of the XML was accepted: $out"
out="$(render "${cold[@]}" --set telemetryIngest.clickhouse.coldStorage.bucket=b --set telemetryIngest.clickhouse.coldStorage.existingSecret=gcs --set 'telemetryIngest.clickhouse.coldStorage.path=a"b' 2>&1 || true)"
grep -q "path must not contain" <<<"$out" || fail "a path with a quote was accepted: $out"
render "${cold[@]}" --set telemetryIngest.clickhouse.coldStorage.bucket=my-bkt --set telemetryIngest.clickhouse.coldStorage.existingSecret=my-gcs --set telemetryIngest.clickhouse.coldStorage.path=/tele/ --set telemetryIngest.clickhouse.coldStorage.coldAfter=72h > "$work/cold.yaml"
python3 - "$work/cold.yaml" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
cm = next(d for d in docs if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "telemetry-clickhouse-storage")
xml = cm["data"]["storage.xml"]
assert "<endpoint>https://storage.googleapis.com/my-bkt/tele/</endpoint>" in xml, xml
assert 'from_env="GCS_ACCESS_KEY_ID"' in xml and 'from_env="GCS_SECRET_ACCESS_KEY"' in xml
assert "<support_batch_delete>false</support_batch_delete>" in xml  # GCS has no batch delete
assert "<default><disk>default</disk></default>" in xml  # the volume an existing table's policy must keep
sts = next(d for d in docs if d["kind"] == "StatefulSet")
pod = sts["spec"]["template"]
c = pod["spec"]["containers"][0]
env = {e["name"]: e for e in c["env"]}
for name, key in (("GCS_ACCESS_KEY_ID", "accessKeyId"), ("GCS_SECRET_ACCESS_KEY", "secretAccessKey")):
    ref = env[name]["valueFrom"]["secretKeyRef"]
    assert ref == {"name": "my-gcs", "key": key}, ref
assert "checksum/storage" in pod["metadata"]["annotations"], "ClickHouse would not restart on a configuration change"
mount = next(m for m in c["volumeMounts"] if m["name"] == "storage-config")
assert mount["mountPath"] == "/etc/clickhouse-server/config.d/storage.xml" and mount["subPath"] == "storage.xml"
dep = next(d for d in docs if d["kind"] == "Deployment" and d["metadata"]["name"] == "telemetry-ingest")
args = dep["spec"]["template"]["spec"]["containers"][0]["args"]
for a in ("--storage-policy=tiered", "--cold-volume=cold", "--cold-after=72h"):
    assert a in args, (a, args)
print("ok: bucket disk, storage policy, keys from the Secret, restart on change, service flags")
PY

echo "PASS"
