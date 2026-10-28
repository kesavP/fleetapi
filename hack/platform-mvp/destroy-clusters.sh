#!/usr/bin/env bash
# Thin wrapper: teardown lives in the Makefile (iterates SPOKES).
set -euo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../.."
exec make clean
