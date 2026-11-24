// Package cli implements the kabr command-line interface on top of the
// standard library flag package (see docs/adr/0003-cli-framework.md).
//
// The entry point is [Run]; cmd/kabr only wires process-level concerns
// (signals, os.Args, os.Exit) around it so that every behavior, including
// exit codes and DSN redaction, is unit-testable.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Gabrielmmorais/kabr/pkg/introspect"
	// Register the PostgreSQL introspector with introspect.Open.
	_ "github.com/Gabrielmmorais/kabr/pkg/introspect/postgres"
	"github.com/Gabrielmmorais/kabr/pkg/llm"
	"github.com/Gabrielmmorais/kabr/pkg/planner"
)

// Exit codes returned by [Run].
const (
	// ExitOK means the command succeeded.
	ExitOK = 0
	// ExitError means the command failed (connection, query, I/O, ...).
	ExitError = 1
	// ExitUsage means the command line was invalid.
	ExitUsage = 2
	// ExitStrict means --strict refused to introspect because the role
	// holds privileges beyond read-only catalog access.
	ExitStrict = 3
)

// DSNEnv is the environment variable read when --dsn is not given.
const DSNEnv = "KABR_DSN"

// OpenFunc opens an introspector; it matches [introspect.Open].
type OpenFunc func(ctx context.Context, dsn string, cfg introspect.Config) (introspect.Introspector, error)

// RefinerFunc builds the LLM refiner of "plan generate"; it matches
// [llm.New] (a nil Refiner means graph-only).
type RefinerFunc func(cfg llm.Config) (planner.Refiner, error)

// newLLMRefiner adapts [llm.New] to [RefinerFunc], keeping a nil refiner nil.
func newLLMRefiner(cfg llm.Config) (planner.Refiner, error) {
	r, err := llm.New(cfg)
	if err != nil || r == nil {
		return nil, err
	}
	return r, nil
}

// Env carries the process environment of one invocation.
type Env struct {
	Stdout io.Writer
	Stderr io.Writer
	// Getenv looks up environment variables; nil means os.Getenv.
	Getenv func(string) string
	// Version is the build version (see [ResolveVersion]).
	Version string
	// Open opens the introspector; nil means introspect.Open.
	Open OpenFunc
	// NewRefiner builds the LLM refiner; nil means llm.New. Tests inject a
	// fake so that no network call is ever made.
	NewRefiner RefinerFunc
	// Now returns the current time (plan approve); nil means time.Now.
	Now func() time.Time
}

func (e *Env) defaults() {
	if e.Stdout == nil {
		e.Stdout = io.Discard
	}
	if e.Stderr == nil {
		e.Stderr = io.Discard
	}
	if e.Getenv == nil {
		e.Getenv = os.Getenv
	}
	if e.Open == nil {
		e.Open = introspect.Open
	}
	if e.NewRefiner == nil {
		e.NewRefiner = newLLMRefiner
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.Version == "" {
		e.Version = ResolveVersion("")
	}
}

// exitError carries an exit code and an already-redacted message.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func usageErr(format string, a ...any) error {
	return &exitError{code: ExitUsage, msg: fmt.Sprintf(format, a...)}
}

const mainUsage = `kabr - turn a relational database into governed, per-domain APIs.

Usage:
  kabr <command> [flags]

Commands:
  introspect   Read the catalog of a database (read-only) and export it
               as canonical JSON, a Mermaid ER diagram or Graphviz DOT
  plan         Generate, show, validate, approve and evaluate a domain
               plan (subcommands: generate, show, validate, approve, eval)
  serve        Serve one domain of an approved plan as a read-only REST
               API with OpenAPI, row limits and column masking
  version      Print the kabr version
  help         Show help for kabr or for a command

Run "kabr help <command>" for the flags of a command.

Exit codes:
  0  success
  1  error (connection, query, output, invalid plan)
  2  invalid usage
  3  --strict refused an over-privileged role
`

// Run executes kabr with the given arguments (without the program name) and
// returns the process exit code. It never prints the DSN password.
func Run(ctx context.Context, args []string, env Env) int {
	env.defaults()
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, mainUsage)
		return ExitUsage
	}
	var err error
	switch cmd, rest := args[0], args[1:]; cmd {
	case "introspect":
		err = runIntrospect(ctx, rest, &env)
	case "plan":
		err = runPlan(ctx, rest, &env)
	case "serve":
		err = runServe(ctx, rest, &env)
	case "version", "--version", "-version":
		if len(rest) > 0 {
			err = usageErr("version takes no arguments")
			break
		}
		fmt.Fprintf(env.Stdout, "kabr %s\n", env.Version)
	case "help", "-h", "-help", "--help":
		err = runHelp(rest, &env)
	default:
		err = usageErr("unknown command %q; run \"kabr help\"", introspect.RedactText(cmd))
	}
	return report(err, &env)
}

func runHelp(args []string, env *Env) error {
	switch {
	case len(args) == 0:
		fmt.Fprint(env.Stdout, mainUsage)
	case len(args) == 1 && args[0] == "introspect":
		fs, _ := newIntrospectFlags()
		printIntrospectUsage(env.Stdout, fs)
	case len(args) >= 1 && args[0] == "plan":
		return planHelp(args[1:], env)
	case len(args) == 1 && args[0] == "serve":
		fs, _ := newServeFlags()
		printServeUsage(env.Stdout, fs)
	case len(args) == 1 && args[0] == "version":
		fmt.Fprintln(env.Stdout, "Usage: kabr version\n\nPrint the kabr version.")
	case len(args) == 1 && args[0] == "help":
		fmt.Fprintln(env.Stdout, "Usage: kabr help [command]")
	default:
		return usageErr("unknown help topic %q; run \"kabr help\"", introspect.RedactText(strings.Join(args, " ")))
	}
	return nil
}

// report prints err (if any) and maps it to an exit code.
func report(err error, env *Env) int {
	if err == nil {
		return ExitOK
	}
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.msg != "" {
			fmt.Fprintf(env.Stderr, "kabr: %s\n", ee.msg)
		}
		if ee.code == ExitUsage {
			fmt.Fprintln(env.Stderr, `Run "kabr help" for usage.`)
		}
		return ee.code
	}
	fmt.Fprintf(env.Stderr, "kabr: %s\n", introspect.RedactText(err.Error()))
	return ExitError
}
