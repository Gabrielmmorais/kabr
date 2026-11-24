package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gabrielmmorais/kabr/pkg/introspect"
	"github.com/Gabrielmmorais/kabr/pkg/llm"
	"github.com/Gabrielmmorais/kabr/pkg/plan"
	"github.com/Gabrielmmorais/kabr/pkg/planner"
	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

var update = flag.Bool("update", false, "rewrite examples/pagila.plan.yaml")

var (
	pagilaCatalogPath = filepath.Join("..", "..", "examples", "pagila.schema.json")
	pagilaPlanPath    = filepath.Join("..", "..", "examples", "pagila.plan.yaml")
	pagilaGoldPath    = filepath.Join("..", "..", "evals", "pagila", "gold.yaml")
	fixedNow          = time.Date(2026, 1, 2, 3, 4, 5, 999, time.FixedZone("BRT", -3*3600))
)

// fakeRefiner never touches the network.
type fakeRefiner struct {
	provider, model string
	ref             plan.Refinement
	err             error
	calls           int
}

func (f *fakeRefiner) Refine(context.Context, *schema.Catalog, *plan.DomainPlan) (plan.Refinement, error) {
	f.calls++
	return f.ref, f.err
}
func (f *fakeRefiner) Provider() string { return f.provider }
func (f *fakeRefiner) Model() string    { return f.model }

// planEnv configures one invocation of runPlanCLI.
type planEnv struct {
	vars       map[string]string
	newRefiner RefinerFunc
	fake       *fakeIntrospector
}

func runPlanCLI(t *testing.T, pe planEnv, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	var r result
	nr := pe.newRefiner
	if nr == nil {
		nr = func(cfg llm.Config) (planner.Refiner, error) {
			t.Fatalf("unexpected refiner construction: %+v", cfg)
			return nil, nil
		}
	}
	r.code = Run(context.Background(), args, Env{
		Stdout:     &out,
		Stderr:     &errOut,
		Getenv:     func(k string) string { return pe.vars[k] },
		Version:    "v0.0.0-test",
		NewRefiner: nr,
		Now:        func() time.Time { return fixedNow },
		Open: func(_ context.Context, dsn string, cfg introspect.Config) (introspect.Introspector, error) {
			r.gotDSN, r.gotCfg = dsn, cfg
			if pe.fake == nil {
				t.Fatal("unexpected Open")
			}
			return pe.fake, nil
		},
	})
	r.stdout, r.stderr = out.String(), errOut.String()
	return r
}

