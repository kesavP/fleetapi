#!/usr/bin/env bash
# Loads locally built images into kind clusters, so a deployment needs no
# registry. Usage: kind-load-images.sh <cluster> <image>...
#
# Only this machine's platform is exported. With Docker's containerd image
# store a plain `docker save` (which `kind load docker-image` uses) carries a
# multi-platform index whose other blobs are absent, and the node's containerd
# refuses to import it ("content digest ... not found").
set -euo pipefail

cluster="${1:?usage: $0 <cluster> <image>...}"
shift
[ $# -gt 0 ] || { echo "no images given" >&2; exit 1; }

case "$(uname -m)" in aarch64|arm64) platform=linux/arm64 ;; *) platform=linux/amd64 ;; esac
archive="$(mktemp --suffix=.tar)"
trap 'rm -f "$archive"' EXIT

if ! docker save --platform "$platform" -o "$archive" "$@" 2>/dev/null; then
  # An older Docker has no --platform and no multi-platform store: plain save is fine.
  docker save -o "$archive" "$@"
fi
kind load image-archive "$archive" --name "$cluster"
echo "loaded into kind-$cluster: $*"
