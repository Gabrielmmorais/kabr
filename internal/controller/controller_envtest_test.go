//go:build envtest

package controller

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kabrv1 "github.com/Gabrielmmorais/kabr/api/v1alpha1"
	"github.com/Gabrielmmorais/kabr/pkg/plan"
	"github.com/Gabrielmmorais/kabr/pkg/serve"
)

// secretPlaceholder is a fake, non-credential Secret value: the operator
// only checks that the key exists.
const secretPlaceholder = "placeholder-not-a-credential"

// approvedPagilaCR loads examples/pagila.plan.yaml, approves it in memory
// and converts it into a DomainPlan of namespace ns bound to DataSource
// "pagila-db".
func approvedPagilaCR(t *testing.T, ns string) *kabrv1.DomainPlan {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "pagila.plan.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err = plan.Approve(p, "alice@example.com", time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	cr, err := kabrv1.DomainPlanFromPlan(p, ns, "pagila-db")
	if err != nil {
		t.Fatal(err)
	}
	return cr
}

// createDataSource creates the DSN Secret and the DataSource "pagila-db".
func createDataSource(t *testing.T, ns string) *kabrv1.DataSource {
	t.Helper()
	ctx := testCtx(t)
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "pagila-reader", Namespace: ns},
		StringData: map[string]string{"dsn": secretPlaceholder},
	}
	if err := k8sClient.Create(ctx, sec); err != nil {
		t.Fatal(err)
	}
	ds := &kabrv1.DataSource{
		ObjectMeta: metav1.ObjectMeta{Name: "pagila-db", Namespace: ns},
		Spec: kabrv1.DataSourceSpec{
			Driver:    kabrv1.DriverPostgres,
			SecretRef: kabrv1.DSNSecretRef{Name: "pagila-reader"},
			Network:   kabrv1.DataSourceNetwork{Port: 5432, CIDRs: []string{"10.0.12.0/24"}},
		},
	}
	if err := k8sClient.Create(ctx, ds); err != nil {
		t.Fatal(err)
	}
	return ds
}

func readyCondition(conds []metav1.Condition) *metav1.Condition {
	return meta.FindStatusCondition(conds, kabrv1.ConditionReady)
}

func waitPlanReason(t *testing.T, ns, name, reason string) *kabrv1.DomainPlan {
	t.Helper()
	var cr kabrv1.DomainPlan
	eventually(t, "DomainPlan reason "+reason, func() string {
		if err := k8sClient.Get(testCtx(t), client.ObjectKey{Namespace: ns, Name: name}, &cr); err != nil {
			return err.Error()
		}
		c := readyCondition(cr.Status.Conditions)
		if c == nil || c.Reason != reason || cr.Status.ObservedGeneration != cr.Generation || c.ObservedGeneration != cr.Generation {
			return fmt.Sprintf("condition %+v, observedGeneration %d/%d", c, cr.Status.ObservedGeneration, cr.Generation)
		}
		return ""
	})
	return &cr
}

