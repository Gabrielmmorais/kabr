# ADR 0007: Runtime query safety

- **Status:** Accepted
- **Date:** 2026-11-24

## Context

Up to M2, Kabr read only the system catalog
([ADR 0002](0002-catalog-only-read-only-access.md)). M3 adds `kabr serve`
(`pkg/serve`): a generic runtime that serves the rows of one domain of an
approved plan over HTTP. It is the first Kabr component that reads user
data, and it does so on behalf of callers that send arbitrary query strings.
The operator runs one such process per domain
([ADR 0006](0006-operator-and-crd-model.md)); it can also run on its own
with `kabr serve --plan FILE --domain NAME`.

The runtime has to stay safe even when the database role is more
privileged than it should be, when the schema changes under it, and when a
caller is hostile. In particular:

1. It must never write, lock for long, or run unbounded queries.
2. No request input may become SQL text.
3. Sensitive columns must not leak, neither directly nor through side
   channels such as filters.
4. Errors, readiness answers and logs must not disclose SQL, the DSN or
   data.
5. It must serve only the catalog that was approved.

## Decision

### Read-only by construction

- Every session is opened with `default_transaction_read_only=on`,
  `search_path=pg_catalog`, a session `statement_timeout`,
  `idle_in_transaction_session_timeout`, fixed `DateStyle`,
  `IntervalStyle` and `TimeZone=UTC`, and `application_name=kabr-serve`.
- Every request runs in its own `BEGIN READ ONLY` transaction with a
  transaction-local `statement_timeout` (set with
  `pg_catalog.set_config(..., true)`, the value a bind parameter), and the
  transaction is always rolled back, never committed.
- The connection pool is small and bounded (`--max-conns`, default 4).
- On every catalog check (at startup and on each recheck) the runtime
  also runs the M1 privilege check, and logs a warning the first time it
  finds the role holding more than read access; it does not refuse (every
  request is still read-only). The intended role
  has `SELECT` on the domain's tables and nothing else.

### No request input in SQL text

Statements are assembled in one file (`pkg/serve/sql.go`) from three kinds
of fragments only:

- fixed text (keywords, `pg_catalog`-qualified functions and casts, the
  `'***'` mask literal, `$n` placeholders);
- identifiers of allowlisted tables and columns, resolved from the live
  catalog and quoted with `pgx.Identifier.Sanitize`;
- placeholder numbers.

Path segments and query keys only *select* an allowlisted table or column
by exact name; they are never written into the SQL. Every value (filter,
key, cursor, limit) is a bind parameter. Catalog type names are used only to
classify a column against a fixed whitelist of built-in types; they never
reach the SQL either. With `search_path=pg_catalog` and every relation
schema-qualified, operators and functions resolve in `pg_catalog` only, so
objects created in user schemas cannot shadow them.

### Bounded reads: keyset pagination and row limits

- `GET /v1/{schema}/{table}` paginates on the primary key:
  `ORDER BY pk ASC` with a `pk > $n` (or row-comparison) predicate for the
  next page and `LIMIT rowLimit+1` (the extra row only signals that a next
  page exists; it is never returned). The cursor is an opaque base64url
  token holding the key values of the last row. There is no `OFFSET`.
- The row limit is 1..1000 (default 100). It comes from the DomainPlan or
  DomainAPI `rowLimit` (bounded by the CRD and re-checked by the runtime);
  `--row-limit` can only lower it. `?limit=` is clamped to it. A response
  never holds more rows than the limit.
- `GET /v1/{schema}/{table}/{pk}` reads one row by primary key, only for
  tables with a single-column key.
- Only `GET` and `HEAD` are accepted.

### Filters: equality only, allowlisted, no oracle

