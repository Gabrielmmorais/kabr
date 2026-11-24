# Evals

This directory holds the evaluation datasets used to score Kabr's domain
planner. Each dataset is a human-written **gold standard** for one catalog;
the `pkg/eval` package compares a planner's output against it.

> The gold is human judgement, not ground truth. Reasonable engineers can
> disagree on where a table belongs, so a perfect score is **not** the goal.
> What matters is that scores do not regress between planner versions.
> This harness is the seed of a future CI eval gate.

## Layout

```
evals/
  <dataset>/
    gold.yaml      # gold standard (format below)
```

Current datasets: `pagila` (catalog `examples/pagila.schema.json`).

## Gold format

```yaml
name: pagila                              # required, unique dataset name
description: ...                          # optional
catalog: examples/pagila.schema.json      # repo-relative catalog path
domains:                                  # required, label -> table IDs
  catalog: [public.film, public.actor]
  rentals: [public.rental]
sensitive:                                # optional must-flag set
  - {table: public.staff, column: password, class: credential}
```

Rules enforced by `eval.LoadGold`:

- Decoding is strict: unknown fields and duplicate keys are errors.
- `name` is non-empty and there is at least one non-empty domain.
- Table IDs use the catalog form `schema.name`; a table may appear in only
  one domain.
- Sensitive entries reference a table listed in `domains`, use a class among
  `pii`, `credential`, `financial`, `other`, and are not duplicated.
- Domain labels are arbitrary: only which tables are grouped together is
  scored, so labels need not match the planner's.

Explain the reasoning behind non-obvious choices in YAML comments.

## Metrics

### Partition (domain grouping)

Computed over every pair of gold tables. A pair is *positive* when both
tables share a label.

| Metric    | Definition |
|-----------|------------|
| Precision | TP / (TP + FP): of the pairs the planner put together, how many belong together |
| Recall    | TP / (TP + FN): of the pairs that belong together, how many the planner kept together |
| F1        | Harmonic mean of precision and recall |
| ARI       | Adjusted Rand Index (Hubert & Arabie): agreement corrected for chance; 1 = identical, ~0 = random, may be negative |

Edge cases: when one side has no positive pair, precision/recall is 1 if the
other side has none either, else 0. Gold tables missing from the prediction
are scored as singletons and listed; predicted tables absent from the gold
are listed and ignored.

### Sensitive columns

Matched on `(table, column)`.

| Metric         | Definition |
|----------------|------------|
| Recall         | gold columns found / gold columns (primary signal) |
| Precision      | gold columns found / distinct predicted columns |
| Class accuracy | among found columns, fraction with the same class |

The gold lists columns that **must** be flagged. Flagging extra columns is
allowed; it only lowers precision. Missed columns are listed in the report.

## Adding a dataset

1. Commit the catalog JSON (for example `examples/<name>.schema.json`,
   produced by `kabr` introspection; catalog only, never row data).
2. Create `evals/<name>/gold.yaml` following the format above, pointing
   `catalog` at that file, and comment the reasoning.
3. Add a test in `pkg/eval` (mirroring the Pagila one) that loads the gold
   and checks every table and sensitive column exists in the catalog.

Milestone M5 will add Chinook, Northwind and AdventureWorks this way.