func listDomainAPIs(t *testing.T, ns string) []kabrv1.DomainAPI {
	t.Helper()
	var list kabrv1.DomainAPIList
	if err := k8sClient.List(testCtx(t), &list, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

func waitDomainAPIs(t *testing.T, ns string, want []string) []kabrv1.DomainAPI {
	t.Helper()
	var items []kabrv1.DomainAPI
	eventually(t, fmt.Sprintf("DomainAPIs %v", want), func() string {
		items = nil
		var names []string
		for _, a := range listDomainAPIs(t, ns) {
			if a.DeletionTimestamp.IsZero() {
				items = append(items, a)
				names = append(names, a.Name)
			}
		}
		slices.Sort(names)
		if !slices.Equal(names, want) {
			return fmt.Sprintf("have %v", names)
		}
		return ""
	})
	return items
}

func TestEnvtestDataSource(t *testing.T) {
	ns := newNamespace(t)
	ctx := testCtx(t)
	ds := &kabrv1.DataSource{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: ns},
		Spec: kabrv1.DataSourceSpec{
			Driver:           kabrv1.DriverPostgres,
			SecretRef:        kabrv1.DSNSecretRef{Name: "db-reader"},
			HashKeySecretRef: &kabrv1.HashKeySecretRef{Name: "db-hash"},
			Network:          kabrv1.DataSourceNetwork{Port: 5432, CIDRs: []string{"10.0.0.0/8"}},
		},
	}
	if err := k8sClient.Create(ctx, ds); err != nil {
		t.Fatal(err)
	}
	waitDS := func(reason string) {
		t.Helper()
		eventually(t, "DataSource reason "+reason, func() string {
			var got kabrv1.DataSource
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(ds), &got); err != nil {
				return err.Error()
			}
			c := readyCondition(got.Status.Conditions)
			if c == nil || c.Reason != reason || got.Status.ObservedGeneration != got.Generation {
				return fmt.Sprintf("%+v", c)
			}
			if strings.Contains(c.Message, secretPlaceholder) {
				t.Fatalf("Secret value leaked into the condition: %q", c.Message)
			}
			return ""
		})
	}
	waitDS(ReasonSecretNotFound)

	// The Secret appears without the key: the metadata watch re-triggers.
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db-reader", Namespace: ns},
		StringData: map[string]string{"other": secretPlaceholder}}
	if err := k8sClient.Create(ctx, sec); err != nil {
		t.Fatal(err)
	}
	waitDS(ReasonSecretKeyMissing)

	sec.StringData = map[string]string{"dsn": secretPlaceholder}
	if err := k8sClient.Update(ctx, sec); err != nil {
		t.Fatal(err)
	}
	waitDS(ReasonSecretNotFound) // now the hash key Secret is missing

	if err := k8sClient.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db-hash", Namespace: ns},
		StringData: map[string]string{"key": strings.Repeat("k", 32)}}); err != nil {
		t.Fatal(err)
	}
	waitDS(ReasonReconciled)
}