func loadPagilaCatalog(t *testing.T) *schema.Catalog {
	t.Helper()
	data, err := os.ReadFile(pagilaCatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	c, err := schema.Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// writePlan writes p (after mutate) to a temp file and returns its path.
func writePlan(t *testing.T, mutate func(p *plan.DomainPlan)) string {
	t.Helper()
	p, err := plan.Unmarshal([]byte(readFile(t, pagilaPlanPath)))
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(p)
	}
	data, err := plan.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "plan.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPlanGenerateGolden(t *testing.T) {
	args := []string{"plan", "generate", "--catalog", pagilaCatalogPath, "--llm", "none"}
	r1 := runPlanCLI(t, planEnv{}, args...)
	r2 := runPlanCLI(t, planEnv{}, args...)
	if r1.code != ExitOK {
		t.Fatalf("exit %d, stderr:\n%s", r1.code, r1.stderr)
	}
	if r1.stdout != r2.stdout || r1.stderr != r2.stderr {
		t.Fatal("two runs differ")
	}
	if *update {
		if err := os.WriteFile(pagilaPlanPath, []byte(r1.stdout), 0o644); err != nil { //nolint:gosec // golden file
			t.Fatal(err)
		}
	}
	if want := readFile(t, pagilaPlanPath); r1.stdout != want {
		t.Errorf("output differs from %s (run with -update after checking the change)", pagilaPlanPath)
	}
	if strings.Contains(r1.stderr, "no LLM credentials") {
		t.Error("explicit --llm none must not print the notice")
	}
	wantSummary := `plan "pagila": 3 domains, 15 tables, 15 sensitive columns (generator: graph)` + "\n"
	if r1.stderr != wantSummary {
		t.Errorf("stderr = %q, want %q", r1.stderr, wantSummary)
	}

	// --out writes the same bytes atomically.
	out := filepath.Join(t.TempDir(), "p.yaml")
	r := runPlanCLI(t, planEnv{}, append(args, "--out", out)...)
	if r.code != ExitOK || r.stdout != "" || readFile(t, out) != r1.stdout {
		t.Errorf("--out: exit %d stdout %q", r.code, r.stdout)
	}
}

func TestPlanGenerateProviderSelection(t *testing.T) {
	tests := []struct {
		name       string
		vars       map[string]string
		args       []string
		wantNotice bool
		wantCfg    *llm.Config // nil: no refiner constructed
	}{
		{"no credentials", nil, nil, true, nil},
		{"api key", map[string]string{"ANTHROPIC_API_KEY": "x"}, nil, false,
			&llm.Config{Provider: "anthropic", Timeout: llm.DefaultTimeout}},
		{"auth token, model and effort", map[string]string{"ANTHROPIC_AUTH_TOKEN": "x"},
			[]string{"--model", "m1", "--effort", "HIGH", "--timeout", "30s"}, false,
			&llm.Config{Provider: "anthropic", Model: "m1", Effort: "high", Timeout: 30 * time.Second}},
		{"model from env", map[string]string{"ANTHROPIC_API_KEY": "x", llm.EnvModel: "env-model"}, nil, false,
			&llm.Config{Provider: "anthropic", Model: "env-model", Timeout: llm.DefaultTimeout}},
		{"explicit none with credentials", map[string]string{"ANTHROPIC_API_KEY": "x"}, []string{"--llm", "none"}, false, nil},
		{"explicit ollama", map[string]string{llm.EnvOllamaURL: "http://ollama.test:1"}, []string{"--llm", "ollama"}, false,
			&llm.Config{Provider: "ollama", BaseURL: "http://ollama.test:1", Timeout: llm.DefaultTimeout}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got *llm.Config
			fake := &fakeRefiner{}
			nr := func(cfg llm.Config) (planner.Refiner, error) {
				got = &cfg
				fake.provider, fake.model = cfg.Provider, cfg.Model
				if fake.model == "" {
					fake.model = "default-model"
				}
				return fake, nil
			}
			args := append([]string{"plan", "generate", "--catalog", pagilaCatalogPath}, tt.args...)
			r := runPlanCLI(t, planEnv{vars: tt.vars, newRefiner: nr}, args...)
			if r.code != ExitOK {
				t.Fatalf("exit %d, stderr:\n%s", r.code, r.stderr)
			}
			if has := strings.Contains(r.stderr, noCredentialsNotice+"\n"); has != tt.wantNotice {
				t.Errorf("notice printed = %v, want %v; stderr:\n%s", has, tt.wantNotice, r.stderr)
			}
			switch {
			case tt.wantCfg == nil && got != nil:
				t.Fatalf("refiner constructed with %+v", *got)
			case tt.wantCfg == nil:
				if !strings.Contains(r.stderr, "(generator: graph)") {
					t.Errorf("stderr:\n%s", r.stderr)
				}
			case got == nil:
				t.Fatal("refiner not constructed")
			default:
				if *got != *tt.wantCfg {
					t.Errorf("config = %+v, want %+v", *got, *tt.wantCfg)
				}
				if fake.calls != 1 {
					t.Errorf("Refine called %d times", fake.calls)
				}
				want := "(generator: llm/" + fake.provider + "/" + fake.model + ")"
				if !strings.Contains(r.stderr, want) {
					t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
				}
				p, err := plan.Unmarshal([]byte(r.stdout))
				if err != nil {
					t.Fatal(err)
				}
				if g := p.Status.Generator; g.Mode != plan.ModeLLM || g.Provider != fake.provider || g.Model != fake.model {
					t.Errorf("generator = %+v", g)
				}
			}
		})
	}
}

