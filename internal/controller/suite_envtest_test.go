//go:build envtest

package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	kabrv1 "github.com/Gabrielmmorais/kabr/api/v1alpha1"
)

// The envtest suite runs only with "-tags envtest" (make envtest) against a
// real kube-apiserver and etcd, with the three reconcilers running in a
// manager. envtest has no kube-controller-manager: no Deployment controller
// (tests patch Deployment status themselves) and no garbage collector (tests
// assert owner references, and only the deletes the operator performs).

const testRuntimeImage = "kabr:envtest"

// k8sClient talks to the envtest API server directly (no cache).
var k8sClient client.Client

func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

func runSuite(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintln(os.Stderr, "KUBEBUILDER_ASSETS is not set; run the envtest suite with \"make envtest\"")
		return 1
	}
	ctrl.SetLogger(logr.Discard())
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "start envtest: %v\n", err)
		return 1
	}
	defer func() {
		if serr := env.Stop(); serr != nil {
			fmt.Fprintf(os.Stderr, "stop envtest: %v\n", serr)
		}
	}()

	scheme := runtime.NewScheme()
	if err = clientgoscheme.AddToScheme(scheme); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err = kabrv1.AddToScheme(scheme); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if k8sClient, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		Client:                 client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}}},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "manager: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := Setup(ctx, mgr, Options{RuntimeImage: testRuntimeImage, SecretRecheckInterval: time.Second}); err != nil {
		fmt.Fprintf(os.Stderr, "setup: %v\n", err)
		return 1
	}
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	code := m.Run()
	cancel()
	if err := <-done; err != nil {
		fmt.Fprintf(os.Stderr, "manager: %v\n", err)
	}
	return code
}

var nsCounter atomic.Int32

// newNamespace creates a fresh namespace for one test.
func newNamespace(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("kabr-test-%d", nsCounter.Add(1))
	if err := k8sClient.Create(testCtx(t), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return name
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// eventually polls cond until it returns "" or the timeout passes; the last
// non-empty string is the failure message.
func eventually(t *testing.T, what string, cond func() string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		if last = cond(); last == "" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s: %s", what, last)
}
