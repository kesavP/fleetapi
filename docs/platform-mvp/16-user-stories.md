# User Stories

Capabilities are in [Capabilities & Use Cases](15-capabilities.md). This page shows how each role uses them.

✅ built and tested

## Stories

| ID | As a | I want to | So that | Status |
|----|------|-----------|---------|--------|
| S1 | Platform admin | onboard a tenant with one values entry | the tenant gets a namespace and three roles on every spoke | ✅ |
| S2 | Platform admin | order a workload for a tenant in several regions with one object | I do not repeat work per region | ✅ |
| S3 | Platform admin | add a region without changing controller code | the platform grows by configuration | ✅ |
| S4 | Platform admin | see reconcile rates and errors per tenant and region | I can spot a failing tenant or region | ✅ |
| S5 | Tenant admin | manage every workload in my namespace | my team is self-sufficient | ✅ |
| S6 | Tenant developer | create and update workloads but not delete them | I ship safely | ✅ |
| S7 | Tenant analyst | read workloads and status only | I can report without risk | ✅ |
| S8 | Tenant | be unable to see or touch another tenant | my data stays mine | ✅ |
| S9 | Tenant admin | order a set of 1–3 small VMs, size S or M | I get a cheap Linux machine to try things on | ✅ |
| S10 | Tenant developer | open a console on my VM | I can log in without any network exposure | ✅ (RBAC tested; see note) |

## S1 · Onboard a tenant

Add the tenant to `tenants:` in `chart/spoke/values.yaml` and its group to Dex, then `make deploy-wave4`.

```mermaid
sequenceDiagram
    actor PA as Platform admin
    participant V as chart/spoke values
    participant H as Helm
    participant S as Each spoke
    PA->>V: add tenant acme-corp + 3 Dex groups
    PA->>H: make deploy-wave4
    H->>S: Namespace acme-corp
    H->>S: Roles admin, developer, analyst
    H->>S: RoleBindings to dex:acme-corp-*
    S-->>PA: tenant ready on us and eu
```

## S2 · Order a global workload

```yaml
apiVersion: platform.example.com/v1alpha1
kind: GlobalWidget
metadata:
  name: acme-prod
  labels: { platform.example.com/tenant: acme-corp }
spec:
  regions: [us, eu]
  message: "hello"
```

```mermaid
sequenceDiagram
    actor PA as Platform admin
    participant K as Kro (hub)
    participant B as Binding controller
    participant US as Spoke us
    participant EU as Spoke eu
    PA->>K: apply GlobalWidget
    K->>K: forEach region, one RegionalWidgetRequest
    K-->>B: acme-prod-us, acme-prod-eu
    B->>US: create Widget in ns acme-corp
    B->>EU: create Widget in ns acme-corp
    US-->>B: phase Ready
    EU-->>B: phase Ready
    B-->>K: status.regions per request
    K-->>PA: GlobalWidget status lists both regions
```

## S3 · Add a region

```mermaid
flowchart LR
    A["SPOKES += name<br/>+ KIND_NODE_IMAGE"] --> B["values-name.yaml<br/>token audience"]
    B --> C["fleet.clusters entry<br/>+ spokeTokens entry"]
    C --> D["make clusters deploy"]
    D --> E["Provider polls ClusterProfile<br/>engages the spoke"]
    E --> F["regions: name works"]
```

No Go code changes. `eu` was added this way. Steps: [Phase 99](99-extending-to-eu-asia.md).

## S5–S8 · Day to day on a spoke

```mermaid
sequenceDiagram
    actor U as Tenant user
    participant D as Dex
    participant A as Spoke API server
    U->>D: log in
    D-->>U: ID token with groups
    U->>A: request Widget in acme-corp
    A->>A: validate token (AuthenticationConfiguration)
    A->>A: RBAC by dex:acme-corp-* group
    alt allowed
        A-->>U: result
    else other tenant or missing verb
        A-->>U: Forbidden
    end
```

| Role | create | update | delete | read | other tenant |
|------|:------:|:------:|:------:|:----:|:------------:|
| admin | ✅ | ✅ | ✅ | ✅ | ❌ |
| developer | ✅ | ✅ | ❌ | ✅ | ❌ |
| analyst | ❌ | ❌ | ❌ | ✅ | ❌ |

Tests 19 and 20 simulate users with `--as` and `--as-group`; test 16 checks Dex discovery and token issue.

## S4 · Watch the platform

```mermaid
flowchart LR
    BC["Binding controller<br/>reconcile_total by tenant, region, result"] --> P["Prometheus"]
    V["OIDC verifier<br/>AUDIT lines"] --> EX["Event exporter + Promtail"]
    EX --> L["Loki"]
    CR["Chainsaw CronJob<br/>every 2 min"] --> L
    P --> G["Grafana<br/>Controller Deep Dive<br/>Cluster Fitness<br/>Chainsaw Results"]
    L --> G
```

## S9–S10 · Order VMs

The operator creates the `VmSet` for the tenant, like a `GlobalWidget` (tenants have no hub identity). Scaling and deleting work too: replicas scale down highest index first, and deleting the `VmSet` removes its VMs and Secret from `eu`. Tests 24–29.

```mermaid
sequenceDiagram
    actor TA as Tenant admin
    participant K as Kro (hub)
    participant VB as VM binding controller
    participant EU as Spoke eu (KubeVirt)
    TA->>K: VmSet {size S, replicas 2, ssh key}
    K->>K: S maps to 1 vCPU, 1 GiB
    K->>K: Secret with tenant label
    K-->>VB: RegionalVmSetRequest
    VB->>EU: copy Secret to ns acme-corp
    VB->>EU: create VirtualMachine x2
    EU-->>VB: VMs Running
    VB-->>K: status Ready, 2 of 2
    TA->>EU: virtctl console (RBAC checked)
```

| Tier | vCPU | Memory |
|------|-----:|-------:|
| S | 1 | 1 GiB |
| M | 2 | 2 GiB |

Console access is enforced by RBAC (admin and developer yes, analyst and other tenants no; test 28). An interactive `virtctl console` session itself is not part of the automated suite.
