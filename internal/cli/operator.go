package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	kabrv1 "github.com/Gabrielmmorais/kabr/api/v1alpha1"
	"github.com/Gabrielmmorais/kabr/internal/controller"
	"github.com/Gabrielmmorais/kabr/pkg/introspect"
)

// operatorLeaderElectionID is the name of the leader election Lease.
const operatorLeaderElectionID = "kabr-operator.kabr.dev"

type operatorFlags struct {
	metricsAddr             string
	probeAddr               string
	leaderElect             bool
	leaderElectionNamespace string
	runtimeImage            string
	watchNamespace          string
}

func newOperatorFlags(getenv func(string) string) (*flag.FlagSet, *operatorFlags) {
	f := &operatorFlags{}
	fs := flag.NewFlagSet("operator", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&f.metricsAddr, "metrics-bind-address", "0",
		"address of the HTTPS metrics endpoint (self-signed certificate, no authentication: protect it with a NetworkPolicy); \"0\" disables it")
	fs.StringVar(&f.probeAddr, "health-probe-bind-address", ":8081", "address of /healthz and /readyz; \"0\" disables them")
	fs.BoolVar(&f.leaderElect, "leader-elect", true, "elect a leader (a Lease) so that only one replica reconciles; use --leader-elect=false outside a cluster")
	fs.StringVar(&f.leaderElectionNamespace, "leader-election-namespace", "",
		"namespace of the leader election Lease (default: the operator's namespace when running in a cluster)")
	fs.StringVar(&f.runtimeImage, "runtime-image", getenv(controller.RuntimeImageEnv),
		"image of every DomainAPI pod (\"kabr serve\"; DomainAPIs cannot override it) (default $"+controller.RuntimeImageEnv+")")
	fs.StringVar(&f.watchNamespace, "watch-namespace", "", "reconcile objects in this namespace only (default: all namespaces)")
	return fs, f
}

func printOperatorUsage(w io.Writer, fs *flag.FlagSet) {
	fmt.Fprintf(w, `Usage: kabr operator [flags]

Run the Kabr Kubernetes operator (kabr.dev/v1alpha1). It reconciles:

  DataSource  checks the driver and that the referenced Secrets hold the
              referenced keys (it never connects to the database)
  DomainPlan  serves nothing unless the approval verifies (digest of the
              approved spec); owns one DomainAPI per domain and deletes them
              when the approval breaks (fail closed)
  DomainAPI   renders a ConfigMap, a Deployment running "kabr serve", a
              Service and a NetworkPolicy (egress only to the database and DNS)

The cluster connection comes from $KUBECONFIG, ~/.kube/config or the
in-cluster service account. Logs are JSON lines on stderr. SIGINT/SIGTERM
stop the operator gracefully (and release the leader Lease).

Flags:
`)
	fs.SetOutput(w)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)
	fmt.Fprintf(w, `
Examples:
  kabr operator --leader-elect=false --runtime-image ghcr.io/example/kabr:v0.3.0
  %s=kabr:dev kabr operator --watch-namespace kabr-demo
`, controller.RuntimeImageEnv)
}

func parseOperator(args []string, env *Env) (*operatorFlags, error) {
	fs, f := newOperatorFlags(env.Getenv)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printOperatorUsage(env.Stdout, fs)
			return nil, &exitError{code: ExitOK}
		}
		return nil, usageErr("operator: %s", introspect.RedactText(err.Error()))
	}
	if fs.NArg() > 0 {
		return nil, usageErr("operator: unexpected argument %q (flags only)", introspect.RedactText(fs.Arg(0)))
	}
	f.runtimeImage = strings.TrimSpace(f.runtimeImage)
	if f.runtimeImage == "" {
		return nil, usageErr("operator: no runtime image; pass --runtime-image or set $%s", controller.RuntimeImageEnv)
	}
	if strings.ContainsAny(f.runtimeImage, " \t\n") {
		return nil, usageErr("operator: --runtime-image %q contains whitespace", f.runtimeImage)
	}
	for name, ns := range map[string]string{"--watch-namespace": f.watchNamespace, "--leader-election-namespace": f.leaderElectionNamespace} {
		if ns != "" && len(validation.IsDNS1123Label(ns)) > 0 {
			return nil, usageErr("operator: %s %q is not a valid namespace name", name, ns)
		}
	}
	if strings.TrimSpace(f.metricsAddr) == "" || strings.TrimSpace(f.probeAddr) == "" {
		return nil, usageErr("operator: bind addresses must not be empty (use \"0\" to disable)")
	}
	return f, nil
}

// operatorRunner runs the manager; tests replace it so that no cluster is
// needed.
var operatorRunner = runManager

func runOperator(ctx context.Context, args []string, env *Env) error {
	f, err := parseOperator(args, env)
	if err != nil {
		return err
	}
	logger := logr.FromSlogHandler(slog.NewJSONHandler(env.Stderr, nil))
	return operatorRunner(ctx, f, logger)
}

// managerOptions builds the controller-runtime manager options of f.
func managerOptions(f *operatorFlags, scheme *runtime.Scheme) ctrl.Options {
	opts := ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress:   f.metricsAddr,
			SecureServing: true,
		},
		HealthProbeBindAddress:        f.probeAddr,
		LeaderElection:                f.leaderElect,
		LeaderElectionID:              operatorLeaderElectionID,
		LeaderElectionNamespace:       f.leaderElectionNamespace,
		LeaderElectionReleaseOnCancel: true,
		GracefulShutdownTimeout:       ptrDuration(30 * time.Second),
		Client: client.Options{Cache: &client.CacheOptions{
			// Never cache Secrets (their data would sit in memory): the
			// DataSource check reads them uncached, keys only.
			DisableFor: []client.Object{&corev1.Secret{}},
		}},
	}
	if f.watchNamespace != "" {
		opts.Cache = cache.Options{DefaultNamespaces: map[string]cache.Config{f.watchNamespace: {}}}
	}
	return opts
}

func ptrDuration(d time.Duration) *time.Duration { return &d }

// newOperatorScheme returns the scheme of the operator: the built-in kinds
// plus kabr.dev/v1alpha1.
func newOperatorScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := kabrv1.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

func runManager(ctx context.Context, f *operatorFlags, logger logr.Logger) error {
	ctrl.SetLogger(logger)
	scheme, err := newOperatorScheme()
	if err != nil {
		return fmt.Errorf("operator: scheme: %w", err)
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("operator: cluster config: %w", err)
	}
	mgr, err := ctrl.NewManager(cfg, managerOptions(f, scheme))
	if err != nil {
		return fmt.Errorf("operator: manager: %w", err)
	}
	if err := controller.Setup(ctx, mgr, controller.Options{RuntimeImage: f.runtimeImage}); err != nil {
		return fmt.Errorf("operator: %w", err)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("operator: healthz: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("operator: readyz: %w", err)
	}
	logger.Info("starting operator", "runtimeImage", f.runtimeImage, "watchNamespace", f.watchNamespace,
		"leaderElect", f.leaderElect)
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("operator: %w", err)
	}
	return nil
}
