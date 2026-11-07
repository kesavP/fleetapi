#!/usr/bin/env bash
# Copies a project's ingest key from the hub to the spoke, where the node-agent
# DaemonSet reads it. The hub's Secret telemetry-ingest-keys holds one ingest key
# and one read key per project (`<project>.ingest`, `<project>.read`); only the
# ingest key goes to the spoke. The read key never leaves the hub.
#
#   copy-telemetry-credentials.sh <project> [spoke-context] [hub-context]
#
# spoke-context defaults to kind-<project> and hub-context to kind-hub; for other
# clusters pass the contexts explicitly (kubectl config get-contexts).
#
# The key goes through a pipe, never a command-line argument, so it does not
# show up in a process list, and it is never printed. Safe to run again: it
# reports whether the Secret was created, updated or already current. The
# node-agent reads the key from its environment when a pod starts, so after a
# key changes the running pods keep the old one until they restart; RESTART=1
# does that, otherwise the script prints the command.
#
# Environment: KUBECTL (another kubectl), HUB_NAMESPACE (monitoring),
# HUB_SECRET (telemetry-ingest-keys), AGENT_NAMESPACE (telemetry-agent),
# AGENT_SECRET (hub-telemetry-credentials), AGENT_DAEMONSET (node-agent).
set -euo pipefail

project="${1:?usage: $0 <project> [spoke-context] [hub-context]}"
# A context that is passed but empty is almost always an unset variable
# (copy-telemetry-credentials.sh gke "$CTX" "$CTX" in a shell where CTX is not
# set). Defaulting to a kind context then would read or write the wrong cluster,
# so only an argument that is absent gets a default.
if { [ $# -ge 2 ] && [ -z "$2" ]; } || { [ $# -ge 3 ] && [ -z "$3" ]; }; then
  echo "error: an empty context was passed; is the variable you used for it set?" >&2
  echo "       kubectl config get-contexts lists the contexts" >&2
  exit 1
fi
spoke_ctx="${2-kind-$project}"
hub_ctx="${3-kind-hub}"
kubectl="${KUBECTL:-kubectl}"
hub_ns="${HUB_NAMESPACE:-monitoring}"
hub_secret="${HUB_SECRET:-telemetry-ingest-keys}"
ns="${AGENT_NAMESPACE:-telemetry-agent}"
secret="${AGENT_SECRET:-hub-telemetry-credentials}"
daemonset="${AGENT_DAEMONSET:-node-agent}"
min_len=16

die() { echo "error: $*" >&2; exit 1; }

# The project name is spliced into a go-template, so it must be a plain DNS label.
[[ "$project" =~ ^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$ ]] || die "project '$project' is not a valid name (lowercase letters, digits and dashes)"

# `with` prints nothing for a missing entry, where `index` alone could print "<no value>".
hub_get() { "$kubectl" --context "$hub_ctx" -n "$hub_ns" get secret "$hub_secret" -o go-template="$1"; }

encoded="$(hub_get "{{with index .data \"$project.ingest\"}}{{.}}{{end}}" 2>&1)" \
  || die "could not read secret $hub_ns/$hub_secret in context $hub_ctx: $encoded"
if [ -z "$encoded" ]; then
  # Names only, never values.
  known="$(hub_get '{{range $k, $_ := .data}}{{$k}}{{"\n"}}{{end}}' 2>/dev/null | sed -n 's/\.ingest$//p' | tr '\n' ' ')"
  die "secret $hub_ns/$hub_secret has no ingest key for project '$project' (projects with one: ${known:-none})"
fi
key="$(printf '%s' "$encoded" | base64 -d)" || die "the key for '$project' is not valid base64"
[ "${#key}" -ge "$min_len" ] || die "the ingest key for '$project' is shorter than $min_len characters; refusing to copy it"

# What the spoke has now, to tell created from updated from unchanged. A missing
# Secret is normal on a first run.
current="$("$kubectl" --context "$spoke_ctx" -n "$ns" get secret "$secret" \
  -o go-template='{{with index .data "apiKey"}}{{.}}{{end}}' 2>/dev/null | base64 -d 2>/dev/null || true)"

# The namespace may not exist yet: the chart does not create it by default, and
# the Secret has to be there before the DaemonSet's pods can start.
"$kubectl" --context "$spoke_ctx" create namespace "$ns" --dry-run=client -o yaml \
  | "$kubectl" --context "$spoke_ctx" apply -f - >/dev/null

printf '%s' "$key" | "$kubectl" --context "$spoke_ctx" -n "$ns" create secret generic "$secret" \
  --from-file=apiKey=/dev/stdin --dry-run=client -o yaml \
  | "$kubectl" --context "$spoke_ctx" apply -f - >/dev/null

if [ -z "$current" ]; then
  status=created
elif [ "$current" = "$key" ]; then
  status=unchanged
else
  status=updated
fi
echo "$status: ingest key for '$project' in secret $ns/$secret on $spoke_ctx"

if [ "$status" = updated ] && "$kubectl" --context "$spoke_ctx" -n "$ns" get daemonset "$daemonset" >/dev/null 2>&1; then
  if [ "${RESTART:-}" = 1 ]; then
    "$kubectl" --context "$spoke_ctx" -n "$ns" rollout restart "daemonset/$daemonset" >/dev/null
    echo "restarted daemonset/$daemonset so the agents pick up the new key"
  else
    echo "the running agents still hold the old key; restart them with:" >&2
    echo "  $kubectl --context $spoke_ctx -n $ns rollout restart daemonset/$daemonset   (or rerun with RESTART=1)" >&2
  fi
fi
