# Kabr - developer tasks. Requires GNU make, Docker with the compose plugin.
# Run `make help` to list the targets.

SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

COMPOSE_FILE ?= deploy/compose/docker-compose.yml
COMPOSE      ?= docker compose -f $(COMPOSE_FILE)

# Demo credentials are never written here. `make demo-secrets` generates
# random passwords into $(DEMO_ENV) (gitignored, mode 600); the demo targets
# load that file in the recipe shell and hand the DSN to kabr through KABR_DSN
# (not --dsn), so it never shows up in the process list either.
DEMO_ENV  ?= deploy/compose/.env.demo
DEMO_ADDR ?= localhost:55432
# $(call demo_dsn,<role>,<password variable from DEMO_ENV>), expanded by the
# shell after LOAD_DEMO_ENV. Passwords are hex, so no URL escaping is needed.
demo_dsn = postgres://$(1):$${$(2)}@$(DEMO_ADDR)/pagila?sslmode=disable
LOAD_DEMO_ENV = test -r $(DEMO_ENV) || { echo "$(DEMO_ENV) not found - run 'make demo-up' first" >&2; exit 1; }; \
	set -a; . ./$(DEMO_ENV); set +a

.PHONY: help build test test-integration test-demo lint vuln generate manifests envtest \
        introspect-demo introspect-demo-unsafe \
        plan-demo eval-pagila demo-secrets demo-fetch demo-up demo-down demo-psql \
        docker-build pagila-image kind-up kind-down kind-load kind-kubeconfig \
        demo-k8s demo-db demo-operator demo-plan demo-fingerprint e2e e2e-test e2e-down

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_.-]+:.*## / {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# --- Go targets ---------------------------------------------------------------
# Pinned tool versions (keep in sync with .github/workflows/ci.yml). All run
# through `go run pkg@version`: reproducible, no global install, no sudo.
GOLANGCI_LINT_VERSION  ?= v2.15.0
GOVULNCHECK_VERSION    ?= v1.9.0
CONTROLLER_GEN_VERSION ?= v0.23.0
SETUP_ENVTEST_VERSION  ?= v0.25.3
# Kubernetes version of the envtest control plane (kube-apiserver + etcd);
# `go run sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION) list`
# shows the versions on offer.
ENVTEST_K8S_VERSION    ?= 1.37.0
# Where setup-envtest stores the control plane binaries (bin/ is gitignored).
ENVTEST_BIN_DIR        ?= bin/envtest

CONTROLLER_GEN = go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)
SETUP_ENVTEST  = go run sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION)

BIN      ?= bin/kabr
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)
EXAMPLES := examples

build: ## Build bin/kabr (version from git describe)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/kabr

test: ## Run unit tests with the race detector
	go test -race -count=1 ./...

test-integration: ## Run unit + integration tests (needs Docker; uses testcontainers)
	go test -race -count=1 -tags integration ./...

lint: ## go vet, gofmt and golangci-lint (pinned, via go run)
	go vet ./... && go vet -tags integration ./... && go vet -tags llm_live ./... && go vet -tags envtest ./... && go vet -tags e2e ./...
	@test -z "$$(gofmt -l .)" || { echo "files need gofmt:"; gofmt -l .; exit 1; }
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

vuln: ## Scan for known vulnerabilities (govulncheck, pinned)
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

# --- Kubernetes API (kabr.dev/v1alpha1) ----------------------------------------
# Generated files are committed; CI fails when they drift from the Go types.

generate: ## Generate DeepCopy code (controller-gen object) for api/ and pkg/plan
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./api/... paths=./pkg/plan/...

manifests: ## Generate CRDs (config/crd/bases) and operator RBAC (config/rbac) from markers
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=kabr-operator paths=./internal/controller/... output:rbac:artifacts:config=config/rbac

# setup-envtest prints a path relative to a relative --bin-dir; "cd && pwd"
# makes it absolute, because each test package runs in its own directory.
envtest: ## Run unit + envtest tests against a real kube-apiserver/etcd (downloaded into bin/envtest)
	@assets="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir "$(ENVTEST_BIN_DIR)" -p path)" && \
	assets="$$(CDPATH= cd -- "$$assets" && pwd)" && \
	echo "KUBEBUILDER_ASSETS=$$assets" && \
	KUBEBUILDER_ASSETS="$$assets" go test -race -count=1 -tags envtest ./...

