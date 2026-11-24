// Package v1alpha1 contains the kabr.dev/v1alpha1 Kubernetes API of the Kabr
// operator. All kinds are namespaced:
//
//   - DataSource: how to reach one database read-only (driver, a Secret with
//     the DSN of a read-only role, and the network egress the generated APIs
//     need);
//   - DomainPlan: an APPROVED domain plan (the pkg/plan spec, embedded
//     unchanged) bound to a DataSource, with its approval (approver, time and
//     spec digest) so the operator can re-verify it;
//   - DomainAPI: one REST API serving one domain of a DomainPlan, created by
//     the operator.
//
// The DomainPlan document format itself (YAML files reviewed by humans) is
// owned by pkg/plan; its JSON Schema is published next to this package as
// domainplan.schema.json. [DomainPlanFromPlan] and [DomainPlan.ToPlan]
// convert between the two.
//
// +kubebuilder:object:generate=true
// +groupName=kabr.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Group is the API group of every Kabr kind.
const Group = "kabr.dev"

// Version is the API version of this package.
const Version = "v1alpha1"

var (
	// GroupVersion is the group and version of the objects in this package.
	GroupVersion = schema.GroupVersion{Group: Group, Version: Version}

	// SchemeBuilder registers the Go types of this package with a scheme.
	// It is the apimachinery builder, so importing this package pulls in
	// k8s.io/apimachinery only, not controller-runtime.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types of this package to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&DataSource{}, &DataSourceList{},
		&DomainPlan{}, &DomainPlanList{},
		&DomainAPI{}, &DomainAPIList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
