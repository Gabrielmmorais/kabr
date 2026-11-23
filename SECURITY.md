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
  every change.
