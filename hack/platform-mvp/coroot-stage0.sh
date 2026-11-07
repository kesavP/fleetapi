#!/usr/bin/env bash
# Stage 0: does Coroot see a Java, a .NET and a Python workload on a real node?
#
# One cluster, one node. Coroot (operator, server, node-agent, cluster-agent)
# and three sample applications plus a caller that hits each over HTTP run in
# it. `check` then asks Coroot's Prometheus whether each container was
# detected, whether the caller's connections show up as edges, and, the point
# of the exercise, whether a pod created AFTER the agent started is detected
# (on kind it is not; see docs/platform-mvp/19-coroot-agents.md).
#
#   coroot-stage0.sh up [--yes]    create the AKS cluster, install Coroot, deploy the apps
#   coroot-stage0.sh workloads     deploy only the apps (PROVISION=none, see below)
#   coroot-stage0.sh check         run the checks (exit 1 if any FAIL)
#   coroot-stage0.sh render        print the Coroot resource and app manifests
#   coroot-stage0.sh down [--yes]  delete everything this script created
#
# `up` and `down` on AKS spend money and delete a resource group; they ask first
# (or take --yes). The cluster is created in its own resource group, tagged
# purpose=coroot-stage0, and `down` refuses any group without that tag.
#
# Settings (environment, all optional):
#   PROVISION=aks|none  aks (default) creates the cluster; none uses an existing
#                       cluster (KUBE_CONTEXT, default: the current context)
#   LOCATION RG CLUSTER VM_SIZE   AKS placement (eastus, coroot-stage0-rg,
#                       coroot-stage0, Standard_D4s_v5). Free control-plane
#                       tier, one Ubuntu node.
#   DOTNET_IMAGE JAVA_IMAGE PYTHON_IMAGE    your own images (CALLER_IMAGE: any image with python3); *_PORT, *_SCHEME
#                       (http|https) and *_CMD (a JSON array) override the
#                       sample defaults. A custom image runs its own entrypoint
#                       unless *_CMD is set.
#   PROM_CONTEXT PROM_NAMESPACE PROM_SERVICE   where Coroot's Prometheus is, if
#                       not in the workload cluster (coroot, coroot-prometheus)
#   OPERATOR_VERSION    Coroot operator chart version (0.10.6)
#
# Passwords go through pipes, never arguments or output. On AKS, kubectl uses a
# private kubeconfig under ~/.coroot-stage0, not your own.
set -euo pipefail

PROVISION="${PROVISION:-aks}"
LOCATION="${LOCATION:-eastus}"
RG="${RG:-coroot-stage0-rg}"
CLUSTER="${CLUSTER:-coroot-stage0}"
VM_SIZE="${VM_SIZE:-Standard_D4s_v5}"
OPERATOR_VERSION="${OPERATOR_VERSION:-0.10.6}"
COROOT_NS=coroot
APPS_NS=stage0
STATE_DIR="${STAGE0_DIR:-$HOME/.coroot-stage0}"
TAG="purpose=coroot-stage0"
CHECK_WAIT="${CHECK_WAIT:-240}"      # seconds to wait for the first metrics
NEW_POD_WAIT="${NEW_POD_WAIT:-150}"  # seconds to wait for a re-created pod

# The sample workloads need no build: a real ASP.NET app, a JVM running the
# JDK's built-in web server, and Python's built-in web server. Own images come
# from whatever registry the cluster can pull from (see the docs page).
DOTNET_IMAGE="${DOTNET_IMAGE:-mcr.microsoft.com/dotnet/samples:aspnetapp}"
JAVA_IMAGE="${JAVA_IMAGE:-eclipse-temurin:21-jdk}"
PYTHON_IMAGE="${PYTHON_IMAGE:-python:3.12-slim}"
CALLER_IMAGE="${CALLER_IMAGE:-python:3.12-slim}"   # needs python3; independent of the app images
DOTNET_PORT="${DOTNET_PORT:-8080}"; JAVA_PORT="${JAVA_PORT:-8080}"; PYTHON_PORT="${PYTHON_PORT:-8080}"
DOTNET_SCHEME="${DOTNET_SCHEME:-http}"; JAVA_SCHEME="${JAVA_SCHEME:-http}"; PYTHON_SCHEME="${PYTHON_SCHEME:-http}"
# `${X-default}`: unset means "use the sample's command if the image is the sample".
DOTNET_CMD="${DOTNET_CMD-}"
JAVA_CMD="${JAVA_CMD-}"
PYTHON_CMD="${PYTHON_CMD-}"
[ -n "$JAVA_CMD" ] || [ "$JAVA_IMAGE" != "eclipse-temurin:21-jdk" ] || JAVA_CMD="[\"jwebserver\",\"-b\",\"0.0.0.0\",\"-p\",\"$JAVA_PORT\",\"-d\",\"/tmp\"]"
[ -n "$PYTHON_CMD" ] || [ "$PYTHON_IMAGE" != "python:3.12-slim" ] || PYTHON_CMD="[\"python\",\"-m\",\"http.server\",\"$PYTHON_PORT\"]"

