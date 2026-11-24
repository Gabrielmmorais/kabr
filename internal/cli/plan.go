package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Gabrielmmorais/kabr/pkg/eval"
	"github.com/Gabrielmmorais/kabr/pkg/introspect"
	"github.com/Gabrielmmorais/kabr/pkg/llm"
	"github.com/Gabrielmmorais/kabr/pkg/plan"
	"github.com/Gabrielmmorais/kabr/pkg/planner"
	"github.com/Gabrielmmorais/kabr/pkg/schema"
	"github.com/Gabrielmmorais/kabr/pkg/segment"
)

// LLM providers accepted by "plan generate --llm".
const (
	ProviderAnthropic = "anthropic"
	ProviderOllama    = "ollama"
	ProviderNone      = "none"
)

// Formats accepted by "plan show --format".
const (
	FormatTable = "table"
	FormatYAML  = "yaml"
)

// Environment variables that make "plan generate" default to Anthropic.
// They are only tested for presence; their values are never read here.
var anthropicCredentialEnvs = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}

var (
	providers   = []string{ProviderAnthropic, ProviderOllama, ProviderNone}
	efforts     = []string{"low", "medium", "high", "xhigh", "max"}
	showFormats = []string{FormatTable, FormatYAML, FormatJSON}
)

//nolint:gosec // G101: a user-facing notice, not a credential.
const noCredentialsNotice = "kabr: no LLM credentials found; generating a graph-only plan (use --llm to choose a provider)"

const planUsage = `Usage: kabr plan <subcommand> [arguments] [flags]

Work with a domain plan (DomainPlan YAML): the grouping of the tables of
one catalog into domains, with the sensitive columns of each domain.

Subcommands:
  generate   Generate a plan from a catalog file or a live database
  show       Print a plan as a readable summary, YAML or JSON
  validate   Check a plan (against a catalog when one is given)
  approve    Validate a plan and mark it Approved
  eval       Score a plan against a human-curated gold standard

Run "kabr help plan <subcommand>" for the flags of a subcommand.
Flags may appear before or after the FILE argument.
`

// planCommand is one "kabr plan" subcommand.
type planCommand struct {
	name  string
	usage string // header printed before the flags
	flags func() (*flag.FlagSet, any)
	run   func(ctx context.Context, env *Env, args []string, f any) error
}

var planCommands = []planCommand{
	{"generate", generateUsage, newGenerateFlags, runPlanGenerate},
	{"show", showUsage, newShowFlags, runPlanShow},
	{"validate", validateUsage, newValidateFlags, runPlanValidate},
	{"approve", approveUsage, newApproveFlags, runPlanApprove},
	{"eval", evalUsage, newEvalFlags, runPlanEval},
}

func findPlanCommand(name string) *planCommand {
	for i := range planCommands {
		if planCommands[i].name == name {
			return &planCommands[i]
		}
	}
	return nil
}

func printPlanCommandUsage(w io.Writer, c *planCommand) {
	fs, _ := c.flags()
	fmt.Fprint(w, c.usage)
	fmt.Fprint(w, "\nFlags:\n")
	fs.SetOutput(w)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)
}

// planHelp serves "kabr help plan [subcommand]".
func planHelp(args []string, env *Env) error {
	switch len(args) {
	case 0:
		fmt.Fprint(env.Stdout, planUsage)
		return nil
	case 1:
		if c := findPlanCommand(args[0]); c != nil {
			printPlanCommandUsage(env.Stdout, c)
			return nil
		}
	}
	return usageErr("unknown help topic %q; run \"kabr help plan\"",
		introspect.RedactText("plan "+strings.Join(args, " ")))
}

func runPlan(ctx context.Context, args []string, env *Env) error {
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, planUsage)
		return usageErr("plan: missing subcommand")
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		return planHelp(args[1:], env)
	}
	c := findPlanCommand(args[0])
	if c == nil {
		return usageErr("plan: unknown subcommand %q; run \"kabr help plan\"", introspect.RedactText(args[0]))
	}
	fs, f := c.flags()
	pos, err := parseInterspersed(fs, args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printPlanCommandUsage(env.Stdout, c)
			return &exitError{code: ExitOK}
		}
		return usageErr("plan %s: %s", c.name, introspect.RedactText(err.Error()))
	}
	return c.run(ctx, env, pos, f)
}

