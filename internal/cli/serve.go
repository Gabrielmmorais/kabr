package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/Gabrielmmorais/kabr/pkg/introspect"
	"github.com/Gabrielmmorais/kabr/pkg/serve"
)

// HashKeyEnv is the environment variable holding the HMAC key of the
// "hash" action of "kabr serve" (when --hash-key-file is not given).
const HashKeyEnv = "KABR_HASH_KEY"

// maxSecretFileBytes bounds --dsn-file and --hash-key-file.
const maxSecretFileBytes = 64 << 10

// maxServeConns bounds --max-conns.
const maxServeConns = 100

type serveFlags struct {
	config          string
	planFile        string
	domain          string
	addr            string
	dsnFile         string
	hashKeyFile     string
	recheckInterval time.Duration
	rowLimit        int
	maxConns        int
}

func newServeFlags() (*flag.FlagSet, *serveFlags) {
	f := &serveFlags{}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&f.config, "config", "", "domain config JSON (as rendered by the operator); excludes --plan/--domain")
	fs.StringVar(&f.planFile, "plan", "", "approved plan YAML to take the domain from (with --domain)")
	fs.StringVar(&f.domain, "domain", "", "domain of --plan to serve")
	fs.StringVar(&f.addr, "addr", serve.DefaultAddr, "listen address")
	fs.StringVar(&f.dsnFile, "dsn-file", "", "read the DSN from this file (default $"+DSNEnv+"; there is no --dsn flag)")
	fs.StringVar(&f.hashKeyFile, "hash-key-file", "", "read the HMAC key of \"hash\" columns from this file (default $"+HashKeyEnv+")")
	fs.DurationVar(&f.recheckInterval, "recheck-interval", serve.DefaultRecheckInterval, "how often to re-check the catalog fingerprint; 0 disables")
	fs.IntVar(&f.rowLimit, "row-limit", 0, "lower the row limit of the config (cannot raise it)")
	fs.IntVar(&f.maxConns, "max-conns", serve.DefaultMaxConns, "maximum database connections")
	return fs, f
}

func printServeUsage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprintf(w, `Usage: kabr serve (--config FILE | --plan FILE --domain NAME) [flags]

Serve one domain of an approved plan as a read-only REST API: rows of the
domain's tables with keyset pagination, row limits and equality filters;
sensitive columns masked, hashed or dropped; an OpenAPI 3.0.3 document
generated from the live catalog.

The DSN comes from $%[1]s or --dsn-file, never from a flag (it would show
in the process list). Use a role with SELECT on the domain's tables and
nothing else. The HMAC key of "hash" columns comes from $%[2]s or
--hash-key-file (at least %[3]d bytes); without one, those columns are
dropped.

The catalog fingerprint is checked at startup and every --recheck-interval:
while it differs from the approved one, or a configured table is missing,
the server keeps running but /readyz and the data endpoints answer 503.

Endpoints:
  GET /v1/{schema}/{table}        list rows (?limit=, ?cursor=, ?column=value)
  GET /v1/{schema}/{table}/{pk}   one row (single-column primary keys only)
  GET /openapi.json               OpenAPI 3.0.3 document
  GET /healthz                    liveness
  GET /readyz                     readiness (database up, catalog unchanged)

Logs are JSON lines on stderr; they never contain the DSN or filter values.
SIGINT/SIGTERM shut the server down gracefully.

Flags:
`, DSNEnv, HashKeyEnv, serve.MinHashKeyBytes)
	fs.SetOutput(w)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)
	fmt.Fprintf(w, `
Examples:
  export %[1]s='postgres://kabr_api:<password>@localhost:5432/pagila?sslmode=disable'
  kabr serve --plan examples/pagila.plan.yaml --domain film
  kabr serve --config /etc/kabr/domain.json --dsn-file /var/run/secrets/kabr/dsn
`, DSNEnv)
}

// serverRunner starts the server; tests replace it to inspect the options
// without opening a connection.
var serverRunner = func(ctx context.Context, opts serve.Options) error {
	s, err := serve.New(opts)
	if err != nil {
		return err
	}
	return s.Run(ctx)
}

// rejectDSNFlag refuses --dsn without echoing its value.
func rejectDSNFlag(args []string) error {
	for _, a := range args {
		if a == "--" {
			return nil
		}
		name := strings.TrimLeft(a, "-")
		if name, _, _ = strings.Cut(name, "="); strings.HasPrefix(a, "-") && name == "dsn" {
			return usageErr("serve: there is no --dsn flag (the DSN would show in the process list); set $%s or use --dsn-file", DSNEnv)
		}
	}
	return nil
}

