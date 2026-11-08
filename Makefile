# Platform MVP Makefile
# Umbrella Helm charts + Flux CD + Chainsaw E2E
#
# Quick start:
#   make deploy        → build+push images (parallel) + deploy everything
#   make grafana       → port-forward Grafana to localhost:3000
#   make validate      → run Chainsaw E2E tests
#   make clean         → destroy everything
#
SHELL := /usr/bin/env bash

export PATH := $(shell go env GOPATH)/bin:$(PATH)

# ── Externalized config ──────────────────────────────────────────────────────
KIND_HUB          ?= hub
KIND_HUB_CONFIG   ?= deploy/platform-mvp/kind/kind-hub.yaml

# Spoke clusters (kind cluster name == spoke name == ClusterProfile name ==
# chart/spoke/values-<name>.yaml). Adding a spoke = one entry here plus a
# KIND_NODE_IMAGE_<name>, a values file, and a hub-services fleet entry.
SPOKES            ?= us eu
KIND_NODE_IMAGE_us ?= kindest/node:v1.30.0
# KubeVirt 1.9 supports Kubernetes 1.34-1.36 only.
KIND_NODE_IMAGE_eu ?= kindest/node:v1.35.1
KIND_SPOKE_CONFIG_TMPL ?= deploy/platform-mvp/kind/kind-spoke.yaml.tmpl
KIND_RENDER_DIR   ?= deploy/platform-mvp/kind/rendered
# Per-spoke apiserver auth dir, mounted into the spoke's control plane.
# Readable/writable by the invoking user (no root-owned /tmp race, survives
# host reboots) — always resolved relative to KIND_SPOKE_CONFIG_TMPL's own
# location, never a machine-specific absolute path. Spoke X uses X-auth/.
SPOKE_AUTH_BASE   ?= $(abspath $(dir $(KIND_SPOKE_CONFIG_TMPL)))

HUB_CHART          ?= deploy/platform-mvp/chart/infrastructure
SPOKE_CHART        ?= deploy/platform-mvp/chart/spoke
CRDS_CHART         ?= deploy/platform-mvp/chart/crds
HUBSVC_CHART       ?= deploy/platform-mvp/chart/hub-services
HELM_TIMEOUT       ?= 15m
# Extra helm arguments for the hub-services and spoke installs, and how images
# reach the clusters. The defaults push to DOCKER_REGISTRY; `make deploy-local`
# loads locally built images into kind instead, with no registry involved.
HUBSVC_HELM_ARGS   ?=
SPOKE_HELM_ARGS    ?=
IMAGE_DELIVERY     ?= push-images-parallel

# Coroot agents on spokes (off unless COROOT_SPOKES lists some, e.g. "us eu").
# The operator is a separate release because it owns the Coroot CRD, which the
# spoke chart's Coroot resource needs to exist before it installs.
COROOT_SPOKES            ?=
COROOT_OPERATOR_VERSION  ?= 0.10.6

# TELEMETRY_HUB=true runs telemetry-ingest and its ClickHouse on the hub (off by
# default). `make validate-telemetry-ingest` then tests it with Chainsaw.
TELEMETRY_HUB            ?= false
# The kind lab reaches the hub over the Docker network, so the service is a NodePort.
TELEMETRY_HUB_ARGS       = $(if $(filter true,$(TELEMETRY_HUB)),--set telemetryIngest.enabled=true --set telemetryIngest.service.type=NodePort)
TI_TEST_DIR              ?= tests/telemetry-ingest

INGRESS_HOST       ?= bm4080.taildf7067.ts.net
GRAFANA_URL        ?= http://$(INGRESS_HOST)
GRAFANA_PF_PORT    ?= 3000
OBS_NS             ?= monitoring

BC_KUBECONFIG      ?= $(HOME)/.kube/config
SPOKE_KC_DIR       ?= hack/platform-mvp

CHAINSAW_DIR       ?= tests/e2e
CHAINSAW_REPORT    ?= chainsaw-report.json
CHAINSAW_RUNNER_DIR ?= deploy/platform-mvp/observability
CRONJOB_SCHEDULE   ?= "*/2 * * * *"

DOCKER_REGISTRY    ?= sojoner
IMAGE_TAG          ?= dev
BC_DIR             ?= platform-mvp/binding-controller
WO_DIR             ?= platform-mvp/widget-operator
OV_DIR             ?= platform-mvp/oidc-verifier
DAP_DIR            ?= platform-mvp/dex-auth-plugin
VMBC_DIR           ?= platform-mvp/vm-binding-controller
VMSG_DIR           ?= platform-mvp/vmset-gateway
AR_DIR             ?= platform-mvp/alert-router
TI_DIR             ?= platform-mvp/telemetry-ingest
UI_DIR             ?= platform-mvp/telemetry-ui

BC_IMAGE           ?= $(DOCKER_REGISTRY)/binding-controller:$(IMAGE_TAG)
WO_IMAGE           ?= $(DOCKER_REGISTRY)/widget-operator:$(IMAGE_TAG)
OV_IMAGE           ?= $(DOCKER_REGISTRY)/oidc-verifier:$(IMAGE_TAG)
DAP_IMAGE          ?= $(DOCKER_REGISTRY)/dex-auth-plugin:$(IMAGE_TAG)
VMBC_IMAGE         ?= $(DOCKER_REGISTRY)/vm-binding-controller:$(IMAGE_TAG)
VMSG_IMAGE         ?= $(DOCKER_REGISTRY)/vmset-gateway:$(IMAGE_TAG)
AR_IMAGE           ?= $(DOCKER_REGISTRY)/alert-router:$(IMAGE_TAG)
TI_IMAGE           ?= $(DOCKER_REGISTRY)/telemetry-ingest:$(IMAGE_TAG)
UI_IMAGE           ?= $(DOCKER_REGISTRY)/telemetry-ui:$(IMAGE_TAG)
ALERTS_IMAGE       ?= $(DOCKER_REGISTRY)/telemetry-alerts:$(IMAGE_TAG)

CRDS_DIR           ?= deploy/platform-mvp/crds
OUT_DIR            ?= bin
BC_BIN             ?= $(OUT_DIR)/binding-controller
FLUX_DIR           ?= deploy/platform-mvp/flux
CERT_MANAGER_VERSION ?= v1.17.1