log() { printf '== %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
need() { for c in "$@"; do command -v "$c" >/dev/null 2>&1 || die "$c is required but not installed"; done; }

confirm() {
  [ "${YES:-}" = 1 ] && return 0
  [ -t 0 ] || die "refusing to $1 without a terminal; pass --yes"
  read -r -p "$1 — continue? [y/N] " ans
  [ "$ans" = y ] || [ "$ans" = Y ] || die "aborted"
}

# kubectl against the workload cluster.
k() { kubectl ${KUBE_CONTEXT:+--context "$KUBE_CONTEXT"} "$@"; }

# ---------------------------------------------------------------- manifests

app_yaml() { # name image port scheme cmd
  local name="$1" image="$2" port="$3" cmd="$5"
  cat <<EOF
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $name
  namespace: $APPS_NS
  labels: {app: $name, app.kubernetes.io/part-of: coroot-stage0}
spec:
  replicas: 1
  selector: {matchLabels: {app: $name}}
  template:
    metadata:
      labels: {app: $name}
    spec:
      containers:
        - name: app
          image: $image
${cmd:+          command: $cmd
}          ports: [{containerPort: $port}]
          readinessProbe:
            tcpSocket: {port: $port}
            initialDelaySeconds: 3
            periodSeconds: 5
          resources:
            requests: {cpu: 50m, memory: 96Mi}
            limits: {cpu: 500m, memory: 512Mi}
---
apiVersion: v1
kind: Service
metadata:
  name: $name
  namespace: $APPS_NS
  labels: {app: $name, app.kubernetes.io/part-of: coroot-stage0}
spec:
  selector: {app: $name}
  ports: [{port: $port, targetPort: $port}]
EOF
}

caller_targets() {
  printf '%s://app-dotnet.%s.svc:%s/ %s://app-java.%s.svc:%s/ %s://app-python.%s.svc:%s/' \
    "$DOTNET_SCHEME" "$APPS_NS" "$DOTNET_PORT" "$JAVA_SCHEME" "$APPS_NS" "$JAVA_PORT" "$PYTHON_SCHEME" "$APPS_NS" "$PYTHON_PORT"
}

workloads_yaml() {
  cat <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: $APPS_NS
  labels: {app.kubernetes.io/part-of: coroot-stage0}
EOF
  app_yaml app-dotnet "$DOTNET_IMAGE" "$DOTNET_PORT" "$DOTNET_SCHEME" "$DOTNET_CMD"
  app_yaml app-java "$JAVA_IMAGE" "$JAVA_PORT" "$JAVA_SCHEME" "$JAVA_CMD"
  app_yaml app-python "$PYTHON_IMAGE" "$PYTHON_PORT" "$PYTHON_SCHEME" "$PYTHON_CMD"
  # The caller makes a new connection to each service every second. TLS
  # verification is off since the samples' certificates, if any, are not ours.
  cat <<EOF
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: caller
  namespace: $APPS_NS
  labels: {app: caller, app.kubernetes.io/part-of: coroot-stage0}
spec:
  replicas: 1
  selector: {matchLabels: {app: caller}}
  template:
    metadata:
      labels: {app: caller}
    spec:
      containers:
        - name: caller
          image: $CALLER_IMAGE
          command: ["python", "-c"]
          args:
            - |
              import os, ssl, time, urllib.request
              ctx = ssl._create_unverified_context()
              targets = os.environ["TARGETS"].split()
              while True:
                  for u in targets:
                      try:
                          urllib.request.urlopen(u, timeout=3, context=ctx if u.startswith("https") else None).read()
                      except Exception as e:
                          print(u, e, flush=True)
                  time.sleep(1)
          env:
            - name: TARGETS
              value: "$(caller_targets)"
          resources:
            requests: {cpu: 20m, memory: 32Mi}
            limits: {cpu: 200m, memory: 128Mi}
EOF
}

# One Coroot resource does both jobs: it runs the server and, because the node-
# and cluster-agents are not disabled, installs them too. A single Keeper is
# enough for one ClickHouse replica.
coroot_yaml() {
  cat <<EOF
apiVersion: coroot.com/v1
kind: Coroot
metadata:
  name: coroot
  namespace: $COROOT_NS
  labels: {app.kubernetes.io/part-of: coroot-stage0}
spec:
  authBootstrapAdminPasswordSecret: {name: coroot-admin, key: password}
  storage: {size: 2Gi}
  clickhouse:
    shards: 1
    replicas: 1
    keeper: {replicas: 1}
    storage: {size: 5Gi}
  prometheus:
    retention: 3d
    storage: {size: 5Gi}
  projects:
    - name: stage0
      apiKeys:
        - description: stage0 agents
          keySecret: {name: coroot-apikey-stage0, key: apiKey}
  apiKeySecret: {name: coroot-apikey-stage0, key: apiKey}
EOF
}

# ---------------------------------------------------------------------- up

provision_aks() {
  need az
  az account show >/dev/null 2>&1 || die "not logged in to Azure: run 'az login' first"
  local sub; sub="$(az account show --query '[name,id]' -o tsv | tr '\t' ' ')"
  log "Azure subscription: $sub"
  confirm "create AKS cluster '$CLUSTER' (1 x $VM_SIZE, free tier) in resource group '$RG' ($LOCATION); it bills until 'down'"
  mkdir -p "$STATE_DIR"; chmod 700 "$STATE_DIR"
  az group create -n "$RG" -l "$LOCATION" --tags "$TAG" -o none
  if az aks show -g "$RG" -n "$CLUSTER" -o none 2>/dev/null; then
    log "cluster $CLUSTER already exists, reusing it"
  else
    log "creating the cluster (about 5 minutes)"
    az aks create -g "$RG" -n "$CLUSTER" --tier free --node-count 1 --node-vm-size "$VM_SIZE" \
      --os-sku Ubuntu --generate-ssh-keys --tags "$TAG" -o none
  fi
  az aks get-credentials -g "$RG" -n "$CLUSTER" --file "$STATE_DIR/kubeconfig" --overwrite-existing >/dev/null
  chmod 600 "$STATE_DIR/kubeconfig"
}

install_coroot() {
  log "Coroot operator $OPERATOR_VERSION"
  k create namespace "$COROOT_NS" --dry-run=client -o yaml | k apply -f - >/dev/null
  k label namespace "$COROOT_NS" pod-security.kubernetes.io/enforce=privileged --overwrite >/dev/null
  helm repo add coroot https://coroot.github.io/helm-charts >/dev/null
  helm repo update coroot >/dev/null
  helm upgrade --install coroot-operator coroot/coroot-operator --version "$OPERATOR_VERSION" \
    -n "$COROOT_NS" --wait --timeout 10m ${KUBE_CONTEXT:+--kube-context "$KUBE_CONTEXT"} >/dev/null

  if ! k -n "$COROOT_NS" get secret coroot-admin >/dev/null 2>&1; then
    head -c 32 /dev/urandom | base64 | tr -d '/+=' | head -c 24 \
      | k -n "$COROOT_NS" create secret generic coroot-admin --from-file=password=/dev/stdin >/dev/null
  fi
  log "Coroot server and agents"
  coroot_yaml | k apply -f - >/dev/null
  until k -n "$COROOT_NS" get pod coroot-coroot-0 >/dev/null 2>&1; do sleep 5; done
  k -n "$COROOT_NS" wait --for=condition=Ready pod/coroot-coroot-0 --timeout=15m >/dev/null
  until k -n "$COROOT_NS" get ds coroot-node-agent >/dev/null 2>&1; do sleep 5; done
  k -n "$COROOT_NS" rollout status ds/coroot-node-agent --timeout=10m
  until k -n "$COROOT_NS" get deploy coroot-cluster-agent >/dev/null 2>&1; do sleep 5; done
  k -n "$COROOT_NS" rollout status deploy/coroot-cluster-agent --timeout=10m
}

deploy_workloads() {
  log "sample workloads (.NET, Java, Python) and a caller"
  workloads_yaml | k apply -f - >/dev/null
  for d in app-dotnet app-java app-python caller; do
    k -n "$APPS_NS" rollout status "deploy/$d" --timeout=10m
  done
}

up() {
  need kubectl helm
  case "$PROVISION" in
    aks) provision_aks; export KUBECONFIG="$STATE_DIR/kubeconfig" ;;
    none) : ;;
    *) die "PROVISION must be aks or none" ;;
  esac
  k wait --for=condition=Ready node --all --timeout=10m >/dev/null
  install_coroot
  deploy_workloads
  cat <<EOF

