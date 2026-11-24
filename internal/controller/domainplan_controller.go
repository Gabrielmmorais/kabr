package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kabrv1 "github.com/Gabrielmmorais/kabr/api/v1alpha1"
	"github.com/Gabrielmmorais/kabr/pkg/plan"
)

// DomainPlanReconciler turns an APPROVED DomainPlan into one DomainAPI per
// domain, and deletes them as soon as the plan is no longer approved.
//
// A plan is approved when plan.VerifyApproval(cr.ToPlan()) and
// ValidateStructure accept it. The approval digest proves the spec was not
// changed after approval; it does not authenticate the approver (anyone who
// may edit DomainPlans can recompute it), so RBAC on DomainPlans is the
// access control. dataSourceRef, rowLimit and replicas are outside the
// digest; the runtime's live catalog fingerprint check is what catches a plan
// pointed at another database.
type DomainPlanReconciler struct {
	Client client.Client
	Scheme *runtime.Scheme
	// Recorder emits a Warning event when DomainAPIs are deleted because the
	// approval broke; nil disables events.
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=kabr.dev,resources=domainplans,verbs=get;list;watch
// +kubebuilder:rbac:groups=kabr.dev,resources=domainplans/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kabr.dev,resources=domainapis,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kabr.dev,resources=datasources,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile implements [reconcile.Reconciler].
func (r *DomainPlanReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cr kabrv1.DomainPlan
	if err := r.Client.Get(ctx, req.NamespacedName, &cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	before := cr.DeepCopy()
	ok, reason, msg, children, err := r.reconcile(ctx, &cr)
	if err != nil {
		return ctrl.Result{}, err
	}
	cr.Status.ObservedGeneration = cr.Generation
	cr.Status.DomainAPIs = children
	setReady(&cr.Status.Conditions, cr.Generation, ok, reason, msg)
	if err := r.Client.Status().Patch(ctx, &cr, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{}, nil
}

// approvalReason maps a VerifyApproval/ValidateStructure error to a reason.
func approvalReason(err error) string {
	switch {
	case errors.Is(err, plan.ErrNotApproved):
		return ReasonNotApproved
	case errors.Is(err, plan.ErrApprovalDigestMissing):
		return ReasonApprovalDigestMissing
	case errors.Is(err, plan.ErrEditedAfterApproval):
		return ReasonEditedAfterApproval
	default:
		return ReasonInvalidPlan
	}
}

// verifyPlan returns nil when cr is approved and structurally valid.
func verifyPlan(cr *kabrv1.DomainPlan) error {
	p := cr.ToPlan()
	if err := plan.VerifyApproval(p); err != nil {
		return err
	}
	return p.ValidateStructure()
}

func (r *DomainPlanReconciler) reconcile(ctx context.Context, cr *kabrv1.DomainPlan) (
	ok bool, reason, msg string, children []string, err error,
) {
	owned, err := r.ownedDomainAPIs(ctx, cr)
	if err != nil {
		return false, "", "", nil, err
	}
	names := func() []string {
		out := make([]string, 0, len(owned))
		for i := range owned {
			out = append(out, owned[i].Name)
		}
		slices.Sort(out)
		return out
	}

	// Fail closed: an unapproved or invalid plan serves nothing.
	if verr := verifyPlan(cr); verr != nil {
		reason := approvalReason(verr)
		n, err := r.deleteDomainAPIs(ctx, owned, nil)
		if err != nil {
			return false, "", "", nil, err
		}
		if n > 0 && r.Recorder != nil {
			// The note carries the reason only: no spec content.
			r.Recorder.Eventf(cr, nil, corev1.EventTypeWarning, reason, "DeleteDomainAPIs",
				"deleted %d DomainAPI(s): the plan is not (or no longer) approved", n)
		}
		return false, reason, failClosedMessage(verr.Error()), nil, nil
	}
	desired := map[string]*plan.Domain{}
	for i := range cr.Spec.Domains {
		d := &cr.Spec.Domains[i]
		name, nerr := DomainAPIName(cr.Name, d.Name)
		if nerr != nil {
			if _, err := r.deleteDomainAPIs(ctx, owned, nil); err != nil {
				return false, "", "", nil, err
			}
			return false, ReasonInvalidPlan, failClosedMessage(nerr.Error()), nil, nil
		}
		desired[name] = d
	}

	var ds kabrv1.DataSource
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: cr.Namespace, Name: cr.Spec.DataSourceRef.Name}, &ds); err != nil {
		if apierrors.IsNotFound(err) {
			// Existing DomainAPIs are kept (their pods cannot start
			// without the DataSource anyway); nothing new is created.
			return false, ReasonDataSourceNotFound,
				fmt.Sprintf("DataSource %q not found in namespace %q", cr.Spec.DataSourceRef.Name, cr.Namespace), names(), nil
		}
		return false, "", "", nil, err
	}

	// Domains that disappeared from the plan.
	if _, err := r.deleteDomainAPIs(ctx, owned, desired); err != nil {
		return false, "", "", nil, err
	}

	children = make([]string, 0, len(desired))
	for name, d := range desired {
		api := &kabrv1.DomainAPI{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cr.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, api, func() error {
			if api.CreationTimestamp.IsZero() || metav1.IsControlledBy(api, cr) {
				if api.Labels == nil {
					api.Labels = map[string]string{}
				}
				api.Labels[LabelManagedBy] = ManagedByValue
				api.Labels[LabelPlan] = cr.Name
				api.Labels[LabelDomain] = d.Name
				api.Spec = DomainAPISpecFor(cr, d)
				return controllerutil.SetControllerReference(cr, api, r.Scheme)
			}
			return fmt.Errorf("%w: DomainAPI %q is not controlled by DomainPlan %q", errNotControlled, name, cr.Name)
		}); err != nil {
			if errors.Is(err, errNotControlled) {
				return false, ReasonConflict, err.Error(), nil, nil
			}
			return false, "", "", nil, err
		}
		children = append(children, name)
	}
	slices.Sort(children)
	return true, ReasonReconciled, fmt.Sprintf("approved by %q (not authenticated: RBAC on DomainPlans is the control); %d DomainAPI(s): %s",
		cr.Spec.Approval.ApprovedBy, len(children), strings.Join(children, ", ")), children, nil
}

// failClosedMessage is the condition message of a plan that serves nothing.
// It is the same on every reconcile (the deletion count is in the event).
func failClosedMessage(problem string) string {
	return "serving nothing (fail closed): all DomainAPIs of this plan are deleted; " + problem
}

// ownedDomainAPIs lists the DomainAPIs of the namespace controlled by cr
// (by owner reference, not by label: labels are editable).
func (r *DomainPlanReconciler) ownedDomainAPIs(ctx context.Context, cr *kabrv1.DomainPlan) ([]kabrv1.DomainAPI, error) {
	var list kabrv1.DomainAPIList
	if err := r.Client.List(ctx, &list, client.InNamespace(cr.Namespace)); err != nil {
		return nil, err
	}
	out := list.Items[:0]
	for i := range list.Items {
		if metav1.IsControlledBy(&list.Items[i], cr) {
			out = append(out, list.Items[i])
		}
	}
	return out, nil
}

// deleteDomainAPIs deletes the owned DomainAPIs whose name is not in keep
// (nil keep deletes all) and returns how many it deleted.
func (r *DomainPlanReconciler) deleteDomainAPIs(ctx context.Context, owned []kabrv1.DomainAPI, keep map[string]*plan.Domain) (int, error) {
	n := 0
	for i := range owned {
		if _, ok := keep[owned[i].Name]; ok {
			continue
		}
		if err := r.Client.Delete(ctx, &owned[i], client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return n, err
		}
		log.FromContext(ctx).Info("deleted DomainAPI", "domainAPI", owned[i].Name)
		n++
	}
	return n, nil
}

// SetupWithManager registers the reconciler with mgr. It owns DomainAPIs (a
// hand edit of a DomainAPI spec is overwritten from the plan) and watches
// DataSources.
func (r *DomainPlanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kabrv1.DomainPlan{}).
		Owns(&kabrv1.DomainAPI{}).
		Watches(&kabrv1.DataSource{}, handler.EnqueueRequestsFromMapFunc(r.plansForDataSource)).
		Named("domainplan").
		Complete(r)
}

func (r *DomainPlanReconciler) plansForDataSource(ctx context.Context, o client.Object) []reconcile.Request {
	var list kabrv1.DomainPlanList
	if err := r.Client.List(ctx, &list, client.InNamespace(o.GetNamespace()),
		client.MatchingFields{indexDataSourceRef: o.GetName()}); err != nil {
		log.FromContext(ctx).Error(err, "list domain plans for data source")
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return out
}
