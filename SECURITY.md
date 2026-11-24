# Security Policy

## Scope

Kabr is pre-1.0 and under active development. Security reports are welcome for
everything in this repository: the `kabr` CLI, the Go packages under `pkg/`,
the CI configuration, and (as they land) the Kubernetes operator and the
generated domain APIs.

The demo database under `deploy/compose/` is a local throwaway bound to
`127.0.0.1` (random passwords generated into a gitignored file, never
committed). Issues that only affect that local demo are out of scope unless
they leak into the CLI or library defaults.

Only the latest commit on `main` is supported. There are no maintained release
branches yet.

## Reporting a vulnerability

**Do not open a public issue for security problems.**

Report privately through GitHub's
[private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability):
open the repository's **Security** tab and choose **Report a vulnerability**.
This creates a draft Security Advisory visible only to you and the maintainer.

Please include the affected version or commit, a description of the impact,
and steps to reproduce. Expect an acknowledgement within 7 days. Fixes are
coordinated through the advisory, and you will be credited unless you prefer
otherwise.

## Security principles

These are design commitments; a deviation from any of them is a bug worth
reporting.

- **Read-only by design.** Kabr connects with a read-only role, opens a
  read-only session and runs every query inside a `READ ONLY` transaction with
  a `statement_timeout`. A least-privilege check warns about, or refuses,
  credentials that can write or create objects. See
  [ADR 0002](docs/adr/0002-catalog-only-read-only-access.md).
- **Catalog only, never rows.** Introspection reads the system catalog
  (structure, constraints, comments) and aggregate planner statistics such as
  row estimates and `n_distinct`. It never selects table data and never reads
  `pg_stats.most_common_vals` or histograms, which can contain real values.
- **No stored credentials.** The CLI does not write the DSN or password to
  disk, to its output or to any cache. Credentials come from the environment
  or flags at run time; in Kubernetes they will come from External Secrets.
- **Redacted DSNs.** Any DSN that appears in logs, errors or output has its
  password removed.
- **Pinned supply chain.** CI actions and Go tools are pinned to explicit
  versions, workflows run with `contents: read`, and `govulncheck` runs on
  every change. In M2 the Go toolchain was raised to
  go1.26.11 (GO-2026-6859, `net/http`) and the indirect dependency
  `go.opentelemetry.io/otel` to v1.47.0 (GO-2026-6974); `govulncheck`
  reports no findings.
- **Nothing is generated without approval.** Segmentation produces a
  `DomainPlan` in phase `Proposed`; only a human running
  `kabr plan approve --by NAME` makes it `Approved`, and the operator (M3)
  will act on approved plans only.

## What is sent to an LLM

Segmentation (`kabr plan generate`) can optionally ask a language model to
refine the deterministic baseline. Design:
[ADR 0005](docs/adr/0005-llm-providers-metadata-only.md).

**Sent** (catalog metadata only, from an explicit allowlist):

- table IDs (`schema.table`) and table comments;
- column name, data type, nullability and comment;
- primary key columns; foreign key columns with the referenced table and
  columns; unique constraint columns;
- the baseline plan: domain names and, per table, its ID, role and shared
  flag.

**Never sent:**

- row data (Kabr never reads rows);
- column default expressions (they may embed literal values);
- planner statistics (row estimates, `n_distinct`);
- index definitions and constraint names;
- the database name, the DSN or any credential;
- the baseline's rationales.

The prompt is built from a dedicated metadata-only structure, never by
serializing the catalog model, and the system prompt contains no catalog
data. A test walks every JSON key of the prompt against the allowlist, and
sentinel tests plant marker values in every forbidden field and assert they
never reach the request. A field added to the catalog model is therefore not
sent unless the allowlist is changed deliberately.

**Caveat: comments are sent verbatim.** Table and column comments are
DBA-written free text and may contain anything. A control to omit them is
future work. If you consider schema names or comments themselves sensitive,
do not use a hosted provider.

**Staying fully local.**

- `--llm none`: graph-only baseline, no network call at all. This is also
  what the CLI uses when no Anthropic credentials are present (with a notice
  on stderr).
- `--llm ollama`: a local model through Ollama
  (`KABR_OLLAMA_URL`, default `http://127.0.0.1:11434`).
- `--llm anthropic` sends the metadata above to the Anthropic API.
  Credentials are resolved by the official SDK: `ANTHROPIC_API_KEY`,
  `ANTHROPIC_AUTH_TOKEN`, an `ant auth login` profile, or workload identity
  federation environment variables. Kabr never stores or logs them. (Without
  `--llm`, the CLI picks `anthropic` only when `ANTHROPIC_API_KEY` or
  `ANTHROPIC_AUTH_TOKEN` is set.)
- `ANTHROPIC_BASE_URL`, read by the SDK, redirects where that metadata is
  sent. Whoever runs Kabr must trust whatever endpoint it points to (a
  proxy or gateway sees the same metadata the Anthropic API would).

**Model output is untrusted.** Answers are constrained with a JSON Schema
and then decoded strictly (exact keys, enum values). Every proposed rename,
table move and classification is validated against the catalog by
`plan.Apply`; invalid items are skipped with a warning, and if the result
does not validate, or the model refuses or is truncated, the deterministic
baseline is used instead. The output is always a `Proposed` plan that a
human must approve before anything is generated from it.

A prompt that carries a field from the "never sent" list, or a model answer
that bypasses validation, is a security bug; please report it as described
above.
