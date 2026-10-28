#!/usr/bin/env bash
# End-to-end check of the spoke pod-metrics path, with one k3s cluster standing
# in for both sides: the rendered hub proxy (metrics-ingest) and the rendered
# spoke agent (kube-state-metrics + Prometheus agent) run in it, and a real
# Prometheus outside receives the remote-written data and evaluates the hub's
# rendered alert rules. A crash-looping pod and a pod stuck Pending are the
# workloads. The credential is copied with the real copy-metrics-credentials.sh
# through a kubectl wrapper. Needs Docker; takes a few minutes.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
chart="$root/deploy/platform-mvp/chart"
k3s_image="${K3S_IMAGE:-rancher/k3s:v1.31.4-k3s1}"
prom_image="${PROM_IMAGE:-prom/prometheus:v2.53.0}"
ksm_image="${KSM_IMAGE:-registry.k8s.io/kube-state-metrics/kube-state-metrics:v2.19.1}"
nginx_image="${NGINX_IMAGE:-nginxinc/nginx-unprivileged:1.27-alpine}"
busybox_image="${BUSYBOX_IMAGE:-busybox:1.36}"
helm_image="${HELM_IMAGE:-alpine/helm:3.16.2}"
curl_image="${CURL_IMAGE:-curlimages/curl:8.11.1}"
net="metricstest-$$"
k3s="$net-k3s"
prom="$net-prom"

work="$(mktemp -d)"
cleanup() {
  docker rm -f "$k3s" "$prom" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
chmod 755 "$work"

fail() { echo "FAIL: $1" >&2; exit 1; }
kc() { docker exec -i "$k3s" kubectl "$@"; }
helm() { docker run --rm -v "$chart":/chart:ro "$helm_image" "$@"; }

# query <promql>: the first sample's value, or empty when there is no result.
query() {
  local q; q="$(python3 -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1]))' "$1")"
  docker exec "$prom" wget -qO- "http://localhost:9090/api/v1/query?query=$q" | python3 -c "
import sys, json
r = json.load(sys.stdin)['data']['result']
print(r[0]['value'][1] if r else '')"
}
# wait_for <description> <promql> [timeout-seconds]: polls until the query returns something.
wait_for() {
  local deadline=$((SECONDS + ${3:-150}))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ -n "$(query "$2")" ] && { echo "ok: $1"; return 0; }
    sleep 4
  done
  fail "$1 (query never returned a result: $2)"
}
firing() { query "ALERTS{alertstate=\"firing\",alertname=\"$1\",$2}"; }

docker network create "$net" >/dev/null

# Short waits, and the only fleet cluster is the one under test.
helm template t /chart/hub-services --set-json 'fleet.clusters=[{"name":"test"}]' \
  --set spokeMetrics.crashLoopFor=15s --set spokeMetrics.notReadyFor=15s --set spokeMetrics.missingFor=15s \
  > "$work/hub.yaml"

python3 - "$work" <<'PY'
import sys, yaml
work = sys.argv[1]
docs = [d for d in yaml.safe_load_all(open(work + "/hub.yaml")) if d]
rule = next(d for d in docs if d["kind"] == "PrometheusRule" and d["metadata"]["name"] == "spoke-metrics")
yaml.safe_dump({"groups": rule["spec"]["groups"]}, open(work + "/rules.yaml", "w"))
open(work + "/prometheus.yml", "w").write("global: {scrape_interval: 5s, evaluation_interval: 5s}\nrule_files: [/etc/prom/rules.yaml]\n")
PY
chmod -R a+rX "$work"

docker run -d --name "$prom" --network "$net" -v "$work":/etc/prom:ro "$prom_image" \
  --config.file=/etc/prom/prometheus.yml --storage.tsdb.path=/tmp/tsdb --web.enable-remote-write-receiver >/dev/null
prom_ip="$(docker inspect -f "{{(index .NetworkSettings.Networks \"$net\").IPAddress}}" "$prom")"

# Pods resolve names through CoreDNS, which cannot see Docker's network, so the
# proxy's upstream is the Prometheus container's address.
helm template t /chart/hub-services --set-json 'fleet.clusters=[{"name":"test"}]' \
  --set "metricsIngest.upstream=http://$prom_ip:9090" > "$work/hub.yaml"
helm template t /chart/spoke --show-only templates/metrics-agent.yaml \
  --set metricsAgent.enabled=true --set metricsAgent.clusterName=test \
  --set metricsAgent.remoteWriteURL=http://metrics-ingest.monitoring.svc/api/v1/write \
  --set metricsAgent.scrapeInterval=10s > "$work/spoke.yaml"

python3 - "$work" <<'PY'
import sys, yaml
work = sys.argv[1]
docs = [d for d in yaml.safe_load_all(open(work + "/hub.yaml")) if d]
keep = [d for d in docs if d["metadata"]["name"] in ("metrics-ingest", "metrics-ingest-auth")]
yaml.safe_dump_all(keep, open(work + "/ingest.yaml", "w"))
PY
chmod -R a+rX "$work"

docker run -d --name "$k3s" --hostname "$k3s" --network "$net" --privileged --tmpfs /run --tmpfs /var/run \
  "$k3s_image" server --disable=traefik --disable=servicelb --disable=metrics-server >/dev/null
for _ in $(seq 1 60); do kc get nodes 2>/dev/null | grep -q " Ready" && break; sleep 3; done
kc get nodes 2>/dev/null | grep -q " Ready" || fail "k3s did not become ready"

echo "== loading images into the cluster"
# Only this machine's platform is exported: with Docker's containerd image
# store a plain `docker save` carries a multi-platform index whose other
# blobs are absent, and ctr refuses to import it.
case "$(uname -m)" in aarch64|arm64) platform=linux/arm64 ;; *) platform=linux/amd64 ;; esac
docker save --platform "$platform" "$prom_image" "$ksm_image" "$nginx_image" "$busybox_image" \
  | docker exec -i "$k3s" ctr -n k8s.io images import - >/dev/null \
  || fail "could not import the images into k3s"

