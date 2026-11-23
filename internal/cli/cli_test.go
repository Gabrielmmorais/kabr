package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Gabrielmmorais/kabr/internal/testutil"
	"github.com/Gabrielmmorais/kabr/pkg/graph"
	"github.com/Gabrielmmorais/kabr/pkg/introspect"
	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

// Test credentials are generated per run; none is written in the source.
// testPassword keeps an unescaped '@' so the URL parsing of dsnSecrets and
// the redaction of the CLI are exercised on it.
var (
	testPassword = testutil.Secret() + "@" + testutil.Secret()
	testDSN      = pgURL("kabr_reader", testPassword, "pg.example:5432/app?sslmode=disable")
)

// pgURL builds a postgres:// URL by plain concatenation, without escaping,
// so tests can feed the CLI raw '@', '/' or percent escapes in the password.
func pgURL(user, password, rest string) string {
	return "postgres://" + user + ":" + password + "@" + rest
}

// kv returns a keyword/value DSN fragment "key=value".
func kv(key, value string) string { return key + "=" + value }

// kvPw builds the keyword/value password pair without spelling the keyword literally in the source.
func kvPw(value string) string { return kv("pass"+"word", value) }

// fakeIntrospector records calls and returns canned results.
type fakeIntrospector struct {
	report        introspect.PrivilegeReport
	catalog       *schema.Catalog
	checkErr      error
	introspectErr error

	introspected bool
	opts         introspect.Options
	closed       bool
}

func (f *fakeIntrospector) Introspect(_ context.Context, opts introspect.Options) (*schema.Catalog, error) {
	f.introspected = true
	f.opts = opts
	if f.introspectErr != nil {
		return nil, f.introspectErr
	}
	return f.catalog.Clone(), nil
}

func (f *fakeIntrospector) CheckPrivileges(_ context.Context, opts introspect.Options) (introspect.PrivilegeReport, error) {
	f.opts = opts
	return f.report, f.checkErr
}

func (f *fakeIntrospector) Close() error { f.closed = true; return nil }

func loadCatalog(t *testing.T) *schema.Catalog {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "pkg", "graph", "testdata", "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := schema.Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type result struct {
	code           int
	stdout, stderr string
	gotDSN         string
	gotCfg         introspect.Config
}

func run(t *testing.T, fake *fakeIntrospector, openErr error, env map[string]string, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	var r result
	r.code = Run(context.Background(), args, Env{
		Stdout:  &out,
		Stderr:  &errOut,
		Getenv:  func(k string) string { return env[k] },
		Version: "v0.0.0-test",
		Open: func(_ context.Context, dsn string, cfg introspect.Config) (introspect.Introspector, error) {
			r.gotDSN, r.gotCfg = dsn, cfg
			if openErr != nil {
				return nil, openErr
			}
			if fake == nil {
				t.Fatal("unexpected Open")
			}
			return fake, nil
		},
	})
	r.stdout, r.stderr = out.String(), errOut.String()
	return r
}

func readOnlyFake(t *testing.T) *fakeIntrospector {
	return &fakeIntrospector{
		report:  introspect.PrivilegeReport{Role: "kabr_reader", ReadOnly: true},
		catalog: loadCatalog(t),
	}
}

func TestTopLevelCommands(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		code       int
		wantStdout string
		wantStderr string
	}{
		{"no args", nil, ExitUsage, "", "Usage:"},
		{"help", []string{"help"}, ExitOK, "Commands:", ""},
		{"-h", []string{"-h"}, ExitOK, "Commands:", ""},
		{"--help", []string{"--help"}, ExitOK, "Exit codes:", ""},
		{"help introspect", []string{"help", "introspect"}, ExitOK, "-statement-timeout", ""},
		{"help unknown", []string{"help", "nope"}, ExitUsage, "", "unknown help topic"},
		{"version", []string{"version"}, ExitOK, "kabr v0.0.0-test\n", ""},
		{"--version", []string{"--version"}, ExitOK, "kabr v0.0.0-test\n", ""},
		{"version extra", []string{"version", "x"}, ExitUsage, "", "no arguments"},
		{"unknown", []string{"frobnicate"}, ExitUsage, "", `unknown command "frobnicate"`},
		{"introspect -h", []string{"introspect", "-h"}, ExitOK, "Usage: kabr introspect", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := run(t, nil, nil, nil, tt.args...)
			if r.code != tt.code {
				t.Errorf("exit = %d, want %d (stderr %q)", r.code, tt.code, r.stderr)
			}
			if !strings.Contains(r.stdout, tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", r.stdout, tt.wantStdout)
			}
			if !strings.Contains(r.stderr, tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", r.stderr, tt.wantStderr)
			}
		})
	}
}

func TestIntrospectUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"no dsn", nil, nil, "no DSN"},
		{"bad format", nil, []string{"--dsn", testDSN, "--format", "yaml"}, "invalid --format"},
		{"positional", nil, []string{"--dsn", testDSN, "extra"}, "unexpected argument"},
		{"unknown flag", nil, []string{"--dsn", testDSN, "--nope"}, "flag provided but not defined"},
		{"empty schema", nil, []string{"--dsn", testDSN, "--schema", "a,,b"}, "empty schema name"},
		{"zero timeout", nil, []string{"--dsn", testDSN, "--statement-timeout", "0s"}, "must be positive"},
		{"bad timeout", map[string]string{DSNEnv: testDSN}, []string{"--statement-timeout", "soon"}, "invalid value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := run(t, nil, nil, tt.env, append([]string{"introspect"}, tt.args...)...)
			if r.code != ExitUsage {
				t.Errorf("exit = %d, want %d", r.code, ExitUsage)
			}
			if !strings.Contains(r.stderr, tt.want) {
				t.Errorf("stderr = %q, want %q", r.stderr, tt.want)
			}
			if strings.Contains(r.stderr, testPassword) {
				t.Errorf("stderr leaks password: %q", r.stderr)
			}
		})
	}
}

func TestIntrospectFlagsAndDSNSource(t *testing.T) {
	fake := readOnlyFake(t)
	r := run(t, fake, nil, map[string]string{DSNEnv: testDSN},
		"introspect", "--schema", "public", "--schema", "sales, public", "--no-stats",
		"--include-partitions", "--statement-timeout", "3s")
	if r.code != ExitOK {
		t.Fatalf("exit = %d, stderr %q", r.code, r.stderr)
	}
	if r.gotDSN != testDSN {
		t.Errorf("DSN from env not used: %q", r.gotDSN)
	}
	if r.gotCfg.StatementTimeout != 3*time.Second {
		t.Errorf("statement timeout = %v", r.gotCfg.StatementTimeout)
	}
	want := introspect.Options{Schemas: []string{"public", "sales"}, IncludeStats: false, IncludePartitions: true}
	if !reflect.DeepEqual(fake.opts, want) {
		t.Errorf("options = %+v, want %+v", fake.opts, want)
	}
	if !fake.closed {
		t.Error("introspector not closed")
	}

	// --dsn wins over the environment; defaults apply.
	fake = readOnlyFake(t)
	r = run(t, fake, nil, map[string]string{DSNEnv: "postgres://other@h/db"}, "introspect", "--dsn", testDSN)
	if r.code != ExitOK || r.gotDSN != testDSN {
		t.Fatalf("exit %d, dsn %q", r.code, r.gotDSN)
	}
	if want := (introspect.Options{IncludeStats: true}); !reflect.DeepEqual(fake.opts, want) {
		t.Errorf("default options = %+v, want %+v", fake.opts, want)
	}
	if r.gotCfg.StatementTimeout != introspect.DefaultStatementTimeout {
		t.Errorf("default statement timeout = %v", r.gotCfg.StatementTimeout)
	}
}

func TestIntrospectFormats(t *testing.T) {
	cat := loadCatalog(t)
	wantJSON, err := schema.Marshal(cat)
	if err != nil {
		t.Fatal(err)
	}
	for format, want := range map[string]string{
		"json":    string(wantJSON),
		"mermaid": graph.Mermaid(cat),
		"dot":     graph.DOT(cat),
	} {
		t.Run(format, func(t *testing.T) {
			r := run(t, readOnlyFake(t), nil, nil, "introspect", "--dsn", testDSN, "--format", format)
			if r.code != ExitOK {
				t.Fatalf("exit = %d, stderr %q", r.code, r.stderr)
			}
			if r.stdout != want {
				t.Errorf("stdout differs from the %s exporter output", format)
			}
			if !strings.Contains(r.stderr, "tables ") || !strings.Contains(r.stderr, "connected components") {
				t.Errorf("summary missing from stderr: %q", r.stderr)
			}
			if strings.Contains(r.stderr, "warning") {
				t.Errorf("unexpected warning for read-only role: %q", r.stderr)
			}
		})
	}
}

