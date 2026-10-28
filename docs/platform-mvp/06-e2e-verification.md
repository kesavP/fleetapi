# Phase 6 — E2E Verification

All 28 Chainsaw E2E tests, run via `make validate`. Tests are declarative YAML files under `tests/e2e/tests/`.

---

## Test Architecture

```mermaid
graph TB
    subgraph "Chainsaw Runner"
        CFG[".chainsaw.yaml<br/>multi-cluster config"]
        TESTS["28 test files<br/>tests/e2e/tests/*/"]
    end

    subgraph "Target Clusters"
        HUB["kind-hub<br/>hub kubeconfig"]
        US["kind-us<br/>us kubeconfig"]
        EU["kind-eu<br/>eu kubeconfig"]
    end

    CFG --> HUB
    CFG --> US
    CFG --> EU
    TESTS -->|"cluster: hub"| HUB
    TESTS -->|"cluster: us"| US
    TESTS -->|"cluster: eu"| EU
    TESTS -->|"cross-cluster ops"| HUB
    TESTS -->|"cross-cluster ops"| US
```

Multi-cluster configuration in `tests/e2e/.chainsaw.yaml`:

```yaml
clusters:
  hub:
    kubeconfig: ./kubeconfig-hub
  us:
    kubeconfig: ./kubeconfig-us-internal
  eu:
    kubeconfig: ./kubeconfig-eu-internal
```

Each test specifies its target cluster via `cluster: hub`, `cluster: us` or `cluster: eu`. Cross-cluster tests use script steps to execute `kubectl` on both clusters.

---

## All 28 Tests

| # | Test | Cluster | What It Validates |
|---|------|---------|-------------------|
| 01 | `hub-cluster-ready` | hub | Hub control-plane node is Ready; core pods running |
| 02 | `us-cluster-ready` | us | Us control-plane + workers Ready; core pods running |
| 03 | `widget-operator-healthy` | us | Widget CRD registered; widget-operator deployment Available; Widget reconciler running |
| 04 | `fleet-registered` | hub | ClusterProfile/us exists; multicluster provider engaged the spoke |
| 05 | `kro-globalwidget-rgd` | hub | Kro RGD registered; creating GlobalWidget{regions:[us]} produces RegionalWidgetRequest |
| 06 | `binding-controller-status-roundtrip` | hub→us | Binding-controller creates Widget on spoke; Widget → Ready; RWR status reflects spoke state |
| 07 | `observability-stack-healthy` | hub | Prometheus, Grafana, Loki pods Ready; all ServiceMonitors present |
| 08 | `chainsaw-cronjob-running` | hub | CronJob scheduled; previous job completed successfully |
| 09 | `ingress-and-log-shipping` | hub | Dex + Grafana ingresses exist with TLS; Event Exporter routes K8s events to Loki |
| 10 | `oidc-trust` | hub→us | Dex IDP healthy; OIDC discovery; JWT issuance; JWKS serving; oidc-verifier cross-cluster JWT validation; AUDIT trail |
| 11 | `rotating-trust` | hub→us | **v2**: Projected SA token volume in binding-controller; Dex retained for human OIDC |
| 12 | `dashboard-metrics` | hub | Prometheus returns data for binding-controller metrics via PromQL queries |
| 13 | `tenant-namespace-isolation` | hub→us | RWR with `label platform.example.com/tenant=acme-corp` → Widget created in `acme-corp` ns on spoke; NOT in default ns |
| 15 | `dashboard-data-rendering` | hub | Grafana dashboards render data for cluster-fitness and controller-deep-dive panels via port-forwarded PromQL queries |
| 16 | `auth-configuration` | us | **v2**: Spoke kube-apiserver uses AuthenticationConfiguration; dual-issuer (hub + Dex); no legacy oidc-* flags |
| 17 | `projected-sa-tokens` | hub→us | **v2**: Hub OIDC discovery; binding-controller projected token volume with correct audience; spoke trust setup |
| 18 | `admission-guardrails` | us | **v2**: Three ValidatingAdmissionPolicy resources exist and are bound; platform-admin RBAC present
| 19 | `multi-tenant-isolation` | hub→us | Cross-tenant isolation (acme-corp | globex-inc)
| 20 | `tenant-rbac-roles` | hub→us | Admin/dev/analyst role enforcement per tenant
| 21 | `platform-admin-access` | hub | Platform admin bypasses admission policies |
| 23 | `eu-cluster-ready` | eu, hub | eu nodes Ready; `ClusterProfile eu` carries `offers-vms`; `eu-kubeconfig` Secret present |
| 24 | `kubevirt-ready` | eu | KubeVirt CR `Deployed`; node `kubevirt.io/schedulable`; hardware virtualization (no emulation) |
| 25 | `vmset-provisioning` | hub | `VmSet` → cloud-init Secret + `RegionalVmSetRequest` with tier cpu/memory; S default; replicas scale; size immutable; bad size/replicas/key rejected |
| 26 | `vmset-lifecycle` | hub→eu | VMs and Secret run on eu; status Ready flows back; scale-down removes highest index; delete cleans eu |
| 27 | `vm-cloudinit` | hub→eu | Cloud-init marker appears in the guest serial log |
| 28 | `vm-console-rbac` | eu | vm-binding-controller scoped to tenant namespaces; console by role tier via SubjectAccessReview |
| 29 | `vmset-unschedulable` | hub | Region without KubeVirt → `Unschedulable`; delete not blocked by the finalizer |
| 30 | `globalwidget-eu` | hub→us,eu | `GlobalWidget{regions:[us,eu]}` → Ready Widget on both spokes; per-region hub status |

