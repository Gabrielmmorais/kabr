// Package planner is Kabr's Segment stage: it turns a catalog into a
// validated [plan.DomainPlan].
//
// The pipeline has two steps:
//
//  1. [Baseline] runs graph segmentation ([segment.Segment]) and maps every
//     community to a domain named after its anchor table (the orphan bucket
//     becomes "unassigned"). Sensitive columns come from [ClassifySensitive],
//     a deterministic, catalog-only heuristic over column-name tokens and data
//     types. The baseline is fully deterministic and always valid.
//  2. [Generate] optionally hands a copy of the baseline to a [Refiner]
//     (implemented by LLM providers) and applies the returned
//     [plan.Refinement] with [plan.Apply]. Any refiner or apply failure falls
//     back to the baseline with a warning; context cancellation is returned
//     as an error instead. The final plan is checked with
//     [plan.DomainPlan.Validate] before it is returned, so an invalid plan is
//     never emitted.
//
// [EvalAssignment] and [EvalSensitive] adapt a plan to the inputs of
// [eval.Evaluate], which the Pagila evaluation test uses as a regression gate.
//
// The package never reads table data: every decision is based on the catalog
// structure (tables, columns, data types, keys).
package planner
