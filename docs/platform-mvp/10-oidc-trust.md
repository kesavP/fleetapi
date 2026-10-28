# Phase 10 — OIDC Cross-Cluster Trust (v2)

**Updated:** feat/oidc-trust-v2 | **Status:** Migrated to AuthenticationConfiguration + Projected SA Tokens

## Overview

Cross-cluster trust follows a split identity model:

| Identity Type | Issuer | Token Type | Rotation | Use Case |
|--------------|--------|-----------|----------|----------|
| **Service** (controllers) | Hub kube-apiserver | Projected SA token (audience-bound) | Kubelet (~1h) | Binding controller → spoke API |
| **Human** (platform admins) | Dex IDP | OIDC id_token / access_token | Standard OIDC refresh | kubectl, platform admin operations |
| **Application** | Dex IDP | Signed JWT | n/a | oidc-verifier `/verify` endpoint |

## Architecture (v2)

```
HUB CLUSTER                              SPOKE CLUSTER (kind-us)
──────────                               ──────────────────────
                                         
[Hub kube-apiserver]                     [spoke kube-apiserver]
  │ OIDC issuer                           │ AuthenticationConfiguration
  │ /.well-known/openid-configuration     │   jwt:
  │ /openid/v1/jwks                       │     - issuer: hub (controllers)
  │                                       │       claimValidationRules:
  │                                       │         SA ns restriction
  │                                       │       claimMappings:
  │                                       │         username: "hub:"+sub
  │                                       │     - issuer: dex (humans)
  │                                       │       claimMappings:
  │                                       │         username: "dex:"+sub
  │                                       │         groups: "dex:"+groups
  │                                       │
[Binding Controller Pod]                  │
  │ SA: binding-controller                │
  │ Projected volumes:                    │   [ValidatingAdmissionPolicy]
  │   /var/run/secrets/tokens/us-token    │     restrict-clusterrole-management
  │     audience: homelab:us-spoke        │     protect-system-namespaces
  │                                       │     protect-auth-config
  │ Kubelet auto-rotates token file       │
  │                                       │   [ClusterRole: hub-binding-controller]
[Dex IDP]                                 │     widgets CRUD, all namespaces
  │ Human auth only                       │
  │ /dex/keys (JWKS)                      │   [Widget Operator]
  │ /dex/token (OAuth2)                   │     (unaffected)
  │                                       │
  │                                       │   [oidc-verifier]
  │                                       │     (unaffected — JWKS polling)
└───────────────────────────────────────  └───────────────────────────────────────
```

## Key Changes from v1

| Component | v1 (legacy) | v2 |
|-----------|------------|-----|
| Spoke auth config | `oidc-*` CLI flags on kube-apiserver | `AuthenticationConfiguration` resource |
| Controller auth | Dex password grant → token-rotator → kubeconfig Secret | Projected SA token → BearerTokenFile in REST config |
| Token rotation | token-rotator (every 5min, SHA256 detection) | Kubelet (configurable, ~1h, file-based) |
| Trust boundaries | Any valid Dex token accepted | `claimValidationRules` restrict by namespace |
| Identity mapping | `oidc:<sub>` | `hub:system:serviceaccount:ns:sa` / `dex:<sub>` |
| Guardrails | None | `ValidatingAdmissionPolicy` (3 policies), plus `system:masters`/`kubeadm:cluster-admins` bypass for cluster-admin identities |
| Token audience | Spoke API server URL (`https://us-control-plane:6443`) | Logical name (`homelab:us-spoke`) — decoupled from network address |
| Controller authorization on spoke | Implicit (kubeconfig cert = cluster-admin) | Explicit `ClusterRole`/`ClusterRoleBinding` scoped to `widgets` only |
| Missing token file | Provider fell back to kubeconfig client cert | Provider clears the cert from the REST config — fails closed, no silent privilege fallback |

## Service Identity Flow