// parseInterspersed parses flags that may appear before or after positional
// arguments (the stdlib flag package stops at the first positional one) and
// returns the positional arguments. Everything after "--" is positional.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		// fs.Parse consumed a "--" terminator if the arguments before rest
		// end with it: then rest is positional as a whole.
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			return append(pos, rest...), nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// oneFile returns the single FILE argument of a subcommand.
func oneFile(cmd string, pos []string) (string, error) {
	switch len(pos) {
	case 0:
		return "", usageErr("plan %s: missing FILE argument", cmd)
	case 1:
		return pos[0], nil
	default:
		return "", usageErr("plan %s: unexpected argument %q (one FILE only)", cmd, introspect.RedactText(pos[1]))
	}
}

// --- catalog source ----------------------------------------------------------

// catalogFlags selects the catalog a plan command works on.
type catalogFlags struct {
	catalog string
	dsn     string
	schemas schemaList
	strict  bool
}

func (c *catalogFlags) register(fs *flag.FlagSet, purpose string) {
	fs.StringVar(&c.catalog, "catalog", "", "catalog JSON file written by \"kabr introspect\" "+purpose)
	fs.StringVar(&c.dsn, "dsn", "", "introspect this database read-only instead of reading --catalog (default $"+DSNEnv+" when --catalog is not set)")
	fs.Var(&c.schemas, "schema", "with a DSN: schema to read; repeatable or comma-separated (default public)")
	fs.BoolVar(&c.strict, "strict", false, "with a DSN: exit 3 without introspecting if the role has privileges beyond read-only")
}

// resolve applies $KABR_DSN and checks that at most one source is set; it
// reports whether a catalog source is available.
func (c *catalogFlags) resolve(cmd string, env *Env) (bool, error) {
	if c.catalog != "" && c.dsn != "" {
		return false, usageErr("plan %s: use exactly one of --catalog or --dsn", cmd)
	}
	if c.catalog == "" && c.dsn == "" {
		c.dsn = strings.TrimSpace(env.Getenv(DSNEnv))
	}
	if c.catalog == "" && c.dsn == "" && (len(c.schemas.values) > 0 || c.strict) {
		return false, usageErr("plan %s: --schema and --strict need a DSN (--dsn or $%s)", cmd, DSNEnv)
	}
	return c.catalog != "" || c.dsn != "", nil
}

// load reads the catalog file or introspects the DSN (the M1 read-only
// path, with its privilege warnings and DSN redaction).
func (c *catalogFlags) load(ctx context.Context, env *Env) (*schema.Catalog, error) {
	if c.catalog != "" {
		data, err := os.ReadFile(c.catalog)
		if err != nil {
			return nil, fmt.Errorf("read catalog: %w", err)
		}
		cat, err := schema.Unmarshal(data)
		if err != nil {
			return nil, fmt.Errorf("read catalog %s: %w", c.catalog, err)
		}
		return cat, nil
	}
	opts := introspect.Options{Schemas: c.schemas.values, IncludeStats: true}
	cat, _, err := introspectCatalog(ctx, env, c.dsn, opts, c.strict, introspect.DefaultStatementTimeout)
	return cat, err
}

func readPlanFile(path string) (*plan.DomainPlan, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the user's FILE argument
	if err != nil {
		return nil, fmt.Errorf("read plan: %w", err)
	}
	p, err := plan.Unmarshal(data)
	if err != nil {
		return nil, fmt.Errorf("read plan %s: %w", path, err)
	}
	return p, nil
}

