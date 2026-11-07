# Deploying the platform on kind

One hub (`kind-hub`) and two spokes (`kind-us`, `kind-eu`), all on your machine, with no registry:
images are built locally and loaded into the clusters. `eu` runs KubeVirt for the `VmSet` service.

> Status: the offline tests and the end-to-end checks that run in Docker (`make test-alerts`,
> `test-alertmanager`, `test-alert-flow`, `test-spoke-probe`, `test-spoke-metrics`) were run. The
> `make deploy-local` flow below was **not** run end to end: it needs kind, which was not available
> where this was written. Expect to fix small things on the first run.

## Prerequisites

| Tool | Version | Why |
|------|---------|-----|
| Docker | 25 or newer | builds images; `docker save --platform` is used to load them |
| kind | 0.26+ | the clusters |
| kubectl | 1.30+ | |
| Helm | 3.16+ | |
| Go | 1.23+ | the Makefile calls `go env` and builds the Go services |
| curl | any | `make crds` downloads CRD manifests |

Optional: Chainsaw (`make validate`), Flux CLI (`make deploy-cd`).

Rough size: seven kind nodes (hub, plus control plane and worker on each spoke) running Prometheus,
Loki, Grafana, KubeVirt and Kro. Plan for 12 GB of free RAM or more.

## 1. Check the host

```bash
ss -ltn | grep -E ':(80|443)\s'    # must print nothing: the hub maps host ports 80 and 443
ls /dev/kvm                        # present: real VMs work. Missing: use emulation (step 2)
```

The charts' Dex and ingress settings point at the author's Tailscale hostname
(`bm4080.taildf7067.ts.net`, `INGRESS_HOST` in the Makefile). Controller-to-spoke trust does not use
Dex, so the platform works without it; only human login through Dex needs a reachable host. Set
`INGRESS_HOST` (and the Dex URLs in `chart/spoke/values.yaml`) if you want that.

## 2. Deploy

```bash
make deploy-local
```

With software emulation for the VM spoke (no `/dev/kvm`):

```bash
make deploy-local SPOKE_HELM_ARGS="--set kubevirt.useEmulation=true"
```

`deploy-local` is `make deploy` with the image delivery swapped from "push to Docker Hub" to
"load into kind", and with the local-images overlays (`local-images-values.yaml` in the hub-services
and spoke charts, which change the pull policy from `Always` to `IfNotPresent` so loaded images are
used). It runs, in order:

1. `make crds`: downloads the Kro, cert-manager, prometheus-operator and KubeVirt CRDs.
2. Builds all images, and creates the hub and spoke clusters (in parallel).
3. `make kind-load-images`: loads the hub images into `kind-hub` and the spoke images into each spoke.
4. Wave 1: platform CRDs on the hub and spokes.
5. Wave 2 (infrastructure on the hub: Prometheus, Alertmanager, Grafana, Loki, cert-manager, Dex,
   ingress) and wave 4 (the spokes) in parallel.
6. Refreshes each spoke's apiserver `AuthenticationConfiguration` once the hub is up.
7. Wave 3: hub services (Kro, the binding controllers, alert-router, metrics-ingest, probes, alerts).

Without the shortcut, run the same steps yourself: `make crds build-images-parallel clusters
kind-load-images deploy-wave1`, then `deploy-wave4` and `deploy-wave2`, then
`ensure-spoke-auth-config`, then `deploy-wave3`, passing `HUBSVC_HELM_ARGS` and `SPOKE_HELM_ARGS` as
above. The plain `make deploy` pushes images to `DOCKER_REGISTRY` (default `sojoner`) and needs
push access to it.

## 3. Copy the metrics credential to the spokes

```bash
make spoke-metrics-credentials
```

The hub generates the credential when `hub-services` is installed, so it cannot exist when the
spokes install. The metrics agents start without it, and begin sending once the Secret appears; no
restart is needed.

## 4. Optional: the VmSet gateway

```bash
helm upgrade hub-services deploy/platform-mvp/chart/hub-services -n default \
  -f deploy/platform-mvp/chart/hub-services/e2e-values.yaml \
  -f deploy/platform-mvp/chart/hub-services/local-images-values.yaml \
  --set vmSetGateway.enabled=true --kube-context kind-hub
```

Callers send their own bearer token, so they also need RBAC on `vmsets` in their namespace.

## 5. Verify

