package planner

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Gabrielmmorais/kabr/pkg/eval"
	"github.com/Gabrielmmorais/kabr/pkg/plan"
	"github.com/Gabrielmmorais/kabr/pkg/schema"
	"github.com/Gabrielmmorais/kabr/pkg/segment"
)

// Regression floors for the graph-only planner on Pagila (default segment
// options, γ = 1). Measured 2026-11-23: partition F1 = 0.6129, ARI = 0.4592
// (γ = 1.5, logged only: F1 = 0.7547, ARI = 0.6723).
// Floors are the measured values floored to 2 decimals minus 0.02.
const (
	pagilaMinF1  = 0.59
	pagilaMinARI = 0.43
)

func loadPagilaGold(t *testing.T) *eval.Gold {
	t.Helper()
	g, err := eval.LoadGold(filepath.Join("..", "..", "evals", "pagila", "gold.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func evaluate(t *testing.T, g *eval.Gold, p *plan.DomainPlan) eval.Report {
	t.Helper()
	r := eval.Evaluate(g, EvalAssignment(p), EvalSensitive(p))
	t.Log(r.String())
	// Extra or missing tables do not lower F1/ARI: check them explicitly.
	if len(r.Partition.MissingInPrediction) != 0 || len(r.Partition.ExtraInPrediction) != 0 {
		t.Errorf("missing in prediction %v, extra in prediction %v",
			r.Partition.MissingInPrediction, r.Partition.ExtraInPrediction)
	}
	return r
}

func TestPagilaEval(t *testing.T) {
	c, g := loadPagila(t), loadPagilaGold(t)
	res, err := Generate(context.Background(), c, Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := evaluate(t, g, res.Plan)
	if r.Sensitive.Recall != 1 || r.Sensitive.ClassAccuracy != 1 {
		t.Errorf("sensitive recall %.3f, class accuracy %.3f; want 1, 1", r.Sensitive.Recall, r.Sensitive.ClassAccuracy)
	}
	if r.Partition.F1 < pagilaMinF1 {
		t.Errorf("partition F1 %.4f below floor %.2f", r.Partition.F1, pagilaMinF1)
	}
	if r.Partition.ARI < pagilaMinARI {
		t.Errorf("partition ARI %.4f below floor %.2f", r.Partition.ARI, pagilaMinARI)
	}
	t.Logf("γ=1: F1=%.4f ARI=%.4f", r.Partition.F1, r.Partition.ARI)

	// Informational only (documented, not asserted): a higher resolution.
	opts := segment.DefaultOptions()
	opts.Resolution = 1.5
	hi, err := Generate(context.Background(), c, Options{Segment: opts})
	if err != nil {
		t.Fatal(err)
	}
	rh := eval.Evaluate(g, EvalAssignment(hi.Plan), EvalSensitive(hi.Plan))
	t.Logf("γ=1.5: F1=%.4f ARI=%.4f domains=%v", rh.Partition.F1, rh.Partition.ARI, domainTables(hi.Plan))
}

// goldRefiner moves every table to its gold domain, renaming the domain that
// owns public.film to "catalog" first.
type goldRefiner struct{ g *eval.Gold }

func (r goldRefiner) Refine(_ context.Context, _ *schema.Catalog, b *plan.DomainPlan) (plan.Refinement, error) {
	var ref plan.Refinement
	if from := b.DomainOf("public.film"); from != "catalog" {
		ref.Renames = append(ref.Renames, plan.DomainRename{From: from, To: "catalog", Description: "What can be rented."})
	}
	gold := r.g.Assignment()
	tables := make([]string, 0, len(gold))
	for tbl := range gold {
		tables = append(tables, tbl)
	}
	slices.Sort(tables)
	for _, tbl := range tables {
		ref.Moves = append(ref.Moves, plan.TableMove{Table: tbl, To: gold[tbl], Rationale: "gold grouping"})
	}
	return ref, nil
}
func (goldRefiner) Provider() string { return "gold" }
func (goldRefiner) Model() string    { return "human" }

func TestPagilaEvalPerfectRefiner(t *testing.T) {
	c, g := loadPagila(t), loadPagilaGold(t)
	res, err := Generate(context.Background(), c, Options{Refiner: goldRefiner{g}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Refined {
		t.Fatalf("not refined: %v", res.Warnings)
	}
	r := evaluate(t, g, res.Plan)
	if r.Partition.F1 != 1 || r.Partition.ARI != 1 {
		t.Errorf("perfect refiner: F1 %.4f ARI %.4f, want 1, 1", r.Partition.F1, r.Partition.ARI)
	}
	if r.Sensitive.Recall != 1 || r.Sensitive.ClassAccuracy != 1 {
		t.Errorf("sensitive columns lost in regrouping: %s", r.String())
	}
	var names []string
	for _, d := range res.Plan.Spec.Domains {
		names = append(names, d.Name)
	}
	slices.Sort(names)
	if want := []string{"catalog", "customers", "rentals", "stores"}; !slices.Equal(names, want) {
		t.Errorf("domains = %v, want %v", names, want)
	}
	t.Logf("warnings: %q", res.Warnings)
}

// domainTables renders "domain: tables" for logs.
func domainTables(p *plan.DomainPlan) []string {
	var out []string
	for _, d := range p.Spec.Domains {
		s := d.Name + ":"
		for _, ta := range d.Tables {
			s += " " + ta.Name
		}
		out = append(out, s)
	}
	return out
}
