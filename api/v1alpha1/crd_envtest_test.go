//go:build envtest

package v1alpha1

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/Gabrielmmorais/kabr/pkg/plan"
)

// This file runs only with "-tags envtest" against a real kube-apiserver and
// etcd started by envtest (make envtest sets KUBEBUILDER_ASSETS). It checks
// that the generated CRDs (config/crd/bases) accept the objects the CLI and
// the operator produce WITHOUT pruning or altering any field of the approved
// plan, and that their schema rejects invalid objects.

const envtestNamespace = "kabr-envtest"

// startEnv boots the control plane with Kabr's CRDs and returns a client.
func startEnv(t *testing.T) client.Client {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is not set; run the envtest suite with \"make envtest\"")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if serr := env.Stop(); serr != nil {
			t.Errorf("stop envtest: %v", serr)
		}
	})
	s := runtime.NewScheme()
	if err = corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err = AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	ctx := testContext(t)
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: envtestNamespace}}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return c
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func validDataSource(name string) *DataSource {
	return &DataSource{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: envtestNamespace},
		Spec: DataSourceSpec{
			Driver:    DriverPostgres,
			SecretRef: DSNSecretRef{Name: "pagila-reader"},
			Network:   DataSourceNetwork{Port: 5432, CIDRs: []string{"10.0.12.0/24", "fd00::/64"}},
		},
	}
}