---

## Test Flow Map

```mermaid
graph TD
    A["01: Hub Ready"] --> B["02: US Ready"]
    B --> C["03: Widget Operator"]
    C --> D["04: Fleet Registration"]
    D --> E["05: Kro GlobalWidget"]
    E --> F["06: Binding Controller"]
    F --> G["07: Observability Stack"]
    G --> H["08: Chainsaw CronJob"]
    H --> I["09: Ingress + Log Shipping"]
    I --> J["10: OIDC Trust"]
    J --> K["11: Rotating Trust"]
    K --> L["12: Dashboard Metrics"]
    L --> M["13: Tenant Isolation"]
    M --> N["15: Dashboard Data"]
    N --> O["16: Auth Configuration"]
    O --> P["17: Projected SA Tokens"]
    P --> Q["18: Admission Guardrails"]
    M --> R["19: Multi-Tenant Isolation"]
    R --> S["20: Tenant RBAC Roles"]
    Q --> S
    Q --> T["21: Platform Admin Access"]
    D --> U["23: EU Ready"]
    U --> V["24: KubeVirt Ready"]
    V --> W["25: VmSet Provisioning"]
    W --> X["26: VmSet Lifecycle"]
    W --> Y["27: VM Cloud-init"]
    W --> Z["29: VmSet Unschedulable"]
    V --> AA["28: VM Console RBAC"]
    U --> AB["30: GlobalWidget on eu"]

    style A fill:#4caf50,color:#fff
    style B fill:#4caf50,color:#fff
    style C fill:#4caf50,color:#fff
    style D fill:#4caf50,color:#fff
    style E fill:#4caf50,color:#fff
    style F fill:#4caf50,color:#fff
    style G fill:#4caf50,color:#fff
    style H fill:#4caf50,color:#fff
    style I fill:#4caf50,color:#fff
    style J fill:#4caf50,color:#fff
    style K fill:#4caf50,color:#fff
    style L fill:#4caf50,color:#fff
    style M fill:#4caf50,color:#fff
    style N fill:#4caf50,color:#fff
    style O fill:#4caf50,color:#fff
    style P fill:#4caf50,color:#fff
    style Q fill:#4caf50,color:#fff
    style R fill:#4caf50,color:#fff
    style S fill:#4caf50,color:#fff
    style T fill:#4caf50,color:#fff
    style U fill:#4caf50,color:#fff
    style V fill:#4caf50,color:#fff
    style W fill:#4caf50,color:#fff
    style X fill:#4caf50,color:#fff
    style Y fill:#4caf50,color:#fff
    style Z fill:#4caf50,color:#fff
    style AA fill:#4caf50,color:#fff
    style AB fill:#4caf50,color:#fff
```

Tests are ordered to form a dependency chain: earlier tests deploy resources that later tests depend on.

---

## Running Tests

