#!/usr/bin/env bash
# Checks the spoke side of the operator-based Coroot path: the spoke chart
# renders nothing unless enabled and refuses to render without a server URL, its
# Coroot resource is accepted by a real API server with the operator's own CRD
# (k3s in a container) and a malformed one is rejected, the stage-0 manifests
# are valid, and the credential copy script moves the key through a pipe, never
# an argument. The hub half (a Coroot server in the hub chart) was removed with
# its template. The operator and agents themselves are not run (eBPF needs a
# real node). Needs Docker and network.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
chart="$root/deploy/platform-mvp/chart"
k3s_image="${K3S_IMAGE:-rancher/k3s:v1.31.4-k3s1}"
helm_image="${HELM_IMAGE:-alpine/helm:3.16.2}"
operator_version="${COROOT_OPERATOR_VERSION:-0.10.6}"
name="corootagents-$$"

work="$(mktemp -d)"
cleanup() { docker rm -f "$name-k3s" >/dev/null 2>&1 || true; rm -rf "$work"; }
trap cleanup EXIT
chmod 755 "$work"

fail() { echo "FAIL: $1" >&2; exit 1; }
helm() { docker run --rm -v "$chart":/chart:ro "$helm_image" "$@"; }
kube() { docker exec -i "$name-k3s" kubectl "$@"; }

echo "== disabled by default: no Coroot resource"
helm template s /chart/spoke > "$work/default.yaml"
if grep -q "kind: Coroot" "$work/default.yaml"; then fail "Coroot rendered while disabled"; fi

echo "== enabled without a server URL is refused"
out="$(helm template s /chart/spoke --set coroot.enabled=true 2>&1 || true)"
grep -q "coroot.corootURL must be set" <<<"$out" || fail "no error for a missing corootURL"

echo "== enabled: the resource points the agents at the hub and reads the key from a Secret"
helm template s /chart/spoke --set coroot.enabled=true --set coroot.corootURL=http://hub-control-plane:30080 \
  --set-json 'coroot.nodeAgentResources={"limits":{"memory":"512Mi"}}' > "$work/enabled.yaml"
python3 - "$work/enabled.yaml" "$work" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
cr = [d for d in docs if d["kind"] == "Coroot"]
assert len(cr) == 1, f"expected one Coroot, got {len(cr)}"
spec = cr[0]["spec"]
assert cr[0]["metadata"]["namespace"] == "coroot"
assert spec["agentsOnly"] == {"corootURL": "http://hub-control-plane:30080", "tlsSkipVerify": False}, spec["agentsOnly"]
assert spec["apiKeySecret"] == {"name": "hub-coroot-credentials", "key": "apiKey"}, spec["apiKeySecret"]
assert "apiKey" not in spec, "a plain-text key must never be rendered"
assert spec["nodeAgent"]["resources"]["limits"]["memory"] == "512Mi"
yaml.safe_dump(cr[0], open(sys.argv[2] + "/coroot.yaml", "w"))
bad = yaml.safe_load(open(sys.argv[2] + "/coroot.yaml"))
bad["spec"]["agentsOnly"]["corootURL"] = "hub-control-plane:30080"
yaml.safe_dump(bad, open(sys.argv[2] + "/coroot-bad-url.yaml", "w"))
PY

echo "== the operator's CRD accepts it on a real API server"
helm template op coroot-operator --repo https://coroot.github.io/helm-charts --version "$operator_version" > "$work/operator.yaml" \
  || fail "could not render the coroot-operator chart $operator_version"
python3 - "$work" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1] + "/operator.yaml")) if d]
crd = next(d for d in docs if d["kind"] == "CustomResourceDefinition" and d["spec"]["names"]["kind"] == "Coroot")
yaml.safe_dump(crd, open(sys.argv[1] + "/crd.yaml", "w"))
PY
chmod -R a+rX "$work"
docker run -d --name "$name-k3s" --privileged --tmpfs /run --tmpfs /var/run \
  "$k3s_image" server --disable=traefik --disable=servicelb --disable=metrics-server >/dev/null
for _ in $(seq 1 120); do
  docker exec "$name-k3s" kubectl get nodes 2>/dev/null | grep -q " Ready" && break
  sleep 3
