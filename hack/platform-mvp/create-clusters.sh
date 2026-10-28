#!/usr/bin/env bash
# Thin wrapper: cluster creation lives in the Makefile (single source of truth
# for the hub -> auth-config -> spokes ordering and the SPOKES list).
set -euo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../.."
exec make clusters
