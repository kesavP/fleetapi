#!/usr/bin/env bash
# Copies a spoke's Coroot API key from the hub to the spoke, where the Coroot
# agents read it. The hub's Coroot server holds one key per spoke, in a Secret
# named coroot-apikey-<spoke> in the coroot namespace with the key `apiKey`.
#
#   copy-coroot-credentials.sh <spoke> [spoke-context] [hub-context]
#
# The key goes through a pipe, never a command-line argument, so it does not
# show up in the process list. Set KUBECTL to use another kubectl.
set -euo pipefail

spoke="${1:?usage: $0 <spoke> [spoke-context] [hub-context]}"
spoke_ctx="${2:-kind-$spoke}"
hub="${3:-kind-hub}"
kubectl="${KUBECTL:-kubectl}"
ns="${COROOT_NAMESPACE:-coroot}"
secret="${COROOT_SECRET:-hub-coroot-credentials}"

key="$("$kubectl" --context "$hub" -n "$ns" get secret "coroot-apikey-$spoke" \
  -o go-template='{{index .data "apiKey"}}' | base64 -d)"
[ -n "$key" ] || { echo "hub secret $ns/coroot-apikey-$spoke has no apiKey" >&2; exit 1; }

# The namespace may not exist yet: the operator install creates it, and this
# runs before that on a first deploy.
"$kubectl" --context "$spoke_ctx" create namespace "$ns" --dry-run=client -o yaml \
  | "$kubectl" --context "$spoke_ctx" apply -f - >/dev/null

printf '%s' "$key" | "$kubectl" --context "$spoke_ctx" -n "$ns" create secret generic "$secret" \
  --from-file=apiKey=/dev/stdin --dry-run=client -o yaml \
  | "$kubectl" --context "$spoke_ctx" apply -f - >/dev/null
echo "copied the Coroot API key to $spoke (secret $ns/$secret)"