done
if ! docker exec "$name-k3s" kubectl get nodes 2>/dev/null | grep -q " Ready"; then
  if docker logs "$name-k3s" 2>&1 | grep -q "too many open files"; then
    fail "k3s could not start: the host is out of inotify instances (fs.inotify.max_user_instances, often because kind clusters are running); raise it or stop other clusters"
  fi
  fail "k3s did not become ready"
fi
kube create namespace coroot >/dev/null
kube apply --server-side -f - < "$work/crd.yaml" >/dev/null
kube wait --for=condition=Established crd/coroots.coroot.com --timeout=60s >/dev/null
kube apply --dry-run=server -f - < "$work/coroot.yaml" >/dev/null || fail "the rendered Coroot resource was rejected"

echo "== a corootURL that is not a URL is rejected"
if kube apply --dry-run=server -f - < "$work/coroot-bad-url.yaml" >/dev/null 2>&1; then
  fail "a malformed corootURL was accepted"
fi

echo "== stage 0: the script's Coroot resource and workloads are valid, and its shell is clean"
bash -n "$root/hack/platform-mvp/coroot-stage0.sh" || fail "coroot-stage0.sh has a syntax error"
"$root/hack/platform-mvp/coroot-stage0.sh" render > "$work/stage0.yaml"
python3 - "$work" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1] + "/stage0.yaml")) if d]
kinds = [d["kind"] for d in docs]
assert kinds.count("Deployment") == 4 and kinds.count("Service") == 3, kinds   # 3 apps + caller
cr = next(d for d in docs if d["kind"] == "Coroot")
assert cr["spec"]["clickhouse"]["keeper"] == {"replicas": 1}
assert cr["spec"]["apiKeySecret"] == cr["spec"]["projects"][0]["apiKeys"][0]["keySecret"], "agents and project must share one key Secret"
apps = {d["metadata"]["name"]: d for d in docs if d["kind"] == "Deployment"}
assert apps["app-java"]["spec"]["template"]["spec"]["containers"][0]["command"][0] == "jwebserver"
targets = next(e["value"] for e in apps["caller"]["spec"]["template"]["spec"]["containers"][0]["env"] if e["name"] == "TARGETS")
assert targets.count("http://") == 3, targets
PY
# the Coroot resource and the apps, against the real API server
kube create namespace stage0 >/dev/null
python3 - "$work" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1] + "/stage0.yaml")) if d]
yaml.safe_dump_all([d for d in docs if d["kind"] != "Namespace"], open(sys.argv[1] + "/stage0-apply.yaml", "w"))
PY
chmod -R a+rX "$work"
kube apply --dry-run=server -f - < "$work/stage0-apply.yaml" >/dev/null || fail "stage 0 manifests were rejected by the API server"

echo "== the credential copy script passes the key by pipe, not argument"
mkdir "$work/bin"
cat > "$work/bin/kubectl" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$STUB_LOG"
case "$*" in
  *"get secret"*) printf 'c2VjcmV0LWtleS12YWx1ZQ==' ;;   # base64 of secret-key-value
  *"create namespace"*) echo "kind: Namespace" ;;
  *"create"*)     cat >/dev/null; echo "kind: Secret" ;;
  *"apply"*)      cat >> "$STUB_APPLIED" ;;
esac
STUB
chmod +x "$work/bin/kubectl"
STUB_LOG="$work/argv.log" STUB_APPLIED="$work/applied.txt" KUBECTL="$work/bin/kubectl" \
  "$root/hack/platform-mvp/copy-coroot-credentials.sh" us ctx-us ctx-hub >/dev/null
if grep -q "secret-key-value" "$work/argv.log"; then fail "the key appeared in a command line"; fi
grep -q "get secret coroot-apikey-us" "$work/argv.log" || fail "script did not read the per-spoke hub secret"
grep -q -- "--context ctx-us" "$work/argv.log" || fail "script did not target the spoke context"
if STUB_LOG="$work/argv2.log" KUBECTL="$work/bin/false-kubectl" "$root/hack/platform-mvp/copy-coroot-credentials.sh" 2>/dev/null; then
  fail "script ran without a spoke argument"
fi

echo "PASS: coroot agents wiring"