Stage 0 is up. Give the agents a minute, then:
  $0 check
Coroot UI (user 'admin'):
  kubectl ${KUBE_CONTEXT:+--context $KUBE_CONTEXT }-n $COROOT_NS port-forward svc/coroot-coroot 8080:8080   # http://localhost:8080
  kubectl ${KUBE_CONTEXT:+--context $KUBE_CONTEXT }-n $COROOT_NS get secret coroot-admin -o go-template='{{index .data "password"}}' | base64 -d
Clean up when done (the cluster bills until then):
  $0 down
EOF
}

# ------------------------------------------------------------------- check

PF_PID=""
cleanup_pf() { [ -z "$PF_PID" ] || kill "$PF_PID" >/dev/null 2>&1 || true; }

start_prometheus_forward() {
  local ctx="${PROM_CONTEXT:-${KUBE_CONTEXT:-}}" ns="${PROM_NAMESPACE:-$COROOT_NS}" svc="${PROM_SERVICE:-coroot-prometheus}"
  PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')"
  kubectl ${ctx:+--context "$ctx"} -n "$ns" port-forward "svc/$svc" "$PORT:9090" >/dev/null 2>&1 &
  PF_PID=$!
  trap cleanup_pf EXIT
  for _ in $(seq 1 30); do
    curl -fs "http://127.0.0.1:$PORT/-/ready" >/dev/null 2>&1 && return 0
    sleep 1
  done
  die "could not reach Coroot's Prometheus ($ns/$svc)"
}

