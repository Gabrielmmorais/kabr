# Kabr

[![CI](https://github.com/Gabrielmmorais/kabr/actions/workflows/ci.yml/badge.svg)](https://github.com/Gabrielmmorais/kabr/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go version](https://img.shields.io/github/go-mod/go-version/Gabrielmmorais/kabr)](go.mod)

**Point Kabr at any relational database with read-only access: an LLM segments
it into domains, a Kubernetes operator ships one governed REST + MCP API per
domain.**

> **Status: early development.** Milestones M1 (read-only catalog
> introspection and the table graph) and M2 (segmentation into a reviewable
> `DomainPlan`, optional LLM refinement, human approval) are implemented and
> tested. Everything after them is design and roadmap; this README marks
> which is which.

## Why

Many companies run on one large relational database that every service reads
and writes directly. Two pressures push against that:

- **Decomposition.** Teams want to split the monolith into services with
  clear ownership, but the first step, deciding which tables belong
  together, is usually done by hand from tribal knowledge.
- **AI agents need data, governed.** Agents and LLM applications are
  increasingly asked to answer questions over production data. Handing them a
  database credential gives them everything, with no masking, row limits or
  per-query audit.

Kabr's answer is to derive domains from the schema itself, have a human
approve them, and expose each domain through a narrow, read-only, audited API
(REST for applications, MCP for agents) instead of the database.

## How it works

| Stage | What it does | Status |
|-------|--------------|--------|
| **1. Connect** | Read-only, catalog-only introspection: tables, columns, keys, foreign keys, indexes, comments, planner estimates. A least-privilege check warns about (or refuses) over-privileged credentials. | ✅ Implemented (M1) |
| **2. Segment** | Build the foreign-key graph, run deterministic community detection, optionally let an LLM refine and name the domains and flag sensitive columns. Output: a `DomainPlan` that a human approves. | ✅ Implemented (M2) |
| **3. Generate** | A Go operator turns an approved plan into one `DomainAPI` per domain: REST/OpenAPI + MCP from a generic runtime, column masking, row limits, table/column allowlist, HPA/KEDA, NetworkPolicy, Gateway API route. | 🗺️ Planned (M3-M4) |
| **4. Govern** | Per-query audit with OpenTelemetry, per-consumer quotas. | 🗺️ Planned (M4) |
| **5. Evaluate** | Segmentation evals in CI against Pagila, Chinook, Northwind and AdventureWorks with hand-labeled domains. | 🟡 Pagila gate in tests (M2); more datasets planned (M5) |

```mermaid
flowchart LR
    db[("Relational DB<br/>read-only role")]
    intro["1 Connect<br/>catalog introspection"]
    seg["2 Segment<br/>FK graph + communities + LLM"]
    plan{{"DomainPlan<br/>human approval"}}
    op["3 Generate<br/>Kabr operator"]
    api["DomainAPI per domain<br/>REST + MCP, masking,<br/>row limits, allowlist"]
    gov["4 Govern<br/>OTel audit, quotas"]
    evals["5 Evaluate<br/>segmentation evals in CI"]
    consumers["Apps and AI agents"]

    db -- "catalog only" --> intro --> seg --> plan --> op --> api
    consumers --> api -- "governed read-only queries" --> db
    api --> gov
    evals -.-> seg
```

The full target design, including the CRDs and the `kabr introspect` sequence
diagram, is in [docs/architecture.md](docs/architecture.md).

## Quickstart

Requirements: Go (version in [go.mod](go.mod)), GNU make, Docker with the
compose plugin.

```sh
make demo-up            # PostgreSQL 17 + Pagila on 127.0.0.1:55432, random passwords
make introspect-demo    # builds bin/kabr, writes examples/pagila.{schema.json,mmd,dot}
make introspect-demo-unsafe   # shows the least-privilege warning; --strict exits 3
make demo-down          # stop, delete the demo volume and its passwords
```

`make demo-up` generates random passwords into `deploy/compose/.env.demo`
(gitignored; `cat` it to see them) and the demo targets read them from there.
See [deploy/compose/README.md](deploy/compose/README.md).

The generated files are committed in [examples/](examples/):
[`pagila.schema.json`](examples/pagila.schema.json) (canonical catalog),
[`pagila.mmd`](examples/pagila.mmd) (Mermaid ER diagram) and
[`pagila.dot`](examples/pagila.dot) (Graphviz).

### Using the CLI

```sh
make build   # or: go build -o bin/kabr ./cmd/kabr

# Prefer the environment variable: a password passed with --dsn ends up in
# your shell history and in the process list.
export KABR_DSN='postgres://kabr_reader:<password>@db.internal:5432/app?sslmode=verify-full'

kabr introspect > app.schema.json                         # canonical JSON to stdout
kabr introspect --format mermaid --out app.mmd            # atomic write (temp file + rename)
kabr introspect --schema public,billing --strict          # refuse over-privileged roles
kabr introspect --format dot | dot -Tsvg > app.svg        # Graphviz
kabr help introspect
```

| Flag | Default | Meaning |
|------|---------|---------|
| `--dsn` | `$KABR_DSN` | `postgres://` URL or keyword/value DSN. |
| `--schema` | `public` | Schema to read; repeatable or comma-separated. |
| `--format` | `json` | `json`, `mermaid` or `dot`. |
| `--out` | stdout | Output file, written atomically. |
| `--strict` | off | Exit 3 without introspecting if the role holds privileges beyond read-only. |
| `--no-stats` | off | Omit planner statistics (row estimates, `n_distinct`). |
| `--include-partitions` | off | Report partitions as tables; by default only the partitioned parent. |
| `--timestamp` | off | Add `introspectedAt` to the JSON; off so output is byte-for-byte reproducible. |
| `--statement-timeout` | `15s` | Upper bound for each catalog query. |

Warnings and a graph summary go to stderr, the export to stdout or `--out`,
so the command composes in pipes. The DSN password never appears in any
output or error message.

| Exit code | Meaning |
|-----------|---------|
| `0` | Success (warnings may still have been printed). |
| `1` | Error: connection, query, output or interruption (Ctrl-C). |
| `2` | Invalid usage (unknown flag, missing DSN, bad value). |
| `3` | `--strict` refused a role with privileges beyond read-only. |

Only PostgreSQL is supported today; MySQL and SQL Server DSNs are recognized
and rejected with a clear "not supported yet" error.

## Example output

`make introspect-demo` against [Pagila](https://github.com/devrimgunduz/pagila)
(the PostgreSQL port of the Sakila sample database) prints:

```text
kabr: introspected database "pagila" (postgres), schemas public -> examples/pagila.mmd (mermaid)
  tables 15 | foreign keys 18 (0 self-referencing tables, 0 to tables outside the selection)
  connected components 2 | orphan tables 1 | most connected public.film (5)
```

The relationships of the generated ER diagram (the full file, with every
column, type and PK/FK/UK marker, is
[examples/pagila.mmd](examples/pagila.mmd)):

```mermaid
erDiagram
    city ||--o{ address : "address_city_id_fkey"
    country ||--o{ city : "city_country_id_fkey"
    address ||--o{ customer : "customer_address_id_fkey"
    store ||--o{ customer : "customer_store_id_fkey"
    language ||--o{ film : "film_language_id_fkey"
    language |o--o{ film : "film_original_language_id_fkey"
    actor ||--o{ film_actor : "film_actor_actor_id_fkey"
    film ||--o{ film_actor : "film_actor_film_id_fkey"
    category ||--o{ film_category : "film_category_category_id_fkey"
    film ||--o{ film_category : "film_category_film_id_fkey"
    film ||--o{ inventory : "inventory_film_id_fkey"
    store ||--o{ inventory : "inventory_store_id_fkey"
    customer ||--o{ rental : "rental_customer_id_fkey"
    inventory ||--o{ rental : "rental_inventory_id_fkey"
    staff ||--o{ rental : "rental_staff_id_fkey"
    address ||--o{ staff : "staff_address_id_fkey"
    store ||--o{ staff : "staff_store_id_fkey"
    address ||--o{ store : "store_address_id_fkey"
```

The orphan is `payment`: in Pagila 3.1 it is a partitioned table whose foreign
keys are declared on each monthly partition, not on the parent. With
`--include-partitions` the seven partitions and their foreign keys appear
(22 tables, 36 foreign keys). Lifting partition foreign keys to the parent is
handled in segmentation: M2 infers the missing references from column
names (see [Segmentation](#segmentation-m2)); M1 reports the catalog as it
is.

`|o--o{` marks an optional (nullable) foreign key, `||--o{` a mandatory one.
Labels are constraint names, so the diagram stays traceable to the database.

## Segmentation (M2)

`kabr plan` turns a catalog into a `DomainPlan`: a YAML document
(`apiVersion: kabr.dev/v1alpha1`, `kind: DomainPlan`) that groups every table
into exactly one domain, flags sensitive columns with a masking action, and
waits for a human to approve it. The M3 operator will act on approved plans
only.

1. **Baseline, no AI.** Deterministic Louvain community detection on the
   foreign-key graph, plus references inferred from column names (`X_id`
   without a declared foreign key) at half weight. Junction, lookup and
   orphan tables are recognized and placed explicitly. Domains are named
   after their anchor table and sensitive columns come from a heuristic over
   column names and types. Same catalog, byte-identical plan.
2. **Optional LLM refinement.** Anthropic (default when credentials are
   present) or a local Ollama model may rename and describe domains, move
   tables and classify sensitive columns. Only allowlisted catalog metadata
   is sent, never rows. Every proposed change is validated against the
   catalog; anything invalid is dropped with a warning, and an unusable
   answer falls back to the baseline.
3. **Human approval.** `kabr plan approve --by NAME` re-validates the plan
   and records who approved it and when. The plan is pinned to a structural
   fingerprint of the catalog, so a plan for a different schema is rejected.

Design: [ADR 0004](docs/adr/0004-segmentation-baseline-llm-approval.md)
(pipeline) and [ADR 0005](docs/adr/0005-llm-providers-metadata-only.md)
(providers, privacy). What reaches an LLM is listed in
[SECURITY.md](SECURITY.md#what-is-sent-to-an-llm).

### Quickstart

No database needed: the committed Pagila catalog is enough.

```sh
make plan-demo          # graph-only plan from examples/pagila.schema.json -> examples/pagila.plan.yaml
make eval-pagila        # score it against evals/pagila/gold.yaml

kabr plan show examples/pagila.plan.yaml                     # table view
kabr plan validate examples/pagila.plan.yaml --catalog examples/pagila.schema.json
kabr plan eval examples/pagila.plan.yaml --gold evals/pagila/gold.yaml

# Generate your own; --llm defaults to anthropic when ANTHROPIC_API_KEY is
# set, otherwise none (with a notice on stderr).
kabr plan generate --catalog examples/pagila.schema.json --llm none --out pagila.plan.yaml
kabr plan generate --dsn "$KABR_DSN" --llm ollama --model llama3.1 --out app.plan.yaml
kabr plan generate --catalog examples/pagila.schema.json --resolution 1.5 --out finer.plan.yaml

kabr plan approve pagila.plan.yaml --by alice --catalog examples/pagila.schema.json
```

| Command | Purpose |
|---------|---------|
| `kabr plan generate [--catalog FILE \| --dsn DSN] [--llm anthropic\|ollama\|none] [--model M] [--effort low\|medium\|high\|xhigh\|max] [--resolution F] [--name N] [--out FILE]` | Build a plan from a catalog file or a live, read-only introspection. |
| `kabr plan show FILE [--format table\|yaml\|json]` | Print a plan. |
| `kabr plan validate FILE [--catalog FILE]` | Strict decoding and validation; with `--catalog`, also coverage and fingerprint. |
| `kabr plan approve FILE --by NAME [--catalog FILE]` | Re-validate and set `status.phase: Approved`. |
| `kabr plan eval FILE --gold FILE` | Score against a gold standard (pairwise F1, ARI, sensitive-column metrics). |

| Variable | Meaning |
|----------|---------|
| `ANTHROPIC_API_KEY` / `ANTHROPIC_AUTH_TOKEN` | Anthropic credentials, read by the official SDK only. |
| `KABR_LLM_MODEL` | Model override (defaults: `claude-opus-5-5`, `llama3.1`). |
| `KABR_OLLAMA_URL` | Ollama base URL (default `http://127.0.0.1:11434`). |

Exit codes are the same as for `kabr introspect` (0, 1, 2, 3).

### Example plan

Excerpt of the committed graph-only plan,
[examples/pagila.plan.yaml](examples/pagila.plan.yaml) (rationales,
descriptions and most columns elided):

```yaml
apiVersion: kabr.dev/v1alpha1
kind: DomainPlan
metadata:
  name: pagila
spec:
  source:
    dialect: postgres
    database: pagila
    catalogFingerprint: sha256:...
  domains:
    - name: film
      tables:
        - name: public.actor
        - name: public.category
        - name: public.film
        - name: public.film_actor
          role: junction
        - name: public.film_category
          role: junction
        - name: public.inventory
        - name: public.language
          role: lookup
    - name: store
      tables:
        - name: public.address
        - name: public.customer
        - name: public.payment
        - name: public.rental
        - name: public.staff
        - name: public.store
      sensitiveColumns:
        - table: public.staff
          column: password
          class: credential
          action: drop
          source: heuristic
        # ...
    - name: city
      tables:
        - name: public.city
        - name: public.country
status:
  phase: Proposed
  generator:
    mode: graph
    modularity: 0.4087
```

### Pagila evaluation

Graph-only baseline against the human gold standard in
[evals/pagila/gold.yaml](evals/pagila/gold.yaml) (4 domains: catalog,
customers, rentals, stores), measured 2026-11-23:

| Resolution γ | Domains | Pairwise F1 | ARI |
|--------------|---------|-------------|-----|
| 1.0 (default) | 3 | 0.613 | 0.459 |
| 1.5 | 4 | 0.755 | 0.672 |

At γ = 1.0 the modularity is Q = 0.4087 and customers, rentals, payments and
staff land in one `store` domain, which is the main gap to the gold. The
default stays at 1.0 so it is not tuned to the only labeled dataset.
Sensitive columns: recall 1.000, class accuracy 1.000, precision 0.733 (15
flagged, 11 in the gold). The unit tests keep floors of F1 ≥ 0.59,
ARI ≥ 0.43 and sensitive recall = 1, and a gold-standard refiner reaches
F1 = ARI = 1, showing the refinement path can fully regroup tables. The gold
is human judgement; see [evals/README.md](evals/README.md).

## Read-only by design

Kabr will be pointed at production databases it does not own, so the M1
introspector is built to be boring and safe. The decisions are in
[ADR 0002](docs/adr/0002-catalog-only-read-only-access.md) and the reporting
process in [SECURITY.md](SECURITY.md).

- **Read-only session:** every connection sets
  `default_transaction_read_only = on` and a `statement_timeout`
  (`--statement-timeout`, default 15s).
- **Read-only transaction, always rolled back:** each step (privilege check,
  introspection) runs in a `REPEATABLE READ READ ONLY` transaction (one
  consistent catalog snapshot) that is rolled back, never committed.
- **Catalog only, never rows:** queries read `pg_catalog` only. A static test
  checks that every relation in every query is a system catalog, and an
  integration test runs the whole introspection as a role whose `SELECT` on
  every user table is revoked.
- **Statistics without values:** row counts come from planner estimates; from
  `pg_stats` only `n_distinct` is read. `most_common_vals` and histograms,
  which contain real data, are never touched.
- **Least-privilege check:** before introspecting, Kabr lists every privilege
  of the role beyond read-only catalog access (write and DDL grants,
  `SUPERUSER`, `BYPASSRLS`, `CREATEROLE`, ...), one warning line per finding
  with the SQL that fixes it. `--strict` turns the warning into a refusal.
- **DSN redaction:** passwords are scrubbed from every error and message,
  for URL and keyword/value DSNs alike, and the DSN is never written to disk.
- **No committed credentials:** no credentials are committed, not even demo
  ones. The demo generates random passwords into a gitignored file, and the
  tests generate theirs at run time.

What the warning looks like with the demo's over-privileged role (89
findings, abridged):

```text
kabr: warning: role "kabr_unsafe" holds 89 privilege(s) beyond read-only catalog access:
  - schema public: CREATE  (fix: REVOKE CREATE ON SCHEMA public FROM kabr_unsafe;)
  - table public.actor: DELETE  (fix: REVOKE DELETE ON TABLE public.actor FROM kabr_unsafe;)
  - table public.actor: INSERT  (fix: REVOKE INSERT ON TABLE public.actor FROM kabr_unsafe;)
  ...
kabr: hint: to revoke every table-level write privilege at once:
  REVOKE INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER ON ALL TABLES IN SCHEMA public FROM kabr_unsafe;
kabr: hint: Kabr only reads the system catalog. A role with LOGIN, CONNECT on
  the database and USAGE on the schemas is enough. ...
kabr: --strict: refusing to introspect as role "kabr_unsafe" (89 privilege(s) beyond read-only)
```

## How Kabr relates to existing tools

- **[PostgREST](https://postgrest.org) and [Hasura](https://hasura.io)**
  turn a database into a REST or GraphQL API, which is excellent when you want
  the database's schema as your API. Kabr instead splits the schema into
  domains first and exposes one narrow API per domain, aimed at gradual
  decomposition and least-privilege access.
- **[MCP Toolbox for Databases](https://github.com/googleapis/genai-toolbox)**
  gives agents database access through tools that you define in
  configuration. Kabr aims to derive the per-domain tool surface from the
  schema, with human approval of the plan, and to generate the matching REST
  API and Kubernetes plumbing alongside it.

## Roadmap

| Milestone | Scope | Status |
|-----------|-------|--------|
| **M1** | Read-only PostgreSQL introspection, canonical catalog, FK graph, Mermaid/DOT export, `kabr introspect`, least-privilege check | ✅ Done |
| **M2** | Deterministic community detection + optional LLM refinement, sensitive-column classification, `DomainPlan` with human approval, `kabr plan`, Pagila eval gate | ✅ Done |
| **M3** | Operator (kubebuilder), `DomainAPI` CRD, generic REST/OpenAPI runtime with masking, row limits and allowlists | 🗺️ Planned |
| **M4** | MCP server per domain, OpenTelemetry audit, quotas | 🗺️ Planned |
| **M5** | Segmentation evals in CI (Pagila, Chinook, Northwind, AdventureWorks) | 🗺️ Planned |
| **M6** | End-to-end demo on kind, docs and polish | 🗺️ Planned |

## Project layout

```text
cmd/kabr/            CLI entry point (signals, exit code)
internal/cli/        kabr commands on the standard library flag package
pkg/schema/          canonical, versioned catalog model (JSON)
pkg/graph/           table graph, metrics, Mermaid and DOT exporters
pkg/introspect/      dialect-agnostic introspection API, DSN redaction
pkg/introspect/postgres/   PostgreSQL introspector (pgx) and privilege check
pkg/segment/         deterministic Louvain segmentation of the table graph
pkg/planner/         baseline DomainPlan, sensitive-column heuristic, refinement
pkg/llm/             LLM refiners (Anthropic, Ollama), metadata-only prompt
pkg/plan/            DomainPlan model, strict YAML, validation, approval
pkg/eval/            scoring against gold standards (F1, ARI, sensitive columns)
api/v1alpha1/        published DomainPlan JSON Schema
evals/               human gold standards (Pagila)
deploy/compose/      demo PostgreSQL + Pagila with read-only and unsafe roles
examples/            committed output of make introspect-demo and make plan-demo
docs/                architecture and ADRs
```

## Development

```sh
make help               # list all targets
make test               # unit tests, race detector
make test-integration   # + integration tests (needs Docker; testcontainers starts PostgreSQL 17)
make test-demo          # integration test against the running demo (after make demo-up)
make lint               # go vet, gofmt, golangci-lint (pinned)
make vuln               # govulncheck (pinned)
make build              # bin/kabr, version from git describe
```

Linters and scanners run through `go run module@version`, pinned to the same
versions as CI, so nothing has to be installed globally. CI runs lint, unit
and integration tests and `govulncheck` on every push and pull request.

Design decisions are recorded as ADRs:
[0001 Go, single module](docs/adr/0001-go-single-module.md),
[0002 catalog-only read-only access](docs/adr/0002-catalog-only-read-only-access.md),
[0003 CLI on the standard library](docs/adr/0003-cli-framework.md),
[0004 segmentation: baseline, LLM refinement, approval](docs/adr/0004-segmentation-baseline-llm-approval.md),
[0005 LLM providers and the metadata-only invariant](docs/adr/0005-llm-providers-metadata-only.md).

## Origin

Kabr evolved from my undergraduate thesis, a manual prototype of per-table
microservices on Kubernetes. Kabr automates and governs that idea: the
decomposition is derived from the schema instead of written by hand, and
every generated API is read-only, scoped and audited.

## License

[MIT](LICENSE). The Pagila sample database used by the demo is downloaded at
run time and distributed under the PostgreSQL License; see
[deploy/compose/README.md](deploy/compose/README.md).
