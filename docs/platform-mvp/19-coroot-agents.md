# Coroot: hub server and spoke agents

[Coroot](https://coroot.com) builds a service map from eBPF: its node-agent
watches real connections on each node, and its cluster-agent adds Kubernetes
inventory from the API. This page covers the hub server and
the spokes' agents: how they are installed and how an agent finds its project.

> **The hub-side Coroot server is no longer part of this repo.** The
> `hub-services` chart's `coroot-server.yaml` template, its `coroot.*` values and
> the `COROOT_HUB` Makefile switch were removed, because the platform stores
> agent data in its own telemetry-ingest service
> ([20-telemetry-ingest.md](20-telemetry-ingest.md)). The sections below that
> describe the hub server, the generated `coroot-apikey-<cluster>` keys and
> `make deploy-wave3 COROOT_HUB=true` are historical. The spoke-side operator
> path and `copy-coroot-credentials.sh` remain but have no hub server to take
> keys from.

**Status:** both sides are written, and the charts and their `Coroot`
resources are tested against the operator's own CRD on a real API server. None
of it has run on a real cluster: the operators, ClickHouse, the node-agent's
eBPF on kind (it shares the host kernel) and the NodePort path from spoke pods
to the hub are all untested.

## Pieces

| Where | What | Source |
|---|---|---|
| each spoke, namespace `coroot` | Coroot operator (separate Helm release, pinned) | `make coroot-operator` step inside `deploy-wave4` |
| each spoke | an agents-only `Coroot` resource: node-agent + cluster-agent, no server | `chart/spoke/templates/coroot.yaml`, values `coroot.*` |
| each spoke | the spoke's API key, as Secret `coroot/hub-coroot-credentials` | `hack/platform-mvp/copy-coroot-credentials.sh` |
| hub, namespace `coroot` | Coroot operator, then the server: one project and generated API key per fleet cluster, plus a `fleet` project over all of them | `make deploy-wave3 COROOT_HUB=true`, `chart/hub-services/templates/coroot-server.yaml`, values `coroot.*` |

The operator is its own release because it ships the `Coroot` CRD; the spoke
chart's resource cannot install before that CRD exists, and the operator needs
a privileged namespace (the node-agent is privileged and uses `hostPID`).

## Turn it on

Hub first, since the keys come from it:

```sh
make deploy-wave3 COROOT_HUB=true         # operator, then the server (ClickHouse + Prometheus of its own)
kubectl --context kind-hub -n coroot get secret coroot-admin -o go-template='{{index .data "password"}}' | base64 -d
kubectl --context kind-hub -n coroot port-forward svc/coroot-coroot 8080:8080   # UI at localhost:8080
```

The operator generates one key Secret per fleet cluster
(`coroot/coroot-apikey-<cluster>`). Then the spokes:

```sh
make spoke-coroot-credentials COROOT_SPOKES="us eu"
make deploy-wave4 COROOT_SPOKES="us eu"   # operator, then each spoke's chart
```

and in that spoke's values (it is off by default, so nothing changes for
spokes you leave out):

```yaml
coroot:
  enabled: true
  corootURL: http://hub-control-plane:30080   # the hub server's address as seen from spoke pods
```

The API key is never in values. `spoke-coroot-credentials` copies each key, so it does not appear in a process list,
and creates the `coroot` namespace first if the operator has not yet.

## Things to know

- **One Service for UI and ingest.** The operator exposes both on one NodePort
  (30080 HTTP, 30317 gRPC), so the login page is reachable wherever the spokes
  are. The admin password is generated into `coroot/coroot-admin`. Keep the
  hub on a private network; there is no TLS here.
- **Weight.** The operator deploys ClickHouse (it cannot be switched off) and
  its own Prometheus, sized small in `coroot.*` for a lab. They are separate
  from the hub's Prometheus on purpose, so Coroot's container metrics do not
  mix with the ones the spoke metrics agent already sends.
- **No agents on the hub** by default (`coroot.hubAgents`): the hub would need
  a project and key of its own.
- **One project per spoke.** The API key selects the Coroot project, so a
  spoke's data stays in its own project; a multi-cluster view on the hub is a
  Coroot project that lists them as members.
- **Operator view only.** Coroot sees every tenant's pods. Do not expose its UI
  to tenants.
- **Plain HTTP** between agents and server on the lab's Docker network, like
  the metrics ingest. Set `coroot.tlsSkipVerify` only for a server with a
  self-signed certificate.
- **VMs** appear as their `virt-launcher` pods; traffic inside a guest is not
  visible.
- The operator version is `COROOT_OPERATOR_VERSION` (default `0.10.6`); it
  upgrades the agents itself unless image versions are pinned in the resource.

## Known limit on kind: pid-based events are not attributed to containers

Observed on a kind spoke (WSL2 kernel 6.6, node-agent 1.37.0, 2026-10-29). The
hub server, projects, keys, credential copy and spoke-to-hub ingest all work,
the agent's eBPF loads, and the agent finds the containers that exist when it
starts: CPU, memory and application type (Python was identified as `python`)
appear for them. Two things do not work:

- a container created after the agent started is never detected (a restart of
  the node-agent picks up the pods then running);
