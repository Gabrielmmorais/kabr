package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Gabrielmmorais/kabr/pkg/graph"
	"github.com/Gabrielmmorais/kabr/pkg/introspect"
	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

// Output formats accepted by --format.
const (
	FormatJSON    = "json"
	FormatMermaid = "mermaid"
	FormatDOT     = "dot"
)

var formats = []string{FormatJSON, FormatMermaid, FormatDOT}

// schemaList is a repeatable flag that also accepts comma-separated values.
type schemaList struct{ values []string }

func (s *schemaList) String() string { return strings.Join(s.values, ",") }

func (s *schemaList) Set(v string) error {
	for part := range strings.SplitSeq(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return errors.New("empty schema name")
		}
		if !slices.Contains(s.values, part) {
			s.values = append(s.values, part)
		}
	}
	return nil
}

type introspectFlags struct {
	dsn               string
	schemas           schemaList
	format            string
	out               string
	strict            bool
	noStats           bool
	includePartitions bool
	timestamp         bool
	statementTimeout  time.Duration
}

func newIntrospectFlags() (*flag.FlagSet, *introspectFlags) {
	f := &introspectFlags{}
	fs := flag.NewFlagSet("introspect", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&f.dsn, "dsn", "", "database DSN, e.g. postgres://user:<password>@host:5432/db (default $"+DSNEnv+"; prefer the env var so the password stays out of shell history)")
	fs.Var(&f.schemas, "schema", "schema to read; repeatable or comma-separated (default public)")
	fs.StringVar(&f.format, "format", FormatJSON, "output format: json, mermaid or dot")
	fs.StringVar(&f.out, "out", "", "write output to this file atomically (default stdout)")
	fs.BoolVar(&f.strict, "strict", false, "exit 3 without introspecting if the role has privileges beyond read-only")
	fs.BoolVar(&f.noStats, "no-stats", false, "omit planner statistics (row estimates, n_distinct)")
	fs.BoolVar(&f.includePartitions, "include-partitions", false, "report partitions as tables (default: only the partitioned parent)")
	fs.BoolVar(&f.timestamp, "timestamp", false, "stamp introspectedAt in JSON output (off by default for deterministic output)")
	fs.DurationVar(&f.statementTimeout, "statement-timeout", introspect.DefaultStatementTimeout, "timeout for each catalog query")
	return fs, f
}

func printIntrospectUsage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprint(w, `Usage: kabr introspect [flags]

Connect read-only to a database, check that the role holds no privileges
beyond read-only catalog access, read the catalog (never table rows) and
export it.

Warnings and a graph summary go to stderr; the export goes to stdout or
--out. The DSN password is never printed.

Flags:
`)
	fs.SetOutput(w)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)
	fmt.Fprintf(w, `
Examples:
  export %[1]s='postgres://kabr_reader:<password>@localhost:5432/pagila?sslmode=disable'
  kabr introspect > pagila.schema.json
  kabr introspect --format mermaid --out pagila.mmd
  kabr introspect --schema public,sales --strict --format dot | dot -Tsvg > erd.svg
`, DSNEnv)
}

func parseIntrospect(args []string, env *Env) (*introspectFlags, error) {
	fs, f := newIntrospectFlags()
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printIntrospectUsage(env.Stdout, fs)
			return nil, &exitError{code: ExitOK}
		}
		return nil, usageErr("introspect: %s", introspect.RedactText(err.Error()))
	}
	if fs.NArg() > 0 {
		return nil, usageErr("introspect: unexpected argument %q (flags only)", introspect.RedactText(fs.Arg(0)))
	}
	if f.dsn == "" {
		f.dsn = strings.TrimSpace(env.Getenv(DSNEnv))
	}
	if f.dsn == "" {
		return nil, usageErr("introspect: no DSN; set $%s or pass --dsn", DSNEnv)
	}
	if !slices.Contains(formats, f.format) {
		return nil, usageErr("introspect: invalid --format %q (want json, mermaid or dot)", f.format)
	}
	if f.statementTimeout <= 0 {
		return nil, usageErr("introspect: --statement-timeout must be positive")
	}
	if f.out == "-" {
		f.out = ""
	}
	return f, nil
}