introspect-demo: build ## Introspect the demo Pagila DB into examples/ (after make demo-up)
	@mkdir -p $(EXAMPLES)
	@$(LOAD_DEMO_ENV); export KABR_DSN="$(call demo_dsn,kabr_reader,KABR_READER_PASSWORD)"; set -ex; \
	./$(BIN) introspect --strict --format json    --out $(EXAMPLES)/pagila.schema.json; \
	./$(BIN) introspect --strict --format mermaid --out $(EXAMPLES)/pagila.mmd; \
	./$(BIN) introspect --strict --format dot     --out $(EXAMPLES)/pagila.dot

introspect-demo-unsafe: build ## Show the least-privilege warning; --strict refuses kabr_unsafe (exit 3)
	@$(LOAD_DEMO_ENV); export KABR_DSN="$(call demo_dsn,kabr_unsafe,KABR_UNSAFE_PASSWORD)"; \
	code=0; ./$(BIN) introspect --strict >/dev/null || code=$$?; \
	if [ "$$code" -eq 3 ]; then echo "OK: --strict refused the over-privileged role (exit 3)"; \
	else echo "FAIL: expected exit 3, got $$code"; exit 1; fi

# --- Domain plan (graph-only: no LLM call, no credentials needed) -------------
PAGILA_GOLD ?= evals/pagila/gold.yaml
# Scratch plan of eval-pagila; bin/ is gitignored.
EVAL_PLAN   ?= bin/pagila.eval.plan.yaml

plan-demo: build ## Generate examples/pagila.plan.yaml from examples/pagila.schema.json (graph-only)
	./$(BIN) plan generate --catalog $(EXAMPLES)/pagila.schema.json --llm none --out $(EXAMPLES)/pagila.plan.yaml

eval-pagila: build ## Score the graph-only Pagila plan against evals/pagila/gold.yaml
	./$(BIN) plan generate --catalog $(EXAMPLES)/pagila.schema.json --llm none --out $(EVAL_PLAN)
	./$(BIN) plan eval $(EVAL_PLAN) --gold $(PAGILA_GOLD)
	go test -count=1 -run Pagila ./pkg/planner/

test-demo: ## Run the integration test against the running demo (after make demo-up)
	@$(LOAD_DEMO_ENV); export KABR_DEMO_DSN="$(call demo_dsn,kabr_reader,KABR_READER_PASSWORD)"; \
	go test -count=1 -tags integration -run TestDemoPagila -v ./pkg/introspect/postgres/

# --- Demo database ------------------------------------------------------------

demo-fetch: ## Download and verify the Pagila sample database
	./scripts/fetch-pagila.sh

demo-secrets: $(DEMO_ENV) ## Generate random demo passwords into deploy/compose/.env.demo (if missing)

