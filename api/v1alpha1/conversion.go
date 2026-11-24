package v1alpha1

import (
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Gabrielmmorais/kabr/pkg/plan"
)

// Kind names of the objects in this package.
const (
	KindDataSource = "DataSource"
	KindDomainPlan = "DomainPlan"
	KindDomainAPI  = "DomainAPI"
)

// DomainPlanFromPlan builds the DomainPlan custom resource of an approved
// plan file: metadata.name is the plan name, metadata.namespace is namespace
// ("" leaves it unset, for "kubectl apply -n"), spec embeds the canonical
// plan spec (see plan.CanonicalSpec), spec.dataSourceRef names dataSource
// and spec.approval copies the plan's approver, approval time and approval
// digest. rowLimit and replicas are left unset (the API server applies their
// defaults).
//
// It refuses a plan that fails plan.DomainPlan.ValidateStructure or
// plan.VerifyApproval (not approved, approved before digests existed, or
// edited after approval), an invalid namespace (a DNS-1123 label) and an
// invalid data source name (a DNS-1123 subdomain). Annotations, generator
// details and warnings of the plan file are not carried over; they are not
// part of what was approved.
func DomainPlanFromPlan(p *plan.DomainPlan, namespace, dataSource string) (*DomainPlan, error) {
	if p == nil {
		return nil, errors.New("v1alpha1: plan is nil")
	}
	if namespace != "" && !plan.IsDNSLabel(namespace) {
		return nil, fmt.Errorf("v1alpha1: namespace %q is not a valid DNS-1123 label", namespace)
	}
	if !plan.IsDNSSubdomain(dataSource) {
		return nil, fmt.Errorf("v1alpha1: data source name %q is not a valid DNS-1123 subdomain", dataSource)
	}
	if err := p.ValidateStructure(); err != nil {
		return nil, fmt.Errorf("v1alpha1: plan %q is invalid: %w", p.Metadata.Name, err)
	}
	if err := plan.VerifyApproval(p); err != nil {
		return nil, fmt.Errorf("v1alpha1: %w", err)
	}
	st := p.Status
	return &DomainPlan{
		TypeMeta: metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: KindDomainPlan},
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Metadata.Name,
			Namespace: namespace,
		},
		Spec: DomainPlanSpec{
			Spec:          plan.CanonicalSpec(p.Spec),
			DataSourceRef: LocalObjectReference{Name: dataSource},
			Approval: &Approval{
				ApprovedBy: st.ApprovedBy,
				ApprovedAt: metav1.NewTime(st.ApprovedAt.UTC()),
				Digest:     st.ApprovalDigest,
			},
		},
	}, nil
}

// ToPlan rebuilds the plan document of the custom resource, so that the
// operator can check it with plan.VerifyApproval and
// plan.DomainPlan.ValidateStructure exactly like a plan file: apiVersion and
// kind of the plan format, metadata.name from the object, a deep copy of the
// embedded spec and, when spec.approval is set, status phase Approved with
// approvedBy, approvedAt (UTC) and approvalDigest from it; otherwise phase
// Proposed. A nil receiver returns nil.
func (in *DomainPlan) ToPlan() *plan.DomainPlan {
	if in == nil {
		return nil
	}
	p := &plan.DomainPlan{
		APIVersion: plan.APIVersion,
		Kind:       plan.Kind,
		Metadata:   plan.Metadata{Name: in.Name},
		Status:     plan.Status{Phase: plan.PhaseProposed},
	}
	in.Spec.Spec.DeepCopyInto(&p.Spec)
	if a := in.Spec.Approval; a != nil {
		p.Status = plan.Status{
			Phase:          plan.PhaseApproved,
			ApprovedBy:     a.ApprovedBy,
			ApprovalDigest: a.Digest,
		}
		if !a.ApprovedAt.IsZero() {
			at := a.ApprovedAt.UTC()
			p.Status.ApprovedAt = &at
		}
	}
	return p
}

// EffectiveRowLimit returns spec.rowLimit, or [DefaultRowLimit] when unset.
func (in *DomainPlanSpec) EffectiveRowLimit() int32 {
	if in.RowLimit == nil {
		return DefaultRowLimit
	}
	return *in.RowLimit
}

// EffectiveReplicas returns spec.replicas, or [DefaultReplicas] when unset.
func (in *DomainPlanSpec) EffectiveReplicas() int32 {
	if in.Replicas == nil {
		return DefaultReplicas
	}
	return *in.Replicas
}
