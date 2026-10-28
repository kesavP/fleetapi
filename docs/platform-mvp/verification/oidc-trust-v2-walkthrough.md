# Cross-Cluster OIDC Trust — End-to-End Walkthrough (v2)

This document walks through the complete cross-cluster OIDC trust flow from
token issuance to authenticated API calls, demonstrating the v2 split identity
model: **projected ServiceAccount tokens for controllers** and **Dex OIDC for
human/platform admin identities**.

## Scenario

A platform admin creates a `GlobalWidget` on the hub cluster. The binding
controller running on the hub must create a `Widget` on the `us` spoke cluster.
The spoke cluster's kube-apiserver validates the controller's identity using a
projected ServiceAccount token issued by the hub's kube-apiserver.

```
┌─────────────────────────────────────────────────────────────────────────┐
│ Step 1: Token Provisioning (Hub)                                         │
│                                                                         │
│  binding-controller Pod                                                 │
│    ServiceAccount: binding-controller                                   │
│    Projected Volume: spoke-tokens                                       │
│      ┌──────────────────────────────────────────────────┐               │
│      │ serviceAccountToken:                              │               │
│      │   path: us-token                                  │               │
│      │   audience: homelab:us-spoke                       │               │
│      │   expirationSeconds: 3600                         │               │
│      └──────────────────────────────────────────────────┘               │
│                                                                         │
│  Kubelet writes token to: /var/run/secrets/tokens/us-token              │
│  Token is auto-rotated ~1h before expiry                                │
│                                                                         │
│  Token claims (decoded):                                                │
│  {                                                                      │
│    "aud": ["homelab:us-spoke"],                                          │
│    "iss": "https://kubernetes.default.svc.cluster.local",               │
│    "sub": "system:serviceaccount:default:binding-controller",           │
│    "exp": 1752000000,                                                   │
│    "iat": 1751996400                                                    │
│  }                                                                      │
└──────────────────────────────┬──────────────────────────────────────────┘
                               │
                               │ Provider: reads ClusterProfile, creates REST config
                               │   Host: https://us-control-plane:6443
                               │   BearerTokenFile: /var/run/secrets/tokens/us-token
                               │
                               ▼
┌─────────────────────────────────────────────────────────────────────────┐
│ Step 2: API Call (Hub → Spoke)                                          │
│                                                                         │
│  RegionalWidgetReconciler                                               │
│    │                                                                    │
│    ├─ mgr.GetCluster(ctx, "us")                                         │
│    │    → cluster-inventory-api Provider returns cluster.Cluster        │
│    │    → REST config has BearerTokenFile (not static token)            │
│    │                                                                    │
│    ├─ spoke.GetClient().Create(ctx, widget)                             │
│    │    → client-go reads token from BearerTokenFile                    │
│    │    → sends HTTP request:                                           │
│    │                                                                     │
│    │    POST /apis/platform.example.com/v1alpha1/namespaces/.../widgets │
│    │    Host: us-control-plane:6443                                     │
│    │    Authorization: Bearer eyJhbGciOiJSUzI1NiIsImtpZCI6...           │
│    │                                                                     │
│    ▼                                                                    │
└──────────────────────────────┬──────────────────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────────────────┐
│ Step 3: Token Validation (Spoke kube-apiserver)                         │
│                                                                         │
│  AuthenticationConfiguration:                                           │
│  ┌────────────────────────────────────────────────────────────────┐    │
│  │ jwt:                                                            │    │
│  │   - issuer:                                                     │    │
│  │       url: https://hub-control-plane:6443                       │    │
│  │       audiences:                                                │    │
│  │         - homelab:us-spoke                                      │    │
│  │       claimValidationRules:                                     │    │
│  │         - expression: >                                         │    │
│  │             claims.sub.startsWith(                              │    │
│  │               "system:serviceaccount:default:")                 │    │
│  │       claimMappings:                                            │    │
│  │         username: {claim: sub, prefix: "hub:"}                  │    │
│  └────────────────────────────────────────────────────────────────┘    │
│                                                                         │
│  Validation steps:                                                      │
│  1. Extract JWT from Authorization header                               │
│  2. Discover issuer: GET /.well-known/openid-configuration on hub       │
│  3. Fetch signing keys: GET /openid/v1/jwks on hub                      │
│  4. Verify JWT signature against hub's public key                       │
│  5. Validate audience: homelab:us-spoke ✓                                │
│  6. Validate issuer: https://hub-control-plane:6443 ✓                   │
│  7. Run claimValidationRules: sub starts with "system:serviceaccount:default:" ✓│
│  8. Map identity: username = hub:system:serviceaccount:default:...      │
│                                                                         │
│  Result: authenticated as hub:system:serviceaccount:default:binding-... │
└──────────────────────────────┬──────────────────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────────────────┐
│ Step 4: Authorization (Spoke RBAC)                                      │
│                                                                         │
│  ClusterRole + ClusterRoleBinding, both named "hub-binding-controller"  │
│  (spoke-side, `chart/spoke/templates/binding-controller-rbac.yaml`):       │
│  ┌────────────────────────────────────────────────────────────────┐    │
│  │ apiVersion: rbac.authorization.k8s.io/v1                        │    │
│  │ kind: ClusterRole                                                │    │
│  │ metadata:                                                        │    │
│  │   name: hub-binding-controller                                   │    │
│  │ rules:                                                           │    │
│  │   - apiGroups: ["platform.example.com"]                          │    │
│  │     resources: ["widgets"]                                       │    │
│  │     verbs: ["get","list","watch","create","update","patch"]      │    │
│  │   - apiGroups: ["platform.example.com"]                          │    │
│  │     resources: ["widgets/status"]                                │    │
│  │     verbs: ["get","list","watch"]                                │    │
│  │ ---                                                              │    │
│  │ kind: ClusterRoleBinding                                         │    │
│  │ metadata:                                                        │    │
│  │   name: hub-binding-controller                                   │    │
│  │ roleRef: {kind: ClusterRole, name: hub-binding-controller}       │    │
│  │ subjects:                                                        │    │
│  │   - kind: User                                                   │    │
│  │     name: hub:system:serviceaccount:default:binding-controller   │    │
│  └────────────────────────────────────────────────────────────────┘    │
│                                                                         │
│  Authorization check:                                                   │
│  User "hub:system:serviceaccount:default:binding-controller"            │
│  wants to CREATE widget in namespace acme-corp                          │
│  → ClusterRoleBinding allows → ✓                                       │
└──────────────────────────────┬──────────────────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────────────────┐
│ Step 5: Guardrail Check (ValidatingAdmissionPolicy)                     │
│                                                                         │
│  Since the controller is creating a Widget (platform.example.com),      │
│  not managing ClusterRoles or modifying system namespaces, the          │
│  admission policies PASS THROUGH without blocking.                      │
│                                                                         │
│  restrict-clusterrole-management:  Not triggered (not rbac API)         │
│  protect-system-namespaces:        Not triggered (namespace: acme-corp) │
│  protect-auth-config:              Not triggered (not auth config API)  │
│                                                                         │
│  Result: ✓ Widget created on spoke                                      │
└─────────────────────────────────────────────────────────────────────────┘
```