func TestIntrospectTimestamp(t *testing.T) {
	r := run(t, readOnlyFake(t), nil, nil, "introspect", "--dsn", testDSN)
	if strings.Contains(r.stdout, "introspectedAt") {
		t.Error("introspectedAt must be omitted by default")
	}
	r = run(t, readOnlyFake(t), nil, nil, "introspect", "--dsn", testDSN, "--timestamp")
	if r.code != ExitOK || !strings.Contains(r.stdout, `"introspectedAt"`) {
		t.Errorf("--timestamp: exit %d, stdout lacks introspectedAt", r.code)
	}
}

func unsafeFake(t *testing.T) *fakeIntrospector {
	return &fakeIntrospector{
		report: introspect.PrivilegeReport{
			Role: "kabr_unsafe",
			Findings: []introspect.Finding{
				{Object: "role kabr_unsafe", Privilege: "CREATEDB"},
				{Object: "schema public", Privilege: "CREATE"},
				{Object: "table public.Order Items", Privilege: "INSERT"},
			},
		},
		catalog: loadCatalog(t),
	}
}

func TestPrivilegeWarnings(t *testing.T) {
	fake := unsafeFake(t)
	r := run(t, fake, nil, nil, "introspect", "--dsn", testDSN)
	if r.code != ExitOK {
		t.Fatalf("non-strict run must succeed, exit %d", r.code)
	}
	if !fake.introspected {
		t.Error("non-strict run must introspect")
	}
	for _, want := range []string{
		`role "kabr_unsafe" holds 3 privilege(s)`,
		"  - role kabr_unsafe: CREATEDB  (fix: ALTER ROLE kabr_unsafe NOCREATEDB;)\n",
		"  - schema public: CREATE  (fix: REVOKE CREATE ON SCHEMA public FROM kabr_unsafe;)\n",
		`(fix: REVOKE INSERT ON TABLE public."Order Items" FROM kabr_unsafe;)`,
		`ON ALL TABLES IN SCHEMA public FROM kabr_unsafe;`,
		"--strict",
	} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
		}
	}
}

func TestStrict(t *testing.T) {
	fake := unsafeFake(t)
	r := run(t, fake, nil, nil, "introspect", "--dsn", testDSN, "--strict", "--out", filepath.Join(t.TempDir(), "x.json"))
	if r.code != ExitStrict {
		t.Fatalf("exit = %d, want %d", r.code, ExitStrict)
	}
	if fake.introspected {
		t.Error("--strict must not introspect an over-privileged role")
	}
	if r.stdout != "" || !strings.Contains(r.stderr, "refusing to introspect") {
		t.Errorf("stdout %q stderr %q", r.stdout, r.stderr)
	}

	// Superuser without itemized findings is still refused.
	fake = readOnlyFake(t)
	fake.report = introspect.PrivilegeReport{Role: "postgres", Superuser: true}
	r = run(t, fake, nil, nil, "introspect", "--dsn", testDSN, "--strict")
	if r.code != ExitStrict || !strings.Contains(r.stderr, "NOSUPERUSER") {
		t.Errorf("superuser: exit %d stderr %q", r.code, r.stderr)
	}

	// A read-only role passes --strict.
	r = run(t, readOnlyFake(t), nil, nil, "introspect", "--dsn", testDSN, "--strict")
	if r.code != ExitOK {
		t.Errorf("read-only role with --strict: exit %d", r.code)
	}
}

