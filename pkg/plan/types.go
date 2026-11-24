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
//
// Spec and the types it holds are also embedded in the DomainPlan custom
// resource (api/v1alpha1): the kubebuilder markers below become its OpenAPI
// validation, and controller-gen generates their DeepCopy methods
// (zz_generated.deepcopy.go). They never change the JSON or YAML encoding.
//
// +kubebuilder:object:generate=true
type Spec struct {
	// Source identifies the catalog the plan was generated for.
	// +required
	Source Source `json:"source"`
	// Domains are the groups of tables; each one becomes one API.
	// +required
	// +kubebuilder:validation:MinItems=1
	Domains []Domain `json:"domains"`
}

// Source identifies the catalog the plan was generated for.
//
// +kubebuilder:object:generate=true
type Source struct {
	// Dialect is the database dialect of the catalog (for example postgres).
	// +required
	// +kubebuilder:validation:MinLength=1
	Dialect string `json:"dialect"`
	// Database is the name of the database the catalog was read from.
	// +required
	// +kubebuilder:validation:MinLength=1
	Database string `json:"database"`
	// SchemaVersion is the version of the catalog document format.
	// +required
	SchemaVersion string `json:"schemaVersion"`
	// CatalogFingerprint is [Fingerprint] of the catalog ("sha256:<hex>").
	// +required
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	CatalogFingerprint string `json:"catalogFingerprint"`
}

// Domain is one group of tables that will become one API.
//
// +kubebuilder:object:generate=true
type Domain struct {
	// Name is a DNS-1123 label, unique within the plan.
	// +required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// Description is a short human-readable summary of the domain.
	// +optional
	Description string `json:"description,omitempty"`
	// Tables are the tables of the domain; a table belongs to one domain.
	// +required
	// +kubebuilder:validation:MinItems=1
	Tables []TableAssignment `json:"tables"`
	// SensitiveColumns are the columns of the domain's tables that must not
	// be exposed verbatim.
	// +optional
	SensitiveColumns []SensitiveColumn `json:"sensitiveColumns,omitempty"`
	// Rationale explains why these tables were grouped together.
	// +optional
	Rationale string `json:"rationale,omitempty"`
}

// Table roles accepted in [TableAssignment.Role].
const (
	RoleCore     = "core"
	RoleLookup   = "lookup"
	RoleJunction = "junction"
	RoleOrphan   = "orphan"
)

// TableAssignment places one table in a domain.
//
// +kubebuilder:object:generate=true
type TableAssignment struct {
	// Name is the table ID "schema.table" (see schema.TableID).
	// +required
	// +kubebuilder:validation:Pattern=`^[^.]*\S[^.]*\..*\S.*$`
	Name string `json:"name"`
	// Shared marks a table that other domains also read (e.g. a lookup).
	// +optional
	Shared bool `json:"shared,omitempty"`
	// Role is one of RoleCore, RoleLookup, RoleJunction, RoleOrphan or empty.
	// +optional
	// +kubebuilder:validation:Enum=core;lookup;junction;orphan
	Role string `json:"role,omitempty"`
	// Rationale explains why the table was placed in this domain.
	// +optional
	Rationale string `json:"rationale,omitempty"`
}

// SensitiveClass classifies a sensitive column.
//
// +kubebuilder:validation:Enum=pii;credential;financial;other
type SensitiveClass string

// Sensitive column classes.
const (
	ClassPII        SensitiveClass = "pii"
	ClassCredential SensitiveClass = "credential"
	ClassFinancial  SensitiveClass = "financial"
	ClassOther      SensitiveClass = "other"
)

// Action is what the generated API does with a sensitive column.
//
// +kubebuilder:validation:Enum=mask;drop;hash
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
//
// +kubebuilder:object:generate=true
type SensitiveColumn struct {
	// Table is the table ID "schema.table".
	// +required
	// +kubebuilder:validation:Pattern=`^[^.]*\S[^.]*\..*\S.*$`
	Table string `json:"table"`
	// Column is the column name.
	// +required
	// +kubebuilder:validation:MinLength=1
	Column string `json:"column"`
	// Class classifies the column: pii, credential, financial or other.
	// +required
	Class SensitiveClass `json:"class"`
	// Action is what the generated API does with the column: mask, drop or
	// hash.
	// +required
	Action Action `json:"action"`
	// Source is one of SourceHeuristic, SourceLLM, SourceHuman or empty.
	// +optional
	// +kubebuilder:validation:Enum=heuristic;llm;human
	Source string `json:"source,omitempty"`
	// Rationale explains why the column is sensitive.
	// +optional
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
	// ApprovalDigest is [SpecDigest] of the spec at approval time, set by
	// [Approve]. An approved plan whose spec no longer hashes to it was
	// edited after approval and fails [VerifyApproval] and
	// [DomainPlan.ValidateStructure].
	ApprovalDigest string    `json:"approvalDigest,omitempty"`
	Generator      Generator `json:"generator,omitzero"`
	Warnings       []string  `json:"warnings,omitempty"`
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