`?column=value` adds an equality condition; several are ANDed and each
parameter may appear once. A filter is accepted only on an allowlisted,
non-sensitive column of a filterable type (not `bytea`, `json`/`jsonb` or
arrays). An unknown column, a dropped column, a sensitive column and a
non-filterable column all get the same answer (400 `invalid_filter`, "does
not exist or is not filterable"), so filters cannot be used to probe
whether a hidden column exists or to test guesses against a masked value.
Values are parsed by the server according to the column type; an invalid
value is a 400, never a 500.

### Sensitive columns fail closed

Rules per `sensitiveColumns` entry of the domain:

| Plan entry | What the API returns |
|------------|----------------------|
| class `credential` (any action) | Column dropped: never selected, absent from responses and from the OpenAPI document. |
| action `drop` | Column dropped: never selected. |
| action `mask` | `"***"` (or `null` when the value is null), computed **in SQL** (`CASE WHEN c IS NULL THEN NULL ELSE '***' END`), so the raw value never leaves the database. |
| action `hash`, hash key configured | Lowercase hex HMAC-SHA256 of the value's text form, keyed with the hash key from a Secret (`--hash-key-file` / `KABR_HASH_KEY`, at least 16 bytes). The value is read as text and hashed in the process; it is never logged or returned. |
| action `hash`, no hash key | Column dropped, with a logged warning. |

Sensitive columns are never filterable. A table whose primary key column is
sensitive is not served at all, because the key path and the cursor would
expose it.

In Kubernetes the operator mounts the hash key Secret (and passes
`--hash-key-file`) only into domains that hash a non-credential column, and
only when the DataSource names a `hashKeySecretRef`, which is optional;
without one, hashed columns are dropped. In the Pagila example the only
`hash` column, `staff.username`, is of class `credential`, so it is dropped
and no pod mounts the key.

### Which tables are served

Only the domain's tables, and of each only the columns that survive the
sensitive-column rules. A configured table is left out, with a logged
warning, when it has no primary key (nothing to paginate on), when a
primary key column is sensitive, or when a key column's type cannot be
paginated (`bytea`, `json`/`jsonb`, arrays). A configured table or
sensitive column that does not exist in the catalog is a configuration
error (not ready). If no table remains, the domain is not ready.

### Errors and readiness never leak

- Errors are JSON `{"error": "<code>", "message": "..."}` with fixed
  messages per code (`not_found`, `invalid_filter`, `invalid_value`,
  `invalid_cursor`, `timeout`, `unavailable`, `internal`, ...). Database
  error text, SQL, the DSN and driver internals are never copied into an
  answer. A statement timeout is a 504, a bad bind value a 400, an
  unavailable database a 503.
- `/readyz` discloses only a coarse category: `starting`,
  `database_unavailable`, `catalog_drift`, `config_error`,
  `table_unreadable` or `internal_error`. The detailed reason
  (fingerprints, table and column names) is logged once per change.
- Logs are JSON lines on stderr. The DSN password is scrubbed from every
  logged error. Request logs record the path, status, duration, table,
  row count and the **names** of the filter columns, never filter values or
  the query string; the path of a by-key request is logged as
  `/v1/{schema}/{table}/{pk}`, so key values are not logged either.

### Serve only the approved catalog

At startup and then periodically (`--recheck-interval`), the runtime
introspects the catalog of every schema the plan covers with the M1
introspector (read-only, catalog-only) and computes `plan.Fingerprint`. The
served model (routes, columns, OpenAPI document) is built only from a
catalog whose fingerprint equals the approved `catalogFingerprint`. On
drift, on a database error, on a configuration mismatch or when a probe
query (zero rows, same projection as the data queries) fails, for example
for lack of `SELECT`, the process keeps running but `/readyz` and every data
endpoint answer 503. While not ready, the check is retried on a shorter
interval than the periodic recheck, so a domain recovers soon after the
cause is fixed. In Kubernetes, the readiness probe on `/readyz` keeps such a
pod out of the Service.

This is also what catches a DomainPlan whose `dataSourceRef` was pointed at
another database after approval (that field is outside the approval digest,
see [ADR 0006](0006-operator-and-crd-model.md)).

## Alternatives considered

- **Dynamic code generation per domain** (generate handlers or a binary
  from the plan). Rejected: a generic runtime configured by data is one
  artifact to review, test and ship, and a config change needs no build.
- **PostgREST or a similar generic API server behind the plan.** Rejected
  for M3: Kabr needs the plan-specific allowlist, masking in SQL, the
  fingerprint gate and the no-oracle filter rule in one place; layering them
  around a general-purpose query language would mean disabling most of it.
- **Offset pagination.** Rejected: cost grows with the offset, and results
  shift under concurrent writes. Keyset pagination on the primary key is
  bounded and stable.
- **Masking in the process** (read the raw value, replace it in Go).
  Rejected for `mask`: masking in SQL keeps the raw value inside the
  database. `hash` necessarily reads the value, because the HMAC key must
  not be sent to the database.
- **Plain SHA-256 for `hash`.** Rejected: unkeyed hashes of low-entropy
  values (emails, usernames) are reversible by dictionary. HMAC with a key
  held in a Secret is not, as long as the key stays secret.
- **Rich filters** (ranges, `LIKE`, `OR`). Deferred: each operator widens
  the inference surface and the cost model. M3 ships equality only.

## Consequences

**Positive**

- Safety does not depend on the role being configured perfectly: even a
  role with write privileges only ever runs `SELECT` in read-only,
  time-bounded, rolled-back transactions.
- A schema change that alters the approved catalog stops the data path
  until the plan is regenerated and re-approved.
- The rules are covered by unit tests over the SQL builder, the model and
  the handlers, by integration tests against real PostgreSQL, and by the
  kind e2e test in CI (masked and dropped columns on Pagila, fail closed on
  an edited DomainPlan, masking surviving an edited DomainAPI).

**Known limits**

- Keys and filters on non-built-in types (enums, domains and other types
  outside the whitelist) compare the column's text form, so pagination of a
  table keyed by such a type follows text order, not the type's own order.
- Tables with a composite primary key are list-only: there is no by-key
  endpoint; filter on the key columns instead.
- The plan does not record whether the catalog was introspected with
  `--include-partitions`. The runtime introspects with the default
  (partitions folded into their parent), so a plan generated from a catalog
  with partitions reports drift at runtime.
- The CRD does not enforce uniqueness of sensitive-column entries with CEL;
  a plan with duplicates is rejected by the operator and by the runtime,
  not by the API server.
- Table-ID parsing and validation exist in both `pkg/plan` and
  `pkg/serve`; they are meant to agree but are not shared code.
- Rows are read with the database role's own visibility; row-level
  filtering per consumer, quotas and per-query audit are M4.