func TestEnvtestDomainPlanLifecycle(t *testing.T) {
	ns := newNamespace(t)
	ctx := testCtx(t)

	// (a) Approved Pagila plan without its DataSource: nothing is created.
	cr := approvedPagilaCR(t, ns)
	if err := k8sClient.Create(ctx, cr); err != nil {
		t.Fatal(err)
	}
	waitPlanReason(t, ns, "pagila", ReasonDataSourceNotFound)
	if n := len(listDomainAPIs(t, ns)); n != 0 {
		t.Fatalf("%d DomainAPIs created without a DataSource", n)
	}

	// With the DataSource: 3 DomainAPIs with owner refs and correct specs.
	createDataSource(t, ns)
	got := waitPlanReason(t, ns, "pagila", ReasonReconciled)
	want := []string{"pagila-city", "pagila-film", "pagila-store"}
	if !slices.Equal(got.Status.DomainAPIs, want) {
		t.Errorf("status.domainAPIs = %v", got.Status.DomainAPIs)
	}
	apis := waitDomainAPIs(t, ns, want)
	wantSchemas := PlanSchemas(&cr.Spec.Spec)
	for _, a := range apis {
		if !metav1.IsControlledBy(&a, got) {
			t.Errorf("%s: not controlled by the plan: %+v", a.Name, a.OwnerReferences)
		}
		d := cr.ToPlan().Domain(a.Spec.Domain)
		if d == nil || a.Name != "pagila-"+a.Spec.Domain {
			t.Fatalf("%s: unexpected domain %q", a.Name, a.Spec.Domain)
		}
		if a.Labels[LabelManagedBy] != ManagedByValue || a.Labels[LabelPlan] != "pagila" || a.Labels[LabelDomain] != d.Name {
			t.Errorf("%s: labels %v", a.Name, a.Labels)
		}
		if a.Spec.CatalogFingerprint != cr.Spec.Source.CatalogFingerprint || !slices.Equal(a.Spec.Schemas, wantSchemas) ||
			a.Spec.Replicas != kabrv1.DefaultReplicas || a.Spec.RowLimit != kabrv1.DefaultRowLimit ||
			a.Spec.DataSourceRef.Name != "pagila-db" || a.Spec.PlanRef.Name != "pagila" || len(a.Spec.Tables) != len(d.Tables) ||
			len(a.Spec.SensitiveColumns) != len(d.SensitiveColumns) {
			t.Errorf("%s: spec %+v", a.Name, a.Spec)
		}
	}

	// replicas: 0 on the plan is copied explicitly.
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(cr), got); err != nil {
		t.Fatal(err)
	}
	got.Spec.Replicas = new(int32)
	if err := k8sClient.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	eventually(t, "replicas 0 copied", func() string {
		for _, a := range listDomainAPIs(t, ns) {
			if a.Spec.Replicas != 0 {
				return a.Name + " still has replicas " + fmt.Sprint(a.Spec.Replicas)
			}
		}
		return ""
	})

	// (c) Remove a domain (and re-approve): its DomainAPI is deleted.
	p := got.ToPlan()
	p.Spec.Domains = slices.DeleteFunc(p.Spec.Domains, func(d plan.Domain) bool { return d.Name == "city" })
	if err := plan.Approve(p, "alice@example.com", time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(cr), got); err != nil {
		t.Fatal(err)
	}
	got.Spec.Spec = plan.CanonicalSpec(p.Spec)
	got.Spec.Approval.Digest = p.Status.ApprovalDigest
	got.Spec.Approval.ApprovedAt = metav1.NewTime(*p.Status.ApprovedAt)
	if err := k8sClient.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	waitDomainAPIs(t, ns, []string{"pagila-film", "pagila-store"})
	if got = waitPlanReason(t, ns, "pagila", ReasonReconciled); !slices.Equal(got.Status.DomainAPIs, []string{"pagila-film", "pagila-store"}) {
		t.Errorf("status.domainAPIs = %v", got.Status.DomainAPIs)
	}

	// (b) Edit the spec after approval: EditedAfterApproval and every
	// DomainAPI is deleted (fail closed).
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(cr), got); err != nil {
		t.Fatal(err)
	}
	for i := range got.Spec.Domains {
		got.Spec.Domains[i].SensitiveColumns = nil
	}
	if err := k8sClient.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	got = waitPlanReason(t, ns, "pagila", ReasonEditedAfterApproval)
	waitDomainAPIs(t, ns, nil)
	if c := readyCondition(got.Status.Conditions); c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "fail closed") {
		t.Errorf("condition = %+v", c)
	}
	if len(got.Status.DomainAPIs) != 0 {
		t.Errorf("status.domainAPIs = %v", got.Status.DomainAPIs)
	}

	// Forged digest and a removed approval are refused the same way.
	got.Spec.Approval.Digest = "sha256:" + strings.Repeat("0", 64)
	if err := k8sClient.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	got = waitPlanReason(t, ns, "pagila", ReasonEditedAfterApproval)
	got.Spec.Approval = nil
	if err := k8sClient.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	waitPlanReason(t, ns, "pagila", ReasonNotApproved)
	if n := len(listDomainAPIs(t, ns)); n != 0 {
		t.Errorf("%d DomainAPIs exist for an unapproved plan", n)
	}
}

// setupServedPlan creates a DataSource and an approved Pagila plan and waits
// for the film DomainAPI and its Deployment.
func setupServedPlan(t *testing.T) (ns string, api *kabrv1.DomainAPI) {
	t.Helper()
	ns = newNamespace(t)
	createDataSource(t, ns)
	if err := k8sClient.Create(testCtx(t), approvedPagilaCR(t, ns)); err != nil {
		t.Fatal(err)
	}
	waitPlanReason(t, ns, "pagila", ReasonReconciled)
	waitDomainAPIs(t, ns, []string{"pagila-city", "pagila-film", "pagila-store"})
	api = &kabrv1.DomainAPI{}
	if err := k8sClient.Get(testCtx(t), client.ObjectKey{Namespace: ns, Name: "pagila-film"}, api); err != nil {
		t.Fatal(err)
	}
	waitObject(t, ns, "pagila-film", &appsv1.Deployment{})
	return ns, api
}

