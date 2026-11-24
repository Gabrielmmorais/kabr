# ADR 0005: LLM providers and the metadata-only invariant

- **Status:** Accepted
- **Date:** 2026-11-23

## Context

[ADR 0004](0004-segmentation-baseline-llm-approval.md) adds an optional LLM
refinement step on top of the deterministic baseline. That step sends
information about a production database to a model, possibly a hosted one,
and reads back text that will shape the generated APIs. Three concerns follow:

1. **Privacy.** [ADR 0002](0002-catalog-only-read-only-access.md) guarantees
   that Kabr never reads rows. The LLM step must not weaken that, and must
   also keep out catalog fields that can carry literal values or
   operational details.
2. **Trust.** Model output can be malformed, truncated, a refusal, or simply
   wrong. It must never produce an invalid or unreviewed plan.
3. **Choice of provider.** Some operators will use a hosted model, others
   require everything to stay on their machines, others want no model.

## Decision

**A provider-agnostic interface.** `planner.Refiner` takes the catalog and a
copy of the baseline plan and returns a `plan.Refinement`. `pkg/llm`
implements it for three providers:

| Provider | Transport | Defaults |
|----------|-----------|----------|
| `anthropic` | Official Go SDK, `github.com/anthropics/anthropic-sdk-go/v2` v2.14.0 | Model `claude-opus-5-5`, effort `medium`, `max_tokens` 16000, adaptive thinking left at the model default |
| `ollama` | Local server, `POST /api/chat` with a JSON-schema `format` | Model `llama3.1`, temperature 0, `KABR_OLLAMA_URL` (default `http://127.0.0.1:11434`) |
| `none` | No model | Graph-only baseline |

Credentials come from the SDK's own chain (`ANTHROPIC_API_KEY` or
`ANTHROPIC_AUTH_TOKEN`); Kabr never reads, stores or logs them.
`KABR_LLM_MODEL` overrides the default model. The CLI defaults to
`anthropic` when credentials are present and otherwise falls back to `none`
with a notice on stderr.

**Structured outputs.** Both providers constrain the answer with the same
JSON Schema (`output_config.format` of type `json_schema` for Anthropic,
`format` for Ollama), so the model is asked for a `Refinement` and nothing
else.

**No server-side fallbacks, no beta features.** One request goes to one
named model. Silent model substitution would make evaluation runs
incomparable; reproducibility against a single known model is worth more
than availability here, because the baseline is always available locally.

**Metadata-only invariant.** The prompt is built from an explicit
metadata-only structure, never by marshaling schema types directly. The
system prompt is a constant with no catalog interpolation.

| Sent | Never sent |
|------|------------|
| Table IDs (`schema.table`) and table comments | Row data (Kabr never reads rows) |
| Column name, data type, nullability, comment | Column default expressions (may embed literals) |
| Primary key columns | Planner statistics (row estimates, `n_distinct`) |
| Foreign key columns, referenced table and columns | Index definitions |
| Unique constraint columns | Constraint names |
| Baseline: domain names; per table its ID, role and shared flag | Database name, DSN, credentials, baseline rationales |

The invariant is enforced by tests: an allowlist test walks every JSON key
of the generated prompt and fails on any key not explicitly permitted, and
sentinel tests plant marker values in every forbidden field and assert they
never appear in the request.

**Model output is untrusted.** The response is decoded strictly (exact keys,
enum values checked). `plan.Apply` validates every proposed rename, move
and classification against the catalog, skips invalid items with a warning,
and returns the baseline with a warning if the overall result does not
validate. A refusal or a `max_tokens` stop reason is a provider error, which
also falls back to the baseline. Context cancellation is the only failure
that aborts the run. In every case the plan is `Proposed` and a human must
approve it before anything is generated.

## Alternatives considered

- **Send the full catalog JSON.** Simplest, but it would include defaults,
  statistics, index definitions and the database name, and any field added
  to `pkg/schema` later would leak automatically. Rejected in favour of an
  allowlist that fails closed.
- **Free-form text with prompt-only instructions.** Rejected: parsing prose is
  fragile, and structured outputs make malformed answers rare and strict
  decoding cheap.
- **A multi-provider abstraction library.** Rejected: two small providers do
  not justify the dependency surface (see the dependency reasoning in
  [ADR 0003](0003-cli-framework.md)).

## Consequences

**Positive**

- Fully local operation is a first-class path (`ollama` or `none`), not an
  afterthought.
- Adding a field to the catalog model cannot leak it to a model without a
  deliberate change to the allowlist and its test.
- A bad, truncated or refused answer degrades to the deterministic baseline
  instead of failing the run or emitting an invalid plan.
- Cost is bounded and predictable: one model request per generated plan.

**Negative / trade-offs**

- Table and column comments are DBA-written free text and are sent verbatim;
  they can contain anything a DBA chose to write. A `--no-comments`-style
  control is future work. Operators who consider schema names or comments
  themselves sensitive should use `ollama` or `none`.
- Schema identifiers (table and column names) do reach a hosted provider
  when `anthropic` is used.
- No automatic failover between providers or models: if the configured model
  is unavailable, the run produces the baseline plus a warning.
- Results from different models are not directly comparable; evaluations
  must record the provider and model (the plan's `status.generator` does).
