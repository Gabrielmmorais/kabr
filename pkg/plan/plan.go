package plan

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

// New returns an empty plan for the catalog: apiVersion, kind, metadata.name,
// spec.source (including the catalog fingerprint) are filled, spec.domains is
// empty and the phase is [PhaseProposed]. A nil catalog yields an empty
// source.
func New(name string, c *schema.Catalog) *DomainPlan {
	p := &DomainPlan{
		APIVersion: APIVersion,
		Kind:       Kind,
		Metadata:   Metadata{Name: name},
		Spec:       Spec{Domains: []Domain{}},
		Status:     Status{Phase: PhaseProposed},
	}
	if c != nil {
		p.Spec.Source = Source{
			Dialect:            c.Dialect,
			Database:           c.Database,
			SchemaVersion:      c.SchemaVersion,
			CatalogFingerprint: Fingerprint(c),
		}
	}
	return p
}

// Marshal returns the deterministic YAML encoding of the plan. Domains are
// sorted by name, tables by name and sensitive columns by (table, column);
// map keys are emitted in alphabetical order. The input is not modified and
// is not validated (call [DomainPlan.Validate] first if needed).
func Marshal(p *DomainPlan) ([]byte, error) {
	if p == nil {
		return nil, errors.New("plan: cannot marshal nil plan")
	}
	c := p.clone()
	c.canonicalize()
	out, err := yaml.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("plan: marshal: %w", err)
	}
	return out, nil
}

// Unmarshal decodes a YAML (or JSON) DomainPlan. Decoding is strict: the
// input must be a single YAML document, unknown fields and duplicate keys are
// rejected, keys must match their field names exactly (case-sensitive; no
// "KIND" or shadow "Kind" next to "kind"), and apiVersion/kind must be
// exactly [APIVersion]/[Kind]. Unmarshal does not otherwise validate the plan; call
// [DomainPlan.ValidateStructure] or [DomainPlan.Validate] afterwards.
func Unmarshal(data []byte) (*DomainPlan, error) {
	if err := checkSingleDocument(data); err != nil {
		return nil, fmt.Errorf("plan: unmarshal: %w", err)
	}
	var p DomainPlan
	if err := yaml.UnmarshalStrict(data, &p); err != nil {
		return nil, fmt.Errorf("plan: unmarshal: %w", err)
	}
	if err := checkExactKeys(data); err != nil {
		return nil, fmt.Errorf("plan: unmarshal: %w", err)
	}
	var errs []error
	if p.APIVersion != APIVersion {
		errs = append(errs, fmt.Errorf("apiVersion: must be %q, got %q", APIVersion, p.APIVersion))
	}
	if p.Kind != Kind {
		errs = append(errs, fmt.Errorf("kind: must be %q, got %q", Kind, p.Kind))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("plan: unmarshal: %w", err)
	}
	return &p, nil
}

// Approve marks the plan as approved by `by` at `now` (stored in UTC,
// truncated to seconds) and records [SpecDigest] of the spec in
// status.approvalDigest, so that any later edit of the spec is detected by
// [VerifyApproval] and [DomainPlan.ValidateStructure]. It refuses an empty
// approver and a plan that fails [DomainPlan.ValidateStructure].
// Re-approving an approved plan updates the approver, timestamp and digest.
func Approve(p *DomainPlan, by string, now time.Time) error {
	if p == nil {
		return errors.New("plan: cannot approve nil plan")
	}
	by = strings.TrimSpace(by)
	if by == "" {
		return errors.New("plan: approver must not be empty")
	}
	if now.IsZero() {
		return errors.New("plan: approval time must not be zero")
	}
	// Validate as a proposal, so a previous (possibly stale) approval is not
	// the reason for a failure.
	if err := p.WithoutApproval().ValidateStructure(); err != nil {
		return fmt.Errorf("plan: cannot approve invalid plan: %w", err)
	}
	at := now.UTC().Truncate(time.Second)
	p.Status.Phase = PhaseApproved
	p.Status.ApprovedBy = by
	p.Status.ApprovedAt = &at
	p.Status.ApprovalDigest = SpecDigest(p.Spec)
	return nil
}

// WithoutApproval returns a deep copy of the plan in [PhaseProposed] with
// approvedBy, approvedAt and approvalDigest cleared: the plan as a proposal.
// Validating it checks everything except the approval, which is what a
// re-approval needs (a plan edited after approval is re-validated and
// approved again instead of being refused for its stale digest).
func (p *DomainPlan) WithoutApproval() *DomainPlan {
	if p == nil {
		return nil
	}
	out := p.clone()
	out.Status.Phase = PhaseProposed
	out.Status.ApprovedBy, out.Status.ApprovedAt, out.Status.ApprovalDigest = "", nil, ""
	return out
}

// Domain returns the domain with the given name, or nil.
func (p *DomainPlan) Domain(name string) *Domain {
	for i := range p.Spec.Domains {
		if p.Spec.Domains[i].Name == name {
			return &p.Spec.Domains[i]
		}
	}
	return nil
}

// DomainOf returns the name of the domain that lists the table ID, or "".
func (p *DomainPlan) DomainOf(tableID string) string {
	for _, d := range p.Spec.Domains {
		for _, t := range d.Tables {
			if t.Name == tableID {
				return d.Name
			}
		}
	}
	return ""
}

// Clone returns a deep copy of the plan, or nil for a nil plan.
func (p *DomainPlan) Clone() *DomainPlan {
	if p == nil {
		return nil
	}
	return p.clone()
}

func (p *DomainPlan) clone() *DomainPlan {
	out := *p
	if p.Metadata.Annotations != nil {
		out.Metadata.Annotations = maps.Clone(p.Metadata.Annotations)
	}
	if p.Spec.Domains != nil {
		out.Spec.Domains = make([]Domain, len(p.Spec.Domains))
		for i, d := range p.Spec.Domains {
			d.Tables = slices.Clone(d.Tables)
			d.SensitiveColumns = slices.Clone(d.SensitiveColumns)
			out.Spec.Domains[i] = d
		}
	}
	if p.Status.ApprovedAt != nil {
		at := *p.Status.ApprovedAt
		out.Status.ApprovedAt = &at
	}
	out.Status.Warnings = slices.Clone(p.Status.Warnings)
	return &out
}

// canonicalize sorts the plan in place into its serialized order.
func (p *DomainPlan) canonicalize() {
	if p.Spec.Domains == nil {
		p.Spec.Domains = []Domain{}
	}
	slices.SortStableFunc(p.Spec.Domains, func(a, b Domain) int { return cmp.Compare(a.Name, b.Name) })
	for i := range p.Spec.Domains {
		d := &p.Spec.Domains[i]
		if d.Tables == nil {
			d.Tables = []TableAssignment{}
		}
		slices.SortStableFunc(d.Tables, func(a, b TableAssignment) int { return cmp.Compare(a.Name, b.Name) })
		slices.SortStableFunc(d.SensitiveColumns, compareSensitive)
	}
}

func compareSensitive(a, b SensitiveColumn) int {
	return cmp.Or(cmp.Compare(a.Table, b.Table), cmp.Compare(a.Column, b.Column))
}