func waitObject(t *testing.T, ns, name string, obj client.Object) {
	t.Helper()
	eventually(t, fmt.Sprintf("%T %s", obj, name), func() string {
		if err := k8sClient.Get(testCtx(t), client.ObjectKey{Namespace: ns, Name: name}, obj); err != nil {
			return err.Error()
		}
		return ""
	})
}

func waitGone(t *testing.T, ns, name string, obj client.Object) {
	t.Helper()
	eventually(t, fmt.Sprintf("%T %s deleted", obj, name), func() string {
		err := k8sClient.Get(testCtx(t), client.ObjectKey{Namespace: ns, Name: name}, obj)
		switch {
		case apierrors.IsNotFound(err):
			return ""
		case err != nil:
			return err.Error()
		case !obj.GetDeletionTimestamp().IsZero():
			return "" // deleting (no garbage collector / finalizers in envtest)
		}
		return "still exists"
	})
}

func readConfig(t *testing.T, ns, api string) (*serve.Config, string) {
	t.Helper()
	var cm corev1.ConfigMap
	waitObject(t, ns, ConfigMapName(api), &cm)
	raw := cm.Data[ConfigFileName]
	cfg, err := serve.DecodeConfig(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("domain.json does not decode: %v\n%s", err, raw)
	}
	return cfg, raw
}