## Human Identity Path (Dex OIDC)

The platform admin uses `kubectl` with the `dex-auth-plugin` exec credential:

```
┌─────────────────────────────────────────────────────────────────────────┐
│ Step H1: kubectl triggers exec credential plugin                        │
│                                                                         │
│  ~/.kube/config:                                                        │
│  users:                                                                 │
│  - name: platform-admin                                                 │
│    user:                                                                │
│      exec:                                                              │
│        command: dex-auth-plugin                                         │
│        env:                                                             │
│        - name: DEX_ISSUER                                               │
│          value: https://dex.example.com/dex                             │
│        - name: DEX_CLIENT_ID                                            │
│          value: platform-admin-client                                   │
│        - name: DEX_CLIENT_SECRET                                        │
│          valueFrom: secretKeyRef                                        │
│                                                                         │
│  dex-auth-plugin:                                                       │
│    POST /dex/token                                                      │
│      grant_type: client_credentials                                     │
│      client_id: platform-admin-client                                   │
│      client_secret: ***                                                 │
│    → {access_token: "eyJ...", expires_in: 3600}                         │
│    → writes ExecCredential to stdout                                    │
│                                                                         │
│  kubectl attaches token to API request:                                 │
│    Authorization: Bearer eyJhbGciOi...                                  │
└──────────────────────────────┬──────────────────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────────────────┐
│ Step H2: Token Validation (Spoke via Dex issuer)                        │
│                                                                         │
│  AuthenticationConfiguration (dex issuer section):                      │
│  ┌────────────────────────────────────────────────────────────────┐    │
│  │   - issuer:                                                     │    │
│  │       url: https://dex.example.com/dex                          │    │
│  │       audiences:                                                │    │
│  │         - kubernetes                                            │    │
│  │       claimMappings:                                            │    │
│  │         username: {claim: sub, prefix: "dex:"}                  │    │
│  │         groups: {claim: groups, prefix: "dex:"}                 │    │
│  └────────────────────────────────────────────────────────────────┘    │
│                                                                         │
│  Validates JWT against Dex JWKS → identity: dex:admin@example.com       │
│  RBAC: ClusterRoleBinding platform-admin-dex → full access              │
│  Guardrails: Bypassed (user is in platform-admin group)                 │
└─────────────────────────────────────────────────────────────────────────┘
```