```bash
kubectl --context kind-hub get clusterprofile -A                  # us and eu
kubectl --context kind-hub -n default get pods                    # controllers Running
kubectl --context kind-hub -n monitoring get pods                 # prometheus, alertmanager, alert-router, metrics-ingest, blackbox-exporter
kubectl --context kind-us  -n default get pods                    # widget-operator, oidc-verifier, kube-state-metrics, metrics-agent
kubectl --context kind-hub api-resources | grep platform.example.com   # confirm the VmSet resource name
```

A widget on both spokes:

```bash
kubectl --context kind-hub apply -f - <<'YAML'
apiVersion: platform.example.com/v1alpha1
kind: GlobalWidget
metadata: {name: demo, labels: {platform.example.com/tenant: acme-corp}}
spec: {regions: [us, eu], message: hello}
YAML
kubectl --context kind-us -n acme-corp get widgets        # a Widget appears, then Ready
kubectl --context kind-hub get globalwidget demo -o jsonpath='{.status.regions}'
```

A VM set on `eu`:

```bash
kubectl --context kind-hub apply -f - <<'YAML'
apiVersion: platform.example.com/v1alpha1
kind: VmSet
metadata: {name: demo, labels: {platform.example.com/tenant: acme-corp}}
spec:
  regions: [eu]
  size: S
  replicas: 1
  sshPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleExampleExampleExampleExampleExamp me@host"
YAML
kubectl --context kind-hub get vmset demo -o yaml         # status.regions: phase, cluster, vms[].pod, podRestarts
kubectl --context kind-eu -n acme-corp get vm,vmi,pods
```

Metrics and alerts:

```bash
kubectl --context kind-hub -n monitoring port-forward svc/hub-kube-prometheus-stack-prometheus 9090 &
#   kube_pod_info{cluster="us"}                      pod state from the spoke
#   container_memory_working_set_bytes{cluster="us"} pod memory from the spoke
#   vmset_region_phase                               VmSet health on the hub
#   probe_success{check="spoke-api"}                 1 per cluster
kubectl --context kind-hub -n monitoring port-forward svc/hub-kube-prometheus-stack-alertmanager 9093 &
kubectl --context kind-hub -n monitoring logs deploy/alert-router -f     # alerts arrive here as text lines
```

If a Service name differs, check `kubectl -n monitoring get svc`. The names above follow the
`hub` release name the Makefile uses.

Expect log lines for kube-prometheus-stack's own default alerts too (for example controller-manager
and scheduler "down" on kind, whose control-plane components are not scrapable). That is noise from
the stack, not from this platform.

## 6. Failure drills

| Drill | Command | Expected |
|-------|---------|----------|
| Crash-looping pod on a spoke | `kubectl --context kind-us -n acme-corp run crasher --image=busybox:1.36 --restart=Always -- sh -c 'exit 1'` | `SpokePodCrashLooping` after about 5 min |
| Spoke API goes away | `docker pause us-control-plane`, then `docker unpause us-control-plane` | `SpokeAPIUnreachable` after about 2 min; clears after unpause |
| Region with no VM support | a VmSet with `regions: [us]` | `VmSetUnschedulable` after 2 min |
| Agent stops | `kubectl --context kind-us -n default scale deploy/metrics-agent --replicas=0` | `SpokeMetricsMissing` after about 15 min |

`docker pause` keeps the container's IP, unlike a stop and start. In the router log, an unchanged
firing alert must not repeat within 10 minutes, and a resolution is always logged.

## 7. Tests

```bash
make validate            # the Chainsaw end-to-end suites against the running clusters
make test-alerts test-alertmanager test-alert-flow test-spoke-probe test-spoke-metrics   # Docker only
```

## 8. Tear down

```bash
make clean
```

## Known caveats

- Spoke pods are assumed to reach the hub at `hub-control-plane:30909` (the metrics-ingest NodePort).
  If a spoke's `metrics-agent` logs show it cannot connect, check name resolution and that port.
- The proxy and the credential travel over plain HTTP. That is acceptable on kind's private Docker
  network only.
- Public images (Prometheus, kube-state-metrics, nginx, blackbox exporter, Kro, KubeVirt) are pulled
  from the internet by the nodes and can hit anonymous Docker Hub rate limits.
- A cluster listed in the hub's `fleet.clusters` that has no metrics agent raises
  `SpokeMetricsMissing`.
- Prometheus has no persistent storage here, so a restart loses history.