func TestEnvtestDomainAPIWorkload(t *testing.T) {
	ns, api := setupServedPlan(t)
	ctx := testCtx(t)

	// (d) ConfigMap: a valid serve.Config built from the plan, no secrets.
	cfg, raw := readConfig(t, ns, api.Name)
	if cfg.Domain != "film" || cfg.Plan != "pagila" || cfg.CatalogFingerprint != api.Spec.CatalogFingerprint ||
		!slices.Equal(cfg.Schemas, api.Spec.Schemas) || !slices.Equal(cfg.Tables, api.Spec.Tables) ||
		len(cfg.SensitiveColumns) == 0 || cfg.RowLimit != int(kabrv1.DefaultRowLimit) {
		t.Errorf("config = %+v", cfg)
	}
	if strings.Contains(raw, secretPlaceholder) || strings.Contains(raw, "pagila-reader") {
		t.Errorf("domain.json contains secret material or references:\n%s", raw)
	}

	var dep appsv1.Deployment
	waitObject(t, ns, api.Name, &dep)
	if !metav1.IsControlledBy(&dep, api) {
		t.Errorf("deployment owner refs %+v", dep.OwnerReferences)
	}
	pod := dep.Spec.Template.Spec
	c := pod.Containers[0]
	if c.Image != testRuntimeImage || !slices.Equal(c.Args[:5], []string{"serve", "--config", "/etc/kabr/domain.json", "--dsn-file", "/var/run/secrets/kabr/dsn"}) ||
		slices.Contains(c.Args, "--hash-key-file") || len(c.Env) != 0 {
		t.Errorf("container = %+v", c)
	}
	if *dep.Spec.Replicas != 1 || c.ReadinessProbe.HTTPGet.Path != "/readyz" || c.LivenessProbe.HTTPGet.Path != "/healthz" ||
		!*pod.SecurityContext.RunAsNonRoot || !*c.SecurityContext.ReadOnlyRootFilesystem || *pod.AutomountServiceAccountToken {
		t.Errorf("deployment spec = %+v", dep.Spec)
	}
	var dsnSecret string
	for _, v := range pod.Volumes {
		if v.Secret != nil && v.Name == dsnVolume {
			dsnSecret = v.Secret.SecretName
		}
	}
	if dsnSecret != "pagila-reader" {
		t.Errorf("dsn volume secret = %q", dsnSecret)
	}
	if strings.Contains(fmt.Sprint(dep.Annotations, dep.Spec.Template.Annotations), secretPlaceholder) {
		t.Error("secret value in annotations")
	}
	sum := dep.Spec.Template.Annotations[AnnotationConfigChecksum]
	if sum != ConfigChecksum([]byte(raw)) {
		t.Errorf("checksum %q does not match the ConfigMap", sum)
	}

	var svc corev1.Service
	waitObject(t, ns, api.Name, &svc)
	if svc.Spec.Ports[0].Port != 8080 || svc.Spec.Selector[LabelDomainAPI] != api.Name || !metav1.IsControlledBy(&svc, api) {
		t.Errorf("service = %+v", svc.Spec)
	}
	var np networkingv1.NetworkPolicy
	waitObject(t, ns, api.Name, &np)
	checkNetworkPolicy(t, &np, api.Name, 5432, 1)
	if np.Spec.Egress[1].To[0].IPBlock == nil || np.Spec.Egress[1].To[0].IPBlock.CIDR != "10.0.12.0/24" {
		t.Errorf("db peer = %+v", np.Spec.Egress[1].To)
	}

	// (e) Status: Progressing until the Deployment is available.
	waitAPIReady(t, ns, api.Name, metav1.ConditionFalse, ReasonProgressing)
	dep.Status = appsv1.DeploymentStatus{ObservedGeneration: dep.Generation, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
	if err := k8sClient.Status().Update(ctx, &dep); err != nil {
		t.Fatal(err)
	}
	got := waitAPIReady(t, ns, api.Name, metav1.ConditionTrue, ReasonAvailable)
	if got.Status.ReadyReplicas != 1 || got.Status.ServiceName != api.Name ||
		got.Status.URL != "http://pagila-film."+ns+".svc:8080" || got.Status.ObservedGeneration != got.Generation {
		t.Errorf("status = %+v", got.Status)
	}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(&dep), &dep); err != nil {
		t.Fatal(err)
	}
	dep.Status = appsv1.DeploymentStatus{ObservedGeneration: dep.Generation, Replicas: 1, UpdatedReplicas: 1,
		Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
			Reason: "ProgressDeadlineExceeded", Message: "ReplicaSet has timed out progressing"}}}
	if err := k8sClient.Status().Update(ctx, &dep); err != nil {
		t.Fatal(err)
	}
	got = waitAPIReady(t, ns, api.Name, metav1.ConditionFalse, ReasonDegraded)
	if c := readyCondition(got.Status.Conditions); !strings.Contains(c.Message, "timed out") {
		t.Errorf("message = %q", c.Message)
	}

	// Config change (rowLimit on the plan) rolls the pods: new checksum.
	var cr kabrv1.DomainPlan
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "pagila"}, &cr); err != nil {
		t.Fatal(err)
	}
	limit := int32(25)
	cr.Spec.RowLimit = &limit
	if err := k8sClient.Update(ctx, &cr); err != nil {
		t.Fatal(err)
	}
	eventually(t, "checksum change", func() string {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(&dep), &dep); err != nil {
			return err.Error()
		}
		if dep.Spec.Template.Annotations[AnnotationConfigChecksum] == sum {
			return "checksum unchanged"
		}
		return ""
	})
	if cfg, raw := readConfig(t, ns, api.Name); cfg.RowLimit != 25 || dep.Spec.Template.Annotations[AnnotationConfigChecksum] != ConfigChecksum([]byte(raw)) {
		t.Errorf("rowLimit %d, checksum mismatch", cfg.RowLimit)
	}

	// Scaled to zero is Ready.
	zero := int32(0)
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(&cr), &cr); err != nil {
		t.Fatal(err)
	}
	cr.Spec.Replicas = &zero
	if err := k8sClient.Update(ctx, &cr); err != nil {
		t.Fatal(err)
	}
	waitAPIReady(t, ns, api.Name, metav1.ConditionTrue, ReasonScaledToZero)
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(&dep), &dep); err != nil {
		t.Fatal(err)
	}
	if *dep.Spec.Replicas != 0 {
		t.Errorf("deployment replicas = %d", *dep.Spec.Replicas)
	}
}

func waitAPIReady(t *testing.T, ns, name string, status metav1.ConditionStatus, reason string) *kabrv1.DomainAPI {
	t.Helper()
	var api kabrv1.DomainAPI
	eventually(t, fmt.Sprintf("DomainAPI %s Ready=%s/%s", name, status, reason), func() string {
		if err := k8sClient.Get(testCtx(t), client.ObjectKey{Namespace: ns, Name: name}, &api); err != nil {
			return err.Error()
		}
		c := readyCondition(api.Status.Conditions)
		if c == nil || c.Status != status || c.Reason != reason || c.ObservedGeneration != api.Generation {
			return fmt.Sprintf("%+v", c)
		}
		return ""
	})
	return &api
}

