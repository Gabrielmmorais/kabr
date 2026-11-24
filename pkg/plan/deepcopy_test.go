package plan

import (
	"reflect"
	"testing"
)

// TestSpecDeepCopy covers the controller-gen DeepCopy of the spec types
// (zz_generated.deepcopy.go), which the DomainPlan custom resource relies on.
func TestSpecDeepCopy(t *testing.T) {
	in := smallPlan(smallCatalog()).Spec
	out := in.DeepCopy()
	if !reflect.DeepEqual(&in, out) {
		t.Fatal("deep copy differs from the original")
	}
	out.Source.Database = "other"
	out.Domains[0].Name = "changed"
	out.Domains[0].Tables[0].Name = "public.changed"
	out.Domains[0].SensitiveColumns[0].Action = ActionHash
	out.Domains = append(out.Domains, Domain{Name: "extra"})
	want := smallPlan(smallCatalog()).Spec
	if !reflect.DeepEqual(in, want) {
		t.Fatal("mutating the copy changed the original")
	}
	if (*Spec)(nil).DeepCopy() != nil || (*Domain)(nil).DeepCopy() != nil || (*SensitiveColumn)(nil).DeepCopy() != nil ||
		(*TableAssignment)(nil).DeepCopy() != nil || (*Source)(nil).DeepCopy() != nil {
		t.Fatal("DeepCopy of nil should be nil")
	}
	d := in.Domains[0].DeepCopy()
	d.Tables[0].Role = RoleOrphan
	if in.Domains[0].Tables[0].Role == RoleOrphan {
		t.Fatal("Domain.DeepCopy shares its tables")
	}
	if *in.Domains[0].SensitiveColumns[0].DeepCopy() != in.Domains[0].SensitiveColumns[0] ||
		*in.Domains[0].Tables[0].DeepCopy() != in.Domains[0].Tables[0] || *in.Source.DeepCopy() != in.Source {
		t.Fatal("value copies differ")
	}
}
