#!/usr/bin/env bash
set -euo pipefail

SPOKE_CHART="${1:?usage: render-spoke-auth-config.sh <path-to-spoke-chart> <spoke-values-file> [hub-ca-cert-file] [dex-ca-cert-file]}"
SPOKE_VALUES="${2:?usage: render-spoke-auth-config.sh <path-to-spoke-chart> <spoke-values-file> [hub-ca-cert-file] [dex-ca-cert-file]}"
HUB_CA_FILE="${3:-}"
DEX_CA_FILE="${4:-}"

HELM_ARGS=(template spoke-auth "${SPOKE_CHART}" -f "${SPOKE_VALUES}" --show-only templates/auth-config.yaml)
if [ -n "${HUB_CA_FILE}" ]; then
  HELM_ARGS+=(--set-file "authConfiguration.hubIssuer.certificateAuthority=${HUB_CA_FILE}")
fi
if [ -n "${DEX_CA_FILE}" ]; then
  HELM_ARGS+=(--set-file "authConfiguration.dexIssuer.certificateAuthority=${DEX_CA_FILE}")
fi

helm "${HELM_ARGS[@]}" | python3 -c '
import sys, yaml
doc = yaml.safe_load(sys.stdin)
sys.stdout.write(doc["data"]["authentication-configuration.yaml"])
'
