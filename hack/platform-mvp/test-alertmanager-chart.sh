#!/usr/bin/env bash
# Checks the hub chart's alertmanager template: nothing renders unless enabled; the pod is locked down and holds the Slack webhook URL only as a mounted
# Secret (never in the manifest); a missing Secret is refused; the alert service sends to it unless another is named; and the config it renders is one the
# REAL Alertmanager accepts, routes an incident and an alert to the right receivers, and turns each into the Slack message it should (checked against a
# stub Slack). Needs Docker and network.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
chart="$root/deploy/platform-mvp/chart"
helm_image="${HELM_IMAGE:-alpine/helm:3.16.4}"
am_image="${ALERTMANAGER_IMAGE:-quay.io/prometheus/alertmanager:v0.27.0}"
work="$(mktemp -d)"
suffix="$$"
net="am-chart-net-$suffix"
cleanup() { docker rm -f "am-$suffix" "slack-$suffix" >/dev/null 2>&1 || true; docker network rm "$net" >/dev/null 2>&1 || true; rm -rf "$work"; }
trap cleanup EXIT

fail() { echo "FAIL: $1" >&2; exit 1; }
render() { # <template> <values...>
  local t="$1"; shift
  docker run --rm -v "$chart":/chart:ro "$helm_image" template h /chart/hub-services -s "templates/$t.yaml" "$@"
}

echo "== disabled by default: nothing renders"
out="$(render alertmanager 2>&1 || true)"
grep -q "could not find template" <<<"$out" || fail "rendered while disabled: $out"

echo "== enabled: a config, a Service and a locked-down pod that mounts the Slack Secret"
render alertmanager --set alertmanager.enabled=true > "$work/all.yaml"
python3 - "$work/all.yaml" "$work" <<'PY'
import sys, yaml, json
docs = {d["kind"]: d for d in yaml.safe_load_all(open(sys.argv[1])) if d}
assert set(docs) == {"ConfigMap", "Service", "Deployment"}, set(docs)
dep = docs["Deployment"]
assert dep["spec"]["replicas"] == 1
pod = dep["spec"]["template"]["spec"]
c = pod["containers"][0]
sc = c["securityContext"]
assert sc["readOnlyRootFilesystem"] is True and sc["runAsNonRoot"] is True and sc["allowPrivilegeEscalation"] is False and sc["capabilities"]["drop"] == ["ALL"], sc
assert pod["automountServiceAccountToken"] is False
assert "--cluster.listen-address=" in c["args"], "gossip should be off: it is one pod"
vols = {v["name"]: v for v in pod["volumes"]}
assert vols["slack"]["secret"]["secretName"] == "alertmanager-slack"
assert vols["slack"]["secret"]["items"] == [{"key": "webhook", "path": "webhook"}], vols["slack"]
assert {m["name"]: m["mountPath"] for m in c["volumeMounts"]}["slack"] == "/etc/alertmanager/slack"
assert docs["Service"]["spec"]["ports"][0]["port"] == 9093
assert "checksum/config" in dep["spec"]["template"]["metadata"]["annotations"], "a change of the config must replace the pod"
cfg = docs["ConfigMap"]["data"]["alertmanager.yml"]
assert "api_url_file: /etc/alertmanager/slack/webhook" in cfg and "hooks.slack.com" not in cfg and "api_url:" not in cfg, "the webhook URL must come from the Secret and never be in the config"
open(sys.argv[2] + "/alertmanager.yml", "w").write(cfg)
PY
grep -qE "hooks\.slack\.com|xoxb-" "$work/all.yaml" && fail "a Slack URL or token rendered into the manifest"
echo "ok"

echo "== a missing Secret name, or key, is refused"
out="$(render alertmanager --set alertmanager.enabled=true --set alertmanager.slack.existingSecret= 2>&1 || true)"
grep -q "existingSecret is required" <<<"$out" || fail "a missing Secret was not refused: $out"
out="$(render alertmanager --set alertmanager.enabled=true --set alertmanager.slack.secretKey= 2>&1 || true)"
grep -q "secretKey is required" <<<"$out" || fail "a missing key was not refused: $out"
echo "ok"