## Test Verification

All flows are validated by 4 Chainsaw E2E test suites:

| Test | What It Validates |
|------|-------------------|
| `10-oidc-trust` | Dex OIDC discovery, JWKS serving, oidc-verifier cross-cluster JWT validation, audit logging |
| `11-rotating-trust-v2` | Binding controller projected volumes, token-rotator disabled, Dex still running for human auth |
| `16-auth-configuration` | AuthenticationConfiguration file mount, dual-issuer setup, legacy oidc-* flags removed |
| `17-projected-sa-tokens` | Hub OIDC discovery, binding-controller token volume, spoke auth config for hub issuer |
| `18-admission-guardrails` | All 3 ValidatingAdmissionPolicy resources, policy bindings, enforcement test |

Run all OIDC trust tests:

```bash
# Run the full OIDC trust suite
make validate

# Or run specific tests
chainsaw test tests/e2e/tests/16-auth-configuration \
  --config tests/e2e/.chainsaw.yaml

chainsaw test tests/e2e/tests/17-projected-sa-tokens \
  --config tests/e2e/.chainsaw.yaml

chainsaw test tests/e2e/tests/18-admission-guardrails \
  --config tests/e2e/.chainsaw.yaml
```

## Key Differences from v1

| Aspect | v1 | v2 |
|--------|----|----|
| Controller token source | Dex password grant (admin@example.com) | Hub kube-apiserver projected SA token |
| Token rotation | token-rotator every 5min | Kubelet configurable (~1h) |
| Spoke auth config | oidc-* CLI flags | AuthenticationConfiguration |
| Trust boundary | Any valid Dex token | claimValidationRules restrict by namespace |
| Identity for controller | oidc:admin | hub:system:serviceaccount:default:binding-controller |
| Identity for admin | oidc:admin@example.com | dex:admin@example.com |
| Guardrails for escalation | None | 3 ValidatingAdmissionPolicies (`system:masters`/`kubeadm:cluster-admins`/`dex:platform-admin`/`system:*` bypass) |
| Controller authorization on spoke | Implicit (kubeconfig cert = cluster-admin) | `ClusterRole hub-binding-controller` scoped to `widgets` only |
| Token audience | Spoke API server URL | Logical name (`homelab:us-spoke`), decoupled from network address |
| Missing token file | Silent fallback to kubeconfig client cert | Provider strips the cert — fails closed |
