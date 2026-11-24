// Package eval scores a predicted domain plan against a human gold standard.
//
// It is the seed of Kabr's evaluation gate: every dataset under evals/ holds a
// gold.yaml file (see [Gold] and [LoadGold]) describing how a human would
// group the tables of a catalog into business domains and which columns must
// be flagged as sensitive. A planner's output is reduced to an [Assignment]
// (table ID -> domain label) plus a list of [PredictedSensitive] columns, and
// [Evaluate] produces a [Report].
//
// Metrics:
//   - [ScorePartition] compares groupings by pairwise co-membership (precision,
//     recall, F1) and by the Adjusted Rand Index. Domain labels are arbitrary;
//     only which tables end up together matters.
//   - [ScoreSensitive] measures recall/precision of sensitive-column detection
//     over (table, column) keys and, among the columns found, how often the
//     predicted class matches the gold class.
//
// The gold standard is human judgement. A perfect score is not the goal;
// the harness exists to catch regressions between planner versions.
//
// The package deliberately does not depend on the planner packages: callers
// adapt their own output into an [Assignment] and []PredictedSensitive.
package eval
