package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Database drivers accepted in [DataSourceSpec.Driver].
const (
	DriverPostgres = "postgres"
)

// Default Secret keys of [DSNSecretRef] and [HashKeySecretRef].
const (
	DefaultDSNSecretKey     = "dsn"
	DefaultHashKeySecretKey = "key"
)

// DataSourceSpec describes how the generated APIs reach one database.
type DataSourceSpec struct {
	// Driver is the database driver.
	// +required
	// +kubebuilder:validation:Enum=postgres
	Driver string `json:"driver"`

	// SecretRef points to a Secret in the same namespace that holds the DSN
	// of a READ-ONLY database role. The DSN is never written inline in a
	// Kabr object.
	// +required
	SecretRef DSNSecretRef `json:"secretRef"`

	// Network is the egress the generated APIs need to reach the database;
	// the operator turns it into a NetworkPolicy.
	// +required
	Network DataSourceNetwork `json:"network"`

	// HashKeySecretRef points to a Secret in the same namespace that holds
	// the HMAC key used for sensitive columns whose action is "hash".
	// Required by the operator only when a plan uses that action.
	// +optional
	HashKeySecretRef *HashKeySecretRef `json:"hashKeySecretRef,omitempty"`
}

// DSNSecretRef selects the key of a Secret that holds a DSN.
type DSNSecretRef struct {
	// Name of the Secret, in the namespace of the DataSource.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
	// Key of the Secret's data that holds the DSN.
	// +optional
	// +kubebuilder:default=dsn
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	Key string `json:"key,omitempty"`
}

// HashKeySecretRef selects the key of a Secret that holds an HMAC key.
type HashKeySecretRef struct {
	// Name of the Secret, in the namespace of the DataSource.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
	// Key of the Secret's data that holds the HMAC key.
	// +optional
	// +kubebuilder:default=key
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	Key string `json:"key,omitempty"`
}

// DataSourceNetwork is the database endpoint as seen by a NetworkPolicy:
// a TCP port plus the address blocks and/or pods that serve it.
//
// +kubebuilder:validation:XValidation:rule="(has(self.cidrs) && size(self.cidrs) > 0) || has(self.podSelector)",message="at least one of cidrs or podSelector is required"
// +kubebuilder:validation:XValidation:rule="!has(self.namespaceSelector) || has(self.podSelector)",message="namespaceSelector requires podSelector"
type DataSourceNetwork struct {
	// Port is the TCP port of the database.
	// +required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// CIDRs are the address blocks of the database (for a database outside
	// the cluster), for example 10.0.12.0/24.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=49
	// +kubebuilder:validation:XValidation:rule="self.all(c, isCIDR(c))",message="every entry must be a valid CIDR"
	// +kubebuilder:validation:XValidation:rule="self.all(c, !isCIDR(c) || cidr(c).prefixLength() > 0)",message="a /0 CIDR allows every address; name the database's address blocks"
	CIDRs []string `json:"cidrs,omitempty"`

	// PodSelector selects the database pods (for a database inside the
	// cluster). Without NamespaceSelector it matches pods in the namespace
	// of the DataSource.
	// +optional
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`

	// NamespaceSelector narrows PodSelector to pods in the matching
	// namespaces.
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`
}

// DataSourceStatus is the observed state of a DataSource.
type DataSourceStatus struct {
	// ObservedGeneration is the metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions describe the state of the DataSource (for example Ready).
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// DataSource describes how Kabr's generated APIs reach one database
// read-only.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories=kabr,shortName=kds
// +kubebuilder:printcolumn:name="Driver",type=string,JSONPath=`.spec.driver`
// +kubebuilder:printcolumn:name="Secret",type=string,JSONPath=`.spec.secretRef.name`
// +kubebuilder:printcolumn:name="Port",type=integer,JSONPath=`.spec.network.port`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type DataSource struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec DataSourceSpec `json:"spec"`
	// +optional
	Status DataSourceStatus `json:"status,omitzero"`
}

// DataSourceList is a list of DataSource objects.
//
// +kubebuilder:object:root=true
type DataSourceList struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DataSource `json:"items"`
}