- connection edges and request metrics are missing even for containers that
  were detected, including a long-lived Python process opening new sockets.

What was checked: the kernel does fire the agent's process and socket programs
(`task_newtask`, `exec`, `sched_process_exit`, `inet_sock_set_state` run counts
rise), and the agent receives the socket events but logs them as `TCP listen
open from unknown container`. The pids in those events are in the host's range
(5xxxxx) while the same processes have small pids inside the kind node (the
test caller is 523742 on the host, 12168 in the node), and the agent registers
containers by the latter. That mismatch, a consequence of a kind node being a
Docker container with its own pid namespace, would explain everything above,
but it is **not proven**: it was not tested on a host where the two pid spaces
are the same, and one event pid (507714) matched neither pid of the pod it
came from.

Treat the service map as unverified until `coroot-stage0.sh check` passes on a
real node (for example AKS), where the agent shares the node's pid namespace.
Do not use kind to demonstrate edges.

## Stage 0 on AKS: one cluster, three runtimes

`hack/platform-mvp/coroot-stage0.sh` answers the question above on a real node
and shows how a .NET, a Java and a Python workload appear:

```sh
hack/platform-mvp/coroot-stage0.sh up        # asks first: creates a free-tier AKS cluster, 1 x Standard_D4s_v5
hack/platform-mvp/coroot-stage0.sh check     # PASS/FAIL per workload, edge and re-created pod
hack/platform-mvp/coroot-stage0.sh down      # deletes the resource group; refuses one not tagged purpose=coroot-stage0
```

It installs Coroot (operator, server, agents; one ClickHouse Keeper) and
deploys an ASP.NET app, a JVM running the JDK's built-in web server, a Python
web server and a caller that requests each once a second. All images are public
(`mcr.microsoft.com`, Docker Hub, `ghcr.io/coroot`), so no registry of your own
is needed. To test your own images set `DOTNET_IMAGE`, `JAVA_IMAGE`,
`PYTHON_IMAGE` (and `*_PORT`, `*_SCHEME`, `*_CMD`); they must be pullable by
the cluster (for a private registry, attach an ACR or add a pull secret).

`check` ends with the decisive test: it deletes the Java pod and waits for
Coroot to report the replacement. If that fails on AKS too, the cause is not
kind. It cannot fully replace looking at the UI, and it checks plain HTTP;
HTTPS visibility per runtime (set `*_SCHEME=https` for an image that serves it)
is only as good as what your images expose.

Status: the script's manifests are validated against the operator's CRD and its
checks were exercised against the kind lab (where, as expected, they report the
failures above). It has **not** been run against Azure.

## Single-node AKS

The agents need a real node, so a one-node AKS cluster is the smallest place
to see service, DNS and node metrics for your own API container.

Quickest, one cluster running its own Coroot server and your image:

```sh
PROVISION=aks PYTHON_IMAGE=<your-api-image> PYTHON_PORT=<port> \
  PYTHON_CMD='["<entrypoint>"]' hack/platform-mvp/coroot-stage0.sh up
hack/platform-mvp/coroot-stage0.sh check
```

(Use the `DOTNET_*` or `JAVA_*` variables if that is your runtime. The image
must be pullable by the cluster; attach an ACR or add a pull secret.) The Node
page in the Coroot UI (`kubectl -n coroot port-forward svc/coroot-coroot 8080:8080`)
shows OS, kernel, cloud, region, CPU and memory consumers, and your container
among them.

To run the spoke chart instead, as a spoke reporting to a hub server:

1. Create the cluster: `az aks create -g <rg> -n <name> --node-count 1
   --node-vm-size Standard_D4s_v5 --generate-ssh-keys`, then
   `az aks get-credentials`. The default Ubuntu node image has a kernel new
   enough for the agent's eBPF.
2. Install the operator (privileged namespace, as `coroot-stage0.sh` does):
   `kubectl create namespace coroot`, label it
   `pod-security.kubernetes.io/enforce=privileged`, then
   `helm upgrade --install coroot-operator coroot/coroot-operator --version <pin> -n coroot --wait`.
3. Copy the spoke's API key from the hub into Secret `coroot/hub-coroot-credentials`
   (`copy-coroot-credentials.sh` shows the shape; it targets kind contexts).
4. Install the spoke chart with `-f aks-values.yaml` and `--set coroot.corootURL=<hub URL>`.
   The hub URL must be reachable from AKS pods (the kind NodePort address is not).

`chart/spoke/aks-values.yaml` holds the values for step 4. The AKS path is
untested here, like the rest of this page.

## Service map page

The service map now reads from telemetry-ingest, not Prometheus. See "Service
map" in [20-telemetry-ingest.md](20-telemetry-ingest.md).

## Test

```sh
make test-coroot-agents
```

Renders both charts (off by default; the spoke refuses without a URL; keys
only ever by Secret reference; the hub declares one project and a key Secret
named `coroot-apikey-<cluster>` per fleet cluster, the name the copy script
reads), checks the rendered resources against the operator's own CRD on a real
API server (k3s), that a malformed URL and a project with both members and
keys are rejected, and the credential script. The operators and agents are not
run.