// TestEnvtestDomainAPIDoesNotTrustItsSpec: editing a DomainAPI cannot widen
// what is served, and a hand-made DomainAPI gets no workload.
func TestEnvtestDomainAPIDoesNotTrustItsSpec(t *testing.T) {
	ns, api := setupServedPlan(t)
	ctx := testCtx(t)
	before, _ := readConfig(t, ns, api.Name)
	if len(before.SensitiveColumns) == 0 {
		t.Fatal("test setup: film has no sensitive columns")
	}

	// Drop the sensitive columns from the DomainAPI copy: the rendered config
	// still masks them (it comes from the verified plan), and the DomainPlan
	// reconciler restores the copy.
	api.Spec.SensitiveColumns = []plan.SensitiveColumn{}
	if err := k8sClient.Update(ctx, api); err != nil {
		t.Fatal(err)
	}
	eventually(t, "DomainAPI spec restored from the plan", func() string {
		var got kabrv1.DomainAPI
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(api), &got); err != nil {
			return err.Error()
		}
		if len(got.Spec.SensitiveColumns) != len(before.SensitiveColumns) {
			return "sensitive columns not restored"
		}
		return ""
	})
	after, _ := readConfig(t, ns, api.Name)
	if len(after.SensitiveColumns) != len(before.SensitiveColumns) {
		t.Errorf("rendered config lost sensitive columns: %+v", after.SensitiveColumns)
	}

	// A DomainAPI not created by the plan (no controller reference) gets
	// no ConfigMap, Deployment, Service or NetworkPolicy.
	rogue := &kabrv1.DomainAPI{
		ObjectMeta: metav1.ObjectMeta{Name: "rogue", Namespace: ns},
		Spec: kabrv1.DomainAPISpec{
			Domain: "film", PlanRef: kabrv1.LocalObjectReference{Name: "pagila"},
			DataSourceRef: kabrv1.LocalObjectReference{Name: "pagila-db"}, CatalogFingerprint: api.Spec.CatalogFingerprint,
			Tables: []string{"public.film"}, Replicas: 1,
		},
	}
	if err := k8sClient.Create(ctx, rogue); err != nil {
		t.Fatal(err)
	}
	waitAPIReady(t, ns, "rogue", metav1.ConditionFalse, ReasonNotOwnedByPlan)
	for _, obj := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}, &networkingv1.NetworkPolicy{}} {
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "rogue"}, obj); !apierrors.IsNotFound(err) {
			t.Errorf("%T for a rogue DomainAPI: %v", obj, err)
		}
	}
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: ConfigMapName("rogue")}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Errorf("ConfigMap for a rogue DomainAPI: %v", err)
	}
}

// TestEnvtestDomainAPIFailClosed: an edit that is not even in the served
// domain (a description) breaks the approval and removes every DomainAPI.
func TestEnvtestDomainAPIFailClosed(t *testing.T) {
	ns, api := setupServedPlan(t)
	ctx := testCtx(t)
	var cr kabrv1.DomainPlan
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "pagila"}, &cr); err != nil {
		t.Fatal(err)
	}
	cr.Spec.Domains[0].Description = "edited after approval"
	if err := k8sClient.Update(ctx, &cr); err != nil {
		t.Fatal(err)
	}
	waitPlanReason(t, ns, "pagila", ReasonEditedAfterApproval)
	waitGone(t, ns, api.Name, &kabrv1.DomainAPI{})
}

