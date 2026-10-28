# Phase 12 — Security Guardrails (Platform Wins Model)

**Status:** Implemented (enabled by default) | **Branch:** feat/oidc-trust-v2

## Overview

The "Platform Wins" model ensures that platform-managed security configuration cannot be modified by tenant identities. Three `ValidatingAdmissionPolicy` resources enforce this at the API server level.

## Policies

### 1. restrict-clusterrole-management

Prevents non-platform-admin identities from creating, updating, or deleting `ClusterRoles` and `ClusterRoleBindings`.

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: restrict-clusterrole-management
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - apiGroups: ["rbac.authorization.k8s.io"]
        apiVersions: ["v1"]
        operations: ["CREATE", "UPDATE", "DELETE"]
        resources: ["clusterroles", "clusterrolebindings"]
  validations:
    - expression: >
        ('system:masters' in request.userInfo.groups)
        || ('kubeadm:cluster-admins' in request.userInfo.groups)
        || ('dex:platform-admin' in request.userInfo.groups)
        || request.userInfo.username.startsWith('system:')
      message: "Only platform-admins, system:masters, or system identities may manage ClusterRoles and ClusterRoleBindings."
```

### 2. protect-system-namespaces

Prevents non-platform-admin identities from modifying resources in `kube-system`, `kube-public`, and `kube-node-lease` namespaces.

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: protect-system-namespaces
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - apiGroups: ["*"]
        apiVersions: ["*"]
        operations: ["CREATE", "UPDATE", "DELETE"]
        resources: ["*"]
    namespaceSelector:
      matchExpressions:
        - key: kubernetes.io/metadata.name
          operator: In
          values: ["kube-system", "kube-public", "kube-node-lease"]
  validations:
    - expression: >
        ('system:masters' in request.userInfo.groups)
        || ('kubeadm:cluster-admins' in request.userInfo.groups)
        || ('dex:platform-admin' in request.userInfo.groups)
        || request.userInfo.username.startsWith('system:')
      message: "Only platform-admins, system:masters, or system identities may modify resources in this namespace."
```

### 3. protect-auth-config

Prevents non-platform-admin identities from modifying the `AuthenticationConfiguration` resource.

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: protect-auth-config
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - apiGroups: ["apiserver.config.k8s.io"]
        apiVersions: ["v1beta1"]
        operations: ["CREATE", "UPDATE", "DELETE"]
        resources: ["authenticationconfigurations"]
  validations:
    - expression: >
        ('system:masters' in request.userInfo.groups)
        || ('kubeadm:cluster-admins' in request.userInfo.groups)
        || ('dex:platform-admin' in request.userInfo.groups)
        || request.userInfo.username.startsWith('system:')
      message: "Only platform-admins, system:masters, or system identities may modify AuthenticationConfiguration."
```

`system:masters` and `kubeadm:cluster-admins` are included because kubeadm-provisioned clusters (kind included) authenticate the built-in cluster-admin client certificate into those groups — without this bypass, the policies would lock out the identity used to install and recover the guardrails themselves.

## Platform Admin RBAC

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: platform-admin
rules:
  - apiGroups: ["*"]
    resources: ["*"]
    verbs: ["*"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: platform-admin-dex
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: platform-admin
subjects:
  - kind: Group
    name: dex:platform-admin
    apiGroup: rbac.authorization.k8s.io
```

## Enabling

The admission policies are **enabled by default** to enforce Platform Wins security:

```yaml
# us/values.yaml
admissionPolicies:
  enabled: true
  platformAdminSubjects:
    - kind: Group
      name: dex:platform-admin
  additionalProtectedNamespaces: []
```

## Bypassing

In emergencies, the `failurePolicy` can be set to `Ignore` to allow all requests through. System identities (`system:*` usernames), the `system:masters`/`kubeadm:cluster-admins` groups (cluster-admin certs), and users in the `dex:platform-admin` group always bypass the policies.

## Recovery

If policies are accidentally deleted:
- **GitOps (Flux)**: Re-applies from git on the next reconciliation
- **Manual**: `helm upgrade us ./deploy/platform-mvp/chart/spoke -f ./deploy/platform-mvp/chart/spoke/values-us.yaml --set admissionPolicies.enabled=true`