func TestPlanGenerateRefinerFailures(t *testing.T) {
	// A failing refinement falls back to the graph plan with a warning: exit 0.
	fake := &fakeRefiner{provider: "anthropic", model: "m", err: errors.New("boom")}
	r := runPlanCLI(t, planEnv{
		vars:       map[string]string{"ANTHROPIC_API_KEY": "x"},
		newRefiner: func(llm.Config) (planner.Refiner, error) { return fake, nil },
	}, "plan", "generate", "--catalog", pagilaCatalogPath)
	if r.code != ExitOK {
		t.Fatalf("exit %d, stderr:\n%s", r.code, r.stderr)
	}
	if !strings.Contains(r.stderr, "warning: llm refinement failed (anthropic): boom; using graph baseline\n") ||
		!strings.Contains(r.stderr, "(generator: graph)") {
		t.Errorf("stderr:\n%s", r.stderr)
	}
	p, err := plan.Unmarshal([]byte(r.stdout))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Status.Warnings) != 1 {
		t.Errorf("warnings = %v", p.Status.Warnings)
	}

	// A refiner that cannot be built is an error.
	r = runPlanCLI(t, planEnv{
		newRefiner: func(llm.Config) (planner.Refiner, error) { return nil, errors.New("bad config") },
	}, "plan", "generate", "--catalog", pagilaCatalogPath, "--llm", "anthropic")
	if r.code != ExitError || !strings.Contains(r.stderr, "kabr: llm: bad config") {
		t.Errorf("exit %d, stderr:\n%s", r.code, r.stderr)
	}
}

func TestPlanGenerateFromDSN(t *testing.T) {
	fake := &fakeIntrospector{
		report:  introspect.PrivilegeReport{Role: "kabr_reader", ReadOnly: true},
		catalog: loadPagilaCatalog(t),
	}
	r := runPlanCLI(t, planEnv{fake: fake, vars: map[string]string{DSNEnv: testDSN}},
		"plan", "generate", "--llm", "none", "--schema", "public")
	if r.code != ExitOK {
		t.Fatalf("exit %d, stderr:\n%s", r.code, r.stderr)
	}
	if r.gotDSN != testDSN || !fake.closed || !fake.opts.IncludeStats || fake.opts.Schemas[0] != "public" {
		t.Errorf("dsn/opts not passed through: closed=%v opts=%+v", fake.closed, fake.opts)
	}
	if r.stdout != readFile(t, pagilaPlanPath) {
		t.Error("plan from DSN differs from the plan from the catalog file")
	}

	// Strict refuses an over-privileged role; errors never leak the DSN.
	unsafe := &fakeIntrospector{
		report:  introspect.PrivilegeReport{Role: "kabr_unsafe", Superuser: true},
		catalog: loadPagilaCatalog(t),
	}
	r = runPlanCLI(t, planEnv{fake: unsafe}, "plan", "generate", "--llm", "none", "--dsn", testDSN, "--strict")
	if r.code != ExitStrict || unsafe.introspected {
		t.Errorf("strict: exit %d introspected %v", r.code, unsafe.introspected)
	}
	failing := &fakeIntrospector{
		report:        introspect.PrivilegeReport{Role: "kabr_reader", ReadOnly: true},
		introspectErr: errors.New("query failed for " + testDSN + " pw " + testPassword),
	}
	r = runPlanCLI(t, planEnv{fake: failing}, "plan", "generate", "--llm", "none", "--dsn", testDSN)
	if r.code != ExitError {
		t.Errorf("exit %d", r.code)
	}
	for _, res := range []result{r} {
		if strings.Contains(res.stderr, testPassword) || strings.Contains(res.stdout, testPassword) {
			t.Fatalf("password leaked:\n%s", res.stderr)
		}
	}
}

