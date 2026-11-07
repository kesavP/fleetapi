# Sourced by checks.sh. It reaches telemetry-ingest one of two ways:
#
#  - in a cluster (the Chainsaw runs): kubectl port-forward to the Service, and
#    the keys from Secret telemetry-ingest-keys, using the kubeconfig that
#    Chainsaw exports as CHAINSAW_KUBECONFIG_HUB (the cluster that runs the
#    service: the kind hub, or a GKE cluster);
#  - TI_URL set: talk to that URL, with keys from TI_KEY_<PROJECT>_INGEST and
#    TI_KEY_<PROJECT>_READ. This runs the same checks against a container or an
#    existing port-forward. The checks that call kubectl then use
#    TI_KUBE_CONTEXT, or kubectl's current context.
#
# TI_PROJECT is the project the checks write to and read from (default us), and
# TI_OTHER_PROJECT a second one for the isolation checks (default eu). Set
# TI_OTHER_PROJECT to the empty string for a deployment with a single project:
# the checks that need two are then skipped, and say so.
set -euo pipefail

NS="${TI_NAMESPACE:-monitoring}"
STATE="${TI_STATE_DIR:-${TMPDIR:-/tmp}}/telemetry-ingest-chainsaw-run"

# kc: kubectl against the cluster that runs the service.
kc() {
  if [ -n "${CHAINSAW_KUBECONFIG_HUB:-}" ]; then
    kubectl --kubeconfig "$CHAINSAW_KUBECONFIG_HUB" "$@"
  elif [ -n "${TI_KUBE_CONTEXT:-}" ]; then
    kubectl --context "$TI_KUBE_CONTEXT" "$@"
  else
    kubectl "$@"
  fi
}

connect() {
  if [ -n "${TI_URL:-}" ]; then
    URL="$TI_URL"
    return
  fi
  : "${CHAINSAW_KUBECONFIG_HUB:?CHAINSAW_KUBECONFIG_HUB is not set; run through make validate-telemetry-ingest or validate-telemetry-ingest-gke}"
  local port=$((20000 + RANDOM % 20000))
  kc -n "$NS" port-forward svc/telemetry-ingest "$port:80" >/dev/null 2>&1 &
  PF_PID=$!
  trap 'kill "$PF_PID" 2>/dev/null || true' EXIT
  URL="http://127.0.0.1:$port"
  local i
  for i in $(seq 1 30); do
    if curl -sf "$URL/readyz" >/dev/null 2>&1; then return; fi
    sleep 1
  done
  echo "telemetry-ingest did not become ready at $URL" >&2
  exit 1
}

# key <project> <ingest|read>
key() {
  if [ -n "${TI_URL:-}" ]; then
    local var
    var="TI_KEY_$(echo "$1" | tr '[:lower:]' '[:upper:]')_$(echo "$2" | tr '[:lower:]' '[:upper:]')"
    echo "${!var}"
    return
  fi
  kc -n "$NS" get secret telemetry-ingest-keys -o go-template="{{index .data \"$1.$2\"}}" | base64 -d
}

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# expect_status <want> <description> <curl args...>
expect_status() {
  local want="$1" desc="$2" got
  shift 2
  got="$(curl -s -o /dev/null -w '%{http_code}' "$@")" || got="curl-error"
  [ "$got" = "$want" ] || fail "$desc: HTTP $got, wanted $want"
  echo "ok: $desc ($got)"
}

# get <project> <read-key> <path>: the response body of a query
get() {
  curl -sf -H "X-API-Key: $2" "$URL/api/v1/projects/$1/$3"
}

# wait_for <description> <seconds> <command...>: rows reach ClickHouse a few
# seconds after they are accepted, so reads retry.
wait_for() {
  local desc="$1" secs="$2" i
  shift 2
  for ((i = 0; i < secs; i++)); do
    if "$@" >/dev/null 2>&1; then
      echo "ok: $desc"
      return 0
    fi
    sleep 1
  done
  fail "timed out after ${secs}s waiting for: $desc"
}

run_id() { cat "$STATE"; }
