# ADR 0001: Go, in a single module

- **Status:** Accepted
- **Date:** 2026-11-23

## Context

Kabr has three kinds of code that must agree on one data model:

1. A CLI (`kabr`) that introspects a database and prints the catalog and its
   table graph.
2. A Kubernetes operator that reconciles `DataSource`, `DomainPlan` and
   `DomainAPI` custom resources (milestone M3).
3. A generic runtime that serves one governed REST + MCP API per domain,
   configured by an approved plan instead of generated code (M3/M4).

All three exchange the canonical catalog defined in `pkg/schema`, and later
the domain plan. The operator is the most constrained component: the mature
tooling for writing operators (kubebuilder, controller-runtime, controller-gen
for CRD and RBAC generation, envtest) is Go-first.

The project has one maintainer, so build, release and dependency overhead
matter as much as architectural purity.

## Decision

- Write Kabr in **Go** (the version in `go.mod`).
- Keep the whole repository as **one Go module**,
  `github.com/Gabrielmmorais/kabr`, with libraries under `pkg/` and binaries
  under `cmd/`.
- `pkg/schema` is the single source of truth for the catalog model. The CLI,
  the operator and the runtime import it directly; there is no separate wire
  format to keep in sync.
- Tests that need external services (databases through testcontainers) sit
  behind the `integration` build tag, so `go test ./...` stays fast and
  hermetic.

## Consequences

**Positive**

- The operator uses kubebuilder and controller-runtime without a language
  bridge.
- A change to `pkg/schema` is type-checked against every consumer in one
  `go build ./...` and one CI run; there are no cross-repo version bumps.
- One `go.mod`, one lint config, one vulnerability scan, one release pipeline.
- Static binaries and small distroless images for the CLI, operator and
  runtime.

**Negative / trade-offs**

- The module's dependency graph is the union of all components: the CLI
  inherits controller-runtime and Kubernetes client dependencies once the
  operator lands. This increases build time and the surface `govulncheck`
  reports on. If it becomes a problem, the operator can move to a nested
  module later; `pkg/schema` would then become a versioned dependency.
- Components cannot be versioned independently; one tag versions everything.
- The LLM ecosystem is richer in Python. Segmentation (M2) calls LLMs over
  HTTP APIs, which Go handles fine, but some research tooling (for example
  community-detection libraries) may need to be implemented or ported in Go.
- Go is less expressive for heavy data manipulation than Python; acceptable
  because Kabr manipulates catalogs (structure), not data.
