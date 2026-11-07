#!/usr/bin/env bash
# Checks the spoke chart's node-agent DaemonSet: nothing renders unless enabled,
# a missing or malformed hub URL, an unpinned image and a clash with the Coroot
# operator path are refused, the rendered DaemonSet has what the agent needs
# (hostPID, privileged, the key by Secret reference only, the spool volume) and
# a real API server (k3s in a container) accepts it. The agent itself is not run
# (eBPF needs a real node). Needs Docker and network. SKIP_APISERVER=1 skips the
# k3s step.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
chart="$root/deploy/platform-mvp/chart"
k3s_image="${K3S_IMAGE:-rancher/k3s:v1.31.4-k3s1}"
helm_image="${HELM_IMAGE:-alpine/helm:3.16.4}"
name="nodeagentchart-$$"

work="$(mktemp -d)"
cleanup() { docker rm -f "$name-k3s" >/dev/null 2>&1 || true; rm -rf "$work"; }
trap cleanup EXIT
chmod 755 "$work"

fail() { echo "FAIL: $1" >&2; exit 1; }
helm() { docker run --rm -v "$chart":/chart:ro "$helm_image" "$@"; }
kube() { docker exec -i "$name-k3s" kubectl "$@"; }

URL=http://hub-control-plane:30081
ENABLE=(--set nodeAgent.enabled=true --set "nodeAgent.collectorURL=$URL" --set nodeAgent.image.tag=1.37.0)

echo "== disabled by default: nothing renders"
helm template s /chart/spoke > "$work/default.yaml"
if grep -q "node-agent" "$work/default.yaml"; then fail "node-agent rendered while disabled"; fi

expect_error() { # <description> <expected message> <helm args...>
  local desc="$1" want="$2" out
  shift 2
  out="$(helm template s /chart/spoke "$@" 2>&1 || true)"
  grep -qF -- "$want" <<<"$out" || fail "$desc: expected an error containing '$want', got: $out"
  echo "ok: $desc"
}
echo "== bad input is refused"
expect_error "no hub URL" "nodeAgent.collectorURL must be set" --set nodeAgent.enabled=true
expect_error "no image tag" "nodeAgent.image.tag must be set" --set nodeAgent.enabled=true --set "nodeAgent.collectorURL=$URL"
expect_error "URL without a scheme" "must be an http(s) URL" --set nodeAgent.enabled=true --set nodeAgent.collectorURL=hub:30081 --set nodeAgent.image.tag=1
expect_error "URL with a query string" "must be an http(s) URL" --set nodeAgent.enabled=true --set 'nodeAgent.collectorURL=http://hub:1/?x=1' --set nodeAgent.image.tag=1
expect_error "both agent paths enabled" "coroot.enabled and nodeAgent.enabled are both set" \
  "${ENABLE[@]}" --set coroot.enabled=true --set coroot.corootURL=http://x:1

echo "== enabled: the DaemonSet has what the agent needs"
# Only this template: the rest of the spoke chart renders its own namespaces and RBAC.
helm template s /chart/spoke "${ENABLE[@]}" --set nodeAgent.createNamespace=true --set nodeAgent.caSecret.name=hub-ca \
  --set 'nodeAgent.extraArgs={--container-denylist=/k8s/kube-system/.*}' -s templates/node-agent.yaml > "$work/enabled.yaml"
python3 - "$work/enabled.yaml" "$work" "$URL" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
url = sys.argv[3]
by_kind = {}
for d in docs:
    by_kind.setdefault(d["kind"], []).append(d)
