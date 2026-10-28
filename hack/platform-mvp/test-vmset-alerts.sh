#!/usr/bin/env bash
# Unit-tests the VmSet and spoke-probe alert rules with promtool. Renders the hub-services
# chart so the rules under test are the ones that would be deployed.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
helm_image="${HELM_IMAGE:-alpine/helm:3.16.2}"
prom_image="${PROM_IMAGE:-prom/prometheus:v2.53.0}"
# Extra `helm template` arguments, for trying other thresholds.
read -r -a helm_args <<< "${HELM_ARGS:-}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
chmod 755 "$work" # the promtool image runs as a non-root user

docker run --rm -v "$root/deploy/platform-mvp/chart":/chart:ro "$helm_image" \
  template t /chart/hub-services "${helm_args[@]}" > "$work/rendered.yaml"

python3 - "$work/rendered.yaml" "$work/rules.yaml" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
rules = {d["metadata"]["name"]: d for d in docs if d.get("kind") == "PrometheusRule"}
for name in ("vmset", "spoke-probes", "spoke-metrics"):
    if name not in rules:
        sys.exit(f"PrometheusRule '{name}' not rendered: is its feature enabled in values?")
groups = [g for r in rules.values() for g in r["spec"]["groups"]]
yaml.safe_dump({"groups": groups}, open(sys.argv[2], "w"))
PY

cp "$root/tests/alerts/vmset-alerts.test.yaml" "$work/"
chmod 644 "$work"/*.yaml
docker run --rm -v "$work":/t:ro --entrypoint promtool "$prom_image" check rules --lint-fatal /t/rules.yaml
docker run --rm -v "$work":/t:ro --entrypoint promtool "$prom_image" test rules /t/vmset-alerts.test.yaml
