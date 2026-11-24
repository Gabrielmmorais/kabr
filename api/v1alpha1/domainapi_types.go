package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Gabrielmmorais/kabr/pkg/plan"
)

// DomainAPISpec is one domain of an approved DomainPlan, served as one REST
// API. The operator creates it from the DomainPlan; the tables and sensitive
// columns are copied from the plan's domain.
type DomainAPISpec struct {
	// Domain is the name of the domain in the plan (a DNS-1123 label).
	// +required
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Domain string `json:"domain"`

	// PlanRef names the DomainPlan (same namespace) the domain comes from.
	// +required
	PlanRef LocalObjectReference `json:"planRef"`

	// DataSourceRef names the DataSource (same namespace) to read from.
	// +required
	DataSourceRef LocalObjectReference `json:"dataSourceRef"`

	// CatalogFingerprint is the plan's catalog fingerprint; the API refuses
	// to serve a database whose catalog no longer matches it.
	// +required
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	CatalogFingerprint string `json:"catalogFingerprint"`

	// Tables are the table IDs ("schema.table") the API serves.
	// +required
	// +listType=set
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:Pattern=`^[^.]*\S[^.]*\..*\S.*$`
	Tables []string `json:"tables"`

	// SensitiveColumns are the columns of these tables that must not be
	// exposed verbatim, with the action to apply (mask, drop or hash).
	// +optional
	SensitiveColumns []plan.SensitiveColumn `json:"sensitiveColumns,omitempty"`

	// RowLimit is the maximum number of rows a list request returns.
	// +optional
	// +kubebuilder:default=100
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	RowLimit int32 `json:"rowLimit,omitempty"`

	// Replicas is the number of API pods. Zero is a valid value (scaled
	// down), so the field is always serialized; an omitted field defaults
	// to 1.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	Replicas int32 `json:"replicas"`

	// Image overrides the container image of the API server.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	Image string `json:"image,omitempty"`
}

// DomainAPIStatus is the observed state of a DomainAPI.
type DomainAPIStatus struct {
	// ObservedGeneration is the metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions describe the state of the API (for example Ready).
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	// ReadyReplicas is the number of API pods ready to serve.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// ServiceName is the name of the Service in front of the API pods.
	// +optional
	ServiceName string `json:"serviceName,omitempty"`

	// URL is the in-cluster base URL of the API.
	// +optional
	URL string `json:"url,omitempty"`
}

// DomainAPI is one REST API serving one domain of an approved DomainPlan.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories=kabr,shortName=kapi
// +kubebuilder:printcolumn:name="Domain",type=string,JSONPath=`.spec.domain`
// +kubebuilder:printcolumn:name="Plan",type=string,JSONPath=`.spec.planRef.name`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type DomainAPI struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec DomainAPISpec `json:"spec"`
	// +optional
	Status DomainAPIStatus `json:"status,omitzero"`
}

// DomainAPIList is a list of DomainAPI objects.
//
// +kubebuilder:object:root=true
type DomainAPIList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DomainAPI `json:"items"`
}
