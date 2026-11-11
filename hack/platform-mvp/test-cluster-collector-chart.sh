#!/usr/bin/env bash
# Checks the spoke chart's cluster-collector template by rendering it: nothing renders unless enabled; the settings it cannot do without are
# required and bad ones refused; the service account can only get and list, and cannot read Secrets or ConfigMaps; the pod runs as a
# non-root user with a read-only filesystem and gets its key from a mounted file, not from the environment.
# Rendering only (needs Docker and network); a cluster is not required.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
chart="$root/deploy/platform-mvp/chart"
helm_image="${HELM_IMAGE:-alpine/helm:3.16.4}"
fail() { echo "FAIL: $1" >&2; exit 1; }
render() { docker run --rm -v "$chart":/chart:ro "$helm_image" template s /chart/spoke -s templates/cluster-collector.yaml "$@"; }
on=(--set clusterCollector.enabled=true --set clusterCollector.collectorURL=http://hub:30081 --set clusterCollector.clusterName=gke --set clusterCollector.image.repository=reg/cluster-collector --set clusterCollector.image.tag=v1)

echo "== disabled by default: nothing renders"
out="$(render 2>&1 || true)"
grep -q "could not find template" <<<"$out" || fail "the template rendered while disabled: $out"

echo "== what it cannot do without, and what is refused"
refuses() { # <what> <message fragment> <args...>
  local what="$1" frag="$2"; shift 2
  local o; o="$(render "$@" 2>&1 || true)"
  grep -q "$frag" <<<"$o" || fail "$what was not refused (wanted '$frag'): $o"
  echo "ok: $what"
}
base=(--set clusterCollector.enabled=true)
refuses "no URL" "collectorURL" "${base[@]}" --set clusterCollector.clusterName=gke --set clusterCollector.image.repository=r --set clusterCollector.image.tag=v1
refuses "no cluster name" "clusterName must be set" "${base[@]}" --set clusterCollector.collectorURL=http://h --set clusterCollector.image.repository=r --set clusterCollector.image.tag=v1
refuses "a cluster name in capitals" "must be lower-case" "${on[@]}" --set clusterCollector.clusterName=GKE
refuses "no image" "image.repository must be set" "${base[@]}" --set clusterCollector.collectorURL=http://h --set clusterCollector.clusterName=gke --set clusterCollector.image.tag=v1
refuses "no tag" "image.tag must be set" "${base[@]}" --set clusterCollector.collectorURL=http://h --set clusterCollector.clusterName=gke --set clusterCollector.image.repository=r
refuses "the tag latest" "not latest" "${on[@]}" --set clusterCollector.image.tag=latest
refuses "a URL with a query" "without a query" "${on[@]}" --set 'clusterCollector.collectorURL=http://hub?token=x'
refuses "unknown events" "warning, all or none" "${on[@]}" --set clusterCollector.events=some

echo "== the hub URL falls back to the node-agent's"
render "${base[@]}" --set clusterCollector.clusterName=gke --set clusterCollector.image.repository=r --set clusterCollector.image.tag=v1 --set nodeAgent.collectorURL=http://hub-from-node-agent:30081 \
  | grep -q -- "--endpoint=http://hub-from-node-agent:30081" || fail "the node-agent's URL was not used"
echo "ok"

work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
render "${on[@]}" > "$work/default.yaml"
render "${on[@]}" --set clusterCollector.tlsSkipVerify=true --set clusterCollector.caSecret.name=hub-ca --set 'clusterCollector.excludeNamespaces={kube-system,istio-system}' --set clusterCollector.events=all --set clusterCollector.interval=1m > "$work/options.yaml"

echo "== the service account can only get and list, and not read Secrets or ConfigMaps"
python3 - "$work/default.yaml" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
role = next(d for d in docs if d["kind"] == "ClusterRole")
seen = set()
for rule in role["rules"]:
    assert set(rule["verbs"]) <= {"get", "list"}, "a rule may do more than get and list: %s" % rule
    for g in rule["apiGroups"]:
        for r in rule["resources"]:
            seen.add((g, r))
assert not any(r in ("secrets", "configmaps", "*") for _, r in seen), "it may read secrets or configmaps: %s" % seen
assert all("*" not in rule["apiGroups"] and "*" not in rule["resources"] and "*" not in rule["verbs"] for rule in role["rules"]), "a wildcard"
need = {("", "nodes"), ("", "namespaces"), ("", "pods"), ("", "persistentvolumeclaims"), ("", "events"), ("apps", "deployments"), ("apps", "replicasets"),
        ("apps", "statefulsets"), ("apps", "daemonsets"), ("batch", "cronjobs"), ("batch", "jobs"), ("autoscaling", "horizontalpodautoscalers"), ("keda.sh", "scaledobjects")}
assert need <= seen, "it cannot read: %s" % (need - seen)
assert seen <= need, "it may read more than it uses: %s" % (seen - need)
bind = next(d for d in docs if d["kind"] == "ClusterRoleBinding")
assert bind["subjects"][0]["name"] == "cluster-collector" and bind["roleRef"]["name"] == role["metadata"]["name"]
print("ok: get and list on exactly the kinds it reports, no Secrets, no ConfigMaps, no wildcards")
PY

echo "== the pod: non-root, read-only, the key from a file"
python3 - "$work/default.yaml" "$work/options.yaml" <<'PY'
import sys, yaml
def dep(path):
    return next(d for d in yaml.safe_load_all(open(path)) if d and d["kind"] == "Deployment")
d = dep(sys.argv[1]); spec = d["spec"]; pod = spec["template"]["spec"]; c = pod["containers"][0]
assert spec["replicas"] == 1 and spec["strategy"]["type"] == "Recreate"
assert pod["securityContext"]["runAsNonRoot"] is True and pod["securityContext"]["seccompProfile"]["type"] == "RuntimeDefault"
sc = c["securityContext"]
assert sc["allowPrivilegeEscalation"] is False and sc["readOnlyRootFilesystem"] is True and sc["capabilities"]["drop"] == ["ALL"] and not sc.get("privileged")
assert not c.get("env"), "environment set: %s" % c.get("env")
assert "--api-key-file=/etc/cluster-collector/key/apiKey" in c["args"], c["args"]
assert not any(a.startswith("--api-key=") or a.startswith("--key") for a in c["args"])
vol = next(v for v in pod["volumes"] if v["name"] == "key")
assert vol["secret"]["secretName"] == "hub-telemetry-credentials" and vol["secret"]["items"][0]["key"] == "apiKey"
assert c["image"] == "reg/cluster-collector:v1"
for flag in ("--endpoint=http://hub:30081", "--cluster=gke", "--interval=30s", "--resync=30m", "--events=warning"):
    assert flag in c["args"], (flag, c["args"])
assert "--tls-skip-verify" not in c["args"] and not any(a.startswith("--exclude") or a.startswith("--ca-file") for a in c["args"])
assert c["livenessProbe"]["httpGet"]["path"] == "/healthz" and c["readinessProbe"]["httpGet"]["path"] == "/readyz"
print("ok: one replica, non-root, no privilege, read-only root, no environment, the key a mounted file, no key on the command line")
o = dep(sys.argv[2])["spec"]["template"]["spec"]; oc = o["containers"][0]
for flag in ("--tls-skip-verify", "--exclude-namespaces=kube-system,istio-system", "--ca-file=/etc/cluster-collector/ca/ca.crt", "--events=all", "--interval=1m"):
    assert flag in oc["args"], (flag, oc["args"])
assert any(v["name"] == "ca" and v["secret"]["secretName"] == "hub-ca" for v in o["volumes"])
print("ok: the options reach the pod")
PY
echo PASS
