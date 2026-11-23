#!/usr/bin/env bash
# End to end: enrolling a cluster without anyone copying a key. A real ClickHouse and the telemetry-ingest image; a real Kubernetes API server
# (k3s). A manage key makes an enrollment; the EXACT command the UI would show (made by the UI's own enrollCommand) is run against k3s, which
# makes the Secret; the cluster-collector image then reports to the hub with that Secret's key. Checks that the Secret is right, that the key
# works and is the cluster's own, that the token works once, that a second enrollment adds a key beside the first, that revoking cuts one key
# off and not the other, and that neither the token nor a key is in a log or in what the API serves.
# Needs Docker; builds two images and pulls k3s and node (a few minutes the first time).
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
suffix="$$"
net="enr-e2e-net-$suffix"
work="$(mktemp -d)"
k3s_image="${K3S_IMAGE:-rancher/k3s:v1.31.4-k3s1}"
ch="enr-e2e-ch-$suffix"; ti="enr-e2e-ti-$suffix"; k3s="enr-e2e-k3s-$suffix"; col="enr-e2e-col-$suffix"
MANAGE=lab-manage-key-000001

cleanup() {
  for n in "$ch" "$ti" "$k3s" "$col"; do docker rm -f "$n" >/dev/null 2>&1 || true; done
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
fail() { echo "FAIL: $1" >&2; exit 1; }
kube() { docker exec -i "$k3s" kubectl "$@"; }
curlc() { docker run --rm -i --network "$net" curlimages/curl "$@"; }
json() { python3 -c "import sys,json; d=json.load(sys.stdin); print(eval(sys.argv[1]))" "$1"; }

docker network create "$net" >/dev/null
echo "==> building the images"
docker build -q -t enr-e2e-collector "$root/platform-mvp/cluster-collector" >/dev/null
docker build -q -t enr-e2e-ingest "$root/platform-mvp/telemetry-ingest" >/dev/null

echo "==> ClickHouse and telemetry-ingest (the project's own keys: ingest, read and manage; none of them goes near the cluster)"
docker run -d --name "$ch" --network "$net" -e CLICKHOUSE_USER=it -e CLICKHOUSE_PASSWORD=it-pass clickhouse/clickhouse-server:24.8 >/dev/null
for _ in $(seq 1 60); do docker exec "$ch" clickhouse-client --user it --password it-pass -q 'SELECT 1' >/dev/null 2>&1 && ok=1 && break; sleep 1; done
[ "${ok:-}" = 1 ] || fail "ClickHouse did not start"
cat >"$work/projects.json" <<JSON
{"projects":[{"name":"lab","ingestKeys":["lab-ingest-key-0000001"],"readKeys":["lab-read-key-00000001"],"manageKeys":["$MANAGE"]}]}
JSON
printf '%s' it-pass >"$work/ch-pass"
chmod 755 "$work"; chmod 644 "$work"/*
docker run -d --name "$ti" --network "$net" -v "$work":/etc/ti:ro enr-e2e-ingest \
  --projects-file=/etc/ti/projects.json --clickhouse-addr="$ch:9000" --clickhouse-user=it --clickhouse-password-file=/etc/ti/ch-pass --listen=:8080 --grpc-listen= >/dev/null
for _ in $(seq 1 60); do curlc -sf "http://$ti:8080/readyz" >/dev/null 2>&1 && ready=1 && break; sleep 1; done
[ "${ready:-}" = 1 ] || { docker logs "$ti" | tail >&2; fail "telemetry-ingest did not become ready"; }

echo "==> a Kubernetes API server (k3s)"
# Only the API server is needed (a Secret is applied and the collector lists what is there), not a node: --disable-agent leaves out the kubelet and
# containerd, which are what need a host with room for them (they do not start beside other clusters' runtimes).
docker run -d --name "$k3s" --network "$net" --privileged --tmpfs /run --tmpfs /var/run \
  "$k3s_image" server --disable-agent --disable=traefik --disable=servicelb --disable=metrics-server --disable=coredns --disable=local-storage >/dev/null
for _ in $(seq 1 120); do kube get --raw /readyz 2>/dev/null | grep -q ok && kube get namespace default >/dev/null 2>&1 && k=1 && break; sleep 3; done
if [ "${k:-}" != 1 ]; then
  docker logs "$k3s" 2>&1 | grep -q "too many open files" && fail "k3s could not start: the host is out of inotify instances (fs.inotify.max_user_instances); raise it or stop other clusters"
  docker logs "$k3s" 2>&1 | tail -15 >&2
  fail "k3s did not become ready"
fi

api() { curlc -s -H "X-API-Key: $1" "${@:3}" "http://$ti:8080/api/v1/projects/lab/$2"; }
hub_key_works() { # <key> -> http status of a heartbeat to the inventory endpoint
  curlc -s -o /dev/null -w '%{http_code}' -X POST -H "X-API-Key: $1" -H 'Content-Type: application/json' \
    -d "{\"version\":1,\"cluster\":\"lab\",\"collected_at\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}" "http://$ti:8080/v1/inventory"
}
secret_key() { kube -n telemetry-agent get secret hub-telemetry-credentials -o jsonpath='{.data.apiKey}' | base64 -d; }

# the UI's own function makes the command: what is run here is what the page would show
ui_command() { # <token>
  docker run --rm -v "$root/platform-mvp/telemetry-ui/src":/src:ro -e TOKEN="$1" -e URL="http://$ti:8080" node:24-alpine \
    node --input-type=module -e "import { enrollCommand } from '/src/api/enrollments.js'; const c = enrollCommand({ url: process.env.URL, token: process.env.TOKEN }); if (!c) { console.error('no command'); process.exit(1) } console.log(c)"
}
run_on_cluster() { # <command>: the command with curl and kubectl pointed at the hub and at k3s
  ( curl() { curlc "$@"; }; kubectl() { kube "$@"; }; eval "$1" )
}
make_token() { api "$MANAGE" enrollments -X POST -H 'Content-Type: application/json' -H 'X-Actor: e2e' -d "{\"cluster\":\"$1\"}"; }

echo "==> making an enrollment (a manage key; the read key may not)"
code="$(curlc -s -o /dev/null -w '%{http_code}' -X POST -H 'X-API-Key: lab-read-key-00000001' -H 'Content-Type: application/json' -d '{"cluster":"gke"}' "http://$ti:8080/api/v1/projects/lab/enrollments")"
[ "$code" = 403 ] || fail "a read key can make an enrollment ($code)"
resp="$(make_token gke)"
token="$(json "d['data']['token']" <<<"$resp")"
id1="$(json "d['data']['id']" <<<"$resp")"
[ "$(json "d['data']['status']" <<<"$resp")" = pending ] || fail "not pending: $resp"
case "$token" in enr_*) ;; *) fail "that is not a token" ;; esac
echo "    ok: a pending enrollment and a one-time token"

echo "==> the command, as the page shows it, run against the cluster"
cmd="$(ui_command "$token")"
case "$cmd" in *"kubectl apply -f -") ;; *) fail "the command does not end in kubectl apply: $cmd" ;; esac
case "$cmd" in *"ing_"*) fail "the command has a key in it" ;; esac
out="$(run_on_cluster "$cmd" 2>&1)" || { echo "$out" >&2; fail "the command failed"; }
echo "$out" | sed 's/^/    /'
echo "$out" | grep -q "secret/hub-telemetry-credentials" || fail "the Secret was not made: $out"
echo "$out" | grep -qE "ing_|enr_" && fail "a key or a token was printed"
kube get namespace telemetry-agent >/dev/null || fail "the namespace was not made"
key1="$(secret_key)"
case "$key1" in ing_*) ;; *) fail "the Secret does not hold an ingest key" ;; esac
[ "${#key1}" -ge 47 ] || fail "the key is short (${#key1})"
[ "$(kube -n telemetry-agent get secret hub-telemetry-credentials -o jsonpath='{.metadata.annotations.platform\.example\.com/enrolled-cluster}')" = gke ] || fail "the Secret does not say which cluster"
echo "    ok: the Secret is made, holds a key of its own, and nothing was printed"

echo "==> the key works, once, and is not one of the project's"
[ "$key1" != "lab-ingest-key-0000001" ] || fail "the cluster was given the project's own key"
[ "$(hub_key_works "$key1")" = 200 ] || fail "the cluster's key is refused"
[ "$(curlc -s -o /dev/null -w '%{http_code}' -H "X-API-Key: $key1" "http://$ti:8080/api/v1/projects/lab/metrics")" = 403 ] || fail "an ingest key can read"
code="$(curlc -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d "{\"token\":\"$token\"}" "http://$ti:8080/v1/enroll")"
[ "$code" = 401 ] || fail "the token worked twice ($code)"
echo "    ok: accepted for ingest, not for reading, and the token is spent"

echo "==> the cluster-collector, using that Secret, reports to the hub"
kube -n telemetry-agent get secret hub-telemetry-credentials -o jsonpath='{.data.apiKey}' | base64 -d > "$work/enrolled-key"
chmod 644 "$work/enrolled-key"
docker exec "$k3s" cat /etc/rancher/k3s/k3s.yaml > "$work/kubeconfig"; chmod 644 "$work/kubeconfig"
ti_ip="$(docker inspect -f '{{(index .NetworkSettings.Networks "'"$net"'").IPAddress}}' "$ti")"
docker run -d --name "$col" --network "container:$k3s" --user 65532:65532 -v "$work":/etc/cc:ro enr-e2e-collector \
  --kubeconfig /etc/cc/kubeconfig --endpoint "http://$ti_ip:8080" --api-key-file /etc/cc/enrolled-key --cluster gke --interval 5s --resync 1m --listen "" >/dev/null
for _ in $(seq 1 40); do
  if api lab-read-key-00000001 inventory | python3 -c "import sys,json; d=json.load(sys.stdin)['data']; sys.exit(0 if any(c['cluster']=='gke' and c['counts'].get('namespace',0)>0 for c in d) else 1)" 2>/dev/null; then got=1; break; fi
  sleep 3
done
[ "${got:-}" = 1 ] || { docker logs "$col" 2>&1 | tail -5 >&2; fail "the collector did not get through with the enrolled key"; }
echo "    ok: the cluster's inventory arrived, authenticated by the key the enrollment made"

echo "==> enrolled again: a second key beside the first, and revoking one leaves the other"
resp2="$(make_token gke)"
token2="$(json "d['data']['token']" <<<"$resp2")"
id2="$(json "d['data']['id']" <<<"$resp2")"
run_on_cluster "$(ui_command "$token2")" >/dev/null 2>&1 || fail "the second enrollment failed"
key2="$(secret_key)"
[ "$key2" != "$key1" ] || fail "a second enrollment gave the same key"
[ "$(hub_key_works "$key1")" = 200 ] && [ "$(hub_key_works "$key2")" = 200 ] || fail "both keys should work until one is revoked"
list="$(api lab-read-key-00000001 enrollments)"
echo "$list" | python3 -c "import sys,json; d=json.load(sys.stdin)['data']; a={e['id']:e for e in d}; sys.exit(0 if a['$id1']['status']=='redeemed' and a['$id2']['status']=='redeemed' and all('token' not in e for e in d) else 1)" || fail "the listing does not show two enrolled clusters, or shows a token: $list"
[ "$(api "$MANAGE" "enrollments/$id1" -X DELETE -o /dev/null -w '%{http_code}')" = 200 ] || fail "the revoke failed"
[ "$(hub_key_works "$key1")" = 401 ] || fail "a revoked key is still accepted"
[ "$(hub_key_works "$key2")" = 200 ] || fail "revoking one key cut off the other"
[ "$(hub_key_works lab-ingest-key-0000001)" = 200 ] || fail "the project's own key stopped working"
echo "    ok: the first key is cut off at once; the second and the project's own work"

echo "==> a restart of the service: the issued keys are still there, the revoked one is still gone"
docker restart "$ti" >/dev/null
for _ in $(seq 1 60); do curlc -sf "http://$ti:8080/readyz" >/dev/null 2>&1 && ready=1 && break; sleep 1; done
[ "$(hub_key_works "$key2")" = 200 ] || fail "an issued key did not survive a restart"
[ "$(hub_key_works "$key1")" = 401 ] || fail "a revoked key came back after a restart"
echo "    ok"

echo "==> neither a token nor a key was logged or served"
all="$work/all.txt"; : >"$all"
for p in enrollments inventory "inventory/namespaces" metrics; do api lab-read-key-00000001 "$p" >>"$all" 2>/dev/null; echo >>"$all"; done
logs="$(docker logs "$ti" 2>&1; docker logs "$col" 2>&1)"
for secret in "${token#enr_}" "${token2#enr_}" "${key1#ing_}" "${key2#ing_}"; do
  grep -qF -- "$secret" "$all" && fail "a secret is in what the API serves"
  grep -qF -- "$secret" <<<"$logs" && fail "a secret is in a log"
done
grep -q "a cluster enrolled" <<<"$logs" || fail "the enrollment was not logged"
echo "    ok"
echo PASS