echo "== hub side: the ingest proxy, in the hub's monitoring namespace"
kc create namespace monitoring >/dev/null
docker cp "$work/ingest.yaml" "$k3s":/tmp/ingest.yaml
kc apply -f /tmp/ingest.yaml >/dev/null || fail "the rendered ingest manifests were rejected"
kc -n monitoring rollout status deployment/metrics-ingest --timeout=120s >/dev/null || { kc -n monitoring describe pods >&2; fail "metrics-ingest did not become ready"; }
echo "ok: metrics-ingest runs with its read-only, non-root security context"

echo "== the spoke gets the hub's credential through the real copy script"
cat > "$work/kubectl" <<WRAP
#!/usr/bin/env bash
# Stands in for kubectl with two contexts: both map to the one k3s cluster.
args=()
while [ \$# -gt 0 ]; do
  if [ "\$1" = --context ]; then shift 2; else args+=("\$1"); shift; fi
done
exec docker exec -i "$k3s" kubectl "\${args[@]}"
WRAP
chmod 755 "$work/kubectl"
KUBECTL="$work/kubectl" "$root/hack/platform-mvp/copy-metrics-credentials.sh" spoke-ctx hub-ctx >/dev/null \
  || fail "copy-metrics-credentials.sh failed"
kc -n default get secret hub-metrics-credentials >/dev/null || fail "the spoke secret was not created"
echo "ok: the credential reached the spoke"

echo "== spoke side: kube-state-metrics and the agent, plus two workloads"
docker cp "$work/spoke.yaml" "$k3s":/tmp/spoke.yaml
kc apply -f /tmp/spoke.yaml >/dev/null || fail "the rendered spoke manifests were rejected"
kc create namespace acme-corp >/dev/null
kc apply -f - >/dev/null <<WORKLOADS
apiVersion: v1
kind: Pod
metadata: {name: crasher, namespace: acme-corp}
spec:
  containers:
    - {name: c, image: $busybox_image, imagePullPolicy: IfNotPresent, command: [sh, -c, "exit 1"]}
---
apiVersion: v1
kind: Pod
metadata: {name: stuck, namespace: acme-corp}
spec:
  nodeSelector: {does-not: exist}
  containers:
    - {name: c, image: $busybox_image, imagePullPolicy: IfNotPresent, command: [sleep, "3600"]}
WORKLOADS
kc -n default rollout status deployment/metrics-agent --timeout=120s >/dev/null || { kc -n default describe pods >&2; fail "the agent did not become ready"; }
kc -n default rollout status deployment/kube-state-metrics --timeout=120s >/dev/null || {
  kc -n default get events --sort-by=.lastTimestamp 2>&1 | tail -8 >&2
  kc -n default logs deployment/kube-state-metrics 2>&1 | tail -5 >&2
  fail "kube-state-metrics did not become ready"
}
echo "ok: the agent and kube-state-metrics run"

echo "== pod metrics arrive at the hub, labelled with the cluster"
wait_for "pod state from the spoke carries cluster=test" 'kube_pod_info{cluster="test",pod="crasher"}'
wait_for "pod memory from the kubelet carries cluster=test" 'container_memory_working_set_bytes{cluster="test",pod!=""}'
[ -z "$(query 'container_memory_working_set_bytes{cluster="test",pod=""}')" ] || fail "cgroup-level series without a pod label should be dropped"
echo "ok: series without a pod label are dropped"
[ -z "$(query 'kube_pod_created{cluster="test"}')" ] || fail "kube_pod_created is not on the allow-list and should be dropped"
echo "ok: the allow-list drops series the hub does not use"

echo "== alerts fire on real workloads"
wait_for "SpokePodCrashLooping fires for the crash-looping pod" 'ALERTS{alertstate="firing",alertname="SpokePodCrashLooping",cluster="test",pod="crasher"}' 200
wait_for "SpokePodNotReady fires for the pod stuck Pending" 'ALERTS{alertstate="firing",alertname="SpokePodNotReady",cluster="test",pod="stuck",phase="Pending"}' 120
[ -z "$(firing SpokeMetricsMissing 'cluster="test"')" ] || fail "metrics are arriving, so no missing-metrics alert is expected"
echo "ok: no missing-metrics alert while the spoke is sending"

echo "== the proxy only lets an authenticated write through"
ingest="http://$k3s:30909"
code() { docker run --rm --network "$net" "$curl_image" -s -o /dev/null -w '%{http_code}' "$@"; }
pw="$(kc -n monitoring get secret metrics-ingest-auth -o go-template='{{index .data "password"}}' | base64 -d)"
[ "$(code -X POST "$ingest/api/v1/write")" = 401 ] || fail "a write without credentials must be refused with 401"
[ "$(code -X POST -u "spoke-metrics:wrong" "$ingest/api/v1/write")" = 401 ] || fail "a write with a wrong password must be refused with 401"
echo "ok: 401 without credentials and with a wrong password"
c="$(code -X POST -u "spoke-metrics:$pw" "$ingest/api/v1/write")"
{ [ "$c" != 401 ] && [ "$c" != 403 ] && [ "$c" != 404 ]; } || fail "a write with the right credentials was refused ($c)"
echo "ok: the right credentials reach Prometheus (it answered $c to an empty body)"
c="$(code "$ingest/api/v1/query?query=up")"
[ "$c" = 404 ] || fail "paths other than /api/v1/write must not be proxied (got $c)"
echo "ok: Prometheus's query API is not reachable through the proxy"

echo "PASS"
