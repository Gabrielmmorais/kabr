package v1alpha1

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/Gabrielmmorais/kabr/pkg/plan"
	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

var (
	pagilaPlanPath    = filepath.Join("..", "..", "examples", "pagila.plan.yaml")
	pagilaCatalogPath = filepath.Join("..", "..", "examples", "pagila.schema.json")
	approvalTime      = time.Date(2026, 11, 24, 9, 30, 15, 0, time.FixedZone("BRT", -3*3600))
)

func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// pagilaPlan returns the committed (Proposed) Pagila plan.
func pagilaPlan(t *testing.T) *plan.DomainPlan {
	t.Helper()
	p, err := plan.Unmarshal(readTestFile(t, pagilaPlanPath))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// approvedPagila returns the Pagila plan approved in memory.
func approvedPagila(t *testing.T) *plan.DomainPlan {
	t.Helper()
	p := pagilaPlan(t)
	if err := plan.Approve(p, "alice@example.com", approvalTime); err != nil {
		t.Fatal(err)
	}
	return p
}

func pagilaCatalog(t *testing.T) *schema.Catalog {
	t.Helper()
	c, err := schema.Unmarshal(readTestFile(t, pagilaCatalogPath))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDomainPlanFromPlan(t *testing.T) {
	p := approvedPagila(t)
	before := p.Clone()
	cr, err := DomainPlanFromPlan(p, "kabr-demo", "pagila-db")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p, before) {
		t.Fatal("DomainPlanFromPlan mutated its input")
	}
	if cr.APIVersion != "kabr.dev/v1alpha1" || cr.Kind != KindDomainPlan || cr.Name != "pagila" || cr.Namespace != "kabr-demo" {
		t.Fatalf("unexpected header: %+v %+v", cr.TypeMeta, cr.ObjectMeta)
	}
	if cr.Spec.DataSourceRef.Name != "pagila-db" || cr.Spec.RowLimit != nil || cr.Spec.Replicas != nil {
		t.Fatalf("unexpected spec knobs: %+v", cr.Spec)
	}
	a := cr.Spec.Approval
	if a == nil || a.ApprovedBy != "alice@example.com" || a.Digest != p.Status.ApprovalDigest ||
		!a.ApprovedAt.Equal(&metav1.Time{Time: approvalTime.Truncate(time.Second)}) {
		t.Fatalf("approval = %+v", a)
	}
	if plan.SpecDigest(cr.Spec.Spec) != a.Digest {
		t.Fatal("embedded spec does not hash to the approval digest")
	}
	// The embedded spec is a copy, in canonical order.
	if !reflect.DeepEqual(cr.Spec.Spec, plan.CanonicalSpec(p.Spec)) {
		t.Fatal("embedded spec is not the canonical plan spec")
	}
	cr.Spec.Domains[0].Tables[0].Name = "public.changed"
	if p.Spec.Domains[0].Tables[0].Name == "public.changed" {
		t.Fatal("embedded spec shares memory with the plan")
	}

	// The namespace is optional.
	if cr, err := DomainPlanFromPlan(approvedPagila(t), "", "pagila-db"); err != nil || cr.Namespace != "" {
		t.Fatalf("no namespace: %v %q", err, cr.Namespace)
	}
}

func TestDomainPlanFromPlanRefuses(t *testing.T) {
	tests := []struct {
		name       string
		plan       func(t *testing.T) *plan.DomainPlan
		ns, ds     string
		wantIs     error
		wantSubstr string
	}{
		{"nil plan", func(*testing.T) *plan.DomainPlan { return nil }, "", "db", nil, "plan is nil"},
		{"proposed plan", pagilaPlan, "", "db", plan.ErrNotApproved, "plan is not approved"},
		{"edited after approval", func(t *testing.T) *plan.DomainPlan {
			p := approvedPagila(t)
			p.Spec.Domains[1].SensitiveColumns = nil
			return p
		}, "", "db", plan.ErrEditedAfterApproval, "edited after approval"},
		{"approved before digests existed", func(t *testing.T) *plan.DomainPlan {
			p := approvedPagila(t)
			p.Status.ApprovalDigest = ""
			return p
		}, "", "db", plan.ErrApprovalDigestMissing, "re-approve"},
		{"structurally invalid", func(t *testing.T) *plan.DomainPlan {
			p := pagilaPlan(t)
			p.Metadata.Name = "Not_DNS"
			return p
		}, "", "db", nil, "metadata.name"},
		{"bad namespace", approvedPagila, "Kabr_NS", "db", nil, "namespace"},
		{"empty data source", approvedPagila, "", "", nil, "data source name"},
		{"bad data source", approvedPagila, "", "DB!", nil, "data source name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cr, err := DomainPlanFromPlan(tt.plan(t), tt.ns, tt.ds)
			if err == nil || cr != nil {
				t.Fatalf("expected refusal, got %+v", cr)
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("err = %v, want errors.Is %v", err, tt.wantIs)
			}
			if !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Errorf("err %q lacks %q", err, tt.wantSubstr)
			}
		})
	}
}

