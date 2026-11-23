//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/Gabrielmmorais/kabr/internal/testutil"
	"github.com/Gabrielmmorais/kabr/pkg/graph"
	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

// The fixture database is shared with pkg/introspect/postgres: same init
// script, same roles (kabr_reader is catalog-only, kabr_unsafe is
// over-privileged) and the same golden catalog. The role passwords are
// generated per run and set once the container is up.
var (
	fixtureDir    = filepath.Join("..", "..", "pkg", "introspect", "postgres", "testdata")
	kabrBin       string
	fixtureHost   string
	fixturePort   string
	fixtureFailed error

	rolePasswords = map[string]string{
		"kabr_reader": testutil.Secret(),
		"kabr_unsafe": testutil.Secret(),
	}
)

const fixtureImage = "postgres:17.12-alpine"

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	tmp, err := os.MkdirTemp("", "kabr-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(tmp)
	kabrBin = filepath.Join(tmp, "kabr")
	build := exec.Command("go", "build", "-ldflags", "-X main.version=v0.0.0-e2e", "-o", kabrBin, ".")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err = build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build kabr:", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ctr, err := tcpostgres.Run(ctx, fixtureImage,
		tcpostgres.WithDatabase("kabr_fixture"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword(testutil.Secret()),
		tcpostgres.WithInitScripts(filepath.Join(fixtureDir, "fixture.sql")),
		testcontainers.WithCmdArgs("-c", "autovacuum=off"),
		tcpostgres.BasicWaitStrategies(),
	)
	if ctr != nil {
		defer func() {
			if termErr := testcontainers.TerminateContainer(ctr); termErr != nil {
				fmt.Fprintln(os.Stderr, "terminate fixture container:", termErr)
			}
		}()
	}
	if err != nil {
		fixtureFailed = err
	} else if fixtureHost, err = ctr.Host(ctx); err != nil {
		fixtureFailed = err
	} else if p, err := ctr.MappedPort(ctx, "5432/tcp"); err != nil {
		fixtureFailed = err
	} else {
		fixturePort = p.Port()
		if adminDSN, err := ctr.ConnectionString(ctx, "sslmode=disable"); err != nil {
			fixtureFailed = err
		} else {
			fixtureFailed = testutil.SetRolePasswords(ctx, adminDSN, rolePasswords)
		}
	}
	return m.Run()
}

func dsn(t *testing.T, user, password string) string {
	t.Helper()
	if fixtureFailed != nil {
		t.Fatalf("start fixture container: %v", fixtureFailed)
	}
	return testutil.PostgresURL(user, password, fixtureHost, fixturePort, "kabr_fixture")
}

type cmdResult struct {
	code           int
	stdout, stderr string
}

// kabr runs the built binary with KABR_DSN set to dsn.
func kabr(t *testing.T, dsn string, args ...string) cmdResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, kabrBin, args...)
	cmd.Env = append(os.Environ(), "KABR_DSN="+dsn)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	r := cmdResult{stdout: out.String(), stderr: errOut.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		r.code = exitErr.ExitCode()
	default:
		t.Fatalf("run kabr: %v", err)
	}
	return r
}

func TestE2EIntrospectJSONMatchesGolden(t *testing.T) {
	pw := rolePasswords["kabr_reader"]
	r := kabr(t, dsn(t, "kabr_reader", pw), "introspect", "--schema", "public", "--schema", "sales", "--strict")
	if r.code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", r.code, r.stderr)
	}
	want, err := os.ReadFile(filepath.Join(fixtureDir, "catalog.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if r.stdout != string(want) {
		t.Errorf("JSON output differs from %s/catalog.golden.json", fixtureDir)
	}
	if strings.Contains(r.stderr, "warning") {
		t.Errorf("catalog-only role must not trigger a warning:\n%s", r.stderr)
	}
	if !strings.Contains(r.stderr, `introspected database "kabr_fixture"`) || !strings.Contains(r.stderr, "connected components") {
		t.Errorf("summary missing:\n%s", r.stderr)
	}
	if strings.Contains(r.stderr, pw) {
		t.Error("stderr leaks the password")
	}
}

