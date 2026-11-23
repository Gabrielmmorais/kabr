# ADR 0002: Catalog-only, read-only database access

- **Status:** Accepted
- **Date:** 2026-11-23

## Context

Kabr is pointed at production relational databases that it does not own. To
segment a schema into domains it needs structure: tables, columns, types,
keys, foreign keys, indexes, comments and a rough idea of size and
cardinality. It does not need the rows themselves, and reading them would turn
Kabr into a data-exfiltration risk: the catalog is later sent to an LLM (M2),
and the generated domain APIs are the governed path to data, not the
introspector.

Operators also hand Kabr a credential. A misconfigured, over-privileged
credential must not let Kabr change anything, even through a bug.

PostgreSQL exposes planner statistics in `pg_stats`. Some fields are pure
aggregates (`n_distinct`, `null_frac`, `avg_width`, `correlation`); others
contain **real column values**: `most_common_vals`, `most_common_elems` and
`histogram_bounds`. On a column holding emails or tax IDs, those fields are
samples of personal data.

## Decision

**Read the catalog, never the rows.**

- Introspection queries only system catalogs (`pg_catalog`, and
  `information_schema` where it is equivalent). It never issues a `SELECT`
  against a user table, not even `LIMIT 1` or `count(*)`.
- Row counts come from the planner estimate (`pg_class.reltuples`), not from
  scanning tables.
- From `pg_stats` Kabr reads **only `n_distinct`**. `most_common_vals`,
  `most_common_freqs`, `most_common_elems`, `histogram_bounds` and related
  value-bearing fields are explicitly excluded and have no place in
  `pkg/schema`. Adding one requires superseding this ADR.

**Read-only at three layers.**

1. **Session:** the connection sets `default_transaction_read_only = on`.
2. **Transaction:** every introspection runs inside
   `BEGIN TRANSACTION READ ONLY`, so even a buggy write fails in the database.
3. **Time bound:** the `statement_timeout` session parameter set at connect time
   (and a client-side context deadline) caps each query, so Kabr cannot hold locks or load a primary for
   long.

**Least-privilege check.** Before introspecting, Kabr inspects the role's
effective privileges (for example `INSERT`/`UPDATE`/`DELETE`/`TRUNCATE` on
tables, `CREATE` on schemas or the database, superuser or `BYPASSRLS`). A role
that can write gets a clear warning; a strict mode refuses to continue. The
demo database ships `kabr_reader` (correct) and `kabr_unsafe` (triggers the
warning) to exercise both paths.

**Out of scope for M1.** Views and materialized views are not modeled yet;
they are skipped rather than half-modeled and will get their own design when
segmentation needs them. Partitioned tables are handled; see the amendment
below.

**Amended 2026-11-23: partitioned tables.** Partitioned tables (`relkind = 'p'`)
are reported as ordinary tables. Partitions (`relispartition`) are omitted by
default and reported as ordinary tables with `--include-partitions`. The
internal per-partition foreign keys PostgreSQL creates when a constraint
references a partitioned table are filtered out, so each foreign key appears
once, pointing at the partitioned table.

## Consequences

**Positive**

- The worst case of a Kabr bug or a leaked Kabr output is disclosure of the
  schema, not of data.
- Data never reaches the LLM through the catalog; PII classification in M2 is
  based on names, types and comments, not on sampled values.
- Over-privileged credentials are visible to the operator instead of silently
  accepted.
- Low load on the target database: catalog queries are cheap and bounded.

**Negative / trade-offs**

- Without value samples, sensitive-column classification can miss columns
  with misleading names (for example `notes` holding personal data). Mitigated
  by human approval of the `DomainPlan`.
- Planner statistics can be stale or missing (never-analyzed tables);
  `estimatedRows` and `nDistinct` are therefore optional in the model.
- Column defaults and comments are catalog text and can, in rare cases,
  contain literal values written by a DBA. They are kept because they are
  schema metadata, but they are treated as potentially sensitive output.
- Schemas that rely heavily on views are only partially represented until a
  later milestone.