func TestToPlanRoundTrip(t *testing.T) {
	approved := approvedPagila(t)
	cr, err := DomainPlanFromPlan(approved, "kabr-demo", "pagila-db")
	if err != nil {
		t.Fatal(err)
	}

	// Through JSON (what a client and the API server exchange) and back.
	data, err := json.Marshal(cr)
	if err != nil {
		t.Fatal(err)
	}
	var decoded DomainPlan
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	for name, obj := range map[string]*DomainPlan{"in memory": cr, "after JSON": &decoded, "deep copy": cr.DeepCopy()} {
		t.Run(name, func(t *testing.T) {
			got := obj.ToPlan()
			if err := plan.VerifyApproval(got); err != nil {
				t.Fatalf("VerifyApproval: %v", err)
			}
			if err := got.Validate(pagilaCatalog(t)); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if got.Status.ApprovedAt.Location() != time.UTC {
				t.Errorf("approvedAt not in UTC: %v", got.Status.ApprovedAt)
			}
			// Same document as the approved file, minus what the CR does
			// not carry (generator details, warnings, annotations).
			want := approved.Clone()
			want.Status.Generator, want.Status.Warnings, want.Metadata.Annotations = plan.Generator{}, nil, nil
			a, err := plan.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			b, err := plan.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(a, b) {
				t.Fatalf("round trip changed the plan:\n%s\n---\n%s", a, b)
			}
		})
	}

	// Without an approval the rebuilt plan is Proposed and does not verify.
	unapproved := cr.DeepCopy()
	unapproved.Spec.Approval = nil
	if got := unapproved.ToPlan(); got.Status.Phase != plan.PhaseProposed || !errors.Is(plan.VerifyApproval(got), plan.ErrNotApproved) {
		t.Fatalf("unapproved: %+v", got.Status)
	}
	// An edit of the embedded spec in the cluster is detected.
	edited := cr.DeepCopy()
	edited.Spec.Domains[0].Tables = edited.Spec.Domains[0].Tables[1:]
	if err := plan.VerifyApproval(edited.ToPlan()); !errors.Is(err, plan.ErrEditedAfterApproval) {
		t.Fatalf("edited: err = %v", err)
	}
	// A zero approval time does not verify.
	zero := cr.DeepCopy()
	zero.Spec.Approval.ApprovedAt = metav1.Time{}
	if got := zero.ToPlan(); got.Status.ApprovedAt != nil || !errors.Is(plan.VerifyApproval(got), plan.ErrNotApproved) {
		t.Fatalf("zero approvedAt: %+v", got.Status)
	}
	var nilPlan *DomainPlan
	if nilPlan.ToPlan() != nil {
		t.Fatal("nil receiver should give nil")
	}
}

func TestDomainPlanJSONShape(t *testing.T) {
	cr, err := DomainPlanFromPlan(approvedPagila(t), "", "pagila-db")
	if err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(cr)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if got := sortedKeys(doc); !reflect.DeepEqual(got, []string{"apiVersion", "kind", "metadata", "spec"}) {
		t.Errorf("top-level keys = %v (no status, no creationTimestamp expected)\n%s", got, data)
	}
	if got := sortedKeys(doc["metadata"].(map[string]any)); !reflect.DeepEqual(got, []string{"name"}) {
		t.Errorf("metadata keys = %v", got)
	}
	// The plan spec is inlined next to the CR-only fields.
	if got := sortedKeys(doc["spec"].(map[string]any)); !reflect.DeepEqual(got, []string{"approval", "dataSourceRef", "domains", "source"}) {
		t.Errorf("spec keys = %v", got)
	}
}

func TestEffectiveDefaults(t *testing.T) {
	var s DomainPlanSpec
	if s.EffectiveRowLimit() != DefaultRowLimit || s.EffectiveReplicas() != DefaultReplicas {
		t.Fatal("unset knobs should give the defaults")
	}
	rl, rep := int32(5), int32(0)
	s.RowLimit, s.Replicas = &rl, &rep
	if s.EffectiveRowLimit() != 5 || s.EffectiveReplicas() != 0 {
		t.Fatal("set knobs should win, including zero replicas")
	}
}

func TestSchemeRegistration(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{KindDataSource, KindDomainPlan, KindDomainAPI, KindDataSource + "List", KindDomainPlan + "List", KindDomainAPI + "List"} {
		if !s.Recognizes(GroupVersion.WithKind(kind)) {
			t.Errorf("scheme does not recognize %s", kind)
		}
	}
	gvks, _, err := s.ObjectKinds(&DomainPlan{})
	if err != nil || len(gvks) != 1 || gvks[0].Group != "kabr.dev" || gvks[0].Version != "v1alpha1" {
		t.Fatalf("ObjectKinds = %v, %v", gvks, err)
	}
}

func sortedKeys(m map[string]any) []string {
	return slices.Sorted(maps.Keys(m))
}
