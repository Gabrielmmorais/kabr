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

.PHONY: help build test test-integration test-demo lint vuln introspect-demo introspect-demo-unsafe \
        plan-demo eval-pagila demo-secrets demo-fetch demo-up demo-down demo-psql

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_.-]+:.*## / {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# --- Go targets ---------------------------------------------------------------
# Pinned tool versions (keep in sync with .github/workflows/ci.yml). Both run
# through `go run pkg@version`: reproducible, no global install, no sudo.
GOLANGCI_LINT_VERSION ?= v2.15.0
GOVULNCHECK_VERSION   ?= v1.9.0

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
	go vet ./... && go vet -tags integration ./... && go vet -tags llm_live ./...
	@test -z "$$(gofmt -l .)" || { echo "files need gofmt:"; gofmt -l .; exit 1; }
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

vuln: ## Scan for known vulnerabilities (govulncheck, pinned)
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

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