func TestPlanUsageErrors(t *testing.T) {
	planFile := writePlan(t, nil)
	tests := []struct {
		name string
		vars map[string]string
		args []string
		want string
	}{
		{"no subcommand", nil, []string{"plan"}, "missing subcommand"},
		{"unknown subcommand", nil, []string{"plan", "nope"}, `unknown subcommand "nope"`},
		{"generate no catalog", nil, []string{"plan", "generate"}, "no catalog"},
		{"generate both sources", nil, []string{"plan", "generate", "--catalog", "x", "--dsn", testDSN}, "exactly one of --catalog or --dsn"},
		{"generate bad llm", nil, []string{"plan", "generate", "--catalog", "x", "--llm", "gpt"}, `invalid --llm "gpt"`},
		{"generate bad effort", nil, []string{"plan", "generate", "--catalog", "x", "--effort", "huge"}, `invalid --effort "huge"`},
		{"generate zero resolution", nil, []string{"plan", "generate", "--catalog", "x", "--resolution", "0"}, "--resolution must be"},
		{"generate bad name", nil, []string{"plan", "generate", "--catalog", "x", "--name", "Bad_Name"}, "not a valid DNS-1123"},
		{"generate bad timeout", nil, []string{"plan", "generate", "--catalog", "x", "--timeout", "0s"}, "--timeout must be positive"},
		{"generate positional", nil, []string{"plan", "generate", "extra"}, `unexpected argument "extra"`},
		{"generate unknown flag", nil, []string{"plan", "generate", "--nope"}, "flag provided but not defined"},
		{"generate schema without dsn", nil, []string{"plan", "generate", "--schema", "public"}, "need a DSN"},
		{"show no file", nil, []string{"plan", "show"}, "missing FILE"},
		{"show two files", nil, []string{"plan", "show", planFile, planFile}, "one FILE only"},
		{"show bad format", nil, []string{"plan", "show", planFile, "--format", "xml"}, `invalid --format "xml"`},
		{"validate no file", nil, []string{"plan", "validate"}, "missing FILE"},
		{"validate both sources", nil, []string{"plan", "validate", planFile, "--catalog", "x", "--dsn", testDSN}, "exactly one"},
		{"approve without --by", nil, []string{"plan", "approve", planFile}, "--by is required"},
		{"approve blank --by", nil, []string{"plan", "approve", planFile, "--by", "  "}, "--by is required"},
		{"eval without --gold", nil, []string{"plan", "eval", planFile}, "--gold is required"},
		{"help unknown subcommand", nil, []string{"help", "plan", "nope"}, "unknown help topic"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runPlanCLI(t, planEnv{vars: tt.vars}, tt.args...)
			if r.code != ExitUsage {
				t.Fatalf("exit %d, want %d; stderr:\n%s", r.code, ExitUsage, r.stderr)
			}
			if !strings.Contains(r.stderr, tt.want) {
				t.Errorf("stderr lacks %q:\n%s", tt.want, r.stderr)
			}
			if strings.Contains(r.stderr, testPassword) {
				t.Fatal("password leaked")
			}
		})
	}
	if before := readFile(t, planFile); before != readFile(t, pagilaPlanPath) {
		t.Fatal("usage errors modified the plan")
	}
}

func TestPlanHelp(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"help", "plan"}, "Subcommands:"},
		{[]string{"plan", "--help"}, "Subcommands:"},
		{[]string{"help", "plan", "generate"}, "-resolution"},
		{[]string{"plan", "approve", "-h"}, "Usage: kabr plan approve"},
		{[]string{"help"}, "plan         Generate"},
	} {
		r := runPlanCLI(t, planEnv{}, tt.args...)
		if r.code != ExitOK || !strings.Contains(r.stdout, tt.want) {
			t.Errorf("%v: exit %d, stdout lacks %q:\n%s", tt.args, r.code, tt.want, r.stdout)
		}
	}
}

func TestPlanReadErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("apiVersion: kabr.dev/v1alpha1\nkind: DomainPlan\nbogus: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"plan", "generate", "--catalog", missing},
		{"plan", "generate", "--catalog", bad},
		{"plan", "show", missing},
		{"plan", "show", bad},
		{"plan", "validate", missing},
		{"plan", "approve", missing, "--by", "a"},
		{"plan", "eval", missing, "--gold", pagilaGoldPath},
		{"plan", "eval", pagilaPlanPath, "--gold", missing},
	} {
		r := runPlanCLI(t, planEnv{}, args...)
		if r.code != ExitError || !strings.HasPrefix(r.stderr, "kabr: ") {
			t.Errorf("%v: exit %d stderr %q", args, r.code, r.stderr)
		}
	}
}

