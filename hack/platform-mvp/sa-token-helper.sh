#!/usr/bin/env sh
set -eu

# sa-token-helper — reads a Kubernetes projected ServiceAccount token
# and returns it as a client-go ExecCredential for kubectl --exec use.
#
# Usage:
#   kubectl --exec=hack/platform-mvp/sa-token-helper
#   kubeconfig exec block:
#     exec:
#       command: /path/to/sa-token-helper
#       args: [/var/run/secrets/tokens/us-token]

TOKEN_FILE="${1:-/var/run/secrets/tokens/us-token}"

if [ ! -f "${TOKEN_FILE}" ]; then
  echo "token file not found: ${TOKEN_FILE}" >&2
  exit 1
fi

TOKEN="$(cat "${TOKEN_FILE}")"

cat <<EOF_JSON
{
  "apiVersion": "client.authentication.k8s.io/v1beta1",
  "kind": "ExecCredential",
  "status": {
    "token": "${TOKEN}"
  }
}
EOF_JSON