func writeOutput(env *Env, out string, data []byte) error {
	if out == "" || out == "-" {
		if _, err := env.Stdout.Write(data); err != nil {
			return fmt.Errorf("write stdout: %w", err)
		}
		return nil
	}
	if err := writeFileAtomic(out, data); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

// --- generate ------------------------------------------------------------------

type generateFlags struct {
	src        catalogFlags
	provider   string
	model      string
	effort     string
	resolution float64
	name       string
	out        string
	timeout    time.Duration
}

const generateUsage = `Usage: kabr plan generate [--catalog FILE | --dsn DSN] [flags]

Segment the catalog into domains (Louvain over foreign keys and inferred
references), flag sensitive columns with heuristics and, when an LLM
provider is selected, let the model rename domains, move tables and
classify more sensitive columns. Only catalog metadata is sent to the
model: never table rows, never the DSN.

The catalog comes from exactly one of --catalog or --dsn ($` + DSNEnv + `).
Without --llm the provider is "anthropic" when $ANTHROPIC_API_KEY or
$ANTHROPIC_AUTH_TOKEN is set and "none" (graph-only) otherwise. If the LLM
call fails or its proposal is rejected, the graph-only plan is written
with a warning and the exit code is still 0.

The plan (YAML) goes to stdout or --out; warnings and a one-line summary
go to stderr.
`

func newGenerateFlags() (*flag.FlagSet, any) {
	f := &generateFlags{}
	fs := flag.NewFlagSet("plan generate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f.src.register(fs, "to plan")
	fs.StringVar(&f.provider, "llm", "", "LLM provider: anthropic, ollama or none (default anthropic with credentials, else none)")
	fs.StringVar(&f.model, "model", "", "LLM model (default $"+llm.EnvModel+", else "+llm.DefaultAnthropicModel+" / "+llm.DefaultOllamaModel+")")
	fs.StringVar(&f.effort, "effort", "", "Anthropic effort: low, medium, high, xhigh or max (default "+llm.DefaultEffort+")")
	fs.Float64Var(&f.resolution, "resolution", segment.DefaultOptions().Resolution, "Louvain resolution: above 1 gives more, smaller domains; below 1 fewer, larger ones")
	fs.StringVar(&f.name, "name", "", "plan name, a DNS-1123 subdomain (default derived from the database name)")
	fs.StringVar(&f.out, "out", "", "write the plan to this file atomically (default stdout)")
	fs.DurationVar(&f.timeout, "timeout", llm.DefaultTimeout, "timeout of the LLM request")
	return fs, f
}

func runPlanGenerate(ctx context.Context, env *Env, pos []string, fv any) error {
	f := fv.(*generateFlags)
	explicitLLM := false
	if len(pos) > 0 {
		return usageErr("plan generate: unexpected argument %q (flags only)", introspect.RedactText(pos[0]))
	}
	if f.provider != "" {
		explicitLLM = true
		f.provider = strings.ToLower(strings.TrimSpace(f.provider))
		if !slices.Contains(providers, f.provider) {
			return usageErr("plan generate: invalid --llm %q (want anthropic, ollama or none)", f.provider)
		}
	}
	if f.effort != "" && !slices.Contains(efforts, strings.ToLower(f.effort)) {
		return usageErr("plan generate: invalid --effort %q (want low, medium, high, xhigh or max)", f.effort)
	}
	if !(f.resolution > 0) || math.IsInf(f.resolution, 0) {
		return usageErr("plan generate: --resolution must be a positive number")
	}
	if f.name != "" && !plan.IsDNSSubdomain(f.name) {
		return usageErr("plan generate: --name %q is not a valid DNS-1123 subdomain", f.name)
	}
	if f.timeout <= 0 {
		return usageErr("plan generate: --timeout must be positive")
	}
	ok, err := f.src.resolve("generate", env)
	if err != nil {
		return err
	}
	if !ok {
		return usageErr("plan generate: no catalog; pass --catalog FILE, --dsn or set $%s", DSNEnv)
	}

	if !explicitLLM {
		f.provider = ProviderNone
		for _, k := range anthropicCredentialEnvs {
			if strings.TrimSpace(env.Getenv(k)) != "" {
				f.provider = ProviderAnthropic
				break
			}
		}
		if f.provider == ProviderNone {
			fmt.Fprintln(env.Stderr, noCredentialsNotice)
		}
	}

	cat, err := f.src.load(ctx, env)
	if err != nil {
		return err
	}

	var refiner planner.Refiner
	if f.provider != ProviderNone {
		model := strings.TrimSpace(f.model)
		if model == "" {
			model = strings.TrimSpace(env.Getenv(llm.EnvModel))
		}
		cfg := llm.Config{
			Provider: f.provider,
			Model:    model,
			Effort:   strings.ToLower(f.effort),
			Timeout:  f.timeout,
		}
		if f.provider == ProviderOllama {
			cfg.BaseURL = strings.TrimSpace(env.Getenv(llm.EnvOllamaURL))
		}
		refiner, err = env.NewRefiner(cfg)
		if err != nil {
			return fmt.Errorf("llm: %w", err)
		}
	}

	segOpts := segment.DefaultOptions()
	segOpts.Resolution = f.resolution
	res, err := planner.Generate(ctx, cat, planner.Options{Name: f.name, Segment: segOpts, Refiner: refiner})
	if err != nil {
		if ctx.Err() != nil {
			return errors.New("interrupted")
		}
		return err
	}
	data, err := plan.Marshal(res.Plan)
	if err != nil {
		return err
	}
	if err := writeOutput(env, f.out, data); err != nil {
		return err
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(env.Stderr, "warning: %s\n", w)
	}
	tables, sensitive := planCounts(res.Plan)
	fmt.Fprintf(env.Stderr, "plan %q: %d domains, %d tables, %d sensitive columns (generator: %s)\n",
		res.Plan.Metadata.Name, len(res.Plan.Spec.Domains), tables, sensitive, generatorLabel(res.Plan.Status.Generator))
	return nil
}

func planCounts(p *plan.DomainPlan) (tables, sensitive int) {
	for _, d := range p.Spec.Domains {
		tables += len(d.Tables)
		sensitive += len(d.SensitiveColumns)
	}
	return tables, sensitive
}

// generatorLabel is "graph", "llm/<provider>/<model>" or "-" when unset.
func generatorLabel(g plan.Generator) string {
	switch {
	case g.Mode == plan.ModeLLM:
		return "llm/" + orDash(g.Provider) + "/" + orDash(g.Model)
	case g.Mode != "":
		return g.Mode
	default:
		return "-"
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// --- show ----------------------------------------------------------------------

type showFlags struct{ format string }

const showUsage = `Usage: kabr plan show FILE [--format table|yaml|json]

Print a plan. "table" (default) is a readable summary: the review status,
the generator, the warnings and, per domain, its tables (with role and
shared markers) and sensitive columns (table.column class -> action).
"yaml" is the canonical plan encoding; "json" the same document as JSON.
The plan is decoded strictly but not validated (see "kabr plan validate").
`

func newShowFlags() (*flag.FlagSet, any) {
	f := &showFlags{}
	fs := flag.NewFlagSet("plan show", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&f.format, "format", FormatTable, "output format: table, yaml or json")
	return fs, f
}

func runPlanShow(_ context.Context, env *Env, pos []string, fv any) error {
	f := fv.(*showFlags)
	file, err := oneFile("show", pos)
	if err != nil {
		return err
	}
	if !slices.Contains(showFormats, f.format) {
		return usageErr("plan show: invalid --format %q (want table, yaml or json)", f.format)
	}
	p, err := readPlanFile(file)
	if err != nil {
		return err
	}
	var data []byte
	switch f.format {
	case FormatYAML:
		data, err = plan.Marshal(p)
	case FormatJSON:
		data, err = planJSON(p)
	default:
		data, err = planTable(p)
	}
	if err != nil {
		return err
	}
	return writeOutput(env, "", data)
}

// planJSON returns the canonical plan (sorted like [plan.Marshal]) as
// indented JSON.
func planJSON(p *plan.DomainPlan) ([]byte, error) {
	y, err := plan.Marshal(p)
	if err != nil {
		return nil, err
	}
	canon, err := plan.Unmarshal(y)
	if err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(canon, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// planTable renders the human-readable summary of a canonical plan.
func planTable(p *plan.DomainPlan) ([]byte, error) {
	y, err := plan.Marshal(p)
	if err != nil {
		return nil, err
	}
	if p, err = plan.Unmarshal(y); err != nil {
		return nil, err
	}
	var b strings.Builder
	st := p.Status
	tables, sensitive := planCounts(p)
	approvedAt := "-"
	if st.ApprovedAt != nil {
		approvedAt = st.ApprovedAt.UTC().Format(time.RFC3339)
	}
	modularity := "-"
	if st.Generator.Modularity != 0 {
		modularity = fmt.Sprintf("%.4f", st.Generator.Modularity)
	}
	src := p.Spec.Source
	hw := tabwriter.NewWriter(&b, 0, 0, 1, ' ', 0)
	fmt.Fprintf(hw, "Plan:\t%s\n", p.Metadata.Name)
	fmt.Fprintf(hw, "Source:\t%s database %q (schema %s)\n", orDash(src.Dialect), src.Database, orDash(src.SchemaVersion))
	fmt.Fprintf(hw, "Fingerprint:\t%s\n", orDash(src.CatalogFingerprint))
	fmt.Fprintf(hw, "Phase:\t%s\n", orDash(string(st.Phase)))
	fmt.Fprintf(hw, "Approved by:\t%s\n", orDash(st.ApprovedBy))
	fmt.Fprintf(hw, "Approved at:\t%s\n", approvedAt)
	fmt.Fprintf(hw, "Generator:\t%s\n", generatorLabel(st.Generator))
	fmt.Fprintf(hw, "Modularity:\t%s\n", modularity)
	fmt.Fprintf(hw, "Totals:\t%d domains, %d tables, %d sensitive columns\n", len(p.Spec.Domains), tables, sensitive)
	if err := hw.Flush(); err != nil {
		return nil, err
	}
	if len(st.Warnings) > 0 {
		b.WriteString("\nWarnings:\n")
		for _, w := range st.Warnings {
			fmt.Fprintf(&b, "  - %s\n", w)
		}
	}
	for _, d := range p.Spec.Domains {
		fmt.Fprintf(&b, "\nDomain %s (%d tables", d.Name, len(d.Tables))
		if n := len(d.SensitiveColumns); n > 0 {
			fmt.Fprintf(&b, ", %d sensitive columns", n)
		}
		b.WriteString(")\n")
		if d.Description != "" {
			fmt.Fprintf(&b, "  %s\n", oneLine(d.Description))
		}
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  TABLE\tROLE\tSHARED")
		for _, t := range d.Tables {
			shared := ""
			if t.Shared {
				shared = "shared"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", t.Name, orDash(t.Role), shared)
		}
		if err := tw.Flush(); err != nil {
			return nil, err
		}
		if len(d.SensitiveColumns) > 0 {
			b.WriteString("  Sensitive columns:\n")
			sw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
			for _, sc := range d.SensitiveColumns {
				fmt.Fprintf(sw, "    %s.%s\t%s -> %s\t%s\n", sc.Table, sc.Column, sc.Class, sc.Action, orDash(sc.Source))
			}
			if err := sw.Flush(); err != nil {
				return nil, err
			}
		}
	}
	// tabwriter pads the last column of rows: trim trailing spaces so the
	// output is stable and diff-friendly.
	lines := strings.Split(b.String(), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return []byte(strings.Join(lines, "\n")), nil
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// --- validate ------------------------------------------------------------------

type validateFlags struct{ src catalogFlags }

const validateUsage = `Usage: kabr plan validate FILE [--catalog FILE | --dsn DSN]

Check a plan. With a catalog (--catalog, --dsn or $` + DSNEnv + `) the plan
is fully validated against it: every table assigned exactly once, no
unknown table or column, and the catalog fingerprint must match (a plan
generated for another catalog, or a catalog that changed since, fails).
Without a catalog only the structure of the plan is checked.

On success prints "plan <name> is valid ..." and exits 0; otherwise prints
every problem to stderr and exits 1.
`

func newValidateFlags() (*flag.FlagSet, any) {
	f := &validateFlags{}
	fs := flag.NewFlagSet("plan validate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f.src.register(fs, "to validate against")
	return fs, f
}

const noCatalogNotice = "kabr: no catalog given; checking the plan structure only (pass --catalog or --dsn to check it against a database)"

func runPlanValidate(ctx context.Context, env *Env, pos []string, fv any) error {
	f := fv.(*validateFlags)
	file, err := oneFile("validate", pos)
	if err != nil {
		return err
	}
	hasCatalog, err := f.src.resolve("validate", env)
	if err != nil {
		return err
	}
	p, err := readPlanFile(file)
	if err != nil {
		return err
	}
	if !hasCatalog {
		fmt.Fprintln(env.Stderr, noCatalogNotice)
		if err = p.ValidateStructure(); err != nil {
			return invalidPlan(env, p, nil, err)
		}
		fmt.Fprintf(env.Stdout, "plan %s is structurally valid (not checked against a catalog)\n", p.Metadata.Name)
		return nil
	}
	cat, err := f.src.load(ctx, env)
	if err != nil {
		return err
	}
	if err := p.Validate(cat); err != nil {
		return invalidPlan(env, p, cat, err)
	}
	fmt.Fprintf(env.Stdout, "plan %s is valid for catalog %s\n", p.Metadata.Name, cat.Database)
	return nil
}

// invalidPlan prints every line of a (joined) validation error to stderr and
// returns an [ExitError] whose message is already printed.
func invalidPlan(env *Env, p *plan.DomainPlan, cat *schema.Catalog, err error) error {
	fmt.Fprintf(env.Stderr, "kabr: plan %s is invalid:\n", p.Metadata.Name)
	for line := range strings.SplitSeq(err.Error(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			fmt.Fprintf(env.Stderr, "  - %s\n", line)
		}
	}
	if cat != nil && errors.Is(err, plan.ErrCatalogMismatch) {
		fmt.Fprintf(env.Stderr,
			"kabr: catalog mismatch: the plan was generated for a different catalog than database %q "+
				"(or the catalog changed since); regenerate it with \"kabr plan generate\"\n", cat.Database)
	}
	return &exitError{code: ExitError}
}

// --- approve -------------------------------------------------------------------

type approveFlags struct {
	src catalogFlags
	by  string
	out string
}

const approveUsage = `Usage: kabr plan approve FILE --by NAME [--catalog FILE | --dsn DSN] [--out FILE]

Mark a plan Approved: set status.phase, status.approvedBy and
status.approvedAt (UTC, whole seconds). The plan is validated first: its
structure always, and against the catalog when one is given (--catalog,
--dsn or $` + DSNEnv + `). An invalid plan is refused (exit 1) and the file
is left untouched. The approved plan replaces FILE atomically, or goes to
--out ("-" for stdout).
`

func newApproveFlags() (*flag.FlagSet, any) {
	f := &approveFlags{}
	fs := flag.NewFlagSet("plan approve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f.src.register(fs, "to validate against before approving")
	fs.StringVar(&f.by, "by", "", "who approves the plan (required)")
	fs.StringVar(&f.out, "out", "", "write the approved plan here instead of replacing FILE")
	return fs, f
}

func runPlanApprove(ctx context.Context, env *Env, pos []string, fv any) error {
	f := fv.(*approveFlags)
	file, err := oneFile("approve", pos)
	if err != nil {
		return err
	}
	by := strings.TrimSpace(f.by)
	if by == "" {
		return usageErr("plan approve: --by is required")
	}
	hasCatalog, err := f.src.resolve("approve", env)
	if err != nil {
		return err
	}
	p, err := readPlanFile(file)
	if err != nil {
		return err
	}
	if hasCatalog {
		var cat *schema.Catalog
		if cat, err = f.src.load(ctx, env); err != nil {
			return err
		}
		if err = p.Validate(cat); err != nil {
			return invalidPlan(env, p, cat, err)
		}
	} else {
		fmt.Fprintln(env.Stderr, noCatalogNotice)
	}
	if err = plan.Approve(p, by, env.Now()); err != nil {
		return invalidPlan(env, p, nil, err)
	}
	data, err := plan.Marshal(p)
	if err != nil {
		return err
	}
	dest := f.out
	if dest == "" {
		dest = file
	}
	if err := writeOutput(env, dest, data); err != nil {
		return err
	}
	if dest == "-" {
		dest = "stdout"
	}
	fmt.Fprintf(env.Stderr, "kabr: plan %s approved by %s at %s -> %s\n",
		p.Metadata.Name, by, p.Status.ApprovedAt.UTC().Format(time.RFC3339), dest)
	return nil
}

// --- eval ----------------------------------------------------------------------

type evalFlags struct{ gold string }

const evalUsage = `Usage: kabr plan eval FILE --gold GOLD.yaml

Score a plan against a human-curated gold standard (for example
evals/pagila/gold.yaml): partition precision/recall/F1 and Adjusted Rand
Index over the domain grouping, and recall/precision/class accuracy of the
sensitive columns. The report goes to stdout. Exits 1 when the plan and the
gold do not cover the same tables (missing or extra tables are not
reflected in the scores).
`

func newEvalFlags() (*flag.FlagSet, any) {
	f := &evalFlags{}
	fs := flag.NewFlagSet("plan eval", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&f.gold, "gold", "", "gold standard YAML (required)")
	return fs, f
}

func runPlanEval(_ context.Context, env *Env, pos []string, fv any) error {
	f := fv.(*evalFlags)
	file, err := oneFile("eval", pos)
	if err != nil {
		return err
	}
	if f.gold == "" {
		return usageErr("plan eval: --gold is required")
	}
	p, err := readPlanFile(file)
	if err != nil {
		return err
	}
	g, err := eval.LoadGold(f.gold)
	if err != nil {
		return fmt.Errorf("read gold: %w", err)
	}
	r := eval.Evaluate(g, planner.EvalAssignment(p), planner.EvalSensitive(p))
	if err := writeOutput(env, "", []byte(r.String())); err != nil {
		return err
	}
	if n, m := len(r.Partition.MissingInPrediction), len(r.Partition.ExtraInPrediction); n > 0 || m > 0 {
		return &exitError{code: ExitError, msg: fmt.Sprintf(
			"plan and gold cover different tables (%d missing in the plan, %d not in the gold)", n, m)}
	}
	return nil
}
