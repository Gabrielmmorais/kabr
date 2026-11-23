// Command kabr turns a relational database into governed, per-domain APIs.
//
// Milestone M1 ships the introspect command: it reads the catalog of a
// PostgreSQL database with a read-only role and exports it as canonical
// JSON, a Mermaid ER diagram or Graphviz DOT. Run "kabr help" for usage.
//
// Exit codes: 0 success, 1 error, 2 invalid usage, 3 --strict refused an
// over-privileged role.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/Gabrielmmorais/kabr/internal/cli"
)

// version is set at build time with -ldflags "-X main.version=<version>";
// when empty, the version recorded by the Go toolchain is used.
var version string

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], cli.Env{
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Getenv:  os.Getenv,
		Version: cli.ResolveVersion(version),
	})
	stop()
	os.Exit(code)
}
