package plan

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSpecDigest(t *testing.T) {
	c := smallCatalog()
	base := smallPlan(c)
	d := SpecDigest(base.Spec)
	if !fingerprintRE.MatchString(d) {
		t.Fatalf("bad digest format %q", d)
	}
	if d == Fingerprint(c) {
		t.Fatal("spec digest must not equal the catalog fingerprint")
	}
	before := base.Clone()
	_ = SpecDigest(base.Spec)
	if !slices.EqualFunc(base.Spec.Domains, before.Spec.Domains, func(a, b Domain) bool { return a.Name == b.Name }) ||
		base.Spec.Domains[1].Tables[0] != before.Spec.Domains[1].Tables[0] {
		t.Fatal("SpecDigest reordered its input")
	}

	same := []struct {
		name   string
		mutate func(p *DomainPlan)
	}{
		{"domains reversed", func(p *DomainPlan) { slices.Reverse(p.Spec.Domains) }},
		{"tables and sensitive columns reversed", func(p *DomainPlan) {
			for i := range p.Spec.Domains {
				slices.Reverse(p.Spec.Domains[i].Tables)
				slices.Reverse(p.Spec.Domains[i].SensitiveColumns)
			}
		}},
		{"empty vs nil sensitive columns", func(p *DomainPlan) { p.Spec.Domains[1].SensitiveColumns = []SensitiveColumn{} }},
		{"status and metadata changed", func(p *DomainPlan) {
			p.Metadata.Annotations = map[string]string{"note": "x"}
			p.Status.Warnings = []string{"w"}
			p.Status.Generator.Mode = ModeLLM
		}},
	}
	for _, tt := range same {
		t.Run("same/"+tt.name, func(t *testing.T) {
			p := smallPlan(c)
			tt.mutate(p)
			if got := SpecDigest(p.Spec); got != d {
				t.Fatalf("digest changed: %s != %s", got, d)
			}
		})
	}

	different := []struct {
		name   string
		mutate func(p *DomainPlan)
	}{
		{"domain renamed", func(p *DomainPlan) { p.Spec.Domains[1].Name = "films" }},
		{"description edited", func(p *DomainPlan) { p.Spec.Domains[0].Description += "." }},
		{"rationale edited", func(p *DomainPlan) { p.Spec.Domains[0].Rationale = "" }},
		{"table moved", func(p *DomainPlan) {
			p.Spec.Domains[2].Tables = append(p.Spec.Domains[2].Tables, p.Spec.Domains[1].Tables[2])
			p.Spec.Domains[1].Tables = p.Spec.Domains[1].Tables[:2]
		}},
		{"table role changed", func(p *DomainPlan) { p.Spec.Domains[1].Tables[0].Role = RoleCore }},
		{"table shared flag", func(p *DomainPlan) { p.Spec.Domains[1].Tables[0].Shared = true }},
		{"sensitive action relaxed", func(p *DomainPlan) { p.Spec.Domains[0].SensitiveColumns[0].Action = ActionMask }},
		{"sensitive column removed", func(p *DomainPlan) { p.Spec.Domains[2].SensitiveColumns = nil }},
		{"fingerprint changed", func(p *DomainPlan) { p.Spec.Source.CatalogFingerprint = "sha256:" + strings.Repeat("0", 64) }},
		{"database changed", func(p *DomainPlan) { p.Spec.Source.Database = "other" }},
	}
	for _, tt := range different {
		t.Run("different/"+tt.name, func(t *testing.T) {
			p := smallPlan(c)
			tt.mutate(p)
			if got := SpecDigest(p.Spec); got == d {
				t.Fatal("digest should change")
			}
		})
	}
}

func TestCanonicalSpec(t *testing.T) {
	p := smallPlan(smallCatalog())
	before := p.Clone()
	cs := CanonicalSpec(p.Spec)
	if p.Spec.Domains[0].Name != before.Spec.Domains[0].Name {
		t.Fatal("CanonicalSpec mutated its input")
	}
	names := []string{cs.Domains[0].Name, cs.Domains[1].Name, cs.Domains[2].Name}
	if !slices.IsSorted(names) {
		t.Fatalf("domains not sorted: %v", names)
	}
	cs.Domains[0].Tables[0].Name = "changed.table"
	for _, d := range p.Spec.Domains {
		for _, ta := range d.Tables {
			if ta.Name == "changed.table" {
				t.Fatal("CanonicalSpec shares memory with its input")
			}
		}
	}
	if empty := CanonicalSpec(Spec{}); empty.Domains == nil {
		t.Fatal("nil domains should become empty")
	}
}