# KubeVirt runs only on these spokes (needs /dev/kvm on the host). Its operator
# manifest is cached under CRDS_DIR like the other upstream manifests.
KUBEVIRT_VERSION   ?= v1.9.1
KUBEVIRT_SPOKES    ?= eu

# ── Help ─────────────────────────────────────────────────────────────────────
.PHONY: help
help:
	@echo "Platform MVP — build, deploy, validate"
	@echo ""
	@echo "Deploy (4-wave Helm chart decomposition):"
	@echo "  make deploy        Full deployment (clusters + 4 waves)"
	@echo "  make deploy-wave1  Wave 1: Install all CRDs on hub + spokes"
	@echo "  make deploy-wave2  Wave 2: Infrastructure on hub (LGTM + Dex + ingress)"
	@echo "  make deploy-wave3  Wave 3: Hub services (Kro + binding-controller + fleet)"
	@echo "  make deploy-wave4  Wave 4: Spokes (widget-operator + oidc-verifier + tenant RBAC; KubeVirt on KUBEVIRT_SPOKES)"
	@echo "  make deploy-cd     Enable GitOps (Flux CD) on hub"
	@echo "  make clusters      Create kind clusters"
	@echo ""
	@echo "Test:"
	@echo "  make test          Run all Go unit tests"
	@echo "  make test-race     Unit tests with race detector"
	@echo "  make test-cover    Unit tests with coverage profiles"
	@echo "  make validate      Run full Chainsaw E2E suite (20 tests)"
	@echo "  make lint          go vet + gofmt check"
	@echo "  make lint-fix      Auto-fix formatting + go vet"
	@echo ""
	@echo "Build:"
	@echo "  make build-images  Build all 4 Docker images in parallel"
	@echo "  make push-images   Push all 4 images to Docker Hub"
	@echo ""
	@echo "Observe:"
	@echo "  make grafana       Port-forward Grafana → localhost:3000"
	@echo "  make grafana-url   Print dashboard URLs"
	@echo ""
	@echo "Clean:"
	@echo "  make clean         Destroy clusters + artifacts"
	@echo "  make clean-artifacts  Remove bin/ and coverage files"
	@echo ""
	@echo "Tools:"
	@echo "  make install-chainsaw   Install Chainsaw CLI"
	@echo "  make install-flux       Install Flux CLI"
	@echo "  make chainsaw-runner    Build in-cluster Chainsaw runner"
	@echo "  make verify-all         Full clean-room: clean → deploy → validate → deploy-cd"

# ── Test ─────────────────────────────────────────────────────────────────────
.PHONY: test test-root test-bc test-wo test-ov test-dap test-vmbc test-vmsg test-ar test-ti test-ti-integration test-ti-chart test-telemetry-credentials test-ui test-ui-chart test-ui-image test-telemetry-alerts-chart test-telemetry-alerts-image ui-install ui-dev test-servicemap test-alerts test-alertmanager test-alert-flow test-spoke-probe test-spoke-metrics test-coroot-agents test-node-agent-chart test-race test-cover
test: test-root test-bc test-wo test-ov test-dap test-vmbc test-vmsg test-ar test-ti
	@echo "==> All unit tests passed"

test-root:
	go test ./... -count=1

test-bc:
	cd $(BC_DIR) && go test ./... -count=1

test-wo:
	cd $(WO_DIR) && go test ./... -count=1

test-ov:
	cd $(OV_DIR) && go test ./... -count=1

test-dap:
	cd $(DAP_DIR) && go test ./... -count=1

test-vmbc:
	cd $(VMBC_DIR) && go test ./... -count=1

test-vmsg:
	cd $(VMSG_DIR) && go test ./... -count=1

test-ar:
	cd $(AR_DIR) && go test ./... -count=1

test-ti:
	cd $(TI_DIR) && go test ./... -count=1

# Against a real ClickHouse started for the run (needs Docker, ~1 min).
test-ti-integration:
	./hack/platform-mvp/test-telemetry-ingest.sh

# The hub chart's Service for telemetry-ingest, and how spokes reach it (rendering only; needs Docker).
test-ti-chart:
	./hack/platform-mvp/test-telemetry-ingest-chart.sh

# The telemetry UI (Node runs in Docker, so the host needs neither Node nor npm).
UI_NODE ?= node:24-alpine
UI_RUN   = docker run --rm -v "$(CURDIR)/$(UI_DIR)":/app -v telemetry-ui-node-modules:/app/node_modules -w /app $(UI_NODE)
ui-install:
	$(UI_RUN) npm ci

test-ui: ui-install
	$(UI_RUN) sh -c 'npm run lint && npm test && npm run build'

# Dev server on :5173, proxying /api to UI_API (default: a port-forward of telemetry-ingest on :8080).
UI_API ?= http://host.docker.internal:8080
ui-dev: ui-install
	docker run --rm -it -p 5173:5173 --add-host=host.docker.internal:host-gateway -e API_TARGET=$(UI_API) \
		-v "$(CURDIR)/$(UI_DIR)":/app -v telemetry-ui-node-modules:/app/node_modules -w /app $(UI_NODE) npm run dev -- --host 0.0.0.0

# The hub chart's telemetry-ui template (rendering only; needs Docker).
test-ui-chart:
	./hack/platform-mvp/test-telemetry-ui-chart.sh

# The hub chart's telemetry-alerts template (rendering only; needs Docker).
test-telemetry-alerts-chart:
	./hack/platform-mvp/test-telemetry-alerts-chart.sh

# The built alert service against a stub API and Alertmanager: firing, auth, metrics, restart, resolve (needs Docker, ~2 min).
test-telemetry-alerts-image:
	./hack/platform-mvp/test-telemetry-alerts-image.sh

# The built image against a stub API: proxying, GET-only, blocked paths, headers, read-only root.
test-ui-image:
	./hack/platform-mvp/test-telemetry-ui-image.sh

# The key-delivery script, against a stub kubectl (no cluster or Docker needed).
test-telemetry-credentials:
	./hack/platform-mvp/test-telemetry-credentials.sh