1. Binding controller pod starts with projected SA token volume, audience `homelab:us-spoke` (a logical name, not a URL — it stays valid if the spoke's network address changes)
2. Kubelet writes token to `/var/run/secrets/tokens/us-token`, rotating it before expiry
3. ClusterProfile provider creates a REST config with `BearerTokenFile` pointing to this file, and strips any TLS client cert/key from the base kubeconfig so the token is the *only* usable credential — a missing token file fails the request instead of silently falling back to a cert
4. client-go reads the current token from the file on each API call
5. Spoke kube-apiserver validates the token against hub's OIDC discovery endpoint
6. `claimValidationRules` ensure only ServiceAccounts from the hub's `default` namespace can authenticate
7. The resulting identity (`hub:system:serviceaccount:default:binding-controller`) is authorized by RBAC — see below

## Spoke RBAC for the Controller

Authentication proves *who* the controller is; it grants no permissions by itself. `chart/spoke/templates/binding-controller-rbac.yaml` authorizes the resulting identity to the minimum needed:

```yaml
# ClusterRole: hub-binding-controller
rules:
  - apiGroups: ["platform.example.com"]
    resources: ["widgets"]
    verbs: ["get", "list", "watch", "create", "update", "patch"]
  - apiGroups: ["platform.example.com"]
    resources: ["widgets/status"]
    verbs: ["get", "list", "watch"]
# ClusterRoleBinding subject: hub:system:serviceaccount:default:binding-controller
```

`ClusterRole` (not per-tenant `Role`) is required because the controller creates Widgets across every tenant namespace — but the rules are scoped to `widgets`/`widgets/status` only, so a compromised or misbehaving controller identity cannot touch anything else on the spoke, including its own RBAC. `bindingControllerRBAC.hubIdentity` in `chart/spoke/values.yaml` is the single source of truth for the subject name and must match the `claimMappings.username` prefix in `authConfiguration`.

## Human Identity Flow

1. Platform admin uses `dex-auth-plugin` (exec credential) or OAuth2 flow
2. Dex issues signed JWT
3. Presented to spoke kube-apiserver as Bearer token
4. Spoke validates against Dex JWKS
5. Identity mapped to `dex:<sub>` with `dex:<groups>`
6. RBAC (`ClusterRoleBindings`) authorizes actions

## Application Trust (oidc-verifier)

The oidc-verifier on the spoke continues to provide application-layer JWT verification:
- Polls Dex JWKS every 5 minutes
- Validates Bearer tokens at `/verify` endpoint
- Emits structured AUDIT logs
- Unaffected by the v2 changes (still uses Dex as its trust anchor)

## Deploying

AuthenticationConfiguration is managed via Helm:

```bash
# us/values.yaml
authConfiguration:
  enabled: true
  hubIssuer:
    enabled: true
    url: "https://hub-control-plane:6443"
    audience: "homelab:us-spoke"
    claimValidationRules:
      - expression: 'claims.sub.startsWith("system:serviceaccount:default:")'
        message: "Only ServiceAccounts from the default namespace on the hub may authenticate."
    claimMappings:
      username:
        claim: sub
        prefix: "hub:"
  dexIssuer:
    enabled: true
    url: "https://dex.example.com/dex"
    audiences:
      - kubernetes
    claimMappings:
      username:
        claim: sub
        prefix: "dex:"
      groups:
        claim: groups
        prefix: "dex:"
```

Each spoke's kind config (generated from `kind-spoke.yaml.tmpl` by `make render-kind-config-<spoke>`) mounts its auth file from `deploy/platform-mvp/kind/<spoke>-auth/` — resolved relative to the template, not a hardcoded path — via:
- `extraMounts` (kind node → container)
- `apiServer.extraVolumes` (kubeadm → kube-apiserver pod)
- `--authentication-config` flag

## Manual Debugging

`hack/platform-mvp/sa-token-helper.sh` wraps a projected SA token file as a kubectl `ExecCredential`, so an operator can run `kubectl --context kind-us` using the same token the controller uses, without ever handling a kubeconfig cert:

```yaml
users:
  - name: binding-controller-debug
    user:
      exec:
        command: hack/platform-mvp/sa-token-helper.sh
        args: ["/var/run/secrets/tokens/us-token"]
        apiVersion: client.authentication.k8s.io/v1beta1
```

## Deprecated Components

- **token-rotator** — replaced by Kubelet-managed projected token rotation
- **`oidc-*` CLI flags** — replaced by AuthenticationConfiguration
- **`{region}-access-kubeconfig` Secrets** — no longer written by token-rotator