func TestVerifyApproval(t *testing.T) {
	c := smallCatalog()
	now := time.Date(2026, 11, 24, 8, 0, 0, 0, time.UTC)
	approved := func() *DomainPlan {
		p := smallPlan(c)
		if err := Approve(p, "alice", now); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tests := []struct {
		name    string
		plan    func() *DomainPlan
		wantIs  error    // nil = must verify
		wantMsg []string // substrings of the error
	}{
		{"approved", approved, nil, nil},
		{"approved, reordered only", func() *DomainPlan {
			p := approved()
			slices.Reverse(p.Spec.Domains)
			for i := range p.Spec.Domains {
				slices.Reverse(p.Spec.Domains[i].Tables)
				slices.Reverse(p.Spec.Domains[i].SensitiveColumns)
			}
			return p
		}, nil, nil},
		{"proposed", func() *DomainPlan { return smallPlan(c) }, ErrNotApproved,
			[]string{"plan is not approved", `"shop" has phase Proposed`, "kabr plan approve"}},
		{"phase unset", func() *DomainPlan {
			p := smallPlan(c)
			p.Status.Phase = ""
			return p
		}, ErrNotApproved, []string{"phase <unset>"}},
		{"approved without approver", func() *DomainPlan {
			p := approved()
			p.Status.ApprovedBy = " "
			return p
		}, ErrNotApproved, []string{"no status.approvedBy"}},
		{"approved without time", func() *DomainPlan {
			p := approved()
			p.Status.ApprovedAt = nil
			return p
		}, ErrNotApproved, []string{"no status.approvedAt"}},
		{"approved before digests existed", func() *DomainPlan {
			p := approved()
			p.Status.ApprovalDigest = ""
			return p
		}, ErrApprovalDigestMissing, []string{"approved before approval digests existed", "no status.approvalDigest", `re-approve it with "kabr plan approve"`}},
		{"edited after approval: table moved", func() *DomainPlan {
			p := approved()
			p.Spec.Domains[2].Tables = append(p.Spec.Domains[2].Tables, p.Spec.Domains[1].Tables[2])
			p.Spec.Domains[1].Tables = p.Spec.Domains[1].Tables[:2]
			return p
		}, ErrEditedAfterApproval, []string{"edited after approval", "review the changes"}},
		{"edited after approval: sensitive column dropped", func() *DomainPlan {
			p := approved()
			p.Spec.Domains[0].SensitiveColumns = p.Spec.Domains[0].SensitiveColumns[1:]
			return p
		}, ErrEditedAfterApproval, nil},
		{"digest of another spec", func() *DomainPlan {
			p := approved()
			other := smallPlan(c)
			other.Spec.Domains[1].Name = "films"
			p.Status.ApprovalDigest = SpecDigest(other.Spec)
			return p
		}, ErrEditedAfterApproval, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tt.plan()
			err := VerifyApproval(p)
			if tt.wantIs == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if verr := p.ValidateStructure(); verr != nil {
					t.Fatalf("verified plan fails ValidateStructure: %v", verr)
				}
				return
			}
			if !errors.Is(err, tt.wantIs) {
				t.Fatalf("err = %v, want errors.Is %v", err, tt.wantIs)
			}
			for _, w := range tt.wantMsg {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
			// Digest problems of an approved plan are also structural
			// problems (what "kabr plan validate" reports).
			if errors.Is(tt.wantIs, ErrApprovalDigestMissing) || errors.Is(tt.wantIs, ErrEditedAfterApproval) {
				if verr := p.ValidateStructure(); !errors.Is(verr, tt.wantIs) {
					t.Errorf("ValidateStructure = %v, want errors.Is %v", verr, tt.wantIs)
				}
			}
		})
	}
	if err := VerifyApproval(nil); err == nil {
		t.Fatal("nil plan should fail")
	}
}

// TestApprovalDigestSurvivesMarshal: the YAML round trip keeps the plan
// verifiable, and an edit of the YAML text after approval is caught.
func TestApprovalDigestSurvivesMarshal(t *testing.T) {
	c := smallCatalog()
	p := smallPlan(c)
	mustApprove(t, p)
	data, err := Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if verr := VerifyApproval(got); verr != nil {
		t.Fatalf("round-tripped plan does not verify: %v", verr)
	}
	mustValid(t, got, c)

	edited := strings.Replace(string(data), "action: drop", "action: mask", 1)
	if edited == string(data) {
		t.Fatal("test setup: nothing replaced")
	}
	tampered, err := Unmarshal([]byte(edited))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyApproval(tampered); !errors.Is(err, ErrEditedAfterApproval) {
		t.Fatalf("VerifyApproval = %v, want ErrEditedAfterApproval", err)
	}
	if err := tampered.Validate(c); !errors.Is(err, ErrEditedAfterApproval) {
		t.Fatalf("Validate = %v, want ErrEditedAfterApproval", err)
	}
}