func parseServe(args []string, env *Env) (*serveFlags, error) {
	if err := rejectDSNFlag(args); err != nil {
		return nil, err
	}
	fs, f := newServeFlags()
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printServeUsage(env.Stdout, fs)
			return nil, &exitError{code: ExitOK}
		}
		return nil, usageErr("serve: %s", introspect.RedactText(err.Error()))
	}
	if fs.NArg() > 0 {
		return nil, usageErr("serve: unexpected argument %q (flags only)", introspect.RedactText(fs.Arg(0)))
	}
	switch {
	case f.config != "" && (f.planFile != "" || f.domain != ""):
		return nil, usageErr("serve: use either --config or --plan with --domain, not both")
	case f.config == "" && f.planFile == "" && f.domain == "":
		return nil, usageErr("serve: pass --config FILE or --plan FILE --domain NAME")
	case f.planFile != "" && f.domain == "":
		return nil, usageErr("serve: --plan needs --domain")
	case f.domain != "" && f.planFile == "":
		return nil, usageErr("serve: --domain needs --plan")
	}
	if strings.TrimSpace(f.addr) == "" {
		return nil, usageErr("serve: --addr must not be empty")
	}
	if f.recheckInterval < 0 {
		return nil, usageErr("serve: --recheck-interval must not be negative")
	}
	if f.rowLimit < 0 || f.rowLimit > serve.MaxRowLimit {
		return nil, usageErr("serve: --row-limit must be between 1 and %d", serve.MaxRowLimit)
	}
	if f.maxConns < 1 || f.maxConns > maxServeConns {
		return nil, usageErr("serve: --max-conns must be between 1 and %d", maxServeConns)
	}
	return f, nil
}

// readSecretFile reads a small secret file and trims surrounding
// whitespace. Errors name the file, never its content.
func readSecretFile(flagName, path string) (string, error) {
	fh, err := os.Open(path) //nolint:gosec // G304: path is the operator's secret file flag
	if err != nil {
		return "", fmt.Errorf("serve: %s: %w", flagName, err)
	}
	defer func() { _ = fh.Close() }()
	data, err := io.ReadAll(io.LimitReader(fh, maxSecretFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("serve: %s: read %s failed", flagName, path)
	}
	if len(data) > maxSecretFileBytes {
		return "", fmt.Errorf("serve: %s: %s is larger than %d bytes", flagName, path, maxSecretFileBytes)
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return "", fmt.Errorf("serve: %s: %s is empty", flagName, path)
	}
	return s, nil
}

// loadServeConfig returns the domain config from --config or --plan/--domain.
func loadServeConfig(f *serveFlags) (*serve.Config, error) {
	if f.config != "" {
		return serve.LoadConfig(f.config)
	}
	p, err := readPlanFile(f.planFile)
	if err != nil {
		return nil, err
	}
	return serve.ConfigFromPlan(p, f.domain)
}

func runServe(ctx context.Context, args []string, env *Env) error {
	f, err := parseServe(args, env)
	if err != nil {
		return err
	}
	cfg, err := loadServeConfig(f)
	if err != nil {
		return err
	}
	if f.rowLimit > 0 {
		if limit := cfg.EffectiveRowLimit(); f.rowLimit > limit {
			return usageErr("serve: --row-limit %d is above the configured row limit %d (it can only lower it)", f.rowLimit, limit)
		}
		cfg.RowLimit = f.rowLimit
	}

	dsn := ""
	if f.dsnFile != "" {
		if dsn, err = readSecretFile("--dsn-file", f.dsnFile); err != nil {
			return err
		}
	} else if dsn = strings.TrimSpace(env.Getenv(DSNEnv)); dsn == "" {
		return usageErr("serve: no DSN; set $%s or pass --dsn-file", DSNEnv)
	}
	var hashKey string
	if f.hashKeyFile != "" {
		if hashKey, err = readSecretFile("--hash-key-file", f.hashKeyFile); err != nil {
			return err
		}
	} else {
		hashKey = strings.TrimSpace(env.Getenv(HashKeyEnv))
	}
	if hashKey != "" && len(hashKey) < serve.MinHashKeyBytes {
		return usageErr("serve: the hash key must be at least %d bytes", serve.MinHashKeyBytes)
	}

	opts := serve.Options{
		Config:          cfg,
		DSN:             dsn,
		Addr:            f.addr,
		RecheckInterval: f.recheckInterval,
		MaxConns:        f.maxConns,
		Logger:          slog.New(slog.NewJSONHandler(env.Stderr, nil)),
	}
	if hashKey != "" {
		opts.HashKey = []byte(hashKey)
	}
	if err := serverRunner(ctx, opts); err != nil {
		return newFail(ctx, dsn)("", err)
	}
	return nil
}