func TestPlanShow(t *testing.T) {
	path := writePlan(t, func(p *plan.DomainPlan) {
		p.Spec.Domains[0].Tables[0].Shared = true
		p.Status.Warnings = []string{"something to review"}
	})
	r := runPlanCLI(t, planEnv{}, "plan", "show", path)
	if r.code != ExitOK {
		t.Fatalf("exit %d, stderr:\n%s", r.code, r.stderr)
	}
	for _, want := range []string{
		"Plan:        pagila\n",
		"Phase:       Proposed\n",
		"Approved by: -\n",
		"Generator:   graph\n",
		"Totals:      3 domains, 15 tables, 15 sensitive columns\n",
		"Warnings:\n  - something to review\n",
		"Domain film (7 tables, 2 sensitive columns)\n",
		"  public.city     core  shared\n",
		"  public.film_actor     junction\n",
		"    public.actor.first_name  pii -> mask  heuristic\n",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("table output lacks %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "\x1b") || strings.Contains(r.stdout, " \n") {
		t.Error("table output has ANSI escapes or trailing spaces")
	}
	if again := runPlanCLI(t, planEnv{}, "plan", "show", path); again.stdout != r.stdout {
		t.Error("show is not deterministic")
	}

	r = runPlanCLI(t, planEnv{}, "plan", "show", "--format", "yaml", pagilaPlanPath)
	if r.code != ExitOK || r.stdout != readFile(t, pagilaPlanPath) {
		t.Errorf("yaml: exit %d, output differs from the canonical file", r.code)
	}
	r = runPlanCLI(t, planEnv{}, "plan", "show", pagilaPlanPath, "--format", "json")
	var doc map[string]any
	if r.code != ExitOK || json.Unmarshal([]byte(r.stdout), &doc) != nil || doc["kind"] != plan.Kind {
		t.Errorf("json: exit %d stdout:\n%s", r.code, r.stdout)
	}
	if p, err := plan.Unmarshal([]byte(r.stdout)); err != nil || p.Metadata.Name != "pagila" {
		t.Errorf("json does not decode as a plan: %v", err)
	}
}

func TestPlanValidate(t *testing.T) {
	r := runPlanCLI(t, planEnv{}, "plan", "validate", pagilaPlanPath, "--catalog", pagilaCatalogPath)
	if r.code != ExitOK || r.stdout != "plan pagila is valid for catalog pagila\n" || r.stderr != "" {
		t.Errorf("exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}

	r = runPlanCLI(t, planEnv{}, "plan", "validate", pagilaPlanPath)
	if r.code != ExitOK || !strings.Contains(r.stdout, "structurally valid") || !strings.Contains(r.stderr, noCatalogNotice) {
		t.Errorf("no catalog: exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}

	// A plan for another catalog: fingerprint mismatch, explicit message.
	tampered := writePlan(t, func(p *plan.DomainPlan) {
		p.Spec.Source.CatalogFingerprint = "sha256:" + strings.Repeat("0", 64)
	})
	r = runPlanCLI(t, planEnv{}, "plan", "validate", tampered, "--catalog", pagilaCatalogPath)
	if r.code != ExitError || r.stdout != "" {
		t.Fatalf("exit %d stdout %q", r.code, r.stdout)
	}
	for _, want := range []string{
		"kabr: plan pagila is invalid:\n",
		"  - spec.source.catalogFingerprint: plan was generated for a different catalog",
		`kabr: catalog mismatch: the plan was generated for a different catalog than database "pagila"`,
	} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
		}
	}

	// A dropped table and a bogus sensitive column: one line per problem.
	dropped := writePlan(t, func(p *plan.DomainPlan) {
		d := &p.Spec.Domains[0]
		d.Tables = d.Tables[1:]
		p.Spec.Domains[1].SensitiveColumns[0].Column = "no_such_column"
	})
	r = runPlanCLI(t, planEnv{}, "plan", "validate", dropped, "--catalog", pagilaCatalogPath)
	if r.code != ExitError ||
		!strings.Contains(r.stderr, "  - spec.domains: catalog table \"public.city\" is not assigned to any domain\n") ||
		!strings.Contains(r.stderr, `has no column "no_such_column"`) ||
		strings.Contains(r.stderr, "catalog mismatch") {
		t.Errorf("exit %d stderr:\n%s", r.code, r.stderr)
	}

	// Structure-only failures without a catalog.
	broken := writePlan(t, func(p *plan.DomainPlan) { p.Spec.Domains[0].Name = "Bad Name" })
	r = runPlanCLI(t, planEnv{}, "plan", "validate", broken)
	if r.code != ExitError || !strings.Contains(r.stderr, "  - spec.domains[0].name") {
		t.Errorf("exit %d stderr:\n%s", r.code, r.stderr)
	}
}

func TestPlanApprove(t *testing.T) {
	path := writePlan(t, nil)
	r := runPlanCLI(t, planEnv{}, "plan", "approve", path, "--by", "alice", "--catalog", pagilaCatalogPath)
	if r.code != ExitOK {
		t.Fatalf("exit %d, stderr:\n%s", r.code, r.stderr)
	}
	want := "kabr: plan pagila approved by alice at 2026-01-02T06:04:05Z -> " + path + "\n"
	if r.stderr != want || r.stdout != "" {
		t.Errorf("stderr %q, want %q", r.stderr, want)
	}
	p, err := plan.Unmarshal([]byte(readFile(t, path)))
	if err != nil {
		t.Fatal(err)
	}
	st := p.Status
	if st.Phase != plan.PhaseApproved || st.ApprovedBy != "alice" || st.ApprovedAt == nil ||
		!st.ApprovedAt.Equal(time.Date(2026, 1, 2, 6, 4, 5, 0, time.UTC)) {
		t.Errorf("status = %+v", st)
	}
	if err := p.Validate(loadPagilaCatalog(t)); err != nil {
		t.Errorf("approved plan is invalid: %v", err)
	}

	// --out leaves FILE alone; "-" writes to stdout; no catalog prints a notice.
	src := writePlan(t, nil)
	out := filepath.Join(t.TempDir(), "approved.yaml")
	r = runPlanCLI(t, planEnv{}, "plan", "approve", "--by", "bob", "--out", out, src)
	if r.code != ExitOK || readFile(t, src) != readFile(t, pagilaPlanPath) ||
		!strings.Contains(readFile(t, out), "approvedBy: bob") || !strings.Contains(r.stderr, noCatalogNotice) {
		t.Errorf("--out: exit %d stderr %q", r.code, r.stderr)
	}
	r = runPlanCLI(t, planEnv{}, "plan", "approve", src, "--by", "bob", "--out", "-")
	if r.code != ExitOK || !strings.Contains(r.stdout, "phase: Approved") || !strings.HasSuffix(r.stderr, "-> stdout\n") {
		t.Errorf("--out -: exit %d stderr %q", r.code, r.stderr)
	}

	// Invalid plans are refused and left untouched.
	for name, mutate := range map[string]func(*plan.DomainPlan){
		"wrong catalog": func(p *plan.DomainPlan) { p.Spec.Source.CatalogFingerprint = "sha256:" + strings.Repeat("a", 64) },
		"bad structure": func(p *plan.DomainPlan) { p.Spec.Domains[0].Name = "Bad Name" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := writePlan(t, mutate)
			before := readFile(t, bad)
			r := runPlanCLI(t, planEnv{}, "plan", "approve", bad, "--by", "alice", "--catalog", pagilaCatalogPath)
			if r.code != ExitError || !strings.Contains(r.stderr, "is invalid") {
				t.Errorf("exit %d stderr:\n%s", r.code, r.stderr)
			}
			if readFile(t, bad) != before {
				t.Error("refused approval modified the file")
			}
		})
	}
}

func TestPlanEval(t *testing.T) {
	r := runPlanCLI(t, planEnv{}, "plan", "eval", pagilaPlanPath, "--gold", pagilaGoldPath)
	if r.code != ExitOK {
		t.Fatalf("exit %d, stderr:\n%s", r.code, r.stderr)
	}
	if !strings.HasPrefix(r.stdout, "eval pagila\npartition: tables=15 ") || !strings.Contains(r.stdout, "\nsensitive: recall=") {
		t.Errorf("stdout:\n%s", r.stdout)
	}

	// A plan that does not cover the gold tables fails.
	partial := writePlan(t, func(p *plan.DomainPlan) {
		d := &p.Spec.Domains[0]
		d.Tables = d.Tables[1:]
	})
	r = runPlanCLI(t, planEnv{}, "plan", "eval", partial, "--gold", pagilaGoldPath)
	if r.code != ExitError || !strings.Contains(r.stdout, "missing in prediction: public.city") ||
		!strings.Contains(r.stderr, "plan and gold cover different tables (1 missing in the plan, 0 not in the gold)") {
		t.Errorf("exit %d stdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
	}
}

func TestParseInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(&bytes.Buffer{})
	by := fs.String("by", "", "")
	pos, err := parseInterspersed(fs, []string{"a", "--by", "x", "b", "--", "--by", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if *by != "x" || strings.Join(pos, ",") != "a,b,--by,c" {
		t.Errorf("by %q pos %v", *by, pos)
	}
}