# q EXPR -> the first sample's value, or empty
q() {
  curl -fsG "http://127.0.0.1:$PORT/api/v1/query" --data-urlencode "query=$1" | python3 -c '
import sys, json
r = json.load(sys.stdin)["data"]["result"]
print(r[0]["value"][1] if r else "")'
}

# labels EXPR -> each series' labels, minus the noisy ones
labels() {
  curl -fsG "http://127.0.0.1:$PORT/api/v1/query" --data-urlencode "query=$1" | python3 -c '
import sys, json
skip = {"__name__", "container_id", "instance", "job", "machine_id", "systemd_unit"}
seen = set()
for r in json.load(sys.stdin)["data"]["result"]:
    s = ", ".join(f"{k}={v}" for k, v in sorted(r["metric"].items()) if k not in skip and v)
    if s not in seen:
        seen.add(s); print("   ", s or "(no labels)")'
}

# wait_for EXPR SECONDS -> succeeds once EXPR returns a sample
wait_for() {
  local end=$((SECONDS + $2))
  while [ "$SECONDS" -lt "$end" ]; do
    [ -n "$(q "$1")" ] && return 0
    sleep 10
  done
  return 1
}

FAILS=0
pass() { printf 'PASS  %s\n' "$*"; }
fail() { printf 'FAIL  %s\n' "$*"; FAILS=$((FAILS + 1)); }
info() { printf 'INFO  %s\n' "$*"; }