echo "== settings reach the config"
render alertmanager --set alertmanager.enabled=true --set alertmanager.route.repeatInterval=2h --set 'alertmanager.route.groupBy={alertname}' \
  --set alertmanager.slack.channel='#ops' --set alertmanager.slack.sendResolved=false --set alertmanager.image.tag=v0.28.0 > "$work/set.yaml"
python3 - "$work/set.yaml" <<'PY'
import sys, yaml
docs = {d["kind"]: d for d in yaml.safe_load_all(open(sys.argv[1])) if d}
cfg = yaml.safe_load(docs["ConfigMap"]["data"]["alertmanager.yml"])
assert cfg["route"]["repeat_interval"] == "2h" and cfg["route"]["group_by"] == ["alertname"], cfg["route"]
slack = cfg["receivers"][0]["slack_configs"][0]
assert slack["channel"] == "#ops" and slack["send_resolved"] is False, slack
assert cfg["receivers"][1]["slack_configs"][0]["send_resolved"] is False
assert docs["Deployment"]["spec"]["template"]["spec"]["containers"][0]["image"].endswith(":v0.28.0")
PY
default="$(python3 -c "
import yaml
cfg = yaml.safe_load(open('$work/alertmanager.yml'))
s = cfg['receivers'][0]['slack_configs'][0]
assert 'channel' not in s, 'a channel was set without being asked'
print('ok')")"
echo "$default"

