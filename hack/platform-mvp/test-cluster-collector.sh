#!/usr/bin/env bash
# Runs the cluster-collector's unit tests (mappers, the Kubernetes source against fake clients, the differ, the runner, the sink, the
# configuration) with the race detector, vet and gofmt, in a Go container. Needs Docker. The end-to-end test against a real API server is
# test-cluster-collector-e2e.sh.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
mod="$root/platform-mvp/cluster-collector"
go_image="${GO_IMAGE:-golang:1.23}"

docker run --rm -v "$mod":/src -v gomodcache:/go/pkg/mod -v gobuildcache:/root/.cache/go-build -w /src \
  -e GOTOOLCHAIN=local -e GOFLAGS=-mod=mod "$go_image" sh -c '
    test -z "$(gofmt -l .)" || { echo "gofmt would change:"; gofmt -l .; exit 1; }
    go vet ./...
    go test ./... -count=1 -race'
echo PASS