# Hex passwords: valid in a URL, in a compose env_file and in a shell file
# without any quoting. openssl when available, /dev/urandom otherwise.
$(DEMO_ENV):
	@umask 077; rnd() { openssl rand -hex 16 2>/dev/null || od -An -N16 -tx1 /dev/urandom | tr -d ' \n'; }; \
	{ echo "# Random LOCAL DEMO passwords, generated by 'make demo-secrets'."; \
	  echo "# Gitignored. Deleted by 'make demo-down' together with the database volume."; \
	  for v in POSTGRES_PASSWORD KABR_READER_PASSWORD KABR_UNSAFE_PASSWORD; do \
	    p=$$(rnd); [ $${#p} -eq 32 ] || { echo "demo-secrets: could not generate a password" >&2; exit 1; }; \
	    echo "$$v=$$p"; \
	  done; } > $@.tmp && chmod 600 $@.tmp && mv $@.tmp $@
	@echo "wrote $@ (random demo passwords; view with: cat $@)"

demo-up: demo-secrets demo-fetch ## Start the demo Postgres (Pagila) and wait until healthy
	$(COMPOSE) up -d --wait

# The passwords are baked into the volume on first start, so the volume and
# $(DEMO_ENV) are deleted together; the next demo-up generates new ones.
demo-down: ## Stop the demo Postgres, delete its volume and the generated passwords
	$(COMPOSE) down -v
	rm -f $(DEMO_ENV)

demo-psql: ## Open psql in the demo database as kabr_reader
	$(COMPOSE) exec postgres sh -c 'PGPASSWORD="$$KABR_READER_PASSWORD" exec psql -h 127.0.0.1 -U kabr_reader -d pagila'

# --- Container image ----------------------------------------------------------
# One image, $(IMG), runs both `kabr operator` and `kabr serve` (distroless,
# nonroot, no shell). Built for the host architecture, which matches the kind
# node. Provenance/SBOM attestations are off: `kind load docker-image` cannot
# import the attestation manifests of an image index.
IMG               ?= kabr:dev
PAGILA_IMG        ?= kabr-pagila:dev
DOCKER_BUILD_ARGS ?= --provenance=false --sbom=false

docker-build: ## Build the kabr container image $(IMG) (VERSION from git describe)
	docker build $(DOCKER_BUILD_ARGS) --build-arg VERSION=$(VERSION) -t $(IMG) .

pagila-image: demo-fetch ## Build the demo Postgres+Pagila image $(PAGILA_IMG) for kind
	docker build $(DOCKER_BUILD_ARGS) -f deploy/kind/pagila/Dockerfile -t $(PAGILA_IMG) deploy/compose

# --- Local Kubernetes (kind) --------------------------------------------------
# kind always runs with $(KIND_KUBECONFIG_FLAG) and every kubectl call goes
# through $(KUBECTL), with an explicit --kubeconfig AND --context:
# ~/.kube/config and the ambient current-context are never read or written.
# Only the binaries are overridable (KIND_BIN, KUBECTL_BIN); KIND, KUBECTL and
# KIND_KUBECONFIG_FLAG are "override" variables, so neither the environment
# nor the command line can drop the flags. Expanding them fails the build
# when KIND_KUBECONFIG or KUBE_CONTEXT is blank, or when KIND_KUBECONFIG is
# ~/.kube/config. Paths are quoted: the checkout may live under a directory
# with spaces.
KIND_VERSION    ?= v0.33.0
KIND_BIN        ?= go run sigs.k8s.io/kind@$(KIND_VERSION)
KUBECTL_BIN     ?= kubectl
KIND_CLUSTER    ?= kabr
# Default node image of kind v0.33.0 (pkg/apis/config/defaults/image.go).
KIND_NODE_IMAGE ?= kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5
KIND_KUBECONFIG ?= $(abspath bin/kind-kabr.kubeconfig)
KUBE_CONTEXT    ?= kind-$(KIND_CLUSTER)
override KIND_GUARD = $(if $(strip $(KIND_KUBECONFIG)),,$(error KIND_KUBECONFIG is empty: refusing to let kind/kubectl fall back to ~/.kube/config))$(if $(strip $(KUBE_CONTEXT)),,$(error KUBE_CONTEXT is empty: refusing to use the ambient current-context))$(if $(filter $(abspath $(HOME)/.kube/config),$(abspath $(strip $(KIND_KUBECONFIG)))),$(error KIND_KUBECONFIG must not be ~/.kube/config))
override KIND                 = $(KIND_GUARD)$(KIND_BIN)
override KIND_KUBECONFIG_FLAG = $(KIND_GUARD)--kubeconfig "$(KIND_KUBECONFIG)"
override KUBECTL              = $(KIND_GUARD)$(KUBECTL_BIN) --kubeconfig "$(KIND_KUBECONFIG)" --context $(KUBE_CONTEXT)
DEMO_NS         ?= kabr-demo
OPERATOR_NS     ?= kabr-system
# Plan applied by demo-plan: approved into bin/ (examples/ stays Proposed).
KIND_PLAN          ?= bin/pagila.approved.plan.yaml
KIND_PLAN_MANIFEST ?= bin/pagila.domainplan.yaml
# Local port of the port-forward used by demo-fingerprint.
FINGERPRINT_PORT   ?= 55433

kind-up: ## Create the kind cluster $(KIND_CLUSTER) (idempotent); kubeconfig in bin/
	@mkdir -p bin
	@if $(KIND) get clusters 2>/dev/null | grep -qx '$(KIND_CLUSTER)'; then \
	  echo "kind cluster $(KIND_CLUSTER) exists"; \
	  $(KIND) export kubeconfig --name $(KIND_CLUSTER) $(KIND_KUBECONFIG_FLAG); \
	else \
	  $(KIND) create cluster --name $(KIND_CLUSTER) --image $(KIND_NODE_IMAGE) \
	    --config deploy/kind/kind-config.yaml $(KIND_KUBECONFIG_FLAG) --wait 180s; \
	fi
	@chmod 600 "$(KIND_KUBECONFIG)"
	$(KUBECTL) wait --for=condition=Ready node --all --timeout=180s

kind-kubeconfig: ## Re-export the kind kubeconfig into bin/ (never ~/.kube/config)
	@mkdir -p bin
	$(KIND) export kubeconfig --name $(KIND_CLUSTER) $(KIND_KUBECONFIG_FLAG)
	@chmod 600 "$(KIND_KUBECONFIG)"

kind-down: ## Delete the kind cluster $(KIND_CLUSTER) and its kubeconfig
	$(KIND) delete cluster --name $(KIND_CLUSTER) $(KIND_KUBECONFIG_FLAG)
	rm -f "$(KIND_KUBECONFIG)"

kind-load: ## Load $(IMG) and $(PAGILA_IMG) into the kind node
	$(KIND) load docker-image $(IMG) $(PAGILA_IMG) --name $(KIND_CLUSTER)

demo-db: ## In kind: random Secrets, Postgres+Pagila, CRDs and the DataSource (after kind-load)
	$(KUBECTL) apply -f deploy/kind/demo/namespace.yaml
	$(KIND_GUARD)KIND_KUBECONFIG="$(KIND_KUBECONFIG)" KUBE_CONTEXT=$(KUBE_CONTEXT) NAMESPACE=$(DEMO_NS) ./deploy/kind/demo-secrets.sh
	$(KUBECTL) apply -k deploy/kind/demo
	$(KUBECTL) -n $(DEMO_NS) rollout status deployment/postgres --timeout=300s
	$(KUBECTL) apply -k config/crd
	$(KUBECTL) wait --for=condition=Established --timeout=60s \
	  crd/datasources.kabr.dev crd/domainplans.kabr.dev crd/domainapis.kabr.dev
	$(KUBECTL) apply -f deploy/kind/datasource.yaml

demo-operator: ## In kind: install the operator (deploy/kind/operator overlay of config/default)
	$(KUBECTL) apply -k deploy/kind/operator
	$(KUBECTL) -n $(OPERATOR_NS) rollout status deployment/kabr-operator --timeout=180s

demo-plan: build ## In kind: approve examples/pagila.plan.yaml into bin/ and apply its DomainPlan
	./$(BIN) plan approve $(EXAMPLES)/pagila.plan.yaml --by kind-demo --catalog $(EXAMPLES)/pagila.schema.json --out $(KIND_PLAN)
	./$(BIN) plan manifest $(KIND_PLAN) --datasource pagila --namespace $(DEMO_NS) --out $(KIND_PLAN_MANIFEST)
	$(KUBECTL) apply -f $(KIND_PLAN_MANIFEST)

# Port-forwards the demo Postgres and validates the example plan against the
# live catalog: `plan validate` fails unless the live catalog fingerprint
# equals the plan's (examples/pagila.schema.json). The DSN is read from the
# Secret into the environment (KABR_DSN), never into argv or the output.
demo-fingerprint: build ## In kind: prove the live Pagila catalog fingerprint equals examples/
	@set -euo pipefail; \
	$(KUBECTL) -n $(DEMO_NS) port-forward svc/postgres $(FINGERPRINT_PORT):5432 >bin/.pf-postgres.log 2>&1 & pf=$$!; \
	trap 'kill $$pf 2>/dev/null || true' EXIT; \
	for i in $$(seq 1 60); do grep -q Forwarding bin/.pf-postgres.log && break; \
	  kill -0 $$pf 2>/dev/null || { cat bin/.pf-postgres.log; exit 1; }; sleep 0.5; done; \
	grep -q Forwarding bin/.pf-postgres.log || { echo "port-forward did not start" >&2; exit 1; }; \
	pw="$$($(KUBECTL) -n $(DEMO_NS) get secret pagila-db -o jsonpath='{.data.KABR_READER_PASSWORD}' | base64 -d)"; \
	export KABR_DSN="postgres://kabr_reader:$$pw@127.0.0.1:$(FINGERPRINT_PORT)/pagila?sslmode=disable"; \
	./$(BIN) plan validate $(EXAMPLES)/pagila.plan.yaml

demo-k8s: docker-build pagila-image ## Full kind demo: images, DB, CRDs, operator, DataSource, DomainPlan
	$(MAKE) kind-load
	$(MAKE) demo-db
	$(MAKE) demo-fingerprint
	$(MAKE) demo-operator
	$(MAKE) demo-plan

e2e-test: ## Run the e2e test against an already deployed demo-k8s (build tag e2e)
	$(KIND_GUARD)KIND_KUBECONFIG="$(KIND_KUBECONFIG)" KUBE_CONTEXT=$(KUBE_CONTEXT) KABR_E2E_NAMESPACE=$(DEMO_NS) \
	KABR_E2E_OPERATOR_NAMESPACE=$(OPERATOR_NS) go test -tags e2e -count=1 -v -timeout 20m ./test/e2e/

e2e: ## kind-up + demo-k8s + the e2e test (leaves the cluster up; make e2e-down deletes it)
	$(MAKE) kind-up
	$(MAKE) demo-k8s
	$(MAKE) e2e-test

e2e-down: kind-down ## Delete the e2e kind cluster
