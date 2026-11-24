# ADR 0003: Standard library `flag` for the CLI

- **Status:** Accepted
- **Date:** 2026-11-23

## Context

Milestone M1 ships the `kabr` binary with three commands: `introspect`,
`version` and `help`. `introspect` has about ten flags (`--dsn`, `--schema`,
`--format`, `--out`, `--strict`, `--no-stats`, `--include-partitions`,
`--timestamp`, `--statement-timeout`). Later milestones add a few more
commands (segmentation in M2, plan approval helpers in M3), but most of the
product surface moves into the Kubernetes operator and its CRDs, not the CLI.

> **Note (2026-11-23, M2):** plan approval shipped earlier than expected, in
> M2, as `kabr plan approve` (one of the `kabr plan` subcommands, still on
> the standard library `flag` package). The decision below is unchanged.

Kabr is handed production database credentials. Every dependency compiled
into the binary is part of what an operator has to trust, and part of what
`govulncheck` and reviewers have to follow.

Options considered:

1. **Standard library `flag`** with a small hand-written subcommand switch.
2. **[spf13/cobra](https://github.com/spf13/cobra)** (usually with
   spf13/pflag, often viper): the de facto standard for large Go CLIs
   (kubectl, helm), with generated help, shell completion and nested
   commands.
3. **[urfave/cli](https://github.com/urfave/cli)**: a lighter framework with
   a declarative command tree, env-var binding and completion.

## Decision

Use the **standard library `flag` package**. `cmd/kabr/main.go` only wires
signals and `os.Exit`; the commands live in `internal/cli`, behind a single
`Run(ctx, args, env) int` function so exit codes, help output and DSN
redaction are covered by plain unit tests.

Conventions that compensate for what a framework would give:

- Every flag is accepted as `-flag` or `--flag` (the `flag` package parses
  both).
- Exit codes are explicit constants: 0 success, 1 error, 2 usage, 3
  `--strict` refusal.
- Help goes to stdout with exit 0 when asked for, usage errors go to stderr
  with exit 2.
- Configuration that is secret comes from the environment (`KABR_DSN`),
  documented in the help text.

## Consequences

**Positive**

- Zero new dependencies: the CLI adds nothing to `go.mod`, the supply chain
  or the `govulncheck` surface.
- The whole CLI behavior is readable in a few hundred lines with no
  framework conventions to learn.
- Small static binary and fast startup.

**Negative / trade-offs**

- No POSIX flag grouping (`-abc`), and flags must come before positional
  arguments (`flag` stops at the first non-flag).
- No generated shell completion or man pages.
- Help text and subcommand dispatch are hand-maintained.

**Revisit when** the CLI grows past roughly five or six commands or needs
nested command groups (likely candidates: M3 plan/approval helpers), or when
shell completion becomes a real user request. Cobra is the expected
replacement; because commands already sit behind `internal/cli`, a migration
would not touch `pkg/`.
