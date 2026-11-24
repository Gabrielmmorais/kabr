package plan

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

var (
	dnsLabelRE    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	fingerprintRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	validRoles    = map[string]bool{"": true, RoleCore: true, RoleLookup: true, RoleJunction: true, RoleOrphan: true}
	validClasses  = map[SensitiveClass]bool{ClassPII: true, ClassCredential: true, ClassFinancial: true, ClassOther: true}
	validActions  = map[Action]bool{ActionMask: true, ActionDrop: true, ActionHash: true}
	validSources  = map[string]bool{"": true, SourceHeuristic: true, SourceLLM: true, SourceHuman: true}
	validPhases   = map[Phase]bool{"": true, PhaseProposed: true, PhaseApproved: true}
	validGenModes = map[string]bool{"": true, ModeGraph: true, ModeLLM: true}
	errNilPlan    = errors.New("plan is nil")
)

// ErrCatalogMismatch is returned (wrapped) by [DomainPlan.Validate] when the
// plan's catalog fingerprint does not match the catalog.
var ErrCatalogMismatch = errors.New("plan was generated for a different catalog")

// IsDNSLabel reports whether s is a valid DNS-1123 label (the format of a
// domain name): at most 63 characters of [a-z0-9-], starting and ending with
// an alphanumeric character.
func IsDNSLabel(s string) bool {
	return len(s) <= 63 && dnsLabelRE.MatchString(s)
}

// IsDNSSubdomain reports whether s is a valid DNS-1123 subdomain (the format
// of metadata.name): at most 253 characters, dot-separated DNS-1123 labels
// (each at most 63 characters).
func IsDNSSubdomain(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for label := range strings.SplitSeq(s, ".") {
		if !IsDNSLabel(label) {
			return false
		}
	}
	return true
}

// isTableID reports whether s looks like "schema.table".
func isTableID(s string) bool {
	sch, name, ok := strings.Cut(s, ".")
	return ok && strings.TrimSpace(sch) != "" && strings.TrimSpace(name) != ""
}

// ValidateStructure checks the plan without a catalog and returns every
// problem found, joined with [errors.Join] and prefixed with its path (for
// example "spec.domains[2].name"). Rules:
//   - apiVersion and kind are exactly [APIVersion] and [Kind];
//   - metadata.name is a DNS-1123 subdomain;
//   - spec.source has a dialect, a database and a "sha256:<hex>" fingerprint;
//   - there is at least one domain; domain names are unique DNS-1123 labels;
//     every domain lists at least one table;
//   - table names look like "schema.table" and no table is listed twice
//     (within or across domains); roles are valid;
//   - sensitive columns reference a table listed in the same domain, have a
//     column name, a valid class, action and source, and are not duplicated;
//   - status.phase and status.generator.mode are valid; an approved plan has
//     approvedBy, approvedAt and an approvalDigest equal to [SpecDigest] of
//     the spec. A missing digest wraps [ErrApprovalDigestMissing] (approved
//     before digests existed: re-approve), a different one wraps
//     [ErrEditedAfterApproval] (the spec was edited after approval).
//     Reordering domains, tables or sensitive columns is not an edit.
//
// Optional enums (role, source, phase, generator mode) accept "" as "unset",
// exactly like the published JSON Schema, which lists "" in those enums.
// Required enums (sensitive column class and action) reject "".
func (p *DomainPlan) ValidateStructure() error {
	if p == nil {
		return errNilPlan
	}
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if p.APIVersion != APIVersion {
		add("apiVersion: must be %q, got %q", APIVersion, p.APIVersion)
	}
	if p.Kind != Kind {
		add("kind: must be %q, got %q", Kind, p.Kind)
	}
	if !IsDNSSubdomain(p.Metadata.Name) {
		add("metadata.name: %q is not a valid DNS-1123 subdomain", p.Metadata.Name)
	}

	src := p.Spec.Source
	if strings.TrimSpace(src.Dialect) == "" {
		add("spec.source.dialect: must not be empty")
	}
	if strings.TrimSpace(src.Database) == "" {
		add("spec.source.database: must not be empty")
	}
	if !fingerprintRE.MatchString(src.CatalogFingerprint) {
		add("spec.source.catalogFingerprint: must match sha256:<64 hex>, got %q", src.CatalogFingerprint)
	}

	if len(p.Spec.Domains) == 0 {
		add("spec.domains: at least one domain is required")
	}
	domainNames := map[string]int{}
	tableOwner := map[string]string{} // table ID -> path of first listing
	for i, d := range p.Spec.Domains {
		path := fmt.Sprintf("spec.domains[%d]", i)
		if !IsDNSLabel(d.Name) {
			add("%s.name: %q is not a valid DNS-1123 label", path, d.Name)
		}
		if j, dup := domainNames[d.Name]; dup {
			add("%s.name: duplicate domain name %q (also spec.domains[%d])", path, d.Name, j)
		} else {
			domainNames[d.Name] = i
		}
		if len(d.Tables) == 0 {
			add("%s.tables: domain %q has no tables", path, d.Name)
		}
		inDomain := map[string]bool{}
		for j, t := range d.Tables {
			tpath := fmt.Sprintf("%s.tables[%d]", path, j)
			if !isTableID(t.Name) {
				add("%s.name: %q is not a table ID of the form schema.table", tpath, t.Name)
			}
			if prev, dup := tableOwner[t.Name]; dup {
				add("%s.name: table %q is already listed at %s", tpath, t.Name, prev)
			} else {
				tableOwner[t.Name] = tpath
			}
			inDomain[t.Name] = true
			if !validRoles[t.Role] {
				add("%s.role: invalid role %q (want core, lookup, junction or orphan)", tpath, t.Role)
			}
		}
		seenCols := map[string]bool{}
		for j, sc := range d.SensitiveColumns {
			spath := fmt.Sprintf("%s.sensitiveColumns[%d]", path, j)
			errs = append(errs, validateSensitive(spath, sc)...)
			if sc.Table != "" && !inDomain[sc.Table] {
				add("%s.table: table %q is not listed in domain %q", spath, sc.Table, d.Name)
			}
			key := sc.Table + "\x00" + sc.Column
			if seenCols[key] {
				add("%s: duplicate sensitive column %s.%s", spath, sc.Table, sc.Column)
			}
			seenCols[key] = true
		}
	}

	st := p.Status
	if !validPhases[st.Phase] {
		add("status.phase: invalid phase %q (want Proposed or Approved)", st.Phase)
	}
	if st.Phase == PhaseApproved {
		if strings.TrimSpace(st.ApprovedBy) == "" {
			add("status.approvedBy: required when phase is Approved")
		}
		if st.ApprovedAt == nil || st.ApprovedAt.IsZero() {
			add("status.approvedAt: required when phase is Approved")
		}
		switch current, problem := approvalDigestProblem(st, p.Spec); {
		case problem == nil:
		case errors.Is(problem, ErrApprovalDigestMissing):
			errs = append(errs, fmt.Errorf("status.approvalDigest: %w: required when phase is Approved; %s", problem, reapproveHint))
		default:
			errs = append(errs, fmt.Errorf("status.approvalDigest: %w: the spec now hashes to %q, not the approved %q; review the changes and %s",
				problem, current, st.ApprovalDigest, reapproveHint))
		}
	}
	if !validGenModes[st.Generator.Mode] {
		add("status.generator.mode: invalid mode %q (want graph or llm)", st.Generator.Mode)
	}
	return errors.Join(errs...)
}