# The service map backend, against a stub of the read API and a fixture cluster (python3 only).
test-servicemap:
	python3 -W error::ResourceWarning -m unittest hack/platform-mvp/servicemap/test_server.py

# Unit-tests the VmSet alert rules with promtool (needs Docker).
test-alerts:
	./hack/platform-mvp/test-vmset-alerts.sh

# Validates the Alertmanager config and its routing with amtool (needs Docker).
test-alertmanager:
	./hack/platform-mvp/test-alertmanager-config.sh

# End to end: real Alertmanager and alert-router, ~60s (needs Docker).
test-alert-flow:
	./hack/platform-mvp/test-alert-flow.sh

# Spoke API probes against a real kube-apiserver (k3s in Docker), ~2 min.
test-spoke-probe:
	./hack/platform-mvp/test-spoke-probe.sh

# Spoke pod metrics end to end: real agent, proxy and Prometheus, ~4 min.
test-spoke-metrics:
	./hack/platform-mvp/test-spoke-metrics.sh

# Coroot agent wiring: chart render, CRD validation on a real API server.
test-coroot-agents:
	./hack/platform-mvp/test-coroot-agents.sh

# The spoke chart's node-agent DaemonSet: render checks plus a real API server (needs Docker).
test-node-agent-chart:
	./hack/platform-mvp/test-node-agent-chart.sh

# Copies the hub's metrics-ingest credential to every spoke, where the metrics
# agent reads it. Run after the hub-services chart is installed and before
# enabling metricsAgent on the spokes.
.PHONY: spoke-metrics-credentials
spoke-metrics-credentials:
	@for s in $(SPOKES); do \
		./hack/platform-mvp/copy-metrics-credentials.sh kind-$$s kind-$(KIND_HUB) || exit 1; \
	done

# Copies each spoke's telemetry-ingest key from the hub. Run after the hub's
# telemetry-ingest is deployed (TELEMETRY_HUB=true) and before installing the
# spokes with nodeAgent.enabled. TELEMETRY_SPOKES defaults to every spoke; add
# RESTART=1 to restart running agents when a key changes.
TELEMETRY_SPOKES ?= $(SPOKES)
.PHONY: spoke-telemetry-credentials
spoke-telemetry-credentials:
	@for s in $(TELEMETRY_SPOKES); do \
		./hack/platform-mvp/copy-telemetry-credentials.sh $$s kind-$$s kind-$(KIND_HUB) || exit 1; \
	done

# Copies each Coroot spoke's API key from the hub's Coroot server. The hub
# Coroot server was removed from the hub chart, so there is nothing to copy from
# any more: this target only works against a server you run yourself. For the
# telemetry-ingest path use spoke-telemetry-credentials above.
.PHONY: spoke-coroot-credentials
spoke-coroot-credentials:
	@for s in $(COROOT_SPOKES); do \
		./hack/platform-mvp/copy-coroot-credentials.sh $$s kind-$$s kind-$(KIND_HUB) || exit 1; \
	done

test-race:
	go test -race ./... -count=1
	cd $(BC_DIR) && go test -race ./... -count=1
	cd $(WO_DIR) && go test -race ./... -count=1
	cd $(OV_DIR) && go test -race ./... -count=1
	cd $(DAP_DIR) && go test -race ./... -count=1
	cd $(VMBC_DIR) && go test -race ./... -count=1
	cd $(VMSG_DIR) && go test -race ./... -count=1
	cd $(AR_DIR) && go test -race ./... -count=1
	cd $(TI_DIR) && go test -race ./... -count=1

test-cover:
	go test -coverprofile=coverage-root.out ./...
	cd $(BC_DIR) && go test -coverprofile=../../coverage-bc.out ./...
	cd $(WO_DIR) && go test -coverprofile=../../coverage-wo.out ./...
	cd $(DAP_DIR) && go test -coverprofile=../../coverage-dap.out ./... 2>/dev/null || true
	@echo "Coverage: coverage-*.out"

# ── Lint ─────────────────────────────────────────────────────────────────────
.PHONY: lint lint-fix
lint:
	@echo "==> go vet + gofmt check"
	go vet ./...
	cd $(BC_DIR) && go vet ./...
	cd $(WO_DIR) && go vet ./...
	cd $(OV_DIR) && go vet ./...
	cd $(VMBC_DIR) && go vet ./...
	cd $(VMSG_DIR) && go vet ./...
	cd $(AR_DIR) && go vet ./...
	cd $(TI_DIR) && go vet ./...
	@if [ -n "$$(gofmt -l .)" ]; then \
		echo "ERROR: files not formatted:"; \
		gofmt -l .; \
		exit 1; \
	fi

lint-fix:
	gofmt -w .
	cd $(BC_DIR) && gofmt -w .
	cd $(WO_DIR) && gofmt -w .
	cd $(OV_DIR) && gofmt -w .
	cd $(VMBC_DIR) && gofmt -w .
	cd $(VMSG_DIR) && gofmt -w .
	cd $(AR_DIR) && gofmt -w .
	cd $(TI_DIR) && gofmt -w .
	go vet ./...
	cd $(BC_DIR) && go vet ./...
	cd $(WO_DIR) && go vet ./...
	cd $(OV_DIR) && go vet ./...

# ── Build ────────────────────────────────────────────────────────────────────
.PHONY: build build-bc build-wo build-ov build-dap
build: build-bc

build-bc:
	@mkdir -p $(OUT_DIR)
	cd $(BC_DIR) && go build -o ../../$(BC_BIN) .

# ── Docker images (build only) ───────────────────────────────────────────────
.PHONY: build-images build-bc-image build-wo-image build-ov-image build-dap-image build-vmbc-image build-vmsg-image build-alert-router-image build-telemetry-ingest-image

build-images: build-bc-image build-wo-image build-ov-image build-dap-image build-vmbc-image build-vmsg-image build-alert-router-image build-telemetry-ingest-image

build-bc-image:
	@echo "==> Building $(BC_IMAGE)"
	docker build -f $(BC_DIR)/Dockerfile -t $(BC_IMAGE) .

build-wo-image:
	@echo "==> Building $(WO_IMAGE)"
	docker build -f $(WO_DIR)/Dockerfile -t $(WO_IMAGE) .

