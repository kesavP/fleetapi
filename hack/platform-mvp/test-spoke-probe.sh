#!/usr/bin/env bash
# Checks the spoke API probes against a real kube-apiserver (k3s in a
# container): the Probe objects are valid against the prometheus-operator CRD,
# the blackbox module from the chart probes a live API server successfully,
# rejects a non-TLS endpoint, and the alerts fire when the API goes away.
# Prometheus here uses a scrape config equivalent to what the operator derives
# from a Probe; the operator itself is not part of this test. Needs Docker.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
chart="$root/deploy/platform-mvp/chart"
k3s_image="${K3S_IMAGE:-rancher/k3s:v1.31.4-k3s1}"
bb_image="${BB_IMAGE:-prom/blackbox-exporter:v0.25.0}"
prom_image="${PROM_IMAGE:-prom/prometheus:v2.53.0}"
helm_image="${HELM_IMAGE:-alpine/helm:3.16.2}"
curl_image="${CURL_IMAGE:-curlimages/curl:8.11.1}"
net="probetest-$$"

work="$(mktemp -d)"
cleanup() {
  docker rm -f "$net-k3s" "$net-bb" "$net-prom" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
chmod 755 "$work"

fail() { echo "FAIL: $1" >&2; exit 1; }
http() { docker run --rm --network "$net" "$curl_image" -s "$@"; }
query() { http -G "http://$net-prom:9090/api/v1/query" --data-urlencode "query=$1" | python3 -c "
import sys, json
r = json.load(sys.stdin)['data']['result']
print(r[0]['value'][1] if r else '')"; }
alerts() { http "http://$net-prom:9090/api/v1/alerts" | python3 -c "
import sys, json
for a in json.load(sys.stdin)['data']['alerts']:
    if a['state'] == 'firing': print(a['labels']['alertname'], a['labels'].get('cluster', ''))"; }

docker network create "$net" >/dev/null

# Short waits and a certificate threshold above k3s's one-year certificate, so
# the expiry alert can fire. The only cluster is the one under test.
docker run --rm -v "$chart":/chart:ro "$helm_image" template t /chart/hub-services \
  --set-json 'fleet.clusters=[{"name":"test"}]' \
  --set spokeProbes.unreachableFor=15s --set spokeProbes.slowFor=15s --set spokeProbes.missingFor=15s \
  --set spokeProbes.certWarnDays=400 --set spokeProbes.certWarnFor=15s > "$work/rendered.yaml"

python3 - "$work" "$net" <<'PY'
import sys, yaml
work, net = sys.argv[1], sys.argv[2]
docs = [d for d in yaml.safe_load_all(open(work + "/rendered.yaml")) if d]
cm = next(d for d in docs if d["kind"] == "ConfigMap" and d["metadata"]["name"] == "blackbox-exporter")
open(work + "/blackbox.yml", "w").write(cm["data"]["blackbox.yml"])
rule = next(d for d in docs if d["kind"] == "PrometheusRule" and d["metadata"]["name"] == "spoke-probes")
yaml.safe_dump({"groups": rule["spec"]["groups"]}, open(work + "/rules.yaml", "w"))
probes = [d for d in docs if d["kind"] == "Probe"]
yaml.safe_dump_all(probes, open(work + "/probes.yaml", "w"))
path = next(p for p in probes)["spec"]["targets"]["staticConfig"]["static"][0].rsplit(":6443", 1)[1]
open(work + "/prometheus.yml", "w").write(f"""global: {{scrape_interval: 5s, evaluation_interval: 5s}}
rule_files: [/etc/prom/rules.yaml]
scrape_configs:
  - job_name: probe/monitoring/spoke-api-test
    metrics_path: /probe
    params: {{module: [spoke_api]}}
    static_configs:
      - targets: ["https://{net}-k3s:6443{path}"]
        labels: {{cluster: test, check: spoke-api}}
      - targets: ["http://{net}-bb:9115/-/healthy"]
        labels: {{cluster: plain, check: spoke-api-negative}}
    relabel_configs:
      - {{source_labels: [__address__], target_label: __param_target}}
      - {{source_labels: [__param_target], target_label: instance}}
      - {{target_label: __address__, replacement: {net}-bb:9115}}
""")
PY
chmod -R a+rX "$work"

docker run -d --name "$net-k3s" --hostname "$net-k3s" --network "$net" --privileged --tmpfs /run --tmpfs /var/run \
  "$k3s_image" server --disable=traefik --disable=servicelb --disable=metrics-server --tls-san "$net-k3s" >/dev/null
docker run -d --name "$net-bb" --network "$net" -v "$work":/t:ro "$bb_image" --config.file=/t/blackbox.yml >/dev/null
docker run -d --name "$net-prom" --network "$net" -v "$work":/etc/prom:ro "$prom_image" \
  --config.file=/etc/prom/prometheus.yml --storage.tsdb.path=/tmp/tsdb >/dev/null

for _ in $(seq 1 60); do
  docker exec "$net-k3s" kubectl get nodes 2>/dev/null | grep -q " Ready" && break
  sleep 3
done
docker exec "$net-k3s" kubectl get nodes 2>/dev/null | grep -q " Ready" || fail "k3s did not become ready"

echo "== the Probe objects are valid against the prometheus-operator CRD"
docker cp "$root/deploy/platform-mvp/crds/prometheus-operator-crds.yaml" "$net-k3s":/tmp/crds.yaml
docker cp "$work/probes.yaml" "$net-k3s":/tmp/probes.yaml
docker exec "$net-k3s" kubectl apply --server-side -f /tmp/crds.yaml >/dev/null || fail "could not install the prometheus-operator CRDs"
docker exec "$net-k3s" kubectl create namespace monitoring >/dev/null
docker exec "$net-k3s" kubectl apply -f /tmp/probes.yaml >/dev/null || fail "the rendered Probe was rejected by the CRD"
echo "ok: Probe accepted"

echo "== probing a live API server"
sleep 25
[ "$(query 'probe_success{cluster="test"}')" = 1 ] || fail "the live API server was not probed successfully"
echo "ok: probe_success is 1 for the live API server"
exp="$(query 'probe_ssl_earliest_cert_expiry{cluster="test"} - time()')"
python3 -c "import sys; sys.exit(0 if float('${exp:-0}') > 86400 else 1)" || fail "certificate expiry was not measured (got '${exp}')"
echo "ok: certificate expiry measured (${exp%.*}s left)"
[ "$(query 'probe_success{cluster="plain"}')" = 0 ] || fail "a plain-HTTP endpoint must fail the probe (fail_if_not_ssl)"
echo "ok: a non-TLS endpoint fails the probe"

firing="$(alerts)"
echo "$firing" | grep -q "^SpokeAPICertificateExpiring test$" || fail "the certificate-expiry warning did not fire (firing: $firing)"
echo "ok: SpokeAPICertificateExpiring fires for a certificate inside the warning window"
echo "$firing" | grep -q "^SpokeAPIUnreachable test$" && fail "no unreachable alert expected while the API is up"
echo "$firing" | grep -q "^SpokeAPIProbeMissing" && fail "no probe-missing alert expected while probing works"

echo "== the API server goes away"
docker stop "$net-k3s" >/dev/null
sleep 30
[ "$(query 'probe_success{cluster="test"}')" = 0 ] || fail "probe_success should drop to 0"
echo "$(alerts)" | grep -q "^SpokeAPIUnreachable test$" || fail "SpokeAPIUnreachable did not fire (firing: $(alerts))"
echo "ok: SpokeAPIUnreachable fires once the API server is gone"

echo "PASS"
