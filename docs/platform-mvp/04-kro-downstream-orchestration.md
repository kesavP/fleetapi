# Phase 4 — Kro Downstream Resource Orchestration

The Kro `GlobalWidget` ResourceGraphDefinition (RGD) on the hub declaratively expands a single `GlobalWidget` resource into one `RegionalWidgetRequest` per target region. The `Widget`/`RegionalWidgetRequest`/`GlobalWidget` resources serve as an end-to-end **guinea pig example** for downstream resource orchestration: they demonstrate the full lifecycle from hub-side declaration through cross-cluster provisioning to spoke-side reconciliation and status aggregation.

---

## Resource Graph

```mermaid
graph LR
    GW["GlobalWidget<br/>(hub)"]
    RWR_US["RegionalWidgetRequest/us<br/>(hub)"]
    RWR_EU["RegionalWidgetRequest/eu<br/>(hub)"]
    RWR_ASIA["RegionalWidgetRequest/asia<br/>(hub)"]
    W_US["Widget<br/>(kind-us)"]
    W_EU["Widget<br/>(kind-eu)"]
    W_ASIA["Widget<br/>(kind-asia)"]

    GW -->|"forEach spec.regions"| RWR_US
    GW -->|"forEach spec.regions"| RWR_EU
    GW -->|"forEach spec.regions"| RWR_ASIA

    RWR_US -.->|"Binding Controller"| W_US
    RWR_EU -.->|"Binding Controller"| W_EU
    RWR_ASIA -.->|"Binding Controller"| W_ASIA
```

## GlobalWidget Schema

Defined in `kro/globalwidget-rgd.yaml`:

```yaml
metadata:
  labels:
    platform.example.com/tenant: string  # Tenant identifier for namespace-scoped isolation
spec:
  regions: []string    # Target regions (us, eu, asia)
  message: string      # Payload carried through to Widgets
```

**Status** (`globalwidget-rgd.yaml:14`):

```yaml
status:
  regions: ${regionalWidgetRequest.map(r, r.status)}
```

This uses Kro's **CEL expression** to aggregate all child `RegionalWidgetRequest` statuses into the parent `GlobalWidget` status — providing a consolidated view across all regions.

## RGD Template

The core template (`globalwidget-rgd.yaml:16-27`) uses `forEach`:

```yaml
resource:
  forEach:
    items: "${schema.spec.regions}"
    name: "${schema.metadata.name}-${region}"
    resource:
      apiVersion: platform.example.com/v1alpha1
      kind: RegionalWidgetRequest
      spec:
        region: "${region}"
        message: "${schema.spec.message}"
      metadata:
        labels:
          platform.example.com/tenant: '${schema.metadata.labels["platform.example.com/tenant"]}'
```

Key behaviors:
- **Naming**: `GlobalWidget.name + "-" + region` (e.g., `acme-production-us`)
- **Template passthrough**: The `platform.example.com/tenant` label is forwarded from the GlobalWidget to each `RegionalWidgetRequest`
- **Zero-code region expansion**: Adding `eu` to `spec.regions` automatically produces a `RegionalWidgetRequest/eu`

## RegionalWidgetRequest CRD

Defined in `deploy/platform-mvp/chart/crds/templates/regionalwidgetrequest-crd.yaml`:

```yaml
spec:
  properties:
    region:    { type: string, enum: [us, eu, asia] }
    message:   { type: string, default: "" }
```

Tenant isolation uses the `platform.example.com/tenant` label on `metadata.labels` — there is no `spec.tenant` field. The label is forwarded from the GlobalWidget's metadata labels through the Kro RGD template.

**Status** subresource: `regions[]` array with `region`, `phase`, `endpoint` per entry.

## Prerequisites

Before applying the RGD, the `RegionalWidgetRequest` CRD must be registered on the hub — Kro resolves resource templates via Kubernetes API discovery. The hub Helm chart handles this ordering via `regionalwidgetrequest-crd.yaml`.

Additionally, Kro's controller needs RBAC for the `platform.example.com` API group (`chart/hub-services/templates/kro-rbac.yaml`).

## Deployment

1. **CRD first**: `kubectl apply -f regionalwidgetrequest-crd.yaml`
2. **Kro RBAC**: `kubectl apply -f kro-rbac.yaml`
3. **RGD**: `kubectl apply -f globalwidget-rgd.yaml`

Or via Helm: `helm install hub ./chart/infrastructure` (templates handle ordering).

## Acceptance

- A `GlobalWidget{regions:[us], message:"hello"}` with label `platform.example.com/tenant: acme-corp` produces exactly one `RegionalWidgetRequest` on hub
- The `RegionalWidgetRequest` carries `region=us`, `message=hello`, and label `platform.example.com/tenant=acme-corp`
- `05-kro-globalwidget` Chainsaw test passes

## Key Files

| File | Purpose |
|------|---------|
| `kro/globalwidget-rgd.yaml` | GlobalWidget RGD definition |
| `kro/regionalwidgetrequest-crd.yaml` | RegionalWidgetRequest CRD |
| `deploy/platform-mvp/chart/crds/templates/regionalwidgetrequest-crd.yaml` | CRD (chart-embedded copy) |
| `deploy/platform-mvp/chart/hub-services/templates/kro-rgd.yaml` | RGD (chart-embedded copy) |
| `deploy/platform-mvp/chart/hub-services/templates/kro-rbac.yaml` | RBAC for platform.example.com API group |