check() {
  need kubectl curl python3
  if [ "$PROVISION" = aks ] && [ -f "$STATE_DIR/kubeconfig" ]; then export KUBECONFIG="$STATE_DIR/kubeconfig"; fi
  start_prometheus_forward

  log "waiting up to ${CHECK_WAIT}s for the agents' first metrics"
  wait_for "container_info{container_id=~\"/k8s/$APPS_NS/.*\"}" "$CHECK_WAIT" || true

  log "detection: is each workload's container known to Coroot?"
  for app in app-dotnet app-java app-python caller; do
    sel="container_id=~\"/k8s/$APPS_NS/$app-.*\""
    if [ -n "$(q "container_info{$sel}")" ]; then
      pass "$app detected"
      t="$(labels "container_application_type{$sel}")"
      if [ -n "$t" ]; then printf '%s\n' "$t" | sed 's/^ */      type: /'; else echo "      type: (none reported)"; fi
    else
      fail "$app not detected"
    fi
  done

  log "edges: does the caller's traffic show up as a connection to each service?"
  for pair in "app-dotnet:$DOTNET_PORT" "app-java:$JAVA_PORT" "app-python:$PYTHON_PORT"; do
    svc="${pair%%:*}"; port="${pair##*:}"
    ip="$(k -n "$APPS_NS" get svc "$svc" -o jsonpath='{.spec.clusterIP}')"
    n="$(q "sum(container_net_tcp_successful_connects_total{container_id=~\"/k8s/$APPS_NS/caller-.*\",destination=\"$ip:$port\"})")"
    if [ -n "$n" ] && [ "${n%%.*}" -gt 0 ] 2>/dev/null; then
      pass "caller -> $svc ($ip:$port): $n connects"
    else
      fail "caller -> $svc ($ip:$port): no connections recorded"
    fi
  done

  log "request metrics (informational: depends on the runtime and on TLS)"
  for app in app-dotnet app-java app-python; do
    n="$(q "count(container_http_requests_total{container_id=~\"/k8s/$APPS_NS/$app-.*\"})")"
    info "$app HTTP request series: ${n:-0}"
  done

  log "new containers: is a pod created after the agent started detected?"
  k -n "$APPS_NS" delete pod -l app=app-java --wait >/dev/null
  k -n "$APPS_NS" rollout status deploy/app-java --timeout=5m >/dev/null
  newpod="$(k -n "$APPS_NS" get pod -l app=app-java -o jsonpath='{.items[0].metadata.name}')"
  if wait_for "container_info{container_id=~\"/k8s/$APPS_NS/$newpod/.*\"}" "$NEW_POD_WAIT"; then
    pass "re-created pod $newpod detected"
  else
    fail "re-created pod $newpod never appeared"
    cat <<EOF
      This is the failure seen on kind: the node-agent registers only containers
      that existed when it started. If it also happens on a real node, the cause
      is not kind's nested namespaces; capture the node-agent log and kernel
      version before concluding anything.
EOF
  fi

  echo
  if [ "$FAILS" -eq 0 ]; then echo "RESULT: all checks passed"; else echo "RESULT: $FAILS check(s) failed"; exit 1; fi
}

# -------------------------------------------------------------------- down

down() {
  case "$PROVISION" in
    aks)
      need az
      tags="$(az group show -n "$RG" --query tags.purpose -o tsv 2>/dev/null || true)"
      [ "$tags" = coroot-stage0 ] || die "resource group '$RG' is missing or not tagged $TAG; refusing to delete it"
      confirm "delete resource group '$RG' and everything in it"
      az group delete -n "$RG" --yes --no-wait
      rm -rf "$STATE_DIR"
      echo "deletion started (runs in the background); check with: az group show -n $RG"
      ;;
    none)
      need kubectl
      confirm "delete namespace '$APPS_NS' (the Coroot install is left in place)"
      k delete namespace "$APPS_NS" --ignore-not-found
      ;;
  esac
}

# -------------------------------------------------------------------- main

cmd="${1:-help}"; shift || true
for a in "$@"; do case "$a" in --yes|-y) YES=1 ;; *) die "unknown argument: $a" ;; esac; done

case "$cmd" in
  up) up ;;
  workloads) need kubectl; deploy_workloads ;;
  check) check ;;
  render) coroot_yaml; echo ---; workloads_yaml ;;
  down) down ;;
  help|-h|--help) sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//' ;;
  *) die "unknown command '$cmd' (up, workloads, check, render, down)" ;;
esac
