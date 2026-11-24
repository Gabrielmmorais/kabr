package v1alpha1

// Condition types shared by the Kabr kinds (metav1.Condition.Type).
const (
	// ConditionReady is True when the object is fully reconciled and usable.
	ConditionReady = "Ready"
)

// LocalObjectReference names another object in the same namespace.
type LocalObjectReference struct {
	// Name of the referenced object (a DNS-1123 subdomain).
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}
