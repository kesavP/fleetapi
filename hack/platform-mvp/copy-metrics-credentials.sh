#!/usr/bin/env bash
# Copies the hub's metrics-ingest credential to a spoke, where the metrics
# agent reads it. Run it after the hub's hub-services chart is installed (which
# generates the credential) and before enabling metricsAgent on the spoke.
#
#   copy-metrics-credentials.sh <spoke-context> [hub-context]
#
# The password goes through a pipe, never a command-line argument, so it does
# not show up in the process list. Set KUBECTL to use another kubectl.
set -euo pipefail

spoke="${1:?usage: $0 <spoke-context> [hub-context]}"
hub="${2:-kind-hub}"
kubectl="${KUBECTL:-kubectl}"
secret="${METRICS_SECRET:-hub-metrics-credentials}"

password="$("$kubectl" --context "$hub" -n monitoring get secret metrics-ingest-auth \
  -o go-template='{{index .data "password"}}' | base64 -d)"
[ -n "$password" ] || { echo "hub secret metrics-ingest-auth has no password" >&2; exit 1; }

printf '%s' "$password" | "$kubectl" --context "$spoke" -n default create secret generic "$secret" \
  --from-file=password=/dev/stdin --dry-run=client -o yaml \
  | "$kubectl" --context "$spoke" apply -f - >/dev/null
echo "copied the metrics credential to $spoke (secret default/$secret)"