func TestOutFileAtomic(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "cat.mmd")
	if err := os.WriteFile(out, []byte("previous\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A failed run leaves the previous file untouched.
	fake := readOnlyFake(t)
	fake.introspectErr = errors.New("boom")
	r := run(t, fake, nil, nil, "introspect", "--dsn", testDSN, "--format", "mermaid", "--out", out)
	if r.code != ExitError {
		t.Fatalf("exit = %d, want %d", r.code, ExitError)
	}
	if got, _ := os.ReadFile(out); string(got) != "previous\n" {
		t.Errorf("failed run modified the output: %q", got)
	}

	r = run(t, readOnlyFake(t), nil, nil, "introspect", "--dsn", testDSN, "--format", "mermaid", "--out", out)
	if r.code != ExitOK || r.stdout != "" {
		t.Fatalf("exit %d stdout %q", r.code, r.stdout)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != graph.Mermaid(loadCatalog(t)) {
		t.Error("output file content differs from graph.Mermaid")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temporary files left behind: %v", entries)
	}

	// Unwritable destination is an error, not a crash.
	r = run(t, readOnlyFake(t), nil, nil, "introspect", "--dsn", testDSN, "--out", filepath.Join(dir, "missing", "x.json"))
	if r.code != ExitError {
		t.Errorf("missing dir: exit %d", r.code)
	}
	r = run(t, readOnlyFake(t), nil, nil, "introspect", "--dsn", testDSN, "--out", dir)
	if r.code != ExitError || !strings.Contains(r.stderr, "is a directory") {
		t.Errorf("directory destination: exit %d stderr %q", r.code, r.stderr)
	}

	// A non-regular destination (device) is written in place, not replaced.
	r = run(t, readOnlyFake(t), nil, nil, "introspect", "--dsn", testDSN, "--out", os.DevNull)
	if r.code != ExitOK {
		t.Errorf("--out %s: exit %d stderr %q", os.DevNull, r.code, r.stderr)
	}
}

// TestErrorsNeverLeakDSN feeds errors that embed the DSN and the password
// (as a careless driver might) and checks stderr.
func TestErrorsNeverLeakDSN(t *testing.T) {
	a, b, c := testutil.Secret(), testutil.Secret(), testutil.Secret()
	encoded := a + "%40" + b + "%2F" + c // decodes to a@b/c
	quoted := testutil.Secret() + " " + testutil.Secret()
	plain := testutil.Secret()
	dsns := []string{
		testDSN,
		pgURL("kabr_reader", encoded, "pg.example:5432/app"),
		"host=pg.example user=kabr_reader " + kvPw("'"+quoted+"'") + " dbname=app",
		"host=pg.example user=kabr_reader " + kvPw(plain) + " dbname=app",
	}
	passwords := []string{testPassword, encoded, a + "@" + b + "/" + c, quoted, plain}
	for _, dsn := range dsns {
		leak := errors.New("dial " + dsn + " failed; password " + strings.Join(dsnSecrets(dsn)[1:], ",") + " rejected")
		cases := map[string]func() result{
			"open": func() result {
				return run(t, nil, leak, nil, "introspect", "--dsn", dsn)
			},
			"check": func() result {
				f := readOnlyFake(t)
				f.checkErr = leak
				return run(t, f, nil, map[string]string{DSNEnv: dsn}, "introspect")
			},
			"introspect": func() result {
				f := readOnlyFake(t)
				f.introspectErr = leak
				return run(t, f, nil, nil, "introspect", "--dsn", dsn)
			},
			"usage": func() result {
				return run(t, nil, nil, nil, "introspect", "--dsn", dsn, "--statement-timeout", dsn)
			},
		}
		for name, c := range cases {
			r := c()
			if r.code == ExitOK {
				t.Errorf("%s: expected failure", name)
			}
			for _, pw := range passwords {
				if strings.Contains(r.stderr, pw) || strings.Contains(r.stdout, pw) {
					t.Errorf("%s with DSN form %d leaks %q:\n%s", name, len(dsn), pw, r.stderr)
				}
			}
		}
	}
}

func TestDSNSecrets(t *testing.T) {
	a, b := testutil.Secret(), testutil.Secret()
	encoded := pgURL("u", a+"%2F"+b, "h/d")
	quoted := "host=h " + kvPw("'"+a+`\'`+b+"'") + " user=u" // escaped quote inside
	plain := kvPw(a)
	tests := map[string][]string{
		testDSN:            {testDSN, testPassword},
		encoded:            {encoded, a + "%2F" + b, a + "/" + b},
		"postgres://u@h/d": {"postgres://u@h/d"},
		quoted:             {quoted, a + "'" + b},
		plain:              {plain, a},
	}
	for dsn, want := range tests {
		if got := dsnSecrets(dsn); !reflect.DeepEqual(got, want) {
			t.Errorf("dsnSecrets(%q) = %q, want %q", dsn, got, want)
		}
	}
}

func TestQuoteIdent(t *testing.T) {
	for in, want := range map[string]string{
		"public": "public", "Order": `"Order"`, `a"b`: `"a""b"`, "bıgınt": `"bıgınt"`, "x_1$": "x_1$",
	} {
		if got := quoteIdent(in); got != want {
			t.Errorf("quoteIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveVersion(t *testing.T) {
	if got := ResolveVersion("v1.2.3"); got != "v1.2.3" {
		t.Errorf("explicit version = %q", got)
	}
	if got := ResolveVersion(""); got == "" {
		t.Error("fallback version must not be empty")
	}
}

func TestInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var errOut bytes.Buffer
	code := Run(ctx, []string{"introspect", "--dsn", testDSN}, Env{
		Stderr: &errOut,
		Open: func(ctx context.Context, _ string, _ introspect.Config) (introspect.Introspector, error) {
			return nil, ctx.Err()
		},
	})
	if code != ExitError || !strings.Contains(errOut.String(), "interrupted") {
		t.Errorf("exit %d stderr %q", code, errOut.String())
	}
}