func runIntrospect(ctx context.Context, args []string, env *Env) error {
	f, err := parseIntrospect(args, env)
	if err != nil {
		return err
	}
	secrets := dsnSecrets(f.dsn)
	fail := func(prefix string, err error) error {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return errors.New("interrupted")
		}
		return introspect.SanitizeError(prefix, err, secrets...)
	}

	opts := introspect.Options{
		Schemas:           f.schemas.values,
		IncludeStats:      !f.noStats,
		IncludePartitions: f.includePartitions,
	}
	in, err := env.Open(ctx, f.dsn, introspect.Config{StatementTimeout: f.statementTimeout})
	if err != nil {
		return fail("connect", err)
	}
	defer func() { _ = in.Close() }()

	rep, err := in.CheckPrivileges(ctx, opts)
	if err != nil {
		return fail("privilege check", err)
	}
	writePrivilegeWarnings(env.Stderr, rep)
	if f.strict && !rep.ReadOnly {
		return &exitError{code: ExitStrict, msg: fmt.Sprintf(
			"--strict: refusing to introspect as role %q (%d privilege(s) beyond read-only)",
			rep.Role, privilegeCount(rep))}
	}

	cat, err := in.Introspect(ctx, opts)
	if err != nil {
		return fail("introspect", err)
	}
	if f.timestamp {
		now := time.Now().UTC().Truncate(time.Second)
		cat.IntrospectedAt = &now
	}
	data, err := render(cat, f.format)
	if err != nil {
		return fail("render", err)
	}
	if f.out == "" {
		if _, err := env.Stdout.Write(data); err != nil {
			return fail("write stdout", err)
		}
	} else if err := writeFileAtomic(f.out, data); err != nil {
		return fail("write output", err)
	}
	writeSummary(env.Stderr, cat, opts, f)
	return nil
}

func render(cat *schema.Catalog, format string) ([]byte, error) {
	switch format {
	case FormatMermaid:
		return []byte(graph.Mermaid(cat)), nil
	case FormatDOT:
		return []byte(graph.DOT(cat)), nil
	default:
		return schema.Marshal(cat)
	}
}

func privilegeCount(rep introspect.PrivilegeReport) int {
	n := len(rep.Findings)
	if n == 0 && rep.Superuser {
		n = 1
	}
	return n
}

func writeSummary(w io.Writer, cat *schema.Catalog, opts introspect.Options, f *introspectFlags) {
	m := graph.Build(cat).Metrics()
	dest := "stdout"
	if f.out != "" {
		dest = f.out
	}
	fmt.Fprintf(w, "kabr: introspected database %q (%s), schemas %s -> %s (%s)\n",
		cat.Database, cat.Dialect, strings.Join(opts.EffectiveSchemas(), ","), dest, f.format)
	fmt.Fprintf(w, "  tables %d | foreign keys %d (%d self-referencing tables, %d to tables outside the selection)\n",
		m.Nodes, m.ForeignKeys, m.SelfReferences, m.ExternalForeignKeys)
	maxDeg := "-"
	if m.MaxDegreeTable != "" {
		maxDeg = fmt.Sprintf("%s (%d)", m.MaxDegreeTable, m.MaxDegree)
	}
	fmt.Fprintf(w, "  connected components %d | orphan tables %d | most connected %s\n",
		m.Components, m.Orphans, maxDeg)
}

// kvPasswordRe extracts the password of a keyword/value DSN.
var kvPasswordRe = regexp.MustCompile(`(?i)(?:^|\s)password\s*=\s*('(?:[^'\\]|\\.)*'|\S+)`)

// dsnSecrets returns literal strings that must never be printed: the DSN
// itself and its password (raw and percent-decoded).
func dsnSecrets(dsn string) []string {
	secrets := []string{dsn}
	if scheme, rest, ok := strings.Cut(dsn, "://"); ok && scheme != "" {
		head := rest
		if i := strings.IndexAny(head, "?#"); i >= 0 && strings.Contains(head[:i], "@") {
			head = head[:i]
		}
		if at := strings.LastIndexByte(head, '@'); at >= 0 {
			if _, pw, ok := strings.Cut(head[:at], ":"); ok && pw != "" {
				secrets = append(secrets, pw)
				if dec, err := url.PathUnescape(pw); err == nil && dec != pw {
					secrets = append(secrets, dec)
				}
			}
		}
		return secrets
	}
	for _, m := range kvPasswordRe.FindAllStringSubmatch(dsn, -1) {
		pw := m[1]
		if len(pw) >= 2 && pw[0] == '\'' && pw[len(pw)-1] == '\'' {
			pw = strings.NewReplacer(`\'`, `'`, `\\`, `\`).Replace(pw[1 : len(pw)-1])
		}
		if pw != "" {
			secrets = append(secrets, pw)
		}
	}
	return secrets
}