func TestEnvtestCRDs(t *testing.T) {
	c := startEnv(t)
	ctx := testContext(t)

	t.Run("DataSource round trip and defaults", func(t *testing.T) {
		ds := validDataSource("pagila-db")
		ds.Spec.HashKeySecretRef = &HashKeySecretRef{Name: "pagila-hash"}
		if err := c.Create(ctx, ds); err != nil {
			t.Fatalf("create: %v", err)
		}
		var got DataSource
		if err := c.Get(ctx, client.ObjectKeyFromObject(ds), &got); err != nil {
			t.Fatal(err)
		}
		if got.Spec.SecretRef.Key != DefaultDSNSecretKey || got.Spec.HashKeySecretRef.Key != DefaultHashKeySecretKey {
			t.Errorf("secret key defaults not applied: %+v %+v", got.Spec.SecretRef, got.Spec.HashKeySecretRef)
		}
		if len(got.Spec.Network.CIDRs) != 2 || got.Spec.Network.Port != 5432 {
			t.Errorf("network = %+v", got.Spec.Network)
		}

		inCluster := validDataSource("in-cluster-db")
		inCluster.Spec.Network = DataSourceNetwork{
			Port:              5432,
			PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "postgres"}},
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "databases"}},
		}
		if err := c.Create(ctx, inCluster); err != nil {
			t.Fatalf("create pod-selector data source: %v", err)
		}
	})

	approved := approvedPagila(t)
	cr, err := DomainPlanFromPlan(approved, envtestNamespace, "pagila-db")
	if err != nil {
		t.Fatal(err)
	}
	sent := cr.DeepCopy()

	t.Run("DomainPlan round trip keeps the approval valid", func(t *testing.T) {
		if err := c.Create(ctx, cr); err != nil {
			t.Fatalf("create: %v", err)
		}
		var got DomainPlan
		if err := c.Get(ctx, client.ObjectKeyFromObject(cr), &got); err != nil {
			t.Fatal(err)
		}
		rebuilt := got.ToPlan()
		if err := plan.VerifyApproval(rebuilt); err != nil {
			t.Fatalf("approval no longer verifies after the API server round trip: %v", err)
		}
		if err := rebuilt.ValidateStructure(); err != nil {
			t.Fatalf("ValidateStructure: %v", err)
		}
		if err := rebuilt.Validate(pagilaCatalog(t)); err != nil {
			t.Fatalf("Validate against the Pagila catalog: %v", err)
		}
		// Byte-for-byte: the embedded spec the server returns is the spec
		// that was sent (nothing pruned, defaulted or reordered).
		a, err := json.Marshal(sent.Spec.Spec)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(got.Spec.Spec)
		if err != nil {
			t.Fatal(err)
		}
		if string(a) != string(b) {
			t.Fatalf("embedded plan spec changed:\nsent: %s\ngot:  %s", a, b)
		}
		if ga, sa := got.Spec.Approval, sent.Spec.Approval; ga == nil || ga.ApprovedBy != sa.ApprovedBy ||
			ga.Digest != sa.Digest || !ga.ApprovedAt.Equal(&sa.ApprovedAt) {
			t.Fatalf("approval changed: %+v, sent %+v", ga, sa)
		}
		// Only the CR-level knobs are defaulted.
		if got.Spec.EffectiveRowLimit() != DefaultRowLimit || got.Spec.RowLimit == nil ||
			got.Spec.Replicas == nil || *got.Spec.Replicas != DefaultReplicas {
			t.Errorf("defaults: rowLimit %v replicas %v", got.Spec.RowLimit, got.Spec.Replicas)
		}

		// An edit in the cluster after approval (dropping the sensitive
		// column annotations of a domain) is detected by the operator.
		dropped := false
		for i := range got.Spec.Domains {
			if len(got.Spec.Domains[i].SensitiveColumns) > 0 {
				got.Spec.Domains[i].SensitiveColumns, dropped = nil, true
				break
			}
		}
		if !dropped {
			t.Fatal("test setup: the Pagila plan has no sensitive columns")
		}
		if err := c.Update(ctx, &got); err != nil {
			t.Fatalf("update: %v", err)
		}
		var edited DomainPlan
		if err := c.Get(ctx, client.ObjectKeyFromObject(cr), &edited); err != nil {
			t.Fatal(err)
		}
		if err := plan.VerifyApproval(edited.ToPlan()); !errors.Is(err, plan.ErrEditedAfterApproval) {
			t.Fatalf("edited in the cluster: VerifyApproval = %v, want ErrEditedAfterApproval", err)
		}
	})

	t.Run("DomainAPI round trip and defaults", func(t *testing.T) {
		d := approved.Spec.Domains[0]
		tables := make([]string, 0, len(d.Tables))
		for _, ta := range d.Tables {
			tables = append(tables, ta.Name)
		}
		api := &DomainAPI{
			ObjectMeta: metav1.ObjectMeta{Name: "pagila-" + d.Name, Namespace: envtestNamespace},
			Spec: DomainAPISpec{
				Domain:             d.Name,
				PlanRef:            LocalObjectReference{Name: "pagila"},
				DataSourceRef:      LocalObjectReference{Name: "pagila-db"},
				CatalogFingerprint: approved.Spec.Source.CatalogFingerprint,
				Tables:             tables,
				SensitiveColumns:   d.SensitiveColumns,
				Replicas:           0,
			},
		}
		if err := c.Create(ctx, api); err != nil {
			t.Fatalf("create: %v", err)
		}
		var got DomainAPI
		if err := c.Get(ctx, client.ObjectKeyFromObject(api), &got); err != nil {
			t.Fatal(err)
		}
		if got.Spec.RowLimit != DefaultRowLimit || got.Spec.Replicas != 0 {
			t.Errorf("rowLimit %d (want default %d), replicas %d (want explicit 0)", got.Spec.RowLimit, DefaultRowLimit, got.Spec.Replicas)
		}
		a, _ := json.Marshal(api.Spec.SensitiveColumns)
		b, _ := json.Marshal(got.Spec.SensitiveColumns)
		if string(a) != string(b) {
			t.Errorf("sensitive columns changed:\nsent: %s\ngot:  %s", a, b)
		}
	})

	t.Run("schema rejects invalid objects", func(t *testing.T) {
		badPlan := func(name string, mutate func(p *DomainPlan)) *DomainPlan {
			p := sent.DeepCopy()
			p.Name = name
			mutate(p)
			return p
		}
		badDS := func(name string, mutate func(ds *DataSource)) *DataSource {
			ds := validDataSource(name)
			mutate(ds)
			return ds
		}
		tests := []struct {
			name string
			obj  client.Object
			want string // substring of the API server's message
		}{
			{"bad digest pattern", badPlan("bad-digest", func(p *DomainPlan) { p.Spec.Approval.Digest = "md5:abc" }), "spec.approval.digest"},
			{"rowLimit 0", badPlan("row-limit-zero", func(p *DomainPlan) { p.Spec.RowLimit = int32Ptr(0) }), "spec.rowLimit"},
			{"rowLimit 1001", badPlan("row-limit-big", func(p *DomainPlan) { p.Spec.RowLimit = int32Ptr(1001) }), "spec.rowLimit"},
			{"replicas 11", badPlan("replicas-big", func(p *DomainPlan) { p.Spec.Replicas = int32Ptr(11) }), "spec.replicas"},
			{"domain name not a DNS label", badPlan("bad-domain", func(p *DomainPlan) { p.Spec.Domains[0].Name = "Bad_Domain" }), "spec.domains[0].name"},
			{"domain without tables", badPlan("no-tables", func(p *DomainPlan) { p.Spec.Domains[0].Tables = nil }), "spec.domains[0].tables"},
			{"no domains", badPlan("no-domains", func(p *DomainPlan) { p.Spec.Domains = nil }), "spec.domains"},
			{"bad sensitive action", badPlan("bad-action", func(p *DomainPlan) {
				p.Spec.Domains[1].SensitiveColumns[0].Action = "encrypt"
			}), "action"},
			{"bad catalog fingerprint", badPlan("bad-fingerprint", func(p *DomainPlan) { p.Spec.Source.CatalogFingerprint = "abc" }), "spec.source.catalogFingerprint"},
			{"missing dataSourceRef", badPlan("no-datasource", func(p *DomainPlan) { p.Spec.DataSourceRef.Name = "" }), "spec.dataSourceRef.name"},
			{"network without cidrs or podSelector", badDS("no-network", func(ds *DataSource) { ds.Spec.Network.CIDRs = nil }), "at least one of cidrs or podSelector"},
			{"namespaceSelector without podSelector", badDS("ns-only", func(ds *DataSource) {
				ds.Spec.Network.NamespaceSelector = &metav1.LabelSelector{}
			}), "namespaceSelector requires podSelector"},
			{"invalid CIDR", badDS("bad-cidr", func(ds *DataSource) { ds.Spec.Network.CIDRs = []string{"10.0.0.300/24"} }), "valid CIDR"},
			{"port 0", badDS("port-zero", func(ds *DataSource) { ds.Spec.Network.Port = 0 }), "spec.network.port"},
			{"unknown driver", badDS("mysql", func(ds *DataSource) { ds.Spec.Driver = "mysql" }), "spec.driver"},
			{"no secret name", badDS("no-secret", func(ds *DataSource) { ds.Spec.SecretRef.Name = "" }), "spec.secretRef.name"},
			{"DomainAPI rowLimit too big", &DomainAPI{
				ObjectMeta: metav1.ObjectMeta{Name: "too-many-rows", Namespace: envtestNamespace},
				Spec: DomainAPISpec{
					Domain: "film", PlanRef: LocalObjectReference{Name: "pagila"}, DataSourceRef: LocalObjectReference{Name: "pagila-db"},
					CatalogFingerprint: approved.Spec.Source.CatalogFingerprint, Tables: []string{"public.film"}, RowLimit: 5000, Replicas: 1,
				},
			}, "spec.rowLimit"},
			{"DomainAPI without tables", &DomainAPI{
				ObjectMeta: metav1.ObjectMeta{Name: "no-tables", Namespace: envtestNamespace},
				Spec: DomainAPISpec{
					Domain: "film", PlanRef: LocalObjectReference{Name: "pagila"}, DataSourceRef: LocalObjectReference{Name: "pagila-db"},
					CatalogFingerprint: approved.Spec.Source.CatalogFingerprint, Replicas: 1,
				},
			}, "spec.tables"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				err := c.Create(ctx, tt.obj)
				if err == nil {
					t.Fatal("invalid object was accepted")
				}
				if !apierrors.IsInvalid(err) {
					t.Fatalf("want an Invalid error, got %v", err)
				}
				if !strings.Contains(err.Error(), tt.want) {
					t.Errorf("error %q does not mention %q", err, tt.want)
				}
			})
		}
	})
}

func int32Ptr(v int32) *int32 { return &v }