echo "== the alert service sends to it unless another Alertmanager is named"
env_of() { render telemetry-alerts --set telemetryAlerts.enabled=true "$@" | python3 -c '
import sys, yaml
for d in yaml.safe_load_all(sys.stdin):
    if d and d["kind"] == "Deployment":
        print({e["name"]: e.get("value") for e in d["spec"]["template"]["spec"]["containers"][0]["env"]}.get("ALERTMANAGER_URL", "none"))'; }
[ "$(env_of)" = none ] || fail "an Alertmanager was named with none switched on"
[ "$(env_of --set alertmanager.enabled=true)" = "http://alertmanager.monitoring.svc.cluster.local:9093" ] || fail "the alert service does not send to this chart's Alertmanager: $(env_of --set alertmanager.enabled=true)"
[ "$(env_of --set alertmanager.enabled=true --set alertmanager.namespace=ops)" = "http://alertmanager.ops.svc.cluster.local:9093" ] || fail "the namespace is not followed"
[ "$(env_of --set alertmanager.enabled=true --set telemetryAlerts.alertmanagerUrl=http://other:9093)" = "http://other:9093" ] || fail "an Alertmanager that was named is not the one used"
[ "$(env_of --set telemetryAlerts.alertmanagerUrl=http://other:9093)" = "http://other:9093" ] || fail "a named Alertmanager is not used on its own"
echo "ok"

echo "== the real Alertmanager accepts the config, and routes by what an alert says"
mkdir -p "$work/slack" "$work/cfg"
cp "$work/alertmanager.yml" "$work/cfg/alertmanager.yml"
printf 'http://slack:8080/hook' > "$work/slack/webhook"
amtool() { docker run --rm --entrypoint /bin/amtool -v "$work/cfg":/etc/alertmanager/config:ro -v "$work/slack":/etc/alertmanager/slack:ro "$am_image" "$@"; }
amtool check-config /etc/alertmanager/config/alertmanager.yml >"$work/check.txt" 2>&1 || { cat "$work/check.txt" >&2; fail "Alertmanager does not accept the config"; }
route() { amtool config routes test --config.file=/etc/alertmanager/config/alertmanager.yml "$@" | tr -d '\r\n'; }
[ "$(route source=incident alertname=IncidentOpened)" = "slack-incidents" ] || fail "an incident is not routed to its own receiver: $(route source=incident alertname=IncidentOpened)"
[ "$(route alertname=ServiceConnectionsFailing severity=critical)" = "slack" ] || fail "an alert is not routed to the plain receiver: $(route alertname=ServiceConnectionsFailing)"
echo "ok"

echo "== the messages Slack gets"
cat > "$work/stub.py" <<'PY'
import http.server, json, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(s):
        body = s.rfile.read(int(s.headers.get("Content-Length") or 0)).decode()
        print("SLACK " + body.strip().replace("\n", "\\n"), flush=True)
        s.send_response(200); s.send_header("Content-Type", "text/plain"); s.end_headers(); s.wfile.write(b"ok")
    def log_message(*a): pass
http.server.HTTPServer(("", 8080), H).serve_forever()
PY
sed -i 's/group_wait: 30s/group_wait: 1s/; s/group_wait: 10s/group_wait: 1s/' "$work/cfg/alertmanager.yml"
docker network create "$net" >/dev/null
docker run -d --name "slack-$suffix" --network "$net" --network-alias slack -v "$work/stub.py":/s.py:ro python:3.12-alpine python -u /s.py >/dev/null
docker run -d --name "am-$suffix" --network "$net" --network-alias am -v "$work/cfg":/etc/alertmanager/config:ro -v "$work/slack":/etc/alertmanager/slack:ro \
  "$am_image" --config.file=/etc/alertmanager/config/alertmanager.yml --storage.path=/tmp/am --web.listen-address=:9093 --cluster.listen-address= >/dev/null
for _ in $(seq 1 30); do
  docker run --rm --network "$net" curlimages/curl -s -o /dev/null http://am:9093/-/ready 2>/dev/null && break
  sleep 1
done
now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"; later="$(date -u -d '+1 hour' +%Y-%m-%dT%H:%M:%SZ)"
post() { docker run --rm --network "$net" curlimages/curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d "$1" http://am:9093/api/v2/alerts; }
[ "$(post "[{\"labels\":{\"alertname\":\"ServiceConnectionsFailing\",\"severity\":\"critical\",\"project\":\"us\",\"service\":\"db/mongo\"},\"annotations\":{\"summary\":\"100% of connections into db/mongo are failing\",\"title\":\"Connections failing\"},\"startsAt\":\"$now\",\"endsAt\":\"$later\"}]")" = 200 ] || fail "Alertmanager did not take the alert"
[ "$(post "[{\"labels\":{\"alertname\":\"IncidentOpened\",\"source\":\"incident\",\"severity\":\"critical\",\"project\":\"us\",\"app\":\"/k8s/db/mongo\",\"incident\":\"vcxxe33p\"},\"annotations\":{\"summary\":\"db/mongo: connections failing\",\"description\":\"1800 of 1800 attempts failed\"},\"startsAt\":\"$now\",\"endsAt\":\"$later\",\"generatorURL\":\"http://ui.example/recorded/vcxxe33p\"}]")" = 200 ] || fail "Alertmanager did not take the incident"
for _ in $(seq 1 40); do
  docker logs "slack-$suffix" 2>&1 | grep -c '^SLACK' | grep -q '^2$' && break
  sleep 1
done
docker logs "slack-$suffix" 2>&1 | grep '^SLACK' | sed 's/^SLACK //' > "$work/slack.jsonl"
python3 - "$work/slack.jsonl" <<'PY'
import sys, json
msgs = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
assert len(msgs) == 2, f"Slack got {len(msgs)} messages, want 2: {msgs}"
def text(m):
    a = m["attachments"][0]
    return a.get("title", "") + "\n" + a.get("text", "")
alert = next(m for m in msgs if "ServiceConnectionsFailing" in text(m))
inc = next(m for m in msgs if "Incident:" in text(m))
t = text(alert)
assert "[FIRING:1] ServiceConnectionsFailing (us)" in t, t
assert "*critical* 100% of connections into db/mongo are failing" in t, t
t = text(inc)
assert "Incident: db/mongo: connections failing" in t, t
assert "*critical* in us, /k8s/db/mongo" in t and "1800 of 1800 attempts failed" in t, t
assert "http://ui.example/recorded/vcxxe33p|Open the incident" in t, t
print("ok: an alert and an incident each became the Slack message they should")
PY
echo "OK: alertmanager chart"