```bash
# Full suite (all 28 tests)
make validate

# Or directly:
cd tests/e2e
chainsaw test . --test-dir tests

# Single test (--config is needed when running from this directory)
chainsaw test --config .chainsaw.yaml tests/10-oidc-trust

# Subset: pass several test directories
chainsaw test --config .chainsaw.yaml tests/18-admission-guardrails tests/19-multi-tenant-isolation tests/20-tenant-rbac-roles tests/21-platform-admin-access
```

## In-Cluster Continuous Testing

The **Chainsaw CronJob** (`chart/infrastructure/templates/chainsaw-cronjob.yaml`) runs the full suite every 2 minutes and pushes JSON results to Loki. The **Chainsaw Results** Grafana dashboard renders pass/fail trends over time.

```mermaid
sequenceDiagram
    participant CRON as CronJob (every 2m)
    participant RUNNER as runner.sh
    participant LOKI as Loki
    participant GRAF as Grafana

    CRON->>RUNNER: Start job
    RUNNER->>RUNNER: chainsaw test → JSON report
    RUNNER->>LOKI: POST /loki/api/v1/push<br/>{chainsaw_results}
    LOKI-->>RUNNER: 204
    GRAF->>LOKI: query {job="chainsaw-runner"}
    LOKI-->>GRAF: test results time series
```

## Test Design Patterns

| Pattern | Used In | Example |
|---------|---------|---------|
| **kubectl apply + assert** | 04, 05, 06, 13 | Apply a resource, assert it becomes Ready |
| **exec into pod** | 03 | `kubectl rollout status` inside cluster |
| **curl + JSON parse** | 09, 10, 12, 15 | Port-forward, HTTP query, JSON assertion |
| **port-forward for distroless** | 12, 15 | No `wget`/`curl` in container → use host port-forward |
| **multi-cluster script** | 06, 10, 11, 13 | Hub-side apply, spoke-side assert across kubeconfigs |
| **py inline URL encode** | 12, 15 | PromQL queries with special chars via `python3 urllib.parse.quote()` |

## Key Files

| File | Purpose |
|------|---------|
| `tests/e2e/.chainsaw.yaml` | Multi-cluster configuration (hub + us kubeconfigs) |
| `tests/e2e/tests/01-hub-cluster-ready/` | Hub readiness test |
| `tests/e2e/tests/02-us-cluster-ready/` | Us readiness test |
| `tests/e2e/tests/03-widget-operator-healthy/` | Widget operator test |
| `tests/e2e/tests/04-fleet-registration/` | Fleet registration test |
| `tests/e2e/tests/05-kro-globalwidget/` | Kro GlobalWidget RGD test |
| `tests/e2e/tests/06-binding-controller/` | Binding controller roundtrip test |
| `tests/e2e/tests/07-observability-stack/` | Observability stack health test |
| `tests/e2e/tests/08-chainsaw-cronjob/` | CronJob test |
| `tests/e2e/tests/09-ingress-log-shipping/` | Ingress + log shipping test |
| `tests/e2e/tests/10-oidc-trust/` | OIDC trust test |
| `tests/e2e/tests/11-rotating-trust/` | v2 rotating trust test (projected SA volumes) |
| `tests/e2e/tests/12-dashboard-metrics/` | Dashboard metrics test |
| `tests/e2e/tests/13-tenant-isolation/` | Tenant isolation test |
| `tests/e2e/tests/15-dashboard-data/` | Dashboard data rendering test |
| `tests/e2e/tests/16-auth-configuration/` | AuthenticationConfiguration validation (v2) |
| `tests/e2e/tests/17-projected-sa-tokens/` | Projected SA token cross-cluster flow (v2) |
| `tests/e2e/tests/18-admission-guardrails/` | ValidatingAdmissionPolicy enforcement (v2)
| `tests/e2e/tests/19-multi-tenant-isolation/` | Cross-tenant isolation test (acme-corp \| globex-inc)
| `tests/e2e/tests/20-tenant-rbac-roles/` | Admin/dev/analyst role enforcement per tenant
| `tests/e2e/tests/21-platform-admin-access/` | Platform admin bypasses admission policies |
| `tests/e2e/tests/23-eu-cluster-ready/` … `30-globalwidget-eu/` | eu spoke, KubeVirt, VmSet service, VM RBAC, eu widget fan-out |