build-ov-image:
	@echo "==> Building $(OV_IMAGE)"
	docker build -f $(OV_DIR)/Dockerfile -t $(OV_IMAGE) .

build-dap-image:
	@echo "==> Building $(DAP_IMAGE)"
	docker build -f $(DAP_DIR)/Dockerfile -t $(DAP_IMAGE) .

build-vmbc-image:
	@echo "==> Building $(VMBC_IMAGE)"
	docker build -f $(VMBC_DIR)/Dockerfile -t $(VMBC_IMAGE) .

# Standalone module: its own directory is the build context.
build-vmsg-image:
	@echo "==> Building $(VMSG_IMAGE)"
	docker build -t $(VMSG_IMAGE) $(VMSG_DIR)

build-alert-router-image:
	@echo "==> Building $(AR_IMAGE)"
	docker build -t $(AR_IMAGE) $(AR_DIR)

build-telemetry-ingest-image:
	@echo "==> Building $(TI_IMAGE)"
	docker build -t $(TI_IMAGE) $(TI_DIR)

# Not part of build-images: the UI and the alert service are opt-in, like the service they read.
.PHONY: build-telemetry-ui-image push-telemetry-ui build-telemetry-alerts-image push-telemetry-alerts
build-telemetry-ui-image:
	@echo "==> Building $(UI_IMAGE)"
	docker build -t $(UI_IMAGE) $(UI_DIR)

build-telemetry-alerts-image:
	@echo "==> Building $(ALERTS_IMAGE)"
	docker build -f $(UI_DIR)/Dockerfile.alerts -t $(ALERTS_IMAGE) $(UI_DIR)

# Build all images in parallel
build-images-parallel:
	@echo "==> Building all images in parallel"
	@$(MAKE) --no-print-directory build-bc-image & \
	$(MAKE) --no-print-directory build-wo-image & \
	$(MAKE) --no-print-directory build-ov-image & \
	$(MAKE) --no-print-directory build-dap-image & \
	$(MAKE) --no-print-directory build-vmbc-image & \
	$(MAKE) --no-print-directory build-vmsg-image & \
	$(MAKE) --no-print-directory build-alert-router-image & \
	$(MAKE) --no-print-directory build-telemetry-ingest-image & \
	wait
	@echo "==> All images built"

# ── Docker push (parallel by default) ────────────────────────────────────────
.PHONY: push-images push-bc push-wo push-ov push-dap push-vmbc push-vmsg push-alert-router push-telemetry-ingest

push-images: push-bc push-wo push-ov push-dap push-vmbc push-vmsg push-alert-router push-telemetry-ingest

push-bc:
	docker push $(BC_IMAGE)
push-wo:
	docker push $(WO_IMAGE)
push-ov:
	docker push $(OV_IMAGE)
push-dap:
	docker push $(DAP_IMAGE)
push-vmbc:
	docker push $(VMBC_IMAGE)
push-vmsg:
	docker push $(VMSG_IMAGE)
push-alert-router:
	docker push $(AR_IMAGE)
push-telemetry-ingest:
	docker push $(TI_IMAGE)
push-telemetry-ui:
	docker push $(UI_IMAGE)
push-telemetry-alerts:
	docker push $(ALERTS_IMAGE)

push-images-parallel:
	@echo "==> Pushing all images to $(DOCKER_REGISTRY)"
	docker push $(BC_IMAGE) & \
	docker push $(WO_IMAGE) & \
	docker push $(OV_IMAGE) & \
	docker push $(DAP_IMAGE) & \
	docker push $(VMBC_IMAGE) & \
	docker push $(VMSG_IMAGE) & \
	docker push $(AR_IMAGE) & \
	docker push $(TI_IMAGE) & \
	wait
	@echo "==> All images pushed"

# ── CRD caching ──────────────────────────────────────────────────────────────
.PHONY: crds

crds: $(CRDS_DIR)/kro-core-install-manifests.yaml \
      $(CRDS_DIR)/clusterprofiles-crd.yaml \
      $(CRDS_DIR)/cert-manager.crds.yaml \
      $(CRDS_DIR)/prometheus-operator-crds.yaml \
      $(CRDS_DIR)/kubevirt-operator.yaml

$(CRDS_DIR):
	mkdir -p $(CRDS_DIR)

$(CRDS_DIR)/kro-core-install-manifests.yaml: | $(CRDS_DIR)
	curl -fsSL -o $@ https://github.com/kubernetes-sigs/kro/releases/download/v0.9.2/kro-core-install-manifests.yaml

$(CRDS_DIR)/clusterprofiles-crd.yaml: | $(CRDS_DIR)
	curl -fsSL -o $@ https://raw.githubusercontent.com/kubernetes-sigs/cluster-inventory-api/main/config/crd/bases/multicluster.x-k8s.io_clusterprofiles.yaml

$(CRDS_DIR)/cert-manager.crds.yaml: | $(CRDS_DIR)
	curl -fsSL -o $@ https://github.com/cert-manager/cert-manager/releases/download/$(CERT_MANAGER_VERSION)/cert-manager.crds.yaml

$(CRDS_DIR)/prometheus-operator-crds.yaml: | $(CRDS_DIR)
	curl -fsSL -o $@ https://github.com/prometheus-operator/prometheus-operator/releases/download/v0.92.1/stripped-down-crds.yaml

$(CRDS_DIR)/kubevirt-operator.yaml: | $(CRDS_DIR)
	curl -fsSL -o $@ https://github.com/kubevirt/kubevirt/releases/download/$(KUBEVIRT_VERSION)/kubevirt-operator.yaml

# ── Clusters ─────────────────────────────────────────────────────────────────
.PHONY: clusters ensure-spoke-auth-config

# Each spoke's kind config is generated from KIND_SPOKE_CONFIG_TMPL on every run
# so its extraMounts hostPath always points at that spoke's auth dir, resolved
# relative to the template's own location rather than hardcoded to one
# machine's checkout path.
render-kind-config-%:
	@mkdir -p "$(KIND_RENDER_DIR)" "$(SPOKE_AUTH_BASE)/$*-auth"
	@sed -e 's|__AUTH_DIR__|$(SPOKE_AUTH_BASE)/$*-auth|' \
		-e 's|__NODE_IMAGE__|$(KIND_NODE_IMAGE_$*)|' \
		$(KIND_SPOKE_CONFIG_TMPL) > $(KIND_RENDER_DIR)/kind-$*.yaml

# Re-derive each spoke apiserver's AuthenticationConfiguration from the chart
# on every `make clusters` / `make deploy` / `make validate` run so a stale or
# missing file self-heals without a separate manual step. Content comes from
# chart/spoke/templates/auth-config.yaml + values-<spoke>.yaml via
# hack/platform-mvp/render-spoke-auth-config.sh — never hand-duplicate it
# here. Requires the hub cluster to already exist: the hub issuer's trust
# anchor is the hub apiserver's own CA (kind's cert isn't publicly trusted),
# extracted fresh each run rather than assumed stable.
# The Dex issuer's CA (the cert-manager self-signed cert backing the ingress,
# secret monitoring/ingress-tls) usually doesn't exist yet the first time this
# runs (before Wave 2) — included once it does. The apiserver hot-reloads
# this file, so a second `make ensure-spoke-auth-config` after Wave 2 (see
# `deploy`) picks it up without restarting anything.
ensure-spoke-auth-config: $(addprefix ensure-auth-config-,$(SPOKES))

ensure-auth-config-%: render-kind-config-%
	@mkdir -p "$(SPOKE_AUTH_BASE)/$*-auth" 2>/dev/null || { \
		echo "ERROR: cannot create $(SPOKE_AUTH_BASE)/$*-auth (likely root-owned after a host reboot)."; \
		exit 1; \
	}
	@docker exec $(KIND_HUB)-control-plane cat /etc/kubernetes/pki/ca.crt > "$(SPOKE_AUTH_BASE)/$*-auth/hub-ca.crt" || { \
		echo "ERROR: cannot read hub CA cert — is the hub cluster up? (kind get clusters)"; \
		exit 1; \
	}
	@kubectl --context kind-$(KIND_HUB) -n $(OBS_NS) get secret ingress-tls -o jsonpath='{.data.ca\.crt}' 2>/dev/null | base64 -d > "$(SPOKE_AUTH_BASE)/$*-auth/dex-ca.crt" || rm -f "$(SPOKE_AUTH_BASE)/$*-auth/dex-ca.crt"
	@DEX_CA=""; [ -s "$(SPOKE_AUTH_BASE)/$*-auth/dex-ca.crt" ] && DEX_CA="$(SPOKE_AUTH_BASE)/$*-auth/dex-ca.crt"; \
	./hack/platform-mvp/render-spoke-auth-config.sh $(SPOKE_CHART) $(SPOKE_CHART)/values-$*.yaml "$(SPOKE_AUTH_BASE)/$*-auth/hub-ca.crt" "$$DEX_CA" > "$(SPOKE_AUTH_BASE)/$*-auth/authentication-configuration.yaml" 2>/dev/null || { \
		echo "ERROR: cannot write $(SPOKE_AUTH_BASE)/$*-auth/authentication-configuration.yaml (likely root-owned after a host reboot)."; \
		echo "  Run this once, then retry:"; \
		echo "  ./hack/platform-mvp/render-spoke-auth-config.sh $(SPOKE_CHART) $(SPOKE_CHART)/values-$*.yaml $(SPOKE_AUTH_BASE)/$*-auth/hub-ca.crt | sudo tee $(SPOKE_AUTH_BASE)/$*-auth/authentication-configuration.yaml > /dev/null"; \
		exit 1; \
	}
	@echo "  $* AuthenticationConfiguration ready at $(SPOKE_AUTH_BASE)/$*-auth/authentication-configuration.yaml"

# hub must exist before ensure-spoke-auth-config (needs its CA cert), and
# that must finish before any spoke starts (it reads the host-mounted file at
# kube-apiserver boot) — no longer parallel.
clusters:
	@echo "==> Creating hub cluster"
	@if ! kind get clusters 2>/dev/null | grep -qx "$(KIND_HUB)"; then \
		kind create cluster --name $(KIND_HUB) --config $(KIND_HUB_CONFIG); \
	fi
	$(MAKE) ensure-spoke-auth-config
	@for s in $(SPOKES); do \
		echo "==> Creating $$s cluster"; \
		if ! kind get clusters 2>/dev/null | grep -qx "$$s"; then \
			kind create cluster --name $$s --config $(KIND_RENDER_DIR)/kind-$$s.yaml || exit 1; \
		fi; \
	done
	@echo "  Verifying nodes..."
	kubectl --context kind-$(KIND_HUB) get nodes
	@for s in $(SPOKES); do kubectl --context kind-$$s get nodes || exit 1; done
	@echo "  Extracting internal kubeconfigs + generating E2E kubeconfigs..."
	kind get kubeconfig --name $(KIND_HUB) > $(CHAINSAW_DIR)/kubeconfig-hub
	@for s in $(SPOKES); do \
		kind get kubeconfig --name $$s --internal > $(SPOKE_KC_DIR)/kubeconfig-$$s-internal; \
		kind get kubeconfig --name $$s --internal > $(CHAINSAW_DIR)/kubeconfig-$$s-internal; \
		IP=$$(docker inspect -f '{{range.NetworkSettings.Networks}}{{.IPAddress}}{{end}}' $$s-control-plane 2>/dev/null); \
		if [ -n "$$IP" ]; then \
			sed -i "s|https://$$s-control-plane:6443|https://$$IP:6443|" $(CHAINSAW_DIR)/kubeconfig-$$s-internal; \
		fi; \
	done
	@echo "==> Clusters ready"

# ── Deploy ───────────────────────────────────────────────────────────────────
# Wave-based deployment: each wave is a Helm chart that groups related resources.
# Flux/GitOps path mirrors this with HelmRelease dependsOn chains.
.PHONY: deploy deploy-wave1 deploy-wave2 deploy-wave3 deploy-wave4
.PHONY: deploy-us deploy-hub  # legacy aliases

# Everything on kind with no registry: images are built locally and loaded
# into the clusters, and the charts are told to use them.
.PHONY: deploy-local kind-load-images
deploy-local:
	$(MAKE) deploy IMAGE_DELIVERY=kind-load-images \
		HUBSVC_HELM_ARGS="-f $(HUBSVC_CHART)/local-images-values.yaml $(HUBSVC_HELM_ARGS)" \
		SPOKE_HELM_ARGS="-f $(SPOKE_CHART)/local-images-values.yaml $(SPOKE_HELM_ARGS)"

# Hub images go to the hub, spoke images to each spoke.
kind-load-images:
	./hack/platform-mvp/kind-load-images.sh $(KIND_HUB) $(BC_IMAGE) $(VMBC_IMAGE) $(VMSG_IMAGE) $(AR_IMAGE) $(TI_IMAGE) $(DAP_IMAGE)
	@for s in $(SPOKES); do \
		./hack/platform-mvp/kind-load-images.sh $$s $(WO_IMAGE) $(OV_IMAGE) || exit 1; \
	done

deploy: crds
	@echo "==> Building images and creating clusters in parallel"
	$(MAKE) build-images-parallel & \
	$(MAKE) clusters & \
	wait
	$(MAKE) $(IMAGE_DELIVERY)
	@echo "==> Wave 1: CRDs on hub + us"
	$(MAKE) deploy-wave1
	@echo "==> Wave 2 (infra on hub) + Wave 4 (spokes) in parallel"
	@$(MAKE) --no-print-directory deploy-wave2 & \
	$(MAKE) --no-print-directory deploy-wave4 & \
	wait
	@echo "==> Wave 2: Infrastructure on hub"
	$(MAKE) deploy-wave2
	@echo "  Waiting for ingress-tls cert (Dex CA source) to be issued..."
	kubectl --context kind-$(KIND_HUB) -n $(OBS_NS) wait --for=jsonpath='{.data.ca\.crt}' secret/ingress-tls --timeout=60s
	@echo "  Refreshing spoke AuthenticationConfigurations (Dex CA now available)..."
	$(MAKE) ensure-spoke-auth-config
	@echo "==> Wave 3: Hub services on hub"
	$(MAKE) deploy-wave3
	@echo "==> Platform deploy complete (Flux not yet installed — run 'make deploy-cd' for GitOps)"

# ── Wave 1: CRDs (both hub and us, cluster-scoped) ──────────────────────────
deploy-wave1: crds
	@echo "==> Wave 1: Installing platform CRDs on hub + spokes"
	helm upgrade --install crds $(CRDS_CHART) \
		--create-namespace --wait --timeout $(HELM_TIMEOUT) \
		--kube-context kind-$(KIND_HUB)
	@for s in $(SPOKES); do \
		helm upgrade --install crds $(CRDS_CHART) \
			--create-namespace --wait --timeout $(HELM_TIMEOUT) \
			--kube-context kind-$$s || exit 1; \
	done
	@echo "  Waiting for Kro CRDs..."
	kubectl --context kind-$(KIND_HUB) wait --for=condition=Established crd/resourcegraphdefinitions.kro.run --timeout=30s
	@for s in $(SPOKES); do \
		kubectl --context kind-$$s wait --for=condition=Established crd/widgets.platform.example.com --timeout=30s || exit 1; \
	done
	@echo "==> Wave 1 complete"

# ── Wave 2: Infrastructure (hub context) ──────────────────────────────────────
deploy-wave2:
	@echo "==> Wave 2: Installing infrastructure on hub"
	@if [ "$(HUB_CHART)/Chart.lock" -ot "$(HUB_CHART)/Chart.yaml" ]; then \
		echo "  Chart.yaml changed, running helm dependency update..."; \
		helm dependency update $(HUB_CHART) > /dev/null; \
	else \
		echo "  Chart.lock is fresh, skipping helm dependency update"; \
	fi
	helm upgrade --install hub $(HUB_CHART) \
		-n $(OBS_NS) --create-namespace --timeout $(HELM_TIMEOUT) \
		--set ingress.host=$(INGRESS_HOST) \
		-f $(HUB_CHART)/e2e-values.yaml \
		--kube-context kind-$(KIND_HUB)
	@echo "  Waiting for core infra..."
	kubectl --context kind-$(KIND_HUB) -n $(OBS_NS) rollout status deploy/hub-cert-manager --timeout=2m &
	kubectl --context kind-$(KIND_HUB) -n $(OBS_NS) rollout status deploy/hub-cert-manager-webhook --timeout=2m &
	kubectl --context kind-$(KIND_HUB) -n $(OBS_NS) rollout status deploy/hub-ingress-nginx-controller --timeout=2m &
	wait
	@echo "  Waiting for Dex..."
	kubectl --context kind-$(KIND_HUB) -n $(OBS_NS) rollout status deploy/dex --timeout=2m
	@echo "  Enabling Grafana Ingress..."
	helm upgrade hub $(HUB_CHART) -n $(OBS_NS) --timeout 60s \
		--set ingress.host=$(INGRESS_HOST) --set ingress.enabled=true \
		-f $(HUB_CHART)/e2e-values.yaml \
		--kube-context kind-$(KIND_HUB)
	@echo "  Waiting for Grafana (may have restarted from the ingress upgrade)..."
	kubectl --context kind-$(KIND_HUB) -n $(OBS_NS) rollout status deploy/hub-grafana --timeout=60s
	@echo "  Waiting for Grafana's dashboard-sidecar to provision ConfigMap dashboards..."
	@echo "  (sidecar sync lag after a fresh deploy is observed to vary widely — up to ~90s)"
	kubectl --context kind-$(KIND_HUB) run dashboard-wait --rm -i --restart=Never --image=curlimages/curl -n $(OBS_NS) -- \
		sh -c 'for i in $$(seq 1 40); do CODE=$$(curl -s -o /dev/null -w "%{http_code}" http://admin:admin@hub-grafana/api/dashboards/uid/controller-deep-dive); [ "$$CODE" = "200" ] && exit 0; sleep 3; done; exit 1'
	@echo "==> Wave 2 complete"

# ── Wave 3: Hub services (hub context) ────────────────────────────────────────
deploy-wave3:
	@echo "==> Wave 3: Installing hub services on hub"
	@for s in $(SPOKES); do \
		[ -f $(SPOKE_KC_DIR)/kubeconfig-$$s-internal ] || kind get kubeconfig --name $$s --internal > $(SPOKE_KC_DIR)/kubeconfig-$$s-internal; \
	done
	kubectl --context kind-$(KIND_HUB) create namespace kro-system 2>/dev/null || true
	helm upgrade --install hub-services $(HUBSVC_CHART) \
		-n default --create-namespace --timeout $(HELM_TIMEOUT) \
		-f $(HUBSVC_CHART)/e2e-values.yaml $(TELEMETRY_HUB_ARGS) $(HUBSVC_HELM_ARGS) \
		--kube-context kind-$(KIND_HUB)
	@echo "  Creating secrets..."
	@for s in $(SPOKES); do \
		kubectl --context kind-$(KIND_HUB) create secret generic $$s-kubeconfig \
			--from-file=value=$(SPOKE_KC_DIR)/kubeconfig-$$s-internal \
			--dry-run=client -o yaml | kubectl --context kind-$(KIND_HUB) apply -f - || exit 1; \
	done
	kubectl --context kind-$(KIND_HUB) create secret generic binding-controller-dex \
		--from-literal=client-secret=us-spoke-controller-secret-demo \
		--dry-run=client -o yaml | kubectl --context kind-$(KIND_HUB) apply -f - &
	wait
	@echo "  Restarting controllers..."
	kubectl --context kind-$(KIND_HUB) -n default rollout restart deployment/binding-controller &
	wait
	kubectl --context kind-$(KIND_HUB) -n default rollout status deployment/binding-controller --timeout=60s
	kubectl --context kind-$(KIND_HUB) -n kro-system rollout status deployment/kro --timeout=2m
	@echo "==> Wave 3 complete"

# ── Wave 4: Spokes (one chart, per-spoke values) ─────────────────────────────
deploy-wave4:
	@for s in $(SPOKES); do \
		echo "==> Wave 4: Installing spoke $$s"; \
		kubectl --context kind-$$s create namespace default --dry-run=client -o yaml | kubectl --context kind-$$s apply -f - || exit 1; \
		if echo " $(KUBEVIRT_SPOKES) " | grep -q " $$s "; then \
			echo "  Installing KubeVirt operator $(KUBEVIRT_VERSION) on $$s"; \
			kubectl --context kind-$$s apply -f $(CRDS_DIR)/kubevirt-operator.yaml > /dev/null || exit 1; \
			kubectl --context kind-$$s wait --for=condition=Established crd/kubevirts.kubevirt.io --timeout=60s || exit 1; \
		fi; \
		if echo " $(COROOT_SPOKES) " | grep -q " $$s "; then \
			echo "  Installing Coroot operator $(COROOT_OPERATOR_VERSION) on $$s"; \
			kubectl --context kind-$$s create namespace coroot --dry-run=client -o yaml | kubectl --context kind-$$s apply -f - || exit 1; \
			kubectl --context kind-$$s label namespace coroot pod-security.kubernetes.io/enforce=privileged --overwrite || exit 1; \
			helm repo add coroot https://coroot.github.io/helm-charts > /dev/null || exit 1; \
			helm upgrade --install coroot-operator coroot/coroot-operator --version $(COROOT_OPERATOR_VERSION) \
				-n coroot --wait --timeout $(HELM_TIMEOUT) --kube-context kind-$$s || exit 1; \
		fi; \
		helm upgrade --install $$s $(SPOKE_CHART) -f $(SPOKE_CHART)/values-$$s.yaml $(SPOKE_HELM_ARGS) \
			-n default --create-namespace --wait --timeout $(HELM_TIMEOUT) \
			--kube-context kind-$$s || exit 1; \
		kubectl --context kind-$$s -n default rollout status deployment/widget-operator --timeout=60s || exit 1; \
		kubectl --context kind-$$s -n default rollout status deployment/oidc-verifier --timeout=60s || exit 1; \
		if echo " $(KUBEVIRT_SPOKES) " | grep -q " $$s "; then \
			echo "  Waiting for KubeVirt on $$s (operator rolls out its components)"; \
			kubectl --context kind-$$s -n kubevirt wait kv/kubevirt --for=condition=Available --timeout=10m || exit 1; \
		fi; \
	done
	@echo "==> Wave 4 complete"

# Legacy aliases
deploy-us: deploy-wave4
deploy-hub: deploy-wave1 deploy-wave2 deploy-wave3

# ── Flux / GitOps ────────────────────────────────────────────────────────────
.PHONY: deploy-flux deploy-cd verify-all

deploy-flux:
	@echo "==> Installing Flux controllers on hub"
	flux install --components=source-controller,helm-controller,kustomize-controller \
		--context=kind-$(KIND_HUB) --namespace=flux-system
	@echo "==> Applying Flux bootstrap"
	kubectl --context kind-$(KIND_HUB) apply -f $(FLUX_DIR)/bootstrap/flux-setup.yaml
	sleep 3
	kubectl --context kind-$(KIND_HUB) apply -f $(FLUX_DIR)/
	@echo "==> Flux ready"

deploy-cd:
	@echo "==> Enabling GitOps (Flux CD) — takes over hub HelmRelease management"
	flux install --components=source-controller,helm-controller,kustomize-controller \
		--context=kind-$(KIND_HUB) --namespace=flux-system
	kubectl --context kind-$(KIND_HUB) apply -f $(FLUX_DIR)/bootstrap/flux-setup.yaml
	sleep 3
	kubectl --context kind-$(KIND_HUB) apply -f $(FLUX_DIR)/
	@echo "==> GitOps enabled — Flux now manages the hub HelmRelease"

verify-all:
	@echo "==> Full clean-room validation: clean → deploy → validate → deploy-cd"
	$(MAKE) clean
	$(MAKE) deploy
	$(MAKE) validate
	$(MAKE) deploy-cd
	@echo "==> Full validation complete (GitOps enabled)"

# ── Chainsaw runner ──────────────────────────────────────────────────────────
.PHONY: chainsaw-runner
chainsaw-runner:
	docker build -t chainsaw-runner:local -f $(CHAINSAW_RUNNER_DIR)/Dockerfile.chainsaw-runner .
	kind load docker-image chainsaw-runner:local --name $(KIND_HUB)
	@echo "  Setting up kubeconfig secrets for CronJob..."
	kubectl --context kind-$(KIND_HUB) -n $(OBS_NS) delete secret kubeconfigs --ignore-not-found
	kubectl --context kind-$(KIND_HUB) -n $(OBS_NS) create secret generic kubeconfigs \
		--from-file=hub="$(HOME)/.kube/config" \
		$(foreach s,$(SPOKES),--from-file=$(s)="$(SPOKE_KC_DIR)/kubeconfig-$(s)-internal") \
		--dry-run=client -o yaml | kubectl apply -f -

# ── Validate ─────────────────────────────────────────────────────────────────
.PHONY: validate
validate: ensure-spoke-auth-config
	@echo "==> Running full Chainsaw E2E test suite"
	@mkdir -p $(CHAINSAW_DIR)
	kind get kubeconfig --name $(KIND_HUB) > $(CHAINSAW_DIR)/kubeconfig-hub
	@for s in $(SPOKES); do kind get kubeconfig --name $$s > $(CHAINSAW_DIR)/kubeconfig-$$s-internal || exit 1; done
	cd $(CHAINSAW_DIR) && chainsaw test . --test-dir tests --fail-fast=false \
		--report-format JSON --report-name $(CHAINSAW_REPORT)

# Chainsaw tests for telemetry-ingest. Separate from `validate` because the
# service is opt-in: deploy it first with `make deploy-local TELEMETRY_HUB=true`
# (or `make deploy-wave3 TELEMETRY_HUB=true` on an existing hub). TI_HELPERS
# tells the tests where their helper scripts are.
.PHONY: validate-telemetry-ingest
validate-telemetry-ingest:
	@echo "==> Running the telemetry-ingest Chainsaw tests"
	kind get kubeconfig --name $(KIND_HUB) > $(TI_TEST_DIR)/kubeconfig-hub
	@for s in $(SPOKES); do kind get kubeconfig --name $$s > $(TI_TEST_DIR)/kubeconfig-$$s-internal || exit 1; done
	cd $(TI_TEST_DIR) && TI_HELPERS="$(CURDIR)/$(TI_TEST_DIR)/helpers" chainsaw test . --test-dir tests \
		--report-format JSON --report-name chainsaw-report-telemetry-ingest

# The same checks against a GKE cluster (or any single cluster that runs the
# service and the node-agent): one project, ClusterIP, the real node-agent.
# GKE_CONTEXT is the kubectl context (kubectl config get-contexts); TI_GKE_PROJECT
# is the project named when the service was installed (default gke).
# Writes test data into that project under chainsaw-prefixed names. Needs the
# Chainsaw CLI, kubectl, jq, curl and python3.
GKE_CONTEXT       ?=
TI_GKE_PROJECT    ?= gke
TI_GKE_TEST_DIR   ?= tests/telemetry-ingest-gke
.PHONY: validate-telemetry-ingest-gke
validate-telemetry-ingest-gke:
	@test -n "$(GKE_CONTEXT)" || { echo "set GKE_CONTEXT=<kubectl context of the cluster>; kubectl config get-contexts lists them" >&2; exit 1; }
	@case "$(GKE_CONTEXT)" in kind-*) echo "GKE_CONTEXT $(GKE_CONTEXT) is a kind cluster; use validate-telemetry-ingest for the lab" >&2; exit 1;; esac
	@echo "==> Running the telemetry-ingest Chainsaw tests against $(GKE_CONTEXT), project $(TI_GKE_PROJECT)"
	umask 077 && kubectl config view --minify --flatten --context "$(GKE_CONTEXT)" > $(TI_GKE_TEST_DIR)/kubeconfig-hub
	cd $(TI_GKE_TEST_DIR) && TI_HELPERS="$(CURDIR)/tests/telemetry-ingest/helpers" TI_PROJECT="$(TI_GKE_PROJECT)" TI_OTHER_PROJECT="" \
		chainsaw test . --test-dir tests --report-format JSON --report-name chainsaw-report-telemetry-ingest-gke

# ── Grafana ──────────────────────────────────────────────────────────────────
.PHONY: grafana grafana-url
grafana:
	@echo "Port-forwarding Grafana → http://localhost:$(GRAFANA_PF_PORT)"
	@echo "  Login: admin / admin"
	kubectl --context kind-$(KIND_HUB) -n $(OBS_NS) port-forward svc/hub-grafana $(GRAFANA_PF_PORT):80

grafana-url:
	@echo "Grafana: $(GRAFANA_URL)  ·  Login: admin / admin"
	@echo ""
	@echo "  $(GRAFANA_URL)/d/cluster-fitness       Cluster Fitness"
	@echo "  $(GRAFANA_URL)/d/chainsaw-results       Chainsaw Test Results"
	@echo "  $(GRAFANA_URL)/d/controller-deep-dive   Controller Deep Dive"
	@echo "  $(GRAFANA_URL)/d/token-rotation         Token Rotation"

# ── Clean ────────────────────────────────────────────────────────────────────
.PHONY: clean clean-artifacts
clean:
	kind delete cluster --name $(KIND_HUB) 2>/dev/null || true
	@for s in $(SPOKES); do kind delete cluster --name $$s 2>/dev/null || true; done
	@for s in $(SPOKES); do rm -f $(SPOKE_KC_DIR)/kubeconfig-$$s-internal $(CHAINSAW_DIR)/kubeconfig-$$s-internal; done
	rm -f $(CHAINSAW_DIR)/kubeconfig-hub $(CHAINSAW_DIR)/$(CHAINSAW_REPORT)

clean-artifacts:
	rm -rf $(OUT_DIR) coverage-root.out coverage-bc.out coverage-wo.out

# ── Full loop ────────────────────────────────────────────────────────────────
.PHONY: all
all: lint test build deploy validate
	@echo "==> Full loop complete"

# ── Install helpers ──────────────────────────────────────────────────────────
.PHONY: install-chainsaw install-flux
install-chainsaw:
	go install github.com/kyverno/chainsaw@latest

install-flux:
	curl -s https://fluxcd.io/install.sh | bash