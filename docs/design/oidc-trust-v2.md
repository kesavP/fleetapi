# Design: OIDC Trust v2 — Cross-Cluster Identity with Projected ServiceAccount Tokens

**Status:** In Progress | **Branch:** `feat/oidc-trust-v2` | **Deprecates:** Token Rotator (password grant path), kind-us OIDC CLI flags

## 1. Motivation

Our current trust model has three problems:

1. **Password grant anti-pattern**: The token-rotator authenticates to Dex as `admin@example.com` (mockPassword connector) to get infrastructure tokens. This conflates human and service identity.
2. **Legacy OIDC integration**: kind-us uses `oidc-*` CLI flags (deprecated in favor of `AuthenticationConfiguration`).
3. **No guardrails**: Any valid Dex token with any `sub` can authenticate to the spoke. No namespace-scoping, no anti-escalation.

The solution adopted by the wider kubernetes community (Argo CD, Karpenter, Cluster API) uses **projected ServiceAccount tokens** with Kubelet-managed rotation and the newer `AuthenticationConfiguration` API with `claimValidationRules`.

## 2. Architecture

### 2.1 Identity Model Split

```
SERVICE IDENTITY (controllers, operators)     HUMAN IDENTITY (platform admins, users)
─────────────────────────────────────────     ─────────────────────────────────────
Issuer: Hub kube-apiserver                    Issuer: Dex (or external IdP)
Token: Projected SA token (audience-bound)    Token: OIDC id_token / access_token
Rotation: Kubelet (configurable, ~1h)         Rotation: Standard OIDC refresh
Mapping: claimValidationRules (namespace)     Mapping: claimMappings (group-based)
Guard: ValidatingAdmissionPolicy              Guard: RBAC + AdmissionPolicy
```

### 2.2 Trust Flow (v2)

```
┌─────────────────────────────────────────────────────────────────────┐
│                        HUB CLUSTER (kind-hub)                       │
│                                                                     │
│  ┌──────────────────────────┐    ┌──────────────────────────────┐   │
│  │ Binding Controller Pod   │    │ Dex IDP (human auth only)    │   │
│  │                          │    │                              │   │
│  │ SA: binding-controller   │    │ /dex/.well-known/oidc-config │   │
│  │                          │    │ /dex/keys (JWKS)             │   │
│  │ Projected Volumes:       │    │ /dex/token (OAuth2)          │   │
│  │  /tokens/us-token        │    │                              │   │
│  │    audience: homelab:    │    │ Clients:                     │   │
│  │     us-spoke             │    │  dex-auth-plugin (kubectl)   │   │
│  │                          │    │  chainsaw-test-client        │   │
│  │ Kubelet rotates every 1h │    └──────────────────────────────┘   │
│  └──────────┬───────────────┘                                       │
│             │                                                       │
│  ┌──────────┴───────────────────────────────────────────────────┐   │
│  │ ClusterProfile Provider (v2)                                  │   │
│  │                                                               │   │
│  │  Reads ClusterProfile CR                                      │   │
│  │  Reads {name}-kubeconfig Secret for server + CA only          │   │
│  │  Creates REST config with BearerTokenFile:                    │   │
│  │    /var/run/secrets/tokens/us-token                           │   │
│  │  Clears TLS client cert from kubeconfig (forces SA token)    │   │
│  │  → mgr.Engage(ctx, "us", cluster)                             │   │
│  │                                                               │   │
│  │  clusterKey = SHA256(Host+CAData) only — token changes        │   │
│  │  in the Secret no longer trigger re-engagement                │   │
│  │  No token-rotator dependency                                  │   │
│  └───────────────────────────────────────────────────────────────┘   │
│                                                                     │
│  Hub API server serves OIDC discovery natively:                     │
│    /.well-known/openid-configuration                                │
│    /openid/v1/jwks                                                  │
└───────────────────────────────┬─────────────────────────────────────┘
                                │
                                │ Network: spoke → hub (OIDC discovery)
                                │
                                ▼
┌─────────────────────────────────────────────────────────────────────┐
│                     SPOKE CLUSTER (kind-us)                         │
│                                                                     │
│  ┌─────────────────────────────────────────────────────────────┐    │
│  │ kube-apiserver (AuthenticationConfiguration)                 │    │
│  │                                                              │    │
│  │ jwt:                                                         │    │
│  │   - issuer: https://hub-api:6443   (hub kube-apiserver)     │    │
│  │     audiences: [homelab:us-spoke]    (logical target)        │    │
│  │     claimValidationRules:                                    │    │
│  │       - Only SA from default namespace on hub                │    │
│  │     claimMappings:                                           │    │
│  │       username: {prefix: "hub:", claim: sub}                 │    │
│  │                                                              │    │
│  │   - issuer: https://dex.example.com/dex  (Dex, human auth)  │    │
│  │     audiences: [kubernetes]                                  │    │
│  │     claimMappings:                                           │    │
│  │       username: {prefix: "dex:", claim: sub}                 │    │
│  │       groups: {prefix: "dex:", claim: groups}                │    │
│  └──────────────────────────────────────────────────────────────┘    │
│                                                                     │
│  ┌─────────────────────────────────────────────────────────────┐    │
│  │ ValidatingAdmissionPolicy (Platform Wins)                    │    │
│  │                                                              │    │
│  │  restrict-clusterrole-management:                            │    │
│  │    → Only platform-admins or system identities may manage    │    │
│  │      ClusterRoles                                            │    │
│  │                                                              │    │
│  │  protect-system-namespaces:                                  │    │
│  │    → Non-system identities denied CREATE/UPDATE/DELETE       │    │
│  │      in kube-system, kube-public, kube-node-lease            │    │
│  │                                                              │    │
│  │  protect-auth-config:                                        │    │
│  │    → Only platform-admins may modify AuthenticationConfig    │    │
│  └──────────────────────────────────────────────────────────────┘    │
│                                                                     │
│  ┌─────────────────────────────────────────────────────────────┐    │
│  │ Spoke RBAC for binding-controller                            │    │
│  │                                                              │    │
│  │  ClusterRole: hub-binding-controller                         │    │
│  │    → Widgets CRUD across all namespaces                      │    │
│  │    → Widget/status read                                      │    │
│  │                                                              │    │
│  │  ClusterRoleBinding → hub:system:serviceaccount:             │    │
│  │                        default:binding-controller             │    │
│  └─────────────────────────────────────────────────────────────┘    │
│                                                                     │
│  ┌─────────────┐                                                    │
│  │ Widget Op   │ (unaffected — no kube-apiserver auth needed)       │
│  │ OIDC Verif. │ (unaffected — direct Dex JWKS polling)             │
│  └─────────────┘                                                    │
└─────────────────────────────────────────────────────────────────────┘
```

### 2.3 Key Changes from Current Design

| Component | Current (v1) | Proposed (v2) | Rationale |
|-----------|-------------|---------------|-----------|
| **Token source for controllers** | Dex password grant (`admin@example.com`) | Hub kube-apiserver projected SA token | No password grant anti-pattern; Kubelet auto-rotation |
| **Token source for humans** | Dex | Dex (unchanged) | Keep existing OIDC flow |
| **Spoke auth config** | `oidc-*` CLI flags on kind-us | `AuthenticationConfiguration` resource | Modern API, per-issuer claim rules, structured prefix |
| **Trust boundaries** | Any Dex token accepted | claimValidationRules restrict to specific namespace | Defense in depth |
| **ClusterRegistration** | token-rotator writes a live token into the kubeconfig Secret | Operator writes the kubeconfig Secret once (server + CA only); provider overrides auth with tokenFile | Remove token-rotator, simpler lifecycle |
| **Credential rotation** | token-rotator every 5min + SHA256 of full kubeconfig | Kubelet rotates projected token file (default 1h); clusterKey narrowed to SHA256(Host+CAData) so token writes don't re-trigger engagement | Built-in Kubernetes mechanism |
| **Guardrails** | None | ValidatingAdmissionPolicy (3 policies) | Platform Wins anti-escalation |
| **Identity mapping** | `oidc:<sub>` | `hub:system:serviceaccount:ns:sa` (controllers) / `dex:<sub>` (humans) | Collision-safe, auditable |

## 3. Implementation Phases

### Phase 1: Spoke AuthenticationConfiguration Migration

Replace kind-us `oidc-*` CLI flags with `AuthenticationConfiguration` resource.

**Changes:**
- `deploy/platform-mvp/kind/kind-us.yaml`: Remove `oidc-*` extraArgs; add `--authentication-config` mount
- `deploy/platform-mvp/chart/spoke/templates/auth-config.yaml`: NEW — `AuthenticationConfiguration` resource
- `deploy/platform-mvp/chart/spoke/values.yaml`: Add `authConfiguration` section
- `tests/e2e/tests/10-oidc-trust/chainsaw-test.yaml`: Update test assertions
- `docs/platform-mvp/10-oidc-trust.md`: Update architecture docs

### Phase 2: Projected ServiceAccount Tokens for Controllers

Replace token-rotator Dex password grant with projected SA tokens.

**Changes:**
- `deploy/platform-mvp/chart/hub-services/templates/binding-controller.yaml`: Add projected volume mounts
- `providers/cluster-inventory-api/provider.go`: BearerTokenFile instead of the kubeconfig Secret's bearer token/cert (server + CA still come from the Secret)
- `deploy/platform-mvp/chart/hub-services/templates/token-rotator.yaml`: REMOVE
- `platform-mvp/token-rotator/`: Mark DEPRECATED
- `docs/platform-mvp/07-token-rotator.md`: Mark DEPRECATED

### Phase 3: ValidatingAdmissionPolicy Guardrails

Implement "Platform Wins" model with 3 admission policies.

**Changes:**
- `deploy/platform-mvp/chart/spoke/templates/admission-guardrails.yaml`: NEW
- `deploy/platform-mvp/chart/spoke/templates/platform-admin-rbac.yaml`: NEW
- `tests/e2e/tests/16-guardrails/`: NEW E2E test suite
- `docs/platform-mvp/12-security-guardrails.md`: NEW doc

### Phase 4: Testing & Documentation

Full E2E validation and documentation updates.

## 4. What Stays Unchanged

| Component | Reason |
|-----------|--------|
| **Dex IDP** | Remains as OIDC issuer for human/admin kubectl access |
| **dex-auth-plugin** | Remains for kubectl exec credential plugin |
| **oidc-verifier** | Remains for application-layer JWT verification (/verify endpoint) |
| **Widget Operator** | No kube-apiserver auth — unaffected |
| **Kro RGD** | No auth changes — unaffected |
| **LGTM stack** | No auth changes — unaffected |
| **Chainsaw CronJob** | Updated test assertions, same infrastructure |
| **ClusterProfile CRD** | Stays as cluster inventory — provider reads from it |

## 5. What Gets Removed/Deprecated

| Component | Disposition |
|-----------|-------------|
| **token-rotator** (controller + deployment) | REMOVED — replaced by Kubelet-managed projected token rotation |
| **token-rotator metrics** | REMOVED (`token_rotator_*`) |
| **{region}-spoke-controller Dex clients** | REMOVED from Dex config |
| **{region}-access-kubeconfig Secrets** | REMOVED — token-rotator no longer writes live tokens into a Secret |
| **kind-us oidc-* CLI flags** | REMOVED — replaced by AuthenticationConfiguration |
| **Provider full-kubeconfig SHA256 change detection** | NARROWED — clusterKey is now SHA256(Host+CAData) only; token rotation in the Secret no longer triggers re-engagement |
| **Provider kubeconfig Secret reading** | UNCHANGED — still reads `{name}-kubeconfig` Secret for server + CA; only the auth section is overridden with the projected SA token file |

## 6. Failure Modes and Mitigations

| Failure Mode | Impact | Mitigation |
|---|---|---|
| Spoke cannot reach hub OIDC discovery or JWKS endpoint | SA token authentication to spoke fails | Make hub issuer and JWKS endpoints highly available and reachable from spoke control plane |
| Token audience does not match `homelab:us-spoke` | Spoke rejects the token | Keep projected token audience and `AuthenticationConfiguration` audiences in the same source of truth |
| Token subject is not from `default` namespace on hub | Spoke rejects the token | Enforced by `claimValidationRules` — tokens from other namespaces silently fail auth |
| RBAC binding missing on spoke | Binding controller authenticates but cannot deploy | Manage spoke RBAC declaratively via `binding-controller-rbac.yaml` template |
| Hub signing keys rotate but spoke cannot refresh JWKS | Authentication failures after key rotation | Monitor JWKS reachability and kube-apiserver authentication errors |
| Permissions too broad on spoke | Binding controller can modify more than intended | ClusterRole scoped to `platform.example.com/widgets` only; namespace boundary enforced by spoke RBAC |
| Projected token file missing in controller pod | Provider falls through to kubeconfig cert (pre-v2.1), now fails closed | Provider clears TLS client cert from kubeconfig — if token file is missing, API calls fail with clear auth errors |
| `AuthenticationConfiguration` misconfigured | No entities can authenticate to spoke | `failurePolicy` on guardrails requires kube-apiserver restart; document recovery procedure |
| Guardrail lockout — all platform-admins removed | Cannot modify ClusterRoles or auth config | `failurePolicy: Fail` with documented recovery: kube-apiserver restart with updated config file
