package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Gabrielmmorais/kabr/pkg/plan"
)

// Defaults of the optional knobs of a DomainPlan (and of each DomainAPI).
const (
	DefaultRowLimit int32 = 100
	MaxRowLimit     int32 = 1000
	DefaultReplicas int32 = 1
	MaxReplicas     int32 = 10
)

// DomainPlanSpec is an approved domain plan bound to a data source.
//
// The plan itself (source catalog and domains) is the pkg/plan spec,
// embedded inline and unchanged: the same Go type, the same JSON fields. The
// operator rebuilds a plan with [DomainPlan.ToPlan] and re-checks it with
// plan.VerifyApproval, so a spec edited in the cluster after approval is
// detected exactly like an edited YAML file.
type DomainPlanSpec struct {
	plan.Spec `json:",inline"`

	// DataSourceRef names the DataSource (same namespace) the APIs of this
	// plan read from.
	// +required
	DataSourceRef LocalObjectReference `json:"dataSourceRef"`

	// Approval records who approved the plan, when, and the digest of the
	// approved spec (plan.SpecDigest). The operator serves nothing for a
	// plan without a valid approval.
	// +optional
	Approval *Approval `json:"approval,omitempty"`

	// RowLimit is the default maximum number of rows a list request of the
	// generated APIs returns.
	// +optional
	// +kubebuilder:default=100
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	RowLimit *int32 `json:"rowLimit,omitempty"`

	// Replicas is the default number of pods of each generated API.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	Replicas *int32 `json:"replicas,omitempty"`
}

// Approval is the approval of a DomainPlan, copied from the status of the
// approved plan file ("kabr plan approve").
type Approval struct {
	// ApprovedBy is who approved the plan.
	// +required
	// +kubebuilder:validation:MinLength=1
	ApprovedBy string `json:"approvedBy"`

	// ApprovedAt is when the plan was approved (UTC, whole seconds).
	// +required
	ApprovedAt metav1.Time `json:"approvedAt"`

	// Digest is plan.SpecDigest of the approved spec.
	// +required
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	Digest string `json:"digest"`
}

// DomainPlanStatus is the observed state of a DomainPlan.
type DomainPlanStatus struct {
	// ObservedGeneration is the metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions describe the state of the plan (for example Ready).
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// DomainAPIs are the names of the DomainAPI objects created for the
	// domains of this plan.
	// +optional
	// +listType=set
	DomainAPIs []string `json:"domainAPIs,omitempty"`
}

// DomainPlan is an approved Kabr domain plan: the grouping of the tables of
// one database into domains, each of which the operator serves as one REST
// API through a DomainAPI. Create it with "kabr plan manifest".
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories=kabr,shortName=kdp
// +kubebuilder:printcolumn:name="DataSource",type=string,JSONPath=`.spec.dataSourceRef.name`
// +kubebuilder:printcolumn:name="Approved By",type=string,JSONPath=`.spec.approval.approvedBy`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type DomainPlan struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec DomainPlanSpec `json:"spec"`
	// +optional
	Status DomainPlanStatus `json:"status,omitzero"`
}

// DomainPlanList is a list of DomainPlan objects.
//
// +kubebuilder:object:root=true
type DomainPlanList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DomainPlan `json:"items"`
}
