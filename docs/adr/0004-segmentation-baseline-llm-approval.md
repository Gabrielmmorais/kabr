# ADR 0004: Segmentation: deterministic baseline, LLM refinement, human approval

- **Status:** Accepted
- **Date:** 2026-11-23

## Context

Milestone M2 turns a catalog (the output of M1) into a proposal of business
domains: which tables belong together, what each group is called, and which
columns hold sensitive data. That proposal later drives the M3 operator,
which will generate one read-only API per domain. A wrong grouping or a
missed sensitive column therefore becomes a wrong API surface.

Three properties matter:

1. **Explainable.** A reviewer must be able to see why a table landed in a
   domain, in terms of the schema.
2. **Reproducible and measurable.** The same catalog must give the same plan,
   so that changes to the segmenter can be scored against a gold standard
   (`evals/`) and regressions caught in CI.
3. **Good names and boundaries.** Graph structure alone cannot tell that
   `customer` and `rental` are different business concepts when they are
   tightly linked; it also cannot name a group meaningfully. Language models
   are good at exactly that, but their output is neither deterministic nor
   trustworthy by default.

## Decision

Segmentation is a three-step pipeline. Each step is a separate package with
its own tests.

1. **Deterministic baseline (`pkg/segment`, `pkg/planner`).**
   - The table graph has one unit of weight per foreign key, plus edges
     inferred from column names (`X_id` referencing table `X` when no
     foreign key covers it) at weight 0.5. Inferred edges cover catalogs
     whose foreign keys are missing or declared elsewhere; on Pagila they
     recover `payment.customer_id`, `payment.rental_id`, `payment.staff_id`
     (the real foreign keys live on the partitions) and
     `store.manager_staff_id`.
   - Tables get a structural role: **junction** (composite primary key made
     only of references to two or more tables), **lookup** (small table, no
     outgoing reference, two or more incoming), **orphan** (no edge) or
     **core**.
   - **Louvain** community detection runs on the core tables only, with
     resolution γ = 1.0 by default (standard modularity). Junctions are
     folded into edges between the tables they link and then placed with
     their owner; lookups are placed with the community holding most of
     their weight and marked shared when referenced from several; orphans
     share one `unassigned` domain.
   - The planner names each domain after its anchor table and classifies
     sensitive columns with a heuristic over column-name tokens and data
     types (`source: heuristic`). Every assignment carries a rationale
     naming the structural evidence.
   - No randomness: sorted visiting order, strict-improvement moves,
     deterministic tie-breaking. The same catalog content yields a
     byte-identical plan.
2. **Optional LLM refinement (`pkg/llm`).** A `Refiner` receives catalog
   metadata and the baseline and may propose only a `Refinement`: domain
   renames and descriptions, table moves between domains, and sensitive
   column classifications. `plan.Apply` validates every item against the
   catalog, skips invalid items with a warning, and returns an error if the
   refined plan does not validate. The fallback lives in `planner.Generate`:
   when the provider fails or `Apply` returns an error, it keeps the
   baseline and adds a warning. With `--llm none` no refiner runs and
   `Apply` is not called. The provider design and the privacy rules are in
   [ADR 0005](0005-llm-providers-metadata-only.md).
3. **Human approval (`pkg/plan`).** The result is a `DomainPlan` document
   (`kabr.dev/v1alpha1`) in phase `Proposed`. `kabr plan approve --by NAME`
   moves it to `Approved` only after re-validating it against a catalog,
   which is required (`--catalog`, `--dsn` or `$KABR_DSN`): structure,
   coverage of every catalog table and the catalog fingerprint. The M3 operator will act
   only on approved plans. Any change made through `plan.Apply` resets the
   approval.

A plan is pinned to the catalog it was generated for by a structural
fingerprint (sha256 over the schema structure, ignoring statistics and
comments). Validation rejects a plan whose fingerprint does not match, a
table missing from or duplicated across domains, a domain name that is not
a DNS-1123 label, and a sensitive column that does not exist or does not
belong to its domain. Plans are decoded strictly: exact-case keys, a single
YAML document, unknown fields rejected.

## Alternatives considered

- **LLM-only segmentation.** Send the catalog and ask for domains. Rejected:
  results vary between runs and model versions, so evals cannot separate a
  segmenter change from model noise; there is no structural explanation to
  review; and the system would have no useful output without a model,
  which rules out air-gapped use.
- **Pure rules, no LLM.** Keep only the baseline. Rejected as the only path,
  kept as the default fallback: it cannot name domains in business terms and
  on Pagila it merges what a human would separate (see Consequences). It
  remains fully supported with `--llm none`.
- **Leiden instead of Louvain.** Leiden guarantees well-connected
  communities and is the stronger algorithm on large graphs. Catalogs are
  small (tens to a few thousand tables), Louvain is short to implement
  without a dependency, and a badly connected community, its known failure
  mode, would be visible in review through the per-table rationales. Louvain was chosen for
  simplicity and determinism. Leiden remains a possible future replacement
  behind the same `segment.Options`.
- **Tuning γ per dataset.** Rejected as a default: γ = 1.0 optimizes the
  same modularity the tool reports and avoids fitting the defaults to the
  one dataset with a gold standard. γ is exposed as `--resolution`.

## Consequences

**Positive**

- Every plan is explainable (rationales per table and domain), reproducible
  (byte-identical baseline) and measurable against `evals/` gold standards
  with pairwise F1 and the Adjusted Rand Index.
- The tool is useful with no model at all, and the LLM can only improve on a
  valid baseline, never produce an unvalidated plan.
- The approval step keeps a human accountable for what the operator will
  expose.

**Negative / trade-offs**

- Baseline quality bounds the graph-only path. On Pagila at γ = 1.0 the
  baseline yields 3 domains (modularity Q = 0.4087) with pairwise F1 0.613
  and ARI 0.459 against the 4-domain gold: customers, rentals, payments and
  staff collapse into one `store` domain. At γ = 1.5 it yields 4 domains,
  F1 0.755, ARI 0.672. CI keeps floors of F1 ≥ 0.59 and ARI ≥ 0.43 at the
  default.
- The heuristic sensitive-column classifier favours recall over precision
  (on Pagila: recall 1.000, class accuracy 1.000, precision 0.733); extra
  flags are expected to be removed by a reviewer editing the plan before
  approval (a refinement can only add or replace sensitive columns, never
  remove them).
- γ is a real knob with no universally right value; operators may need to
  tune it per database.
- A gold-standard refiner reaches F1 = ARI = 1 in tests, which proves that
  the refinement path can fully regroup tables; it does not prove that a
  real model will.