func validateSensitive(path string, sc SensitiveColumn) []error {
	var errs []error
	if !isTableID(sc.Table) {
		errs = append(errs, fmt.Errorf("%s.table: %q is not a table ID of the form schema.table", path, sc.Table))
	}
	if strings.TrimSpace(sc.Column) == "" {
		errs = append(errs, fmt.Errorf("%s.column: must not be empty", path))
	}
	if !validClasses[sc.Class] {
		errs = append(errs, fmt.Errorf("%s.class: invalid class %q (want pii, credential, financial or other)", path, sc.Class))
	}
	if !validActions[sc.Action] {
		errs = append(errs, fmt.Errorf("%s.action: invalid action %q (want mask, drop or hash)", path, sc.Action))
	}
	if !validSources[sc.Source] {
		errs = append(errs, fmt.Errorf("%s.source: invalid source %q (want heuristic, llm or human)", path, sc.Source))
	}
	return errs
}

// Validate runs [DomainPlan.ValidateStructure] and then checks the plan
// against the catalog: every catalog table is listed in exactly one domain,
// no unknown table is listed, every sensitive column exists in its table, and
// the plan's fingerprint equals [Fingerprint] of the catalog (a mismatch
// wraps [ErrCatalogMismatch]: "plan was generated for a different catalog").
// All problems are returned joined with [errors.Join].
func (p *DomainPlan) Validate(c *schema.Catalog) error {
	if p == nil {
		return errNilPlan
	}
	if c == nil {
		return errors.New("catalog is nil")
	}
	var errs []error
	if err := p.ValidateStructure(); err != nil {
		errs = append(errs, err)
	}
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if fp := Fingerprint(c); p.Spec.Source.CatalogFingerprint != fp {
		errs = append(errs, fmt.Errorf("spec.source.catalogFingerprint: %w (plan %q, catalog %q)",
			ErrCatalogMismatch, p.Spec.Source.CatalogFingerprint, fp))
	}

	tables := make(map[string]*schema.Table, len(c.Tables))
	for i := range c.Tables {
		tables[c.Tables[i].ID()] = &c.Tables[i]
	}
	listed := map[string]bool{}
	for i, d := range p.Spec.Domains {
		for j, t := range d.Tables {
			listed[t.Name] = true
			if tables[t.Name] == nil {
				add("spec.domains[%d].tables[%d].name: unknown table %q (not in catalog)", i, j, t.Name)
			}
		}
		for j, sc := range d.SensitiveColumns {
			path := fmt.Sprintf("spec.domains[%d].sensitiveColumns[%d]", i, j)
			t := tables[sc.Table]
			switch {
			case t == nil:
				add("%s.table: unknown table %q (not in catalog)", path, sc.Table)
			case t.Column(sc.Column) == nil:
				add("%s.column: table %q has no column %q", path, sc.Table, sc.Column)
			}
		}
	}
	for i := range c.Tables {
		if id := c.Tables[i].ID(); !listed[id] {
			add("spec.domains: catalog table %q is not assigned to any domain", id)
		}
	}
	return errors.Join(errs...)
}
