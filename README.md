# Platform MVP: Kro + multicluster-runtime on kind

A guided, test-driven walkthrough proving a multi-cluster platform architecture using
[Kro](https://github.com/kubernetes-sigs/kro) (ResourceGraphDefinition),
[multicluster-runtime](https://github.com/kubernetes-sigs/multicluster-runtime), and
[Chainsaw](https://github.com/kyverno/chainsaw) for declarative E2E validation —
all running locally on `kind`.

**Concept**: A platform team defines a `GlobalWidget` API via Kro on a hub cluster.
A binding controller (multicluster-runtime) fans expanded intents out to a spoke
cluster running a trivial widget-operator. No cloud dependency.

## Where to start

- [Capabilities & Use Cases](docs/platform-mvp/15-capabilities.md) — what the platform offers and who uses it
- [User Stories](docs/platform-mvp/16-user-stories.md) — flows per role, with diagrams
- [How It Is Built](docs/platform-mvp/17-how-it-is-built.md) — deploy order, trust bootstrap, dev loop
- [Architecture overview](docs/platform-mvp/00-overview.md) — full detail

## Why This Architecture

The "widget" itself is a placeholder — what's being validated is the pattern underneath, and it's meant to hold up past a single kind demo:

- **Scales out without code changes.** Adding a region is a `ClusterProfile` + kubeconfig Secret + spoke deployment. The hub-side logic (Kro RGD, binding-controller, provider) resolves cluster identity generically through `mgr.GetCluster(ctx, region)` — no per-region branches to write or maintain. See [`docs/platform-mvp/99-extending-to-eu-asia.md`](docs/platform-mvp/99-extending-to-eu-asia.md).
- **Tenancy is data, not schema.** A tenant is a `platform.example.com/tenant` label value, not a CRD field or a per-tenant code path. Onboarding a tenant means adding a namespace + 3 RoleBindings (admin/developer/analyst) in `values.yaml`. See [`docs/platform-mvp/09-multi-tenancy.md`](docs/platform-mvp/09-multi-tenancy.md).
- **Auth pattern: split identity, least privilege, fail closed.** Controllers authenticate with audience-bound, Kubelet-rotated projected ServiceAccount tokens — no static credentials to leak or rotate by hand. Humans authenticate separately via Dex OIDC. Each identity is authorized minimally (the controller's spoke RBAC is scoped to `widgets` only), the provider clears the fallback TLS cert so a missing token fails the request instead of silently escalating, and `ValidatingAdmissionPolicy` guardrails stop any non-admin identity — controller or human — from touching ClusterRoles or the auth config itself. See [`docs/platform-mvp/10-oidc-trust.md`](docs/platform-mvp/10-oidc-trust.md) and [`docs/design/oidc-trust-v2.md`](docs/design/oidc-trust-v2.md).

## Architecture

```
 hub (kind-hub)                            us / eu (kind-us, kind-eu — same chart)
 ┌──────────────────────┐                 ┌──────────────────────┐
 │ Kro RGD              │                 │ widget-operator      │
 │ GlobalWidget ──┐     │                 │  Widget CRD          │
 │                │     │ binding-ctrl    │                      │
 │ RegionalWidget─┼─────┼────────────────▶│  Widget instances    │
 │ Request (RWR)       │ watches hub,    │                      │
 │                │     │ creates on us   │ oidc-verifier        │
 ├──────────────────────┤                 │  /verify (JWKS-based)│
 │ Observability        │                 │  AUDIT → stdout      │
 │  grafana             │                 └──────────────────────┘
 │  prometheus          │
 │  loki                │                  host (bm4080.taildf7067.ts.net)
 │  event-exporter      │                  ┌──────────────────────┐
 │  ingress-nginx       │                  │ Tailscale            │
 │ Dex IDP (OIDC)       │                  │  :30080 → hub:80     │
 │ cert-manager (TLS)   │                  │  :30443 → hub:443    │
 │ chainsaw CronJob     │                  │ grafana at /         │
 └──────────────────────┘                  │ dex at /dex          │
                                           └──────────────────────┘
```

| Cluster | Nodes | Purpose |
|---------|-------|---------|
| `kind-hub` | 1 control-plane | Hub: Kro API, observability, Dex OIDC, cert-manager, Flux CD, ingress |
| `kind-us` | 1 ctrl-plane + 1 worker | Spoke: widget-operator, oidc-verifier |
| `kind-eu` | 1 ctrl-plane + 1 worker | Spoke (Kubernetes 1.35, labelled `offers-vms`, runs KubeVirt for the `VmSet` service): same components as `us` plus KubeVirt |

## Prerequisites

| Tool | Min Version | Check |
|------|-------------|-------|
| [Go](https://go.dev/dl/) | 1.23+ | `go version` |
| [Docker](https://docs.docker.com/engine/install/) | 24+ | `docker version` |
| [kind](https://kind.sigs.k8s.io/#installation) | 0.26+ | `kind version` |
| [kubectl](https://kubernetes.io/docs/tasks/tools/) | 1.30+ | `kubectl version --client` |
| [Chainsaw](https://kyverno.github.io/chainsaw/latest/quick-start/) | latest | `chainsaw version` |
| [Helm](https://helm.sh/docs/intro/install/) | 3.16+ | `helm version` |
| [Flux CLI](https://fluxcd.io/flux/installation/) | 2.4+ | `flux --version` |

Install:
```bash
go install github.com/kyverno/chainsaw@latest
make install-flux
```

## Quick Start

```bash
make all        # Full loop: lint → test → build → deploy → validate
make grafana    # Port-forward Grafana → http://localhost:3000 (admin/admin)
make clean      # Destroy everything
```

### Step by Step

```bash
make lint       # go vet + gofmt check
make test       # unit tests (root + binding-controller + widget-operator)
make build      # build binding-controller binary → bin/
make deploy     # create clusters + deploy us (widget-operator) + hub (LGTM + Kro + Flux)
make validate   # run all Chainsaw E2E test suites at once
make clean      # destroy everything
```

### Dashboard Links

After deployment, access Grafana at `http://bm4080.taildf7067.ts.net` (login: `admin` / `admin`):

| Dashboard | URL | What It Shows |
|-----------|-----|---------------|
| Cluster Fitness | [`/d/cluster-fitness`](http://bm4080.taildf7067.ts.net/d/cluster-fitness) | Node readiness, CPU%, memory, API server health across both clusters |
| Chainsaw Test Results | [`/d/chainsaw-results`](http://bm4080.taildf7067.ts.net/d/chainsaw-results) | E2E test pass/fail, duration trends, pass rate over time |
| Controller Deep Dive | [`/d/controller-deep-dive`](http://bm4080.taildf7067.ts.net/d/controller-deep-dive) | Reconcile throughput, latency, per-tenant breakdown, spoke API latency |
| Token Rotation | [`/d/token-rotation`](http://bm4080.taildf7067.ts.net/d/token-rotation) | Token rotation rate per region, last rotation age, error rate, debug logs |

```bash
make grafana-url   # print all dashboard URLs
make grafana       # port-forward Grafana → http://localhost:3000 (admin/admin)
```

## Guided Validation — Chainsaw Test Suites

Each test validates one layer of the platform.

### Core Platform (tests 1-6)

```bash
make validate-p1-p6
```

| Test | What It Proves |
|------|----------------|
| 01-hub-cluster-ready | Hub cluster has 1 node Ready |
| 02-us-cluster-ready | Us spoke has 2 nodes (1 ctrl-plane + 1 worker) |
| 04-fleet-registration | ClusterProfile `us` registered on hub via multicluster API |
| 05-kro-globalwidget | Kro RGD expands GlobalWidget → RegionalWidgetRequest per region |
| 06-binding-controller | Controller creates Widget on us spoke |

**What this proves**: A platform team defined a `GlobalWidget` on the hub cluster.
Kro expanded it to `RegionalWidgetRequest` per region. The binding controller
watched and provisioned Widget instances on the spoke cluster.

### Observability Stack (tests 7-9)

```bash
make validate-p7-p9
```

| Test | What It Proves |
|------|----------------|
| 07-observability-stack | Prometheus, Grafana, Loki running; ServiceMonitors exist |
| 08-chainsaw-cronjob | Chainsaw CronJob deployed in cluster |
| 09-ingress-log-shipping | Ingress controller routes to Grafana, Loki accepts pushes |

**What this proves**: LGTM stack deployed via umbrella Helm chart. Grafana
reachable through nginx ingress. Metrics and logs aggregated.

### OIDC Trust (test 10)

```bash
chainsaw test tests/e2e --test 10-oidc-trust
```

| Test | What It Proves |
|------|----------------|
| 10-oidc-trust | Dex issues JWT, oidc-verifier on spoke validates, audit logs in Loki |

**What this proves**: Workload identity (client_credentials) flows from hub Dex
IDP to spoke oidc-verifier. Cross-cluster JWKS-based trust. Full audit trail
from JWT verification → stdout → promtail → Loki → Grafana.

### Rotating Trust & Observability (tests 11-12)

```bash
chainsaw test tests/e2e --test 11-rotating-trust
chainsaw test tests/e2e --test 12-dashboard-metrics
```

| Test | What It Proves |
|------|----------------|
| 11-rotating-trust | **v2**: Projected ServiceAccount tokens provide controller auth; Dex retained for human OIDC |
| 12-dashboard-metrics | All 3 Grafana dashboards loaded; custom Prometheus metrics for binding-controller scraped and visible |

**What this proves**: In v2, cross-cluster trust flows through Kubelet-managed projected
ServiceAccount tokens. Controller credentials auto-rotate — no token-rotator needed.
Dex remains for human user identity via OIDC. Custom metrics
(`binding_controller_reconcile_total`) are served by the application, scraped by
Prometheus, and visible in Grafana dashboards.

## Development Workflow (TDD)

```
🔴 RED → 🟢 GREEN → 🔄 REFACTOR
```

```bash
# Before every change:
make tdd-lint     # verify clean baseline

# RED: Write a failing test
make test         # must FAIL

# GREEN: Write minimal code to pass
make test         # must PASS

# REFACTOR: Improve code, keep tests passing
make tdd-lint     # lint check
make test         # must still PASS
```

## Makefile Targets

| Target | Description |
|--------|-------------|
| `make all` | Full loop: lint → test → build → deploy → validate |
| `make test` | Run all unit tests (5 packages) |
| `make test-race` | Unit tests with race detector |
| `make test-cover` | Unit tests with coverage profiles |
| `make lint` | go vet + gofmt check |
| `make lint-fix` | Auto-fix formatting + go vet |
| `make tdd-lint` | Pre-change lint baseline |
| `make build-images` | Build all 4 Docker images (parallel) |
| `make deploy` | Full 4-wave deployment (CRDs → infra + US → hub-services) |
| `make deploy-wave1` | Wave 1: Install all platform CRDs on hub + us |
| `make deploy-wave2` | Wave 2: Infrastructure (LGTM + Dex + ingress) on hub |
| `make deploy-wave3` | Wave 3: Hub controllers + Kro + fleet on hub |
| `make deploy-wave4` | Wave 4: Widget operator + OIDC verifier on us |
| `make deploy-cd` | Enable GitOps via Flux CD on hub |
| `make validate` | Run all 20 Chainsaw E2E tests |
| `make grafana` | Port-forward Grafana → localhost:3000 |
| `make grafana-url` | Print dashboard URLs |
| `make chainsaw-runner` | Build in-cluster Chainsaw CronJob runner |
| `make install-chainsaw` | Install chainsaw CLI |
| `make install-flux` | Install flux CLI |
| `make clean` | Destroy clusters and artifacts |
| `make clean-artifacts` | Remove bin/ and coverage files only |

## Project Structure

```
.
├── deploy/platform-mvp/
│   ├── chart/                       # 4-wave Helm chart decomposition
│   │   ├── crds/                    #   Wave 1: All platform CRDs
│   │   │   ├── templates/           #     Kro, ClusterProfile, Widget, cert-manager,
│   │   │   │                       #     Prometheus, RegionalWidgetRequest CRDs
│   │   │   ├── Chart.yaml
│   │   │   └── values.yaml
│   │   ├── infrastructure/          #   Wave 2: Hub shared services
│   │   │   ├── templates/           #     Dex, cert-manager-issuer, dashboards,
│   │   │   │                       #     event-exporter, chainsaw-cronjob,
│   │   │   │                       #     grafana-ingress, dex-ingress
│   │   │   ├── dashboards/          #     3 Grafana dashboard JSONs
│   │   │   ├── Chart.yaml           #     Deps: kube-prometheus-stack, loki, promtail,
│   │   │   ├── e2e-values.yaml      #           ingress-nginx, cert-manager
│   │   │   └── values.yaml
│   │   ├── hub-services/            #   Wave 3: Hub controllers + KRO + fleet
│   │   │   ├── templates/           #     Kro controller, binding-controller,
│   │   │   │                       #     RGD, fleet, servicemonitors
│   │   │   ├── Chart.yaml
│   │   │   ├── e2e-values.yaml
│   │   │   └── values.yaml
│   │   └── us/                      #   Wave 4: Spoke chart
│   │       ├── templates/           #     widget-operator, oidc-verifier,
│   │       │                       #     admission-policies, tenant-rbac
│   │       ├── Chart.yaml
│   │       └── values.yaml
│   ├── flux/                        # Flux CD manifests
│   │   ├── bootstrap/               #   One-time bootstrap (GitRepository, Kustomization)
│   │   ├── helmrepositories.yaml    #   4 HelmRepository sources
│   │   └── hub-helmrelease.yaml     #   3 HelmRelease resources with dependsOn
│   ├── kind/                        # kind cluster configs
│   ├── crds/                        # Pre-fetched CRD sources (cached for chart use)
│   └── observability/               # Dockerfile.chainsaw-runner
├── hack/platform-mvp/             # Shell scripts
├── platform-mvp/
│   ├── binding-controller/        # RegionalWidgetRequest → spoke reconciler (Go)
│   ├── vm-binding-controller/     # RegionalVmSetRequest → KubeVirt VMs on VM-capable spokes (Go)
│   ├── widget-operator/           # Trivial spoke operator (Go)
│   ├── oidc-verifier/             # JWKS-based JWT verifier (Go)
│   └── dex-auth-plugin/           # Dex token acquisition plugin (Go)
├── providers/
│   └── cluster-inventory-api/     # ClusterProfile-backed Provider (Go)
├── tests/e2e/                     # Chainsaw test suites (28 tests)
│   └── .chainsaw.yaml
├── docs/platform-mvp/             # Implementation docs per phase (incl. OIDC)
├── .claude/                       # AI assistant working docs (plans/specs)
├── Makefile                       # Build, deploy, validate automation
└── README.md                      # This file
```

## Cleanup

```bash
make clean            # destroy clusters, remove artifacts
make clean-artifacts  # remove bin/ and coverage files only
```

---

## License

© 2026 sojoner