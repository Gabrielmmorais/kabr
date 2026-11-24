package plan

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

// refinedPrefix marks rationales that come from a [Refinement].
const refinedPrefix = "refined: "

// Apply applies a refinement to a plan and returns a NEW plan; p is never
// modified. Steps, in order:
//
//  1. Renames: domain From becomes To (Description replaced when non-empty,
//     Rationale set to "refined: <rationale>" when non-empty). Renames are
//     applied sequentially, so a later rename may refer to an earlier To.
//  2. Moves: the table moves to domain To (a name after renames); an unknown
//     but valid domain name creates that domain. The table keeps its role
//     and shared flag; its rationale becomes "refined: <rationale>".
//     Sensitive columns of a moved table follow it. Domains left without
//     tables are removed (with a warning).
//  3. Sensitive columns: merged into the domain that owns the table, deduped
//     by (table, column); the refinement entry replaces an existing one.
//
// Invalid items (unknown table, column or domain, invalid domain name, rename
// collision, invalid class/action/source) are skipped and reported in the
// returned warnings; warnings are not copied into the plan's status. If the
// resulting plan fails [DomainPlan.Validate] against c, Apply returns an
// error and the caller should keep the baseline. The result is always in
// [PhaseProposed] with approval cleared: a changed plan must be re-approved.
func Apply(p *DomainPlan, r Refinement, c *schema.Catalog) (*DomainPlan, []string, error) {
	if p == nil {
		return nil, nil, errors.New("plan: cannot refine nil plan")
	}
	if c == nil {
		return nil, nil, errors.New("plan: cannot refine without a catalog")
	}
	out := p.clone()
	var warnings []string
	warn := func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}

	for i, rn := range r.Renames {
		d := out.Domain(rn.From)
		switch {
		case d == nil:
			warn("renames[%d]: skipped: unknown domain %q", i, rn.From)
			continue
		case !IsDNSLabel(rn.To):
			warn("renames[%d]: skipped: %q is not a valid DNS-1123 label", i, rn.To)
			continue
		case rn.To != rn.From && out.Domain(rn.To) != nil:
			warn("renames[%d]: skipped: domain %q already exists", i, rn.To)
			continue
		}
		d.Name = rn.To
		if rn.Description != "" {
			d.Description = rn.Description
		}
		if rn.Rationale != "" {
			d.Rationale = refinedPrefix + rn.Rationale
		}
	}

	for i, mv := range r.Moves {
		if c.Table(splitID(mv.Table)) == nil {
			warn("moves[%d]: skipped: unknown table %q", i, mv.Table)
			continue
		}
		from := out.DomainOf(mv.Table)
		if from == "" {
			warn("moves[%d]: skipped: table %q is not assigned in the plan", i, mv.Table)
			continue
		}
		if !IsDNSLabel(mv.To) {
			warn("moves[%d]: skipped: %q is not a valid DNS-1123 label", i, mv.To)
			continue
		}
		src := out.Domain(from)
		var ta TableAssignment
		for j, t := range src.Tables {
			if t.Name == mv.Table {
				ta = t
				if from != mv.To {
					src.Tables = append(src.Tables[:j:j], src.Tables[j+1:]...)
				}
				break
			}
		}
		if mv.Rationale != "" {
			ta.Rationale = refinedPrefix + mv.Rationale
		}
		if from == mv.To {
			setAssignment(src, ta)
			continue
		}
		dst := out.Domain(mv.To)
		if dst == nil {
			out.Spec.Domains = append(out.Spec.Domains, Domain{Name: mv.To, Rationale: refinedPrefix + "domain created by a table move"})
			// Re-fetch: append may have reallocated and invalidated src.
			dst = &out.Spec.Domains[len(out.Spec.Domains)-1]
		}
		dst.Tables = append(dst.Tables, ta)
	}

	// Drop empty domains, then re-home sensitive columns with their tables.
	kept := out.Spec.Domains[:0]
	var orphaned []SensitiveColumn
	for _, d := range out.Spec.Domains {
		if len(d.Tables) == 0 {
			warn("domain %q removed: no tables left after moves", d.Name)
			orphaned = append(orphaned, d.SensitiveColumns...)
			continue
		}
		kept = append(kept, d)
	}
	out.Spec.Domains = kept
	for i := range out.Spec.Domains {
		d := &out.Spec.Domains[i]
		var stay []SensitiveColumn
		for _, sc := range d.SensitiveColumns {
			if out.DomainOf(sc.Table) == d.Name {
				stay = append(stay, sc)
			} else {
				orphaned = append(orphaned, sc)
			}
		}
		d.SensitiveColumns = stay
	}
	for _, sc := range orphaned {
		if owner := out.Domain(out.DomainOf(sc.Table)); owner != nil {
			upsertSensitive(owner, sc)
		}
	}

	for i, sc := range r.SensitiveColumns {
		t := c.Table(splitID(sc.Table))
		switch {
		case t == nil:
			warn("sensitiveColumns[%d]: skipped: unknown table %q", i, sc.Table)
			continue
		case t.Column(sc.Column) == nil:
			warn("sensitiveColumns[%d]: skipped: table %q has no column %q", i, sc.Table, sc.Column)
			continue
		}
		if errs := validateSensitive(fmt.Sprintf("sensitiveColumns[%d]", i), sc); len(errs) > 0 {
			warn("skipped: %v", strings.ReplaceAll(errors.Join(errs...).Error(), "\n", "; "))
			continue
		}
		owner := out.Domain(out.DomainOf(sc.Table))
		if owner == nil {
			warn("sensitiveColumns[%d]: skipped: table %q is not assigned in the plan", i, sc.Table)
			continue
		}
		upsertSensitive(owner, sc)
	}

	out.Status.Phase = PhaseProposed
	out.Status.ApprovedBy = ""
	out.Status.ApprovedAt = nil

	if err := out.Validate(c); err != nil {
		return nil, warnings, fmt.Errorf("plan: refined plan is invalid: %w", err)
	}
	return out, warnings, nil
}

// splitID splits a table ID "schema.table" for schema.Catalog.Table.
func splitID(id string) (sch, name string) {
	sch, name, _ = strings.Cut(id, ".")
	return sch, name
}

func setAssignment(d *Domain, ta TableAssignment) {
	for i := range d.Tables {
		if d.Tables[i].Name == ta.Name {
			d.Tables[i] = ta
			return
		}
	}
	d.Tables = append(d.Tables, ta)
}

func upsertSensitive(d *Domain, sc SensitiveColumn) {
	for i := range d.SensitiveColumns {
		if d.SensitiveColumns[i].Table == sc.Table && d.SensitiveColumns[i].Column == sc.Column {
			d.SensitiveColumns[i] = sc
			return
		}
	}
	d.SensitiveColumns = append(d.SensitiveColumns, sc)
}
