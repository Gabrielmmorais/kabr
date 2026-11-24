package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-logr/logr"

	kabrv1 "github.com/Gabrielmmorais/kabr/api/v1alpha1"
	"github.com/Gabrielmmorais/kabr/internal/controller"
)

// captureOperator replaces operatorRunner for one test (no cluster).
func captureOperator(t *testing.T, err error) *[]operatorFlags {
	t.Helper()
	var got []operatorFlags
	prev := operatorRunner
	operatorRunner = func(_ context.Context, f *operatorFlags, _ logr.Logger) error {
		got = append(got, *f)
		return err
	}
	t.Cleanup(func() { operatorRunner = prev })
	return &got
}

func runOperatorCLI(env map[string]string, args ...string) result {
	var out, errOut bytes.Buffer
	code := Run(context.Background(), append([]string{"operator"}, args...), Env{
		Stdout: &out, Stderr: &errOut,
		Getenv:  func(k string) string { return env[k] },
		Version: "v0.0.0-test",
	})
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

func TestOperatorFlags(t *testing.T) {
	got := captureOperator(t, nil)
	r := runOperatorCLI(map[string]string{controller.RuntimeImageEnv: "kabr:env"})
	if r.code != ExitOK {
		t.Fatalf("code %d, stderr %s", r.code, r.stderr)
	}
	want := operatorFlags{metricsAddr: "0", probeAddr: ":8081", leaderElect: true, runtimeImage: "kabr:env"}
	if len(*got) != 1 || (*got)[0] != want {
		t.Fatalf("flags = %+v, want %+v", *got, want)
	}

	*got = nil
	r = runOperatorCLI(map[string]string{controller.RuntimeImageEnv: "kabr:env"},
		"--runtime-image", "kabr:flag", "--leader-elect=false", "--watch-namespace", "kabr-demo",
		"--leader-election-namespace", "kabr-system", "--metrics-bind-address", ":8443", "--health-probe-bind-address", "0")
	if r.code != ExitOK {
		t.Fatalf("code %d, stderr %s", r.code, r.stderr)
	}
	want = operatorFlags{metricsAddr: ":8443", probeAddr: "0", runtimeImage: "kabr:flag",
		watchNamespace: "kabr-demo", leaderElectionNamespace: "kabr-system"}
	if (*got)[0] != want {
		t.Fatalf("flags = %+v, want %+v", (*got)[0], want)
	}

	opts := managerOptions(&want, nil)
	if !opts.Metrics.SecureServing || opts.Metrics.BindAddress != ":8443" || opts.LeaderElection ||
		opts.LeaderElectionID != operatorLeaderElectionID || opts.HealthProbeBindAddress != "0" {
		t.Errorf("manager options = %+v", opts)
	}
	if _, ok := opts.Cache.DefaultNamespaces["kabr-demo"]; !ok || len(opts.Cache.DefaultNamespaces) != 1 {
		t.Errorf("cache namespaces = %v", opts.Cache.DefaultNamespaces)
	}
	if opts.Client.Cache == nil || len(opts.Client.Cache.DisableFor) != 1 {
		t.Error("Secrets must never be cached")
	}
	if s, err := newOperatorScheme(); err != nil || !s.IsVersionRegistered(kabrv1.GroupVersion) {
		t.Errorf("scheme: %v", err)
	}
}

func TestOperatorUsageErrors(t *testing.T) {
	captureOperator(t, nil)
	img := map[string]string{controller.RuntimeImageEnv: "kabr:env"}
	tests := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"no runtime image", nil, nil, "no runtime image"},
		{"blank runtime image", nil, []string{"--runtime-image", "  "}, "no runtime image"},
		{"bad watch namespace", img, []string{"--watch-namespace", "Not_Valid"}, "--watch-namespace"},
		{"bad lease namespace", img, []string{"--leader-election-namespace", "a.b"}, "--leader-election-namespace"},
		{"empty probe address", img, []string{"--health-probe-bind-address", ""}, "must not be empty"},
		{"positional argument", img, []string{"extra"}, "unexpected argument"},
		{"unknown flag", img, []string{"--nope"}, "flag provided but not defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runOperatorCLI(tt.env, tt.args...)
			if r.code != ExitUsage || !strings.Contains(r.stderr, tt.want) {
				t.Errorf("code %d, stderr %q; want usage error containing %q", r.code, r.stderr, tt.want)
			}
		})
	}
}

func TestOperatorHelpAndFailure(t *testing.T) {
	captureOperator(t, errors.New("boom"))
	r := runOperatorCLI(nil, "--help")
	if r.code != ExitOK || !strings.Contains(r.stdout, "Usage: kabr operator") ||
		!strings.Contains(r.stdout, "--runtime-image") || !strings.Contains(r.stdout, controller.RuntimeImageEnv) {
		t.Errorf("help: code %d stdout %q", r.code, r.stdout)
	}
	var out bytes.Buffer
	if code := Run(context.Background(), []string{"help", "operator"}, Env{Stdout: &out}); code != ExitOK ||
		!strings.Contains(out.String(), "--leader-elect") {
		t.Errorf("kabr help operator: code %d out %q", code, out.String())
	}
	out.Reset()
	if Run(context.Background(), []string{"help"}, Env{Stdout: &out}); !strings.Contains(out.String(), "operator") ||
		!strings.Contains(out.String(), "manifest") {
		t.Errorf("main usage lacks operator/manifest: %s", out.String())
	}
	r = runOperatorCLI(map[string]string{controller.RuntimeImageEnv: "kabr:env"})
	if r.code != ExitError || !strings.Contains(r.stderr, "boom") {
		t.Errorf("runner failure: code %d stderr %q", r.code, r.stderr)
	}
}
