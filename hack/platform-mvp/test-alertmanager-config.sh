#!/usr/bin/env bash
# Validates the Alertmanager config in the infrastructure chart values with
# amtool: the file must load, and each alert must route to the right receiver.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
am_image="${AM_IMAGE:-prom/alertmanager:v0.27.0}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
chmod 755 "$work" # the alertmanager image runs as a non-root user

python3 - "$root/deploy/platform-mvp/chart/infrastructure/values.yaml" "$work/alertmanager.yml" <<'PY'
import sys, yaml
v = yaml.safe_load(open(sys.argv[1]))
cfg = v["kube-prometheus-stack"]["alertmanager"]["config"]
yaml.safe_dump(cfg, open(sys.argv[2], "w"))
PY
# The token file is mounted into the Alertmanager pod; give the config check
# a stand-in so a missing file cannot mask a real error.
mkdir -p "$work/secrets/alert-router-token" "$work/config"
echo test-token > "$work/secrets/alert-router-token/token"
sed -i "s#/etc/alertmanager/secrets#/t/secrets#; s#/etc/alertmanager/config/\*.tmpl#/t/config/*.tmpl#" "$work/alertmanager.yml"
chmod -R a+rX "$work"

amtool() { docker run --rm -v "$work":/t:ro --entrypoint amtool "$am_image" "$@"; }

amtool check-config /t/alertmanager.yml

# expect <receiver> <labels...>: the alert must land on exactly that receiver.
expect() {
  local receiver="$1"; shift
  if ! amtool config routes test --config.file=/t/alertmanager.yml --verify.receivers="$receiver" "$@" >/dev/null; then
    echo "FAIL: $* did not route to $receiver" >&2
    exit 1
  fi
  echo "ok: $* -> $receiver"
}

expect alert-router alertname=VmSetNotReady tenant=acme-corp vmset=web region=us severity=warning
expect alert-router alertname=VmSetPodsNotRunning tenant=acme-corp vmset=web region=us
expect alert-router alertname=SpokeAPIUnreachable cluster=us severity=critical
expect alert-router alertname=SpokeAPICertificateExpiring cluster=eu severity=warning
expect alert-router alertname=SpokePodCrashLooping cluster=us namespace=acme-corp pod=p severity=warning
expect alert-router alertname=SpokeMetricsMissing cluster=eu severity=warning
expect alert-router alertname=KubePodCrashLooping namespace=default severity=warning
expect null alertname=Watchdog severity=none
expect null alertname=InfoInhibitor severity=none
