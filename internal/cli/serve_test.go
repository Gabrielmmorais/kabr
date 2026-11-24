package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gabrielmmorais/kabr/internal/testutil"
	"github.com/Gabrielmmorais/kabr/pkg/plan"
	"github.com/Gabrielmmorais/kabr/pkg/serve"
)

var serveFingerprint = "sha256:" + strings.Repeat("cd", 32)

// captureServer replaces serverRunner for one test and records the options.
func captureServer(t *testing.T, err error) *[]serve.Options {
	t.Helper()
	var got []serve.Options
	prev := serverRunner
	serverRunner = func(_ context.Context, opts serve.Options) error {
		got = append(got, opts)
		return err
	}
	t.Cleanup(func() { serverRunner = prev })
	return &got
}

func runServeCLI(env map[string]string, args ...string) result {
	var out, errOut bytes.Buffer
	code := Run(context.Background(), append([]string{"serve"}, args...), Env{
		Stdout: &out, Stderr: &errOut,
		Getenv:  func(k string) string { return env[k] },
		Version: "v0.0.0-test",
	})
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func serveConfigFile(t *testing.T) string {
	t.Helper()
	data, err := json.Marshal(&serve.Config{
		Domain: "film", Plan: "pagila", CatalogFingerprint: serveFingerprint,
		RowLimit: 50, Tables: []string{"public.film"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return writeFile(t, "domain.json", data)
}

// approvedPagilaPlan writes examples/pagila.plan.yaml, approved, to a file.
func approvedPagilaPlan(t *testing.T, approve bool) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "pagila.plan.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if approve {
		if err = plan.Approve(p, "reviewer", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)); err != nil {
			t.Fatal(err)
		}
	}
	out, err := plan.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return writeFile(t, "plan.yaml", out)
}

func TestServeHelp(t *testing.T) {
	for _, args := range [][]string{{"help", "serve"}, {"serve", "-h"}, {"serve", "--help"}} {
		var out bytes.Buffer
		code := Run(context.Background(), args, Env{Stdout: &out})
		if code != ExitOK {
			t.Errorf("%v: exit %d", args, code)
		}
		for _, want := range []string{"Usage: kabr serve", "-config", "-plan", "-dsn-file", "-hash-key-file",
			"-recheck-interval", "-row-limit", "KABR_DSN", "KABR_HASH_KEY", "/readyz", "/openapi.json"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%v: usage lacks %q", args, want)
			}
		}
		if strings.Contains(out.String(), "-dsn string") {
			t.Errorf("%v: usage advertises a --dsn flag", args)
		}
	}
	var out bytes.Buffer
	Run(context.Background(), []string{"help"}, Env{Stdout: &out})
	if !strings.Contains(out.String(), "serve        Serve one domain") {
		t.Error("main usage does not list serve")
	}
}

func TestServeUsageErrors(t *testing.T) {
	cfgFile := serveConfigFile(t)
	planFile := approvedPagilaPlan(t, true)
	env := map[string]string{DSNEnv: "postgres://u@h/db"}
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"nothing", nil, "pass --config FILE or --plan FILE --domain NAME"},
		{"both", []string{"--config", cfgFile, "--plan", planFile, "--domain", "film"}, "not both"},
		{"config and domain", []string{"--config", cfgFile, "--domain", "film"}, "not both"},
		{"plan without domain", []string{"--plan", planFile}, "--plan needs --domain"},
		{"domain without plan", []string{"--domain", "film"}, "--domain needs --plan"},
		{"positional", []string{"--config", cfgFile, "extra"}, "unexpected argument"},
		{"unknown flag", []string{"--config", cfgFile, "--nope"}, "flag provided but not defined"},
		{"negative recheck", []string{"--config", cfgFile, "--recheck-interval", "-1s"}, "--recheck-interval"},
		{"row limit too high", []string{"--config", cfgFile, "--row-limit", "5000"}, "--row-limit"},
		{"row limit above config", []string{"--config", cfgFile, "--row-limit", "60"}, "can only lower it"},
		{"no conns", []string{"--config", cfgFile, "--max-conns", "0"}, "--max-conns"},
		{"empty addr", []string{"--config", cfgFile, "--addr", ""}, "--addr"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := captureServer(t, nil)
			r := runServeCLI(env, tt.args...)
			if r.code != ExitUsage || !strings.Contains(r.stderr, tt.want) {
				t.Errorf("exit %d, stderr %q; want exit 2 mentioning %q", r.code, r.stderr, tt.want)
			}
			if len(*got) != 0 {
				t.Error("server started on a usage error")
			}
		})
	}
}

