package planner

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Gabrielmmorais/kabr/pkg/plan"
	"github.com/Gabrielmmorais/kabr/pkg/schema"
	"github.com/Gabrielmmorais/kabr/pkg/segment"
)

// fakeRefiner returns a fixed refinement (or error).
type fakeRefiner struct {
	ref    plan.Refinement
	err    error
	mutate bool // scribble on the baseline it receives
	calls  int
}

func (f *fakeRefiner) Refine(ctx context.Context, _ *schema.Catalog, b *plan.DomainPlan) (plan.Refinement, error) {
	f.calls++
	if f.mutate {
		b.Spec.Domains = nil
		b.Metadata.Name = "scribbled"
	}
	if err := ctx.Err(); err != nil {
		return plan.Refinement{}, err
	}
	return f.ref, f.err
}
func (*fakeRefiner) Provider() string { return "fake" }
func (*fakeRefiner) Model() string    { return "fake-1" }

func sensitiveOf(p *plan.DomainPlan) map[string]plan.SensitiveColumn {
	out := map[string]plan.SensitiveColumn{}
	for _, d := range p.Spec.Domains {
		for _, sc := range d.SensitiveColumns {
			out[sc.Table+"."+sc.Column] = sc
		}
	}
	return out
}

func TestGenerateGraphOnly(t *testing.T) {
	c := loadPagila(t)
	res, err := Generate(context.Background(), c, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Refined || len(res.Warnings) != 0 || res.Plan.Status.Generator.Mode != plan.ModeGraph {
		t.Errorf("unexpected result: refined=%v warnings=%v gen=%+v", res.Refined, res.Warnings, res.Plan.Status.Generator)
	}
	if res.Plan == res.Baseline {
		t.Error("Plan and Baseline must be distinct copies")
	}
	if verr := res.Plan.Validate(c); verr != nil {
		t.Fatal(verr)
	}
	named, err := Generate(context.Background(), c, Options{Name: "custom.name"})
	if err != nil || named.Plan.Metadata.Name != "custom.name" {
		t.Fatalf("named: %v %+v", err, named)
	}
}

func TestGenerateRefinedSuccess(t *testing.T) {
	c := loadPagila(t)
	f := &fakeRefiner{mutate: true, ref: plan.Refinement{
		Renames: []plan.DomainRename{
			{From: "film", To: "catalog", Description: "Films and their metadata.", Rationale: "film catalog"},
			{From: "store", To: "customers"},
		},
		Moves: []plan.TableMove{
			{Table: "public.inventory", To: "rentals", Rationale: "per-store stock"},
		},
		SensitiveColumns: []plan.SensitiveColumn{
			{Table: "public.actor", Column: "first_name", Class: plan.ClassOther, Action: plan.ActionMask, Source: plan.SourceLLM, Rationale: "public figures"},
		},
	}}
	res, err := Generate(context.Background(), c, Options{Refiner: f})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Refined || len(res.Warnings) != 0 {
		t.Fatalf("refined=%v warnings=%v", res.Refined, res.Warnings)
	}
	g := res.Plan.Status.Generator
	if g.Mode != plan.ModeLLM || g.Provider != "fake" || g.Model != "fake-1" || g.Modularity != res.Baseline.Status.Generator.Modularity {
		t.Errorf("generator = %+v", g)
	}
	if res.Plan.DomainOf("public.inventory") != "rentals" || res.Plan.DomainOf("public.film") != "catalog" || res.Plan.DomainOf("public.customer") != "customers" {
		t.Errorf("unexpected grouping: inventory=%q film=%q customer=%q",
			res.Plan.DomainOf("public.inventory"), res.Plan.DomainOf("public.film"), res.Plan.DomainOf("public.customer"))
	}
	// The baseline handed to the refiner was a copy.
	if res.Baseline.Metadata.Name != "pagila" || res.Baseline.Domain("film") == nil {
		t.Error("refiner mutated the baseline")
	}
	base, refined := sensitiveOf(res.Baseline), sensitiveOf(res.Plan)
	for k, sc := range base {
		got, ok := refined[k]
		if !ok {
			t.Errorf("heuristic sensitive column %s lost by refinement", k)
			continue
		}
		if k == "public.actor.first_name" {
			if got.Class != plan.ClassOther || got.Source != plan.SourceLLM {
				t.Errorf("reclassification not applied: %+v", got)
			}
			continue
		}
		if got != sc {
			t.Errorf("%s changed: %+v -> %+v", k, sc, got)
		}
	}
	if err := res.Plan.Validate(c); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateFallbacks(t *testing.T) {
	c := loadPagila(t)
	tests := []struct {
		name     string
		refiner  *fakeRefiner
		apply    func(*plan.DomainPlan, plan.Refinement, *schema.Catalog) (*plan.DomainPlan, []string, error)
		refined  bool
		warnings []string // substrings, one per expected warning, in sorted order
		wantErr  string
	}{
		{
			name:     "refiner error",
			refiner:  &fakeRefiner{err: errors.New("boom")},
			warnings: []string{"llm refinement failed (fake): boom; using graph baseline"},
		},
		{
			name:    "apply rejects invalid plan",
			refiner: &fakeRefiner{},
			apply: func(*plan.DomainPlan, plan.Refinement, *schema.Catalog) (*plan.DomainPlan, []string, error) {
				return nil, []string{"moves[0]: skipped: x"}, errors.New("plan: refined plan is invalid: spec.domains: empty")
			},
			warnings: []string{"llm refinement rejected (fake): plan: refined plan is invalid", "llm refinement: moves[0]: skipped: x"},
		},
		{
			name:    "apply returns nothing",
			refiner: &fakeRefiner{},
			apply: func(*plan.DomainPlan, plan.Refinement, *schema.Catalog) (*plan.DomainPlan, []string, error) {
				return nil, nil, nil
			},
			warnings: []string{"llm refinement rejected (fake): no plan returned"},
		},
		{
			name:    "apply returns an invalid plan without error",
			refiner: &fakeRefiner{},
			apply: func(p *plan.DomainPlan, _ plan.Refinement, _ *schema.Catalog) (*plan.DomainPlan, []string, error) {
				bad := p.Clone()
				bad.Spec.Domains = bad.Spec.Domains[:1]
				return bad, nil, nil
			},
			wantErr: "refusing to emit an invalid plan",
		},
		{
			name: "partially invalid items",
			refiner: &fakeRefiner{ref: plan.Refinement{
				Renames: []plan.DomainRename{{From: "nope", To: "x"}, {From: "film", To: "Bad Name"}, {From: "city", To: "geography"}},
				Moves:   []plan.TableMove{{Table: "public.ghost", To: "film"}},
				SensitiveColumns: []plan.SensitiveColumn{
					{Table: "public.film", Column: "no_such", Class: plan.ClassPII, Action: plan.ActionMask},
				},
			}},
			refined: true,
			warnings: []string{
				`llm refinement: moves[0]: skipped: unknown table "public.ghost"`,
				`llm refinement: renames[0]: skipped: unknown domain "nope"`,
				`llm refinement: renames[1]: skipped: "Bad Name" is not a valid DNS-1123 label`,
				`llm refinement: sensitiveColumns[0]: skipped: table "public.film" has no column "no_such"`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.apply != nil {
				orig := applyRefinement
				applyRefinement = tt.apply
				t.Cleanup(func() { applyRefinement = orig })
			}
			res, err := Generate(context.Background(), c, Options{Refiner: tt.refiner})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				if res != nil {
					t.Error("result must be nil on error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.Refined != tt.refined {
				t.Errorf("refined = %v, want %v", res.Refined, tt.refined)
			}
			if len(res.Warnings) != len(tt.warnings) {
				t.Fatalf("warnings = %q, want %d", res.Warnings, len(tt.warnings))
			}
			for i, w := range tt.warnings {
				if !strings.Contains(res.Warnings[i], w) {
					t.Errorf("warnings[%d] = %q, want containing %q", i, res.Warnings[i], w)
				}
			}
			if !slices.Equal(res.Plan.Status.Warnings, res.Warnings) || !slices.IsSorted(res.Warnings) {
				t.Errorf("status warnings %q != result warnings %q (or unsorted)", res.Plan.Status.Warnings, res.Warnings)
			}
			wantMode := plan.ModeGraph
			if tt.refined {
				wantMode = plan.ModeLLM
				if res.Plan.Domain("geography") == nil {
					t.Error("valid rename not applied")
				}
			}
			if res.Plan.Status.Generator.Mode != wantMode {
				t.Errorf("mode = %q, want %q", res.Plan.Status.Generator.Mode, wantMode)
			}
			if len(res.Baseline.Status.Warnings) != 0 {
				t.Error("baseline must stay free of warnings")
			}
			if err := res.Plan.Validate(c); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGenerateContextCanceled(t *testing.T) {
	c := loadPagila(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fakeRefiner{}
	if _, err := Generate(ctx, c, Options{Refiner: f}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if f.calls != 0 {
		t.Error("refiner called with a canceled context")
	}

	// Canceled while the refiner runs: an error, not a silent fallback.
	ctx2, cancel2 := context.WithCancel(context.Background())
	r := refinerFunc(func(context.Context) (plan.Refinement, error) {
		cancel2()
		return plan.Refinement{}, errors.New("request aborted")
	})
	if _, err := Generate(ctx2, c, Options{Refiner: r}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

type refinerFunc func(context.Context) (plan.Refinement, error)

func (f refinerFunc) Refine(ctx context.Context, _ *schema.Catalog, _ *plan.DomainPlan) (plan.Refinement, error) {
	return f(ctx)
}
func (refinerFunc) Provider() string { return "func" }
func (refinerFunc) Model() string    { return "m" }

func TestGenerateErrors(t *testing.T) {
	//nolint:staticcheck // SA1012: nil context is deliberately tolerated.
	if _, err := Generate(nil, nil, Options{}); err == nil || !strings.Contains(err.Error(), "catalog is nil") {
		t.Errorf("nil catalog: err = %v", err)
	}
	if _, err := Generate(context.Background(), schema.New(schema.DialectPostgres, "x"), Options{}); err == nil {
		t.Error("empty catalog: want error")
	}
	if _, err := Generate(context.Background(), loadPagila(t), Options{Name: "Not Valid"}); err == nil {
		t.Error("invalid name: want error")
	}
}

func TestEvalAdapters(t *testing.T) {
	if len(EvalAssignment(nil)) != 0 || EvalSensitive(nil) != nil {
		t.Error("nil plan must yield empty results")
	}
	p, err := Baseline(loadPagila(t), "", segment.Options{})
	if err != nil {
		t.Fatal(err)
	}
	a := EvalAssignment(p)
	if len(a) != 15 || a["public.film"] != "film" {
		t.Errorf("assignment = %v", a)
	}
	if got := len(EvalSensitive(p)); got != 15 {
		t.Errorf("sensitive = %d, want 15", got)
	}
}
