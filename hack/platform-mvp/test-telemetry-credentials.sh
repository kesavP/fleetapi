#!/usr/bin/env bash
# Tests copy-telemetry-credentials.sh against a stub kubectl, so it needs no
# cluster and no Docker. It checks what matters about moving a secret: the key
# travels by pipe and is never in an argument or in the output, only the ingest
# key is read from the hub and only the spoke is written, created / updated /
# unchanged are told apart, a changed key prompts a restart of the running
# agents, and every failure writes nothing to the spoke.
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/copy-telemetry-credentials.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fail() { echo "FAIL: $1" >&2; echo "---- output:" >&2; cat "$work/out" >&2 2>/dev/null || true; echo "---- kubectl calls:" >&2; cat "$work/log" >&2 2>/dev/null || true; exit 1; }

cat > "$work/kubectl" <<'STUB'
#!/usr/bin/env bash
echo "$*" >> "$STUB_LOG"
hub="${STUB_HUB_SECRET:-telemetry-ingest-keys}"
agent="${STUB_AGENT_SECRET:-hub-telemetry-credentials}"
ds="${STUB_DS_NAME:-node-agent}"
case "$*" in
  *"get secret $hub"*range*) printf 'eu.ingest\neu.read\nus.ingest\nus.read\n' ;;
  *"get secret $hub"*)
    if [ "${STUB_HUB_MISSING:-}" = 1 ]; then echo 'Error from server (NotFound): secrets "telemetry-ingest-keys" not found' >&2; exit 1; fi
    if [ "${STUB_NO_ENTRY:-}" != 1 ]; then printf '%s' "$STUB_KEY_B64"; fi ;;
  *"get secret $agent"*)
    if [ -n "${STUB_CURRENT_B64:-}" ]; then printf '%s' "$STUB_CURRENT_B64"; else exit 1; fi ;;
  *"get daemonset $ds"*) [ "${STUB_DS:-}" = 1 ] || exit 1 ;;
  *"create namespace"*) echo "kind: Namespace" ;;
  *"create secret"*) cat > "$STUB_STDIN"; echo "kind: Secret" ;;
  *apply*) cat > /dev/null ;;
  *"rollout restart"*) : ;;
esac
STUB
chmod +x "$work/kubectl"

KEY="us-ingest-key-0123456789abcdef"
OTHER="old-ingest-key-fedcba9876543210"
b64() { printf '%s' "$1" | base64 -w0; }

# run <expected exit: 0|nonzero> <env assignments...> -- <script args...>
run() {
  local want="$1"; shift
  local envs=()
  while [ "$1" != "--" ]; do envs+=("$1"); shift; done
  shift
  : > "$work/log"; rm -f "$work/stdin"
  local rc=0
  env "${envs[@]}" STUB_LOG="$work/log" STUB_STDIN="$work/stdin" KUBECTL="$work/kubectl" \
    "$script" "$@" > "$work/out" 2>&1 || rc=$?
  if [ "$want" = 0 ] && [ "$rc" != 0 ]; then fail "expected success, exit $rc"; fi
  if [ "$want" = nonzero ] && [ "$rc" = 0 ]; then fail "expected a failure, but it succeeded"; fi
}
has() { grep -qF -- "$1" "$2" || fail "expected '$1' in $2"; }
hasnt() { if grep -qF -- "$1" "$2"; then fail "did not expect '$1' in $2"; fi; }
wrote_nothing() { if grep -qE '(^| )(apply|create) ' "$work/log"; then fail "$1: something was written to a cluster"; fi; }

echo "== first run: created, key by pipe only, hub read-only"
run 0 STUB_KEY_B64="$(b64 "$KEY")" -- us ctx-us ctx-hub
has "created: ingest key for 'us' in secret telemetry-agent/hub-telemetry-credentials on ctx-us" "$work/out"
hasnt "$KEY" "$work/out"
hasnt "$KEY" "$work/log"
[ "$(cat "$work/stdin")" = "$KEY" ] || fail "the key did not arrive on stdin of 'create secret'"
has "--context ctx-hub -n monitoring get secret telemetry-ingest-keys" "$work/log"
has "--context ctx-us create namespace telemetry-agent" "$work/log"
has "--context ctx-us -n telemetry-agent create secret generic hub-telemetry-credentials --from-file=apiKey=/dev/stdin" "$work/log"
hasnt ".read" "$work/log"                                   # the read key is never requested
hasnt ".manage" "$work/log"                                 # nor the manage key: it can change incidents, and stays on the hub
if grep -E -- '--context ctx-hub .*(apply|create) ' "$work/log" >/dev/null; then fail "the hub was written to"; fi
echo "ok"

echo "== defaults: kind-<project> and kind-hub"
run 0 STUB_KEY_B64="$(b64 "$KEY")" -- us
has "--context kind-hub -n monitoring get secret" "$work/log"
has "--context kind-us create namespace" "$work/log"
echo "ok"

echo "== same key already there: unchanged, no restart"
run 0 STUB_KEY_B64="$(b64 "$KEY")" STUB_CURRENT_B64="$(b64 "$KEY")" STUB_DS=1 -- us ctx-us ctx-hub
has "unchanged:" "$work/out"
hasnt "rollout restart" "$work/log"
hasnt "restart them" "$work/out"
echo "ok"

echo "== key changed, agents running: updated, and told to restart"
run 0 STUB_KEY_B64="$(b64 "$KEY")" STUB_CURRENT_B64="$(b64 "$OTHER")" STUB_DS=1 -- us ctx-us ctx-hub
has "updated:" "$work/out"
has "rollout restart daemonset/node-agent" "$work/out"
hasnt "rollout restart" "$work/log"                          # told, not done
hasnt "$KEY" "$work/out"; hasnt "$OTHER" "$work/out"
echo "ok"

echo "== RESTART=1 restarts the agents"
run 0 RESTART=1 STUB_KEY_B64="$(b64 "$KEY")" STUB_CURRENT_B64="$(b64 "$OTHER")" STUB_DS=1 -- us ctx-us ctx-hub
has "--context ctx-us -n telemetry-agent rollout restart daemonset/node-agent" "$work/log"
echo "ok"

echo "== key changed but no DaemonSet yet: nothing to restart"
run 0 STUB_KEY_B64="$(b64 "$KEY")" STUB_CURRENT_B64="$(b64 "$OTHER")" -- us ctx-us ctx-hub
has "updated:" "$work/out"
hasnt "restart" "$work/out"
hasnt "rollout restart" "$work/log"
echo "ok"

echo "== no ingest key for the project: refused, lists the projects that have one, writes nothing"
run nonzero STUB_NO_ENTRY=1 -- us ctx-us ctx-hub
has "no ingest key for project 'us'" "$work/out"
has "eu us" "$work/out"
wrote_nothing "missing entry"
echo "ok"

echo "== hub secret missing: refused, writes nothing"
run nonzero STUB_HUB_MISSING=1 -- us ctx-us ctx-hub
has "could not read secret monitoring/telemetry-ingest-keys" "$work/out"
wrote_nothing "missing hub secret"
echo "ok"

echo "== a short key is refused and not echoed"
run nonzero STUB_KEY_B64="$(b64 abc12)" -- us ctx-us ctx-hub
has "shorter than 16 characters" "$work/out"
hasnt "abc12" "$work/out"
wrote_nothing "short key"
echo "ok"

echo "== bad arguments"
run nonzero -- 
has "usage:" "$work/out"
run nonzero STUB_KEY_B64="$(b64 "$KEY")" -- 'Bad;Name' ctx-us ctx-hub
has "is not a valid name" "$work/out"
wrote_nothing "bad project name"
if [ -s "$work/log" ]; then fail "a bad project name reached kubectl"; fi
echo "ok"

echo "== an empty context is an error, never a fallback to a kind context"
run nonzero STUB_KEY_B64="$(b64 "$KEY")" -- us "" ctx-hub
has "an empty context was passed" "$work/out"
run nonzero STUB_KEY_B64="$(b64 "$KEY")" -- us ctx-us ""
has "an empty context was passed" "$work/out"
run nonzero STUB_KEY_B64="$(b64 "$KEY")" -- gke "" ""
has "an empty context was passed" "$work/out"
if [ -s "$work/log" ]; then fail "an empty context reached kubectl"; fi
echo "ok"

echo "== environment overrides"
run 0 HUB_NAMESPACE=obs HUB_SECRET=keys AGENT_NAMESPACE=agents AGENT_SECRET=creds AGENT_DAEMONSET=agent RESTART=1 \
  STUB_HUB_SECRET=keys STUB_AGENT_SECRET=creds STUB_DS_NAME=agent STUB_DS=1 \
  STUB_KEY_B64="$(b64 "$KEY")" STUB_CURRENT_B64="$(b64 "$OTHER")" -- us ctx-us ctx-hub
has "--context ctx-hub -n obs get secret keys" "$work/log"
has "--context ctx-us -n agents create secret generic creds" "$work/log"
has "--context ctx-us -n agents rollout restart daemonset/agent" "$work/log"
has "updated: ingest key for 'us' in secret agents/creds" "$work/out"
echo "ok"

echo "PASS"