func TestServeRefusesDSNFlag(t *testing.T) {
	cfgFile := serveConfigFile(t)
	pw := testutil.Secret()
	dsn := pgURL("kabr_api", pw, "db:5432/app")
	for _, args := range [][]string{
		{"--config", cfgFile, "--dsn", dsn},
		{"--config", cfgFile, "--dsn=" + dsn},
		{"-dsn", dsn, "--config", cfgFile},
	} {
		got := captureServer(t, nil)
		r := runServeCLI(nil, args...)
		if r.code != ExitUsage || !strings.Contains(r.stderr, "there is no --dsn flag") {
			t.Errorf("%v: exit %d: %s", args[2:], r.code, r.stderr)
		}
		if strings.Contains(r.stderr, pw) {
			t.Error("the DSN password was echoed")
		}
		if len(*got) != 0 {
			t.Error("server started")
		}
	}
}

func TestServeDSNAndHashKeySources(t *testing.T) {
	cfgFile := serveConfigFile(t)
	envDSN := pgURL("kabr_api", testutil.Secret(), "env:5432/app")
	fileDSN := pgURL("kabr_api", testutil.Secret(), "file:5432/app")
	envKey := testutil.Secret()
	fileKey := testutil.Secret()

	t.Run("no DSN", func(t *testing.T) {
		captureServer(t, nil)
		r := runServeCLI(nil, "--config", cfgFile)
		if r.code != ExitUsage || !strings.Contains(r.stderr, "no DSN; set $KABR_DSN or pass --dsn-file") {
			t.Errorf("exit %d: %s", r.code, r.stderr)
		}
	})
	t.Run("env", func(t *testing.T) {
		got := captureServer(t, nil)
		r := runServeCLI(map[string]string{DSNEnv: " " + envDSN + "\n", HashKeyEnv: envKey}, "--config", cfgFile)
		if r.code != ExitOK || len(*got) != 1 {
			t.Fatalf("exit %d: %s", r.code, r.stderr)
		}
		o := (*got)[0]
		if o.DSN != envDSN || string(o.HashKey) != envKey {
			t.Errorf("DSN or hash key not taken from the environment")
		}
		if o.Addr != serve.DefaultAddr || o.RecheckInterval != 5*time.Minute || o.MaxConns != serve.DefaultMaxConns || o.Logger == nil {
			t.Errorf("defaults: %+v", o)
		}
		if o.Config.Domain != "film" || o.Config.RowLimit != 50 {
			t.Errorf("config: %+v", o.Config)
		}
	})
	t.Run("files win over env", func(t *testing.T) {
		got := captureServer(t, nil)
		dsnFile := writeFile(t, "dsn", []byte(fileDSN+"\n"))
		keyFile := writeFile(t, "key", []byte(fileKey+"\n"))
		r := runServeCLI(map[string]string{DSNEnv: envDSN, HashKeyEnv: envKey},
			"--config", cfgFile, "--dsn-file", dsnFile, "--hash-key-file", keyFile,
			"--addr", "127.0.0.1:9090", "--recheck-interval", "0", "--row-limit", "10", "--max-conns", "2")
		if r.code != ExitOK || len(*got) != 1 {
			t.Fatalf("exit %d: %s", r.code, r.stderr)
		}
		o := (*got)[0]
		if o.DSN != fileDSN || string(o.HashKey) != fileKey {
			t.Error("files must take precedence over the environment")
		}
		if o.Addr != "127.0.0.1:9090" || o.RecheckInterval != 0 || o.MaxConns != 2 || o.Config.RowLimit != 10 {
			t.Errorf("flags not applied: %+v %+v", o, o.Config)
		}
	})
	t.Run("no hash key", func(t *testing.T) {
		got := captureServer(t, nil)
		if r := runServeCLI(map[string]string{DSNEnv: envDSN}, "--config", cfgFile); r.code != ExitOK || (*got)[0].HashKey != nil {
			t.Errorf("exit %d, hash key %v", r.code, (*got)[0].HashKey)
		}
	})
	t.Run("short hash key", func(t *testing.T) {
		captureServer(t, nil)
		r := runServeCLI(map[string]string{DSNEnv: envDSN, HashKeyEnv: "short"}, "--config", cfgFile)
		if r.code != ExitUsage || !strings.Contains(r.stderr, "at least 16 bytes") || strings.Contains(r.stderr, "short\n") {
			t.Errorf("exit %d: %s", r.code, r.stderr)
		}
	})
	t.Run("bad secret files", func(t *testing.T) {
		captureServer(t, nil)
		secret := testutil.Secret()
		empty := writeFile(t, "empty", []byte(" \n"))
		missing := filepath.Join(t.TempDir(), "missing")
		big := writeFile(t, "big", bytes.Repeat([]byte(secret), maxSecretFileBytes/len(secret)+1))
		for _, args := range [][]string{
			{"--dsn-file", empty}, {"--dsn-file", missing}, {"--dsn-file", big},
			{"--hash-key-file", empty}, {"--hash-key-file", missing},
		} {
			r := runServeCLI(map[string]string{DSNEnv: envDSN}, append([]string{"--config", cfgFile}, args...)...)
			if r.code != ExitError || !strings.Contains(r.stderr, args[0]) {
				t.Errorf("%v: exit %d: %s", args, r.code, r.stderr)
			}
			if strings.Contains(r.stderr, secret) {
				t.Errorf("%v: secret file content echoed", args)
			}
		}
	})
}

