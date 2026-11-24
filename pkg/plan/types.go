package plan

import "time"

// APIVersion is the apiVersion of every DomainPlan document.
const APIVersion = "kabr.dev/v1alpha1"

// Kind is the kind of every DomainPlan document.
const Kind = "DomainPlan"

// DomainPlan groups the tables of one catalog into domains.
type DomainPlan struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`
	Status     Status   `json:"status,omitzero"`
}

// Metadata identifies the plan.
type Metadata struct {
	// Name is a DNS-1123 subdomain.
	Name        string            `json:"name"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Spec is the desired grouping.
type Spec struct {
	Source  Source   `json:"source"`
	Domains []Domain `json:"domains"`
}

// Source identifies the catalog the plan was generated for.
type Source struct {
	Dialect       string `json:"dialect"`
	Database      string `json:"database"`
	SchemaVersion string `json:"schemaVersion"`
	// CatalogFingerprint is [Fingerprint] of the catalog ("sha256:<hex>").
	CatalogFingerprint string `json:"catalogFingerprint"`
}

// Domain is one group of tables that will become one API.
type Domain struct {
	// Name is a DNS-1123 label, unique within the plan.
	Name             string            `json:"name"`
	Description      string            `json:"description,omitempty"`
	Tables           []TableAssignment `json:"tables"`
	SensitiveColumns []SensitiveColumn `json:"sensitiveColumns,omitempty"`
	Rationale        string            `json:"rationale,omitempty"`
}

// Table roles accepted in [TableAssignment.Role].
const (
	RoleCore     = "core"
	RoleLookup   = "lookup"
	RoleJunction = "junction"
	RoleOrphan   = "orphan"
)

// TableAssignment places one table in a domain.
type TableAssignment struct {
	// Name is the table ID "schema.table" (see schema.TableID).
	Name string `json:"name"`
	// Shared marks a table that other domains also read (e.g. a lookup).
	Shared bool `json:"shared,omitempty"`
	// Role is one of RoleCore, RoleLookup, RoleJunction, RoleOrphan or empty.
	Role      string `json:"role,omitempty"`
	Rationale string `json:"rationale,omitempty"`
}

// SensitiveClass classifies a sensitive column.
type SensitiveClass string

// Sensitive column classes.
const (
	ClassPII        SensitiveClass = "pii"
	ClassCredential SensitiveClass = "credential"
	ClassFinancial  SensitiveClass = "financial"
	ClassOther      SensitiveClass = "other"
)

// Action is what the generated API does with a sensitive column.
type Action string

// Sensitive column actions.
const (
	ActionMask Action = "mask"
	ActionDrop Action = "drop"
	ActionHash Action = "hash"
)

// Sources accepted in [SensitiveColumn.Source].
const (
	SourceHeuristic = "heuristic"
	SourceLLM       = "llm"
	SourceHuman     = "human"
)

// SensitiveColumn flags one column that must not be exposed verbatim.
type SensitiveColumn struct {
	// Table is the table ID "schema.table".
	Table  string         `json:"table"`
	Column string         `json:"column"`
	Class  SensitiveClass `json:"class"`
	Action Action         `json:"action"`
	// Source is one of SourceHeuristic, SourceLLM, SourceHuman or empty.
	Source    string `json:"source,omitempty"`
	Rationale string `json:"rationale,omitempty"`
}

// Phase is the review state of a plan.
type Phase string

// Plan phases.
const (
	PhaseProposed Phase = "Proposed"
	PhaseApproved Phase = "Approved"
)

// Status records the review state and how the plan was generated.
type Status struct {
	Phase      Phase      `json:"phase,omitempty"`
	ApprovedBy string     `json:"approvedBy,omitempty"`
	ApprovedAt *time.Time `json:"approvedAt,omitempty"`
	Generator  Generator  `json:"generator,omitzero"`
	Warnings   []string   `json:"warnings,omitempty"`
}

// Generator modes accepted in [Generator.Mode].
const (
	ModeGraph = "graph"
	ModeLLM   = "llm"
)

// Generator describes what produced the plan.
type Generator struct {
	// Mode is ModeGraph or ModeLLM (or empty).
	Mode       string  `json:"mode,omitempty"`
	Provider   string  `json:"provider,omitempty"`
	Model      string  `json:"model,omitempty"`
	Modularity float64 `json:"modularity,omitempty"`
}

// Refinement is what an LLM (or a human tool) may propose on top of a
// baseline plan. Nothing else can be changed through [Apply].
type Refinement struct {
	Renames          []DomainRename    `json:"renames,omitempty"`
	Moves            []TableMove       `json:"moves,omitempty"`
	SensitiveColumns []SensitiveColumn `json:"sensitiveColumns,omitempty"`
}

// DomainRename renames a domain and optionally replaces its description.
type DomainRename struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Description string `json:"description,omitempty"`
	Rationale   string `json:"rationale,omitempty"`
}

// TableMove moves a table to another domain. To is the domain name after
// renames were applied; an unknown (valid) name creates a new domain.
type TableMove struct {
	Table     string `json:"table"`
	To        string `json:"to"`
	Rationale string `json:"rationale,omitempty"`
}