// TestEnvtestImageCannotBeSet: DomainAPI has no image field; a patch that
// sets one is pruned by the API server and the workload keeps the operator's
// runtime image.
func TestEnvtestImageCannotBeSet(t *testing.T) {
	ns, api := setupServedPlan(t)
	ctx := testCtx(t)
	patch := []byte(`{"spec":{"image":"attacker.example/evil:latest"}}`)
	if err := k8sClient.Patch(ctx, api, client.RawPatch(types.MergePatchType, patch)); err != nil {
		t.Fatalf("patch: %v", err)
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(kabrv1.GroupVersion.WithKind(kabrv1.KindDomainAPI))
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(api), u); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", "image"); found {
		t.Fatal("spec.image was stored; it must be pruned")
	}
	// Force a reconcile (replicas change) and check the image.
	var cr kabrv1.DomainPlan
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "pagila"}, &cr); err != nil {
		t.Fatal(err)
	}
	two := int32(2)
	cr.Spec.Replicas = &two
	if err := k8sClient.Update(ctx, &cr); err != nil {
		t.Fatal(err)
	}
	var dep appsv1.Deployment
	eventually(t, "deployment with 2 replicas", func() string {
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: api.Name}, &dep); err != nil {
			return err.Error()
		}
		if *dep.Spec.Replicas != 2 {
			return "replicas not updated"
		}
		return ""
	})
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Image != testRuntimeImage {
			t.Errorf("container %s runs %q, want the runtime image %q", c.Name, c.Image, testRuntimeImage)
		}
	}
}

// TestEnvtestForgedControllerRef: a hand-made DomainAPI carrying a copied
// controller reference to a real, approved plan never gets a workload.
func TestEnvtestForgedControllerRef(t *testing.T) {
	ns, api := setupServedPlan(t)
	ctx := testCtx(t)
	var cr kabrv1.DomainPlan
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "pagila"}, &cr); err != nil {
		t.Fatal(err)
	}
	forgedRef := []metav1.OwnerReference{*metav1.NewControllerRef(&cr, kabrv1.GroupVersion.WithKind(kabrv1.KindDomainPlan))}
	cases := []struct{ name, domain string }{
		{"forged-film", "film"},     // valid domain, wrong name
		{"pagila-nosuch", "nosuch"}, // right naming scheme, unknown domain
	}
	for _, tc := range cases {
		rogue := &kabrv1.DomainAPI{
			ObjectMeta: metav1.ObjectMeta{Name: tc.name, Namespace: ns, OwnerReferences: forgedRef},
			Spec: kabrv1.DomainAPISpec{
				Domain: tc.domain, PlanRef: kabrv1.LocalObjectReference{Name: "pagila"},
				DataSourceRef: kabrv1.LocalObjectReference{Name: "pagila-db"}, CatalogFingerprint: api.Spec.CatalogFingerprint,
				Tables: []string{"public.film"}, Replicas: 1,
			},
		}
		if err := k8sClient.Create(ctx, rogue); err != nil {
			t.Fatal(err)
		}
	}
	// The plan reconciler deletes owned DomainAPIs it did not create; until
	// then the DomainAPI reconciler must not create anything. Poll for a
	// while and assert that no workload object ever appears.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, tc := range cases {
			for _, obj := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}, &networkingv1.NetworkPolicy{}} {
				if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: tc.name}, obj); !apierrors.IsNotFound(err) {
					t.Fatalf("%T for forged DomainAPI %s: %v", obj, tc.name, err)
				}
			}
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: ConfigMapName(tc.name)}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
				t.Fatalf("ConfigMap for forged DomainAPI %s: %v", tc.name, err)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, tc := range cases {
		waitGone(t, ns, tc.name, &kabrv1.DomainAPI{})
	}
	// The real DomainAPIs are untouched.
	waitDomainAPIs(t, ns, []string{"pagila-city", "pagila-film", "pagila-store"})
}

func TestEnvtestRejectsCatchAllCIDR(t *testing.T) {
	ns := newNamespace(t)
	for i, c := range []string{"0.0.0.0/0", "::/0"} {
		ds := &kabrv1.DataSource{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("open-%d", i), Namespace: ns},
			Spec: kabrv1.DataSourceSpec{
				Driver: kabrv1.DriverPostgres, SecretRef: kabrv1.DSNSecretRef{Name: "x"},
				Network: kabrv1.DataSourceNetwork{Port: 5432, CIDRs: []string{"10.0.0.0/8", c}},
			},
		}
		err := k8sClient.Create(testCtx(t), ds)
		if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "/0") {
			t.Errorf("%s: want Invalid mentioning /0, got %v", c, err)
		}
	}
}