func TestServeFromPlan(t *testing.T) {
	env := map[string]string{DSNEnv: pgURL("kabr_api", testutil.Secret(), "db:5432/pagila")}
	got := captureServer(t, nil)
	r := runServeCLI(env, "--plan", approvedPagilaPlan(t, true), "--domain", "film")
	if r.code != ExitOK || len(*got) != 1 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	c := (*got)[0].Config
	if c.Domain != "film" || c.Plan != "pagila" || len(c.Tables) == 0 || !strings.HasPrefix(c.CatalogFingerprint, "sha256:") ||
		strings.Join(c.Schemas, ",") != "public" || c.EffectiveRowLimit() != serve.DefaultRowLimit {
		t.Errorf("config from plan: %+v", c)
	}

	r = runServeCLI(env, "--plan", approvedPagilaPlan(t, false), "--domain", "film")
	if r.code != ExitError || !strings.Contains(r.stderr, "not approved") {
		t.Errorf("proposed plan: exit %d: %s", r.code, r.stderr)
	}
	r = runServeCLI(env, "--plan", approvedPagilaPlan(t, true), "--domain", "nope")
	if r.code != ExitError || !strings.Contains(r.stderr, `has no domain "nope"`) {
		t.Errorf("unknown domain: exit %d: %s", r.code, r.stderr)
	}
	if len(*got) != 1 {
		t.Error("server started for an unusable plan")
	}
}

func TestServeConfigErrorsAndRunnerFailure(t *testing.T) {
	env := map[string]string{DSNEnv: pgURL("kabr_api", testPassword, "db:5432/app")}
	captureServer(t, nil)
	bad := writeFile(t, "bad.json", []byte(`{"domain":"film","Plan":"x"}`))
	if r := runServeCLI(env, "--config", bad); r.code != ExitError || !strings.Contains(r.stderr, "decode config") {
		t.Errorf("bad config: exit %d: %s", r.code, r.stderr)
	}
	if r := runServeCLI(env, "--config", filepath.Join(t.TempDir(), "nope.json")); r.code != ExitError {
		t.Errorf("missing config: exit %d", r.code)
	}

	captureServer(t, errors.New("listen failed for "+env[DSNEnv]))
	r := runServeCLI(env, "--config", serveConfigFile(t))
	if r.code != ExitError || !strings.Contains(r.stderr, "listen failed") {
		t.Errorf("runner failure: exit %d: %s", r.code, r.stderr)
	}
	if strings.Contains(r.stderr, testPassword) {
		t.Error("runner error leaked the DSN password")
	}
}
