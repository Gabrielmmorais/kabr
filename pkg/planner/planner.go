package planner

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/Gabrielmmorais/kabr/pkg/eval"
	"github.com/Gabrielmmorais/kabr/pkg/plan"
	"github.com/Gabrielmmorais/kabr/pkg/schema"
	"github.com/Gabrielmmorais/kabr/pkg/segment"
)

// Refiner proposes a [plan.Refinement] on top of a baseline plan. It is
// implemented by the LLM providers; the baseline it receives is a copy it
// may freely modify.
type Refiner interface {
	Refine(ctx context.Context, c *schema.Catalog, baseline *plan.DomainPlan) (plan.Refinement, error)
	Provider() string
	Model() string
}

// Options configures [Generate].
type Options struct {
	// Name is metadata.name. Empty means [DeriveName] of the catalog
	// database (a DNS-1123 subdomain), falling back to [DefaultName].
	Name string
	// Segment tunes the graph segmentation; the zero value means
	// [segment.DefaultOptions].
	Segment segment.Options
	// Refiner refines the baseline; nil means graph-only.
	Refiner Refiner
}

// Result is the outcome of [Generate].
type Result struct {
	// Plan is the final plan (refined, or the baseline); it always passes
	// [plan.DomainPlan.Validate] against the catalog.
	Plan *plan.DomainPlan
	// Baseline is the graph-only plan, before any refinement.
	Baseline *plan.DomainPlan
	// Refined reports whether the refinement was applied.
	Refined bool
	// Warnings are sorted and deduplicated; they are also copied into
	// Plan.Status.Warnings.
	Warnings []string
}

// applyRefinement is plan.Apply; a seam for tests.
var applyRefinement = plan.Apply

// Generate builds the plan for c: the [Baseline], then, when opts.Refiner is
// set, the refinement applied with [plan.Apply]. A refiner error or a
// refinement that yields an invalid plan falls back to the baseline with a
// warning; items Apply skipped become warnings of a refined plan. Heuristic
// sensitive columns survive refinement unless the refinement reclassifies
// the same column. A canceled context is returned as an error, never as a
// silent fallback. The returned plan always passes Validate(c).
func Generate(ctx context.Context, c *schema.Catalog, opts Options) (*Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("planner: %w", err)
	}
	base, err := Baseline(c, opts.Name, opts.Segment)
	if err != nil {
		return nil, err
	}
	res := &Result{Baseline: base}
	final := base.Clone()

	if r := opts.Refiner; r != nil {
		provider := r.Provider()
		ref, rerr := r.Refine(ctx, c, base.Clone())
		if cerr := ctx.Err(); cerr != nil {
			return nil, fmt.Errorf("planner: llm refinement (%s): %w", provider, cerr)
		}
		if rerr != nil {
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("llm refinement failed (%s): %v; using graph baseline", provider, rerr))
		} else {
			refined, applyWarnings, aerr := applyRefinement(base, ref, c)
			for _, w := range applyWarnings {
				res.Warnings = append(res.Warnings, "llm refinement: "+w)
			}
			if aerr != nil || refined == nil {
				if aerr == nil {
					aerr = errors.New("no plan returned")
				}
				res.Warnings = append(res.Warnings,
					fmt.Sprintf("llm refinement rejected (%s): %v; using graph baseline", provider, aerr))
			} else {
				final = refined
				final.Status.Generator = plan.Generator{
					Mode:       plan.ModeLLM,
					Provider:   provider,
					Model:      r.Model(),
					Modularity: base.Status.Generator.Modularity,
				}
				res.Refined = true
			}
		}
	}

	slices.Sort(res.Warnings)
	res.Warnings = slices.Compact(res.Warnings)
	final.Status.Warnings = slices.Clone(res.Warnings)
	if err := final.Validate(c); err != nil {
		return nil, fmt.Errorf("planner: refusing to emit an invalid plan: %w", err)
	}
	res.Plan = final
	return res, nil
}

// EvalAssignment maps every table of p to its domain name.
func EvalAssignment(p *plan.DomainPlan) eval.Assignment {
	out := eval.Assignment{}
	if p == nil {
		return out
	}
	for _, d := range p.Spec.Domains {
		for _, t := range d.Tables {
			out[t.Name] = d.Name
		}
	}
	return out
}

// EvalSensitive lists the sensitive columns of p, in plan order.
func EvalSensitive(p *plan.DomainPlan) []eval.PredictedSensitive {
	if p == nil {
		return nil
	}
	var out []eval.PredictedSensitive
	for _, d := range p.Spec.Domains {
		for _, sc := range d.SensitiveColumns {
			out = append(out, eval.PredictedSensitive{Table: sc.Table, Column: sc.Column, Class: string(sc.Class)})
		}
	}
	return out
}