func TestE2EMermaidAndDOTToFile(t *testing.T) {
	d := dsn(t, "kabr_reader", rolePasswords["kabr_reader"])
	golden, err := os.ReadFile(filepath.Join(fixtureDir, "catalog.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	cat, err := schema.Unmarshal(golden)
	if err != nil {
		t.Fatal(err)
	}
	for format, want := range map[string]string{"mermaid": graph.Mermaid(cat), "dot": graph.DOT(cat)} {
		out := filepath.Join(t.TempDir(), "erd."+format)
		r := kabr(t, d, "introspect", "--schema", "public,sales", "--format", format, "--out", out)
		if r.code != 0 || r.stdout != "" {
			t.Fatalf("%s: exit %d stdout %q stderr:\n%s", format, r.code, r.stdout, r.stderr)
		}
		got, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s output differs from the exporter applied to the golden catalog", format)
		}
	}
}

func TestE2EStrictRefusesUnsafeRole(t *testing.T) {
	pw := rolePasswords["kabr_unsafe"]
	out := filepath.Join(t.TempDir(), "never.json")
	r := kabr(t, dsn(t, "kabr_unsafe", pw), "introspect", "--strict", "--out", out)
	if r.code != 3 {
		t.Fatalf("exit %d, want 3; stderr:\n%s", r.code, r.stderr)
	}
	for _, want := range []string{
		`kabr: warning: role "kabr_unsafe"`,
		"  - role kabr_unsafe: CREATEDB",
		"fix: REVOKE CREATE ON SCHEMA public FROM kabr_unsafe;",
		"refusing to introspect",
	} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
		}
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("--strict refusal must not write output (stat err %v)", err)
	}
	if strings.Contains(r.stderr, pw) {
		t.Error("stderr leaks the password")
	}

	// Without --strict the same role only gets the warning.
	r = kabr(t, dsn(t, "kabr_unsafe", pw), "introspect", "--format", "mermaid")
	if r.code != 0 || !strings.Contains(r.stderr, "warning") || !strings.HasPrefix(r.stdout, "erDiagram") {
		t.Errorf("non-strict unsafe run: exit %d stderr:\n%s", r.code, r.stderr)
	}
}

func TestE2EBadPasswordDoesNotLeak(t *testing.T) {
	pw := testutil.Secret() // not the role's password
	r := kabr(t, dsn(t, "kabr_reader", pw), "introspect")
	if r.code != 1 {
		t.Fatalf("exit %d, want 1; stderr:\n%s", r.code, r.stderr)
	}
	if strings.Contains(r.stderr, pw) || strings.Contains(r.stdout, pw) {
		t.Errorf("output leaks the password:\n%s", r.stderr)
	}
	if !strings.Contains(r.stderr, "kabr: connect:") {
		t.Errorf("unexpected error message:\n%s", r.stderr)
	}

	// Same with the DSN on the command line instead of the environment.
	r = kabr(t, "", "introspect", "--dsn", dsn(t, "kabr_reader", pw))
	if r.code != 1 || strings.Contains(r.stderr, pw) {
		t.Errorf("--dsn: exit %d stderr:\n%s", r.code, r.stderr)
	}
}

func TestE2EVersionAndUsage(t *testing.T) {
	if r := kabr(t, "", "version"); r.code != 0 || r.stdout != "kabr v0.0.0-e2e\n" {
		t.Errorf("version: exit %d stdout %q", r.code, r.stdout)
	}
	if r := kabr(t, "", "introspect"); r.code != 2 || !strings.Contains(r.stderr, "no DSN") {
		t.Errorf("missing DSN: exit %d stderr %q", r.code, r.stderr)
	}
}