assert not [k for k in by_kind if k == "Coroot"], "a Coroot resource was rendered"
ns, = by_kind["Namespace"]
assert ns["metadata"]["labels"]["pod-security.kubernetes.io/enforce"] == "privileged"
sa, = by_kind["ServiceAccount"]
assert sa["automountServiceAccountToken"] is False
ds, = by_kind["DaemonSet"]
pod = ds["spec"]["template"]["spec"]
assert pod["hostPID"] is True, "the agent reads the host through /proc/1/root; hostPID is required"
assert pod["automountServiceAccountToken"] is False
assert {"operator": "Exists"} in pod["tolerations"], "tainted nodes must be covered"
assert pod["nodeSelector"] == {"kubernetes.io/os": "linux"}
c, = pod["containers"]
assert c["securityContext"]["privileged"] is True
assert c["image"] == "ghcr.io/coroot/coroot-node-agent:1.37.0", c["image"]
assert "--cgroupfs-root=/host/sys/fs/cgroup" in c["args"] and "--container-denylist=/k8s/kube-system/.*" in c["args"], c["args"]
env = {e["name"]: e for e in c["env"]}
assert env["COLLECTOR_ENDPOINT"]["value"] == url, env["COLLECTOR_ENDPOINT"]
key = env["API_KEY"]
assert "value" not in key, "the API key must never be rendered in plain text"
assert key["valueFrom"]["secretKeyRef"] == {"name": "hub-telemetry-credentials", "key": "apiKey"}, key
assert env["CA_FILE"]["value"] == "/etc/node-agent/ca/ca.crt"
mounts = {m["name"]: m for m in c["volumeMounts"]}
assert mounts["cgroupfs"]["readOnly"] is True and mounts["cgroupfs"]["mountPath"] == "/host/sys/fs/cgroup"
assert mounts["debugfs"]["mountPath"] == "/sys/kernel/debug"
assert mounts["spool"]["mountPath"] == env["WAL_DIR"]["value"], "the spool must be on the mounted volume"
vols = {v["name"]: v for v in pod["volumes"]}
assert vols["ca"]["secret"]["secretName"] == "hub-ca"
# The spool volume has to be larger than the spool, or the pod is evicted before the cap applies.
def size(s):
    n = int(''.join(ch for ch in s if ch.isdigit()))
    u = ''.join(ch for ch in s if ch.isalpha()).lower()
    return n * {"kb": 10**3, "mb": 10**6, "gb": 10**9, "ki": 2**10, "mi": 2**20, "gi": 2**30}[u]
assert size(vols["spool"]["emptyDir"]["sizeLimit"]) > size(env["MAX_SPOOL_SIZE"]["value"]), "spool volume smaller than the spool"
for k in ("ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding"):
    assert k not in by_kind, f"{k} rendered: the agent needs no Kubernetes API access"
yaml.safe_dump_all([ns, sa, ds], open(sys.argv[2] + "/objects.yaml", "w"))
print("ok: hostPID, privileged, key by Secret reference, spool volume, CA, extra args, no RBAC")
PY

echo "== without a CA Secret nothing about a CA is rendered"
helm template s /chart/spoke "${ENABLE[@]}" -s templates/node-agent.yaml > "$work/noca.yaml"
if grep -qE "CA_FILE|node-agent/ca" "$work/noca.yaml"; then fail "CA wiring rendered without nodeAgent.caSecret.name"; fi

if [ "${SKIP_APISERVER:-}" = 1 ]; then echo "== API server step skipped"; exit 0; fi
echo "== a real API server accepts the rendered objects"
chmod -R a+rX "$work"
docker run -d --name "$name-k3s" --privileged --tmpfs /run --tmpfs /var/run \
  "$k3s_image" server --disable=traefik --disable=servicelb --disable=metrics-server >/dev/null
# Read the logs into a variable first: `docker logs | grep -q` under pipefail
# reports failure when grep exits early and docker gets SIGPIPE.
inotify_hint="k3s could not start: the host is out of inotify instances (fs.inotify.max_user_instances, often because kind clusters are running); raise it (sudo sysctl fs.inotify.max_user_instances=512) or stop other clusters, or run with SKIP_APISERVER=1"
ready=0
for _ in $(seq 1 120); do
  if docker exec "$name-k3s" kubectl get nodes 2>/dev/null | grep -q " Ready"; then ready=1; break; fi
  logs="$(docker logs "$name-k3s" 2>&1 || true)"
  if grep -q "too many open files" <<<"$logs"; then fail "$inotify_hint"; fi
  sleep 3
done
[ "$ready" = 1 ] || fail "k3s did not become ready"
kube apply --dry-run=server -f - < "$work/objects.yaml" >/dev/null || fail "the API server rejected the rendered objects"
echo "ok: Namespace, ServiceAccount and DaemonSet accepted (server-side dry run)"
echo "PASS"
