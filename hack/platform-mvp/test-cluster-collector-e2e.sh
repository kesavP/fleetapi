#!/usr/bin/env bash
# End to end: a real Kubernetes API server (k3s in a container) with workloads in it, the cluster-collector image reading it, the
# telemetry-ingest image storing what it sends in a real ClickHouse, and the read API answering.
# Checks the inventory arrives, follows a scale to zero and a deletion, repairs a deletion the collector missed (a restart, then the sweep), shows
# a failing pod's warning events, and that nothing secret that is in the cluster's objects (annotations, environment, commands, a Secret, a KEDA
# trigger's metadata) comes through. Needs Docker; builds two images and pulls k3s (a few minutes the first time).
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
suffix="$$"
net="cc-e2e-net-$suffix"
work="$(mktemp -d)"
k3s_image="${K3S_IMAGE:-rancher/k3s:v1.31.4-k3s1}"
ch="cc-e2e-ch-$suffix"; ti="cc-e2e-ti-$suffix"; k3s="cc-e2e-k3s-$suffix"; col="cc-e2e-col-$suffix"

cleanup() {
  for n in "$ch" "$ti" "$k3s" "$col"; do docker rm -f "$n" >/dev/null 2>&1 || true; done
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
fail() { echo "FAIL: $1" >&2; exit 1; }
kube() { docker exec -i "$k3s" kubectl "$@"; }

docker network create "$net" >/dev/null
echo "==> building the images"
docker build -q -t cc-e2e-collector "$root/platform-mvp/cluster-collector" >/dev/null
docker build -q -t cc-e2e-ingest "$root/platform-mvp/telemetry-ingest" >/dev/null

echo "==> ClickHouse and telemetry-ingest"
docker run -d --name "$ch" --network "$net" -e CLICKHOUSE_USER=it -e CLICKHOUSE_PASSWORD=it-pass clickhouse/clickhouse-server:24.8 >/dev/null
for _ in $(seq 1 60); do docker exec "$ch" clickhouse-client --user it --password it-pass -q 'SELECT 1' >/dev/null 2>&1 && ok=1 && break; sleep 1; done
[ "${ok:-}" = 1 ] || fail "ClickHouse did not start"
cat >"$work/projects.json" <<'JSON'
{"projects":[{"name":"lab","ingestKeys":["lab-ingest-key-0000001"],"readKeys":["lab-read-key-00000001"]}]}
JSON
printf '%s' it-pass >"$work/ch-pass"
printf '%s' lab-ingest-key-0000001 >"$work/ingest-key"
chmod 755 "$work"; chmod 644 "$work"/*
docker run -d --name "$ti" --network "$net" -v "$work":/etc/ti:ro cc-e2e-ingest \
  --projects-file=/etc/ti/projects.json --clickhouse-addr="$ch:9000" --clickhouse-user=it --clickhouse-password-file=/etc/ti/ch-pass --listen=:8080 --grpc-listen= >/dev/null
for _ in $(seq 1 60); do docker run --rm --network "$net" curlimages/curl -sf "http://$ti:8080/readyz" >/dev/null 2>&1 && ready=1 && break; sleep 1; done
[ "${ready:-}" = 1 ] || { docker logs "$ti" | tail >&2; fail "telemetry-ingest did not become ready"; }
ti_ip="$(docker inspect -f '{{(index .NetworkSettings.Networks "'"$net"'").IPAddress}}' "$ti")"

echo "==> a Kubernetes API server (k3s)"
docker run -d --name "$k3s" --network "$net" --privileged --tmpfs /run --tmpfs /var/run \
  "$k3s_image" server --disable=traefik --disable=servicelb --disable=metrics-server >/dev/null
for _ in $(seq 1 120); do kube get nodes 2>/dev/null | grep -q " Ready" && k=1 && break; sleep 3; done
if [ "${k:-}" != 1 ]; then
  docker logs "$k3s" 2>&1 | grep -q "too many open files" && fail "k3s could not start: the host is out of inotify instances (fs.inotify.max_user_instances, often because kind clusters are running); raise it or stop other clusters"
  fail "k3s did not become ready"
fi

echo "==> workloads, with secrets in them"
# KEDA is not installed in k3s: a minimal CRD stands in, so a ScaledObject (and a trigger's metadata, where a connection string lives) can exist
kube apply -f - >/dev/null <<'YAML'
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: scaledobjects.keda.sh}
spec:
  group: keda.sh
  scope: Namespaced
  names: {kind: ScaledObject, plural: scaledobjects, singular: scaledobject}
  versions:
    - name: v1alpha1
      served: true
      storage: true
      schema: {openAPIV3Schema: {type: object, x-kubernetes-preserve-unknown-fields: true}}
YAML
kube wait --for=condition=Established crd/scaledobjects.keda.sh --timeout=60s >/dev/null
kube apply -f - >/dev/null <<'YAML'
apiVersion: v1
kind: Namespace
metadata: {name: shop}
---
apiVersion: v1
kind: Secret
metadata: {name: db-credentials, namespace: shop}
stringData: {password: SECRET-VALUE-IN-A-SECRET}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: cartservice
  namespace: shop
  annotations: {note: SECRET-ANNOTATION-VALUE}
  labels: {app: cartservice}
spec:
  replicas: 2
  selector: {matchLabels: {app: cartservice}}
  template:
    metadata: {labels: {app: cartservice}}
    spec:
      containers:
        - name: app
          image: busybox:1.36
          command: ["sh", "-c", "echo SECRET-COMMAND-VALUE; sleep 3600"]
          env:
            - {name: DB_PASSWORD, value: SECRET-ENV-VALUE}
            - name: FROM_SECRET
              valueFrom: {secretKeyRef: {name: db-credentials, key: password}}
          resources: {requests: {cpu: 50m, memory: 32Mi}, limits: {memory: 64Mi}}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: adservice, namespace: shop}
spec:
  replicas: 1
  selector: {matchLabels: {app: adservice}}
  template:
    metadata: {labels: {app: adservice}}
    spec: {containers: [{name: app, image: "busybox:1.36", command: ["sleep", "3600"]}]}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: paymentservice, namespace: shop}
spec:
  replicas: 1
  selector: {matchLabels: {app: paymentservice}}
  template:
    metadata: {labels: {app: paymentservice}}
    spec: {containers: [{name: app, image: "busybox:1.36", command: ["sleep", "3600"]}]}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: broken, namespace: shop}
spec:
  replicas: 1
  selector: {matchLabels: {app: broken}}
  template:
    metadata: {labels: {app: broken}}
    spec: {containers: [{name: app, image: "registry.invalid/does-not-exist:1"}]}
---
apiVersion: apps/v1
kind: DaemonSet
metadata: {name: agent, namespace: shop}
spec:
  selector: {matchLabels: {app: agent}}
  template:
    metadata: {labels: {app: agent}}
    spec: {containers: [{name: app, image: "busybox:1.36", command: ["sleep", "3600"]}]}
---
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: db, namespace: shop}
spec:
  serviceName: db
  replicas: 1
  selector: {matchLabels: {app: db}}
  template:
    metadata: {labels: {app: db}}
    spec: {containers: [{name: app, image: "busybox:1.36", command: ["sleep", "3600"]}]}
  volumeClaimTemplates:
    - metadata: {name: data}
      spec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 1Gi}}}
---
apiVersion: batch/v1
kind: CronJob
metadata: {name: cleanup, namespace: shop}
spec:
  schedule: "*/1 * * * *"
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: Never
          containers: [{name: app, image: "busybox:1.36", command: ["true"]}]
---
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata: {name: cartservice, namespace: shop}
spec:
  scaleTargetRef: {apiVersion: apps/v1, kind: Deployment, name: cartservice}
  minReplicas: 1
  maxReplicas: 4
  metrics: [{type: Resource, resource: {name: cpu, target: {type: Utilization, averageUtilization: 70}}}]
---
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata: {name: worker, namespace: shop, annotations: {note: SECRET-ANNOTATION-VALUE}}
spec:
  scaleTargetRef: {name: adservice}
  minReplicaCount: 0
  maxReplicaCount: 5
  triggers:
    - type: rabbitmq
      metadata: {host: "amqp://user:SECRET-TRIGGER-VALUE@mq:5672", queueName: orders}
YAML

echo "==> cluster-collector (reads k3s with its own kubeconfig, sends to the ingest service)"
docker exec "$k3s" cat /etc/rancher/k3s/k3s.yaml >"$work/kubeconfig"
chmod 644 "$work/kubeconfig"
# sharing k3s's network namespace makes its API server reachable as 127.0.0.1:6443, an address its certificate is valid for
start_collector() {
  docker rm -f "$col" >/dev/null 2>&1 || true
  docker run -d --name "$col" --network "container:$k3s" --user 65532:65532 -v "$work":/etc/cc:ro cc-e2e-collector \
    --kubeconfig /etc/cc/kubeconfig --endpoint "http://$ti_ip:8080" --api-key-file /etc/cc/ingest-key --cluster k3s-lab \
    --interval 5s --resync 1m --events all --listen "" >/dev/null
}
# --check first: what it can read, with no error
docker run --rm --network "container:$k3s" --user 65532:65532 -v "$work":/etc/cc:ro cc-e2e-collector --kubeconfig /etc/cc/kubeconfig --check >"$work/check.out" 2>"$work/check.err" \
  || { cat "$work/check.out" "$work/check.err" >&2; fail "--check found something that cannot be read"; }
grep -qE '^deployment +[0-9]+' "$work/check.out" || fail "--check did not list deployments: $(cat "$work/check.out")"
start_collector

api() { docker run --rm --network "$net" curlimages/curl -sf -H "X-API-Key: lab-read-key-00000001" "http://$ti:8080/api/v1/projects/lab/$1"; }
waitfor() { # <what> <path> <python predicate over the parsed json "d">
  local what="$1" path="$2" pred="$3"
  for _ in $(seq 1 60); do
    if api "$path" 2>/dev/null | python3 -c "import sys,json; d=json.load(sys.stdin)['data']; sys.exit(0 if ($pred) else 1)" 2>/dev/null; then echo "    ok: $what"; return 0; fi
    sleep 3
  done
  echo "--- last answer for $path:" >&2; api "$path" >&2 || true; docker logs "$col" 2>&1 | tail -8 >&2
  fail "$what"
}

echo "==> reading it back"
waitfor "the cluster has reported, and what is in it" "inventory" \
  "[c for c in d if c['cluster']=='k3s-lab'] and [c for c in d if c['cluster']=='k3s-lab'][0]['counts'].get('deployment',0)>=4 and [c for c in d if c['cluster']=='k3s-lab'][0]['counts'].get('node',0)==1"
waitfor "the node, with its capacity" "inventory/nodes" "len(d)==1 and d[0]['data']['ready'] is True and d[0]['data']['capacity']['cpu_m']>0 and d[0]['data']['capacity']['memory_bytes']>0 and d[0]['data']['kubelet'].startswith('v1.')"
waitfor "the deployments, as the workloads they are" "inventory/workloads?namespace=shop" \
  "{w['name'] for w in d if w['kind']=='deployment'}>={'cartservice','adservice','paymentservice','broken'} and {w['kind'] for w in d}>={'deployment','statefulset','daemonset','cronjob'}"
waitfor "cartservice wants two, with its resources" "inventory/deployments?name=cartservice" \
  "len(d)==1 and d[0]['data']['desired']==2 and d[0]['data']['containers'][0]['requests']['cpu_m']==50 and d[0]['data']['containers'][0]['limits']['memory_bytes']==64*1024*1024 and d[0]['labels']['app']=='cartservice'"
waitfor "its pods, found through the replica set" "inventory/pods?namespace=shop" \
  "len([p for p in d if p['data']['workload_kind']=='deployment' and p['data']['workload_name']=='cartservice'])==2"
waitfor "the stateful set's pod, and the claim its template made" "inventory/pvcs?namespace=shop" "any(p['name'].startswith('data-db-') and p['data']['requested_bytes']==1024**3 for p in d)"
waitfor "the cron job" "inventory/cronjobs" "any(c['name']=='cleanup' and c['data']['schedule']=='*/1 * * * *' and c['data']['suspend'] is False for c in d)"
waitfor "the autoscaler" "inventory/hpas" "any(h['name']=='cartservice' and h['data']['target_name']=='cartservice' and h['data']['min']==1 and h['data']['max']==4 and h['data']['metrics'][0]['target']=='70%' for h in d)"
waitfor "the KEDA scaled object, its trigger types only" "inventory/scaledobjects" "any(s['name']=='worker' and s['data']['triggers']==['rabbitmq'] and s['data']['max']==5 and s['data']['target_name']=='adservice' for s in d)"
waitfor "the broken pod, waiting for its image" "inventory/pods?namespace=shop" \
  "any(p['data']['workload_name']=='broken' and p['data']['containers'][0]['state']=='waiting' and p['data']['containers'][0]['reason'] in ('ImagePullBackOff','ErrImagePull') for p in d)"
waitfor "the warning events of it" "inventory/events?namespace=shop&type=Warning&name=&limit=200" "any(e['reason'] in ('Failed','BackOff') and e['kind']=='Pod' for e in d)"

echo "==> scaled to zero: the new state is current, the old one is in the history"
kube -n shop scale deployment cartservice --replicas=0 >/dev/null
waitfor "cartservice wants none" "inventory/deployments?name=cartservice" "len(d)==1 and d[0]['data']['desired']==0 and d[0]['data']['ready']==0"
waitfor "its pods are gone" "inventory/pods?namespace=shop" "not [p for p in d if p['data']['workload_name']=='cartservice']"
uid="$(api "inventory/deployments?name=cartservice" | python3 -c "import sys,json; print(json.load(sys.stdin)['data'][0]['uid'])")"
waitfor "the history: now zero, before two" "inventory/deployments/$uid/history" "[v['data']['desired'] for v in d if not v['deleted']][:1]==[0] and 2 in [v['data']['desired'] for v in d if not v['deleted']]"
waitfor "the scaling in the events" "inventory/events?namespace=shop&kind=Deployment&name=cartservice" "any(e['reason']=='ScalingReplicaSet' for e in d)"

echo "==> deleted while the collector watches: it goes at once"
kube -n shop delete deployment adservice >/dev/null
waitfor "adservice is gone" "inventory/deployments?namespace=shop" "'adservice' not in [w['name'] for w in d]"
waitfor "and its deletion is in the history" "inventory/deployments/$(api "inventory/deployments?namespace=shop&name=paymentservice" | python3 -c "import sys,json; print(json.load(sys.stdin)['data'][0]['uid'])")/history" "len(d)>=1"

echo "==> deleted while the collector is down: the sweep after its restart repairs it"
docker rm -f "$col" >/dev/null
kube -n shop delete deployment paymentservice >/dev/null
sleep 2
api "inventory/deployments?namespace=shop" | python3 -c "import sys,json; sys.exit(0 if 'paymentservice' in [w['name'] for w in json.load(sys.stdin)['data']] else 1)" || fail "paymentservice was already gone (the check below would prove nothing)"
start_collector
waitfor "paymentservice is swept" "inventory/deployments?namespace=shop" "'paymentservice' not in [w['name'] for w in d] and 'cartservice' in [w['name'] for w in d]"

echo "==> no secret came through"
all="$work/all.json"
: >"$all"
for p in "inventory" "inventory/nodes" "inventory/namespaces" "inventory/workloads?limit=1000" "inventory/pods?limit=1000" "inventory/pvcs" "inventory/hpas" "inventory/scaledobjects" "inventory/events?limit=1000&from=2000-01-01T00:00:00Z"; do
  api "$p" >>"$all"; echo >>"$all"
done
[ "$(wc -c <"$all")" -gt 5000 ] || fail "there is hardly anything to search ($(wc -c <"$all") bytes)"
for secret in SECRET-ANNOTATION-VALUE SECRET-ENV-VALUE SECRET-COMMAND-VALUE SECRET-VALUE-IN-A-SECRET SECRET-TRIGGER-VALUE lab-ingest-key lab-read-key; do
  if grep -q "$secret" "$all"; then grep -o ".\{60\}$secret.\{20\}" "$all" | head -3 >&2; fail "'$secret' is in what the API serves"; fi
done
docker logs "$col" 2>&1 | grep -qE "SECRET-|lab-ingest-key" && fail "a secret is in the collector's log"
docker logs "$ti" 2>&1 | grep -qE "SECRET-|lab-ingest-key" && fail "a secret is in the ingest service's log"
grep -q "lab-ingest-key" "$work/check.out" "$work/check.err" && fail "the key is in the output of --check"
echo "    ok: none of the annotation, the environment, the command, the Secret, the trigger metadata or the keys"

echo "==> the project's wall: another key sees nothing of it"
code="$(docker run --rm --network "$net" curlimages/curl -s -o /dev/null -w '%{http_code}' -H "X-API-Key: lab-ingest-key-0000001" "http://$ti:8080/api/v1/projects/lab/inventory")"
[ "$code" = 403 ] || fail "an ingest key can read the inventory ($code)"
echo PASS
