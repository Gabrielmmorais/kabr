package controller

import (
	"context"
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kabrv1 "github.com/Gabrielmmorais/kabr/api/v1alpha1"
	"github.com/Gabrielmmorais/kabr/pkg/plan"
	"github.com/Gabrielmmorais/kabr/pkg/serve"
)

// DomainAPIReconciler renders and owns the workload of one DomainAPI: a
// ConfigMap with domain.json, a Deployment running "kabr serve", a ClusterIP
// Service and a NetworkPolicy.
//
// It does NOT trust the DomainAPI spec for what is served: DomainAPIs are
// editable by anyone with RBAC on them and carry no approval binding. On
// every reconcile it resolves spec.planRef, requires that DomainPlan to be the
// DomainAPI's controller (owner reference with the same UID), re-verifies the
// plan's approval and builds the runtime config with
// serve.ConfigFromPlan(plan, spec.domain), and requires the DomainAPI's name
// to be DomainAPIName(plan, spec.domain) for a domain of that plan (a
// hand-made DomainAPI with a forged controller reference gets nothing). Only
// rowLimit (capped by the runtime's maximum) and replicas come from the
// DomainAPI; the image is always the operator's runtime image. When the
// DomainAPI's copy of the plan diverges, the DomainPlan reconciler (which
// owns DomainAPIs) overwrites it. A missing, foreign or unapproved plan
// means Ready=False and no workload: existing children are deleted (fail
// closed).
type DomainAPIReconciler struct {
	Client client.Client
	Scheme *runtime.Scheme
	// RuntimeImage is the image of every API pod.
	RuntimeImage string
}

// +kubebuilder:rbac:groups=kabr.dev,resources=domainapis,verbs=get;list;watch
// +kubebuilder:rbac:groups=kabr.dev,resources=domainapis/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kabr.dev,resources=domainplans,verbs=get;list;watch
// +kubebuilder:rbac:groups=kabr.dev,resources=datasources,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps;services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

// verdict is the outcome of one reconcile, written to the status.
type verdict struct {
	ok             bool
	reason, msg    string
	readyReplicas  int32
	serviceName    string
	url            string
	deleteChildren bool
}

// Reconcile implements [reconcile.Reconciler].
func (r *DomainAPIReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var api kabrv1.DomainAPI
	if err := r.Client.Get(ctx, req.NamespacedName, &api); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !api.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	before := api.DeepCopy()
	v, err := r.reconcile(ctx, &api)
	if err != nil {
		return ctrl.Result{}, err
	}
	if v.deleteChildren {
		if err := r.deleteChildren(ctx, &api); err != nil {
			return ctrl.Result{}, err
		}
	}
	api.Status.ObservedGeneration = api.Generation
	api.Status.ReadyReplicas = v.readyReplicas
	api.Status.ServiceName = v.serviceName
	api.Status.URL = v.url
	setReady(&api.Status.Conditions, api.Generation, v.ok, v.reason, v.msg)
	if err := r.Client.Status().Patch(ctx, &api, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{}, nil
}

// failClosed is a verdict that removes the workload.
func failClosed(reason, msg string) verdict {
	return verdict{reason: reason, msg: "no workload (fail closed): " + msg, deleteChildren: true}
}

// verifiedConfig resolves and verifies the owning plan and returns the
// runtime config of api's domain, or a fail-closed verdict.
func (r *DomainAPIReconciler) verifiedConfig(ctx context.Context, api *kabrv1.DomainAPI) (*serve.Config, *kabrv1.DomainPlan, *verdict, error) {
	var cr kabrv1.DomainPlan
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: api.Namespace, Name: api.Spec.PlanRef.Name}, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			v := failClosed(ReasonPlanNotFound, fmt.Sprintf("DomainPlan %q not found", api.Spec.PlanRef.Name))
			return nil, nil, &v, nil
		}
		return nil, nil, nil, err
	}
	if !metav1.IsControlledBy(api, &cr) {
		v := failClosed(ReasonNotOwnedByPlan, fmt.Sprintf("DomainPlan %q is not the controller of this DomainAPI; DomainAPIs are created by the operator from approved DomainPlans", cr.Name))
		return nil, nil, &v, nil
	}
	if err := verifyPlan(&cr); err != nil {
		v := failClosed(approvalReason(err), err.Error())
		return nil, nil, &v, nil
	}
	// A controller reference is just a UID anyone can copy into a DomainAPI
	// they create; only the object the plan reconciler would create counts.
	if cr.ToPlan().Domain(api.Spec.Domain) == nil {
		v := failClosed(ReasonNotOwnedByPlan, fmt.Sprintf("DomainPlan %q has no domain %q", cr.Name, api.Spec.Domain))
		return nil, nil, &v, nil
	}
	if want, err := DomainAPIName(cr.Name, api.Spec.Domain); err != nil || want != api.Name {
		v := failClosed(ReasonNotOwnedByPlan, fmt.Sprintf("a DomainAPI for domain %q of DomainPlan %q must be named %q", api.Spec.Domain, cr.Name, want))
		return nil, nil, &v, nil
	}
	cfg, err := serve.ConfigFromPlan(cr.ToPlan(), api.Spec.Domain)
	if err != nil {
		reason := ReasonInvalidPlan
		if errors.Is(err, plan.ErrNotApproved) || errors.Is(err, plan.ErrApprovalDigestMissing) || errors.Is(err, plan.ErrEditedAfterApproval) {
			reason = approvalReason(err)
		}
		v := failClosed(reason, err.Error())
		return nil, nil, &v, nil
	}
	// rowLimit is outside the approval digest; the CRD bounds it and the
	// runtime refuses anything above serve.MaxRowLimit.
	if l := int(api.Spec.RowLimit); l > 0 && l <= serve.MaxRowLimit {
		cfg.RowLimit = l
	}
	return cfg, &cr, nil, nil
}

func (r *DomainAPIReconciler) reconcile(ctx context.Context, api *kabrv1.DomainAPI) (verdict, error) {
	cfg, cr, v, err := r.verifiedConfig(ctx, api)
	if err != nil || v != nil {
		if v == nil {
			return verdict{}, err
		}
		return *v, nil
	}
	if api.Spec.DataSourceRef.Name != cr.Spec.DataSourceRef.Name {
		// The DomainPlan reconciler owns this DomainAPI and overwrites the
		// divergent copy; the next reconcile follows.
		return verdict{reason: ReasonOutOfSync, msg: "spec.dataSourceRef differs from the DomainPlan; waiting for the DomainPlan reconciler to restore it"}, nil
	}
	var ds kabrv1.DataSource
	if err = r.Client.Get(ctx, types.NamespacedName{Namespace: api.Namespace, Name: cr.Spec.DataSourceRef.Name}, &ds); err != nil {
		if apierrors.IsNotFound(err) {
			return verdict{reason: ReasonDataSourceNotFound, msg: fmt.Sprintf("DataSource %q not found", cr.Spec.DataSourceRef.Name)}, nil
		}
		return verdict{}, err
	}
	if reason, msg := validateDataSourceSpec(&ds.Spec); reason != "" {
		return verdict{reason: reason, msg: "DataSource " + ds.Name + ": " + msg}, nil
	}
	image := r.RuntimeImage
	if image == "" {
		return verdict{reason: ReasonNoRuntimeImage, msg: "the operator has no --runtime-image ($" + RuntimeImageEnv + ")"}, nil
	}

	data, err := EncodeConfig(cfg)
	if err != nil {
		v := failClosed(ReasonInvalidPlan, err.Error())
		return v, nil
	}
	dep, err := r.apply(ctx, api, data, image, &ds, usesHash(cfg))
	if errors.Is(err, errNotControlled) {
		return verdict{reason: ReasonConflict, msg: err.Error()}, nil
	}
	if err != nil {
		return verdict{}, err
	}
	ok, reason, msg := deploymentReadiness(dep, api.Spec.Replicas)
	return verdict{
		ok: ok, reason: reason, msg: msg,
		readyReplicas: dep.Status.ReadyReplicas,
		serviceName:   api.Name,
		url:           ServiceURL(api.Name, api.Namespace),
	}, nil
}

// apply creates or updates the four children and returns the Deployment.
func (r *DomainAPIReconciler) apply(ctx context.Context, api *kabrv1.DomainAPI, data []byte, image string,
	ds *kabrv1.DataSource, hashed bool,
) (*appsv1.Deployment, error) {
	// NetworkPolicy first, so the pods never run without it.
	wantNP := RenderNetworkPolicy(api, ds)
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: wantNP.Name, Namespace: api.Namespace}}
	if err := r.createOrUpdate(ctx, api, np, func() {
		np.Labels = mergeLabels(np.Labels, wantNP.Labels)
		np.Spec = wantNP.Spec
	}); err != nil {
		return nil, err
	}

	wantCM := RenderConfigMap(api, data)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: wantCM.Name, Namespace: api.Namespace}}
	if err := r.createOrUpdate(ctx, api, cm, func() {
		cm.Labels = mergeLabels(cm.Labels, wantCM.Labels)
		cm.Data = wantCM.Data
		cm.BinaryData = nil
	}); err != nil {
		return nil, err
	}

	wantSvc := RenderService(api)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: wantSvc.Name, Namespace: api.Namespace}}
	if err := r.createOrUpdate(ctx, api, svc, func() {
		svc.Labels = mergeLabels(svc.Labels, wantSvc.Labels)
		// Only the fields the operator owns: clusterIP and the other
		// allocated fields stay as the API server set them.
		svc.Spec.Type = wantSvc.Spec.Type
		svc.Spec.Selector = wantSvc.Spec.Selector
		svc.Spec.Ports = wantSvc.Spec.Ports
	}); err != nil {
		return nil, err
	}

	wantDep := RenderDeployment(api, DeploymentInput{
		Image: image, ConfigChecksum: ConfigChecksum(data), DataSource: ds,
		MountHashKey: hashed && ds.Spec.HashKeySecretRef != nil,
	})
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: wantDep.Name, Namespace: api.Namespace}}
	if err := r.createOrUpdate(ctx, api, dep, func() {
		dep.Labels = mergeLabels(dep.Labels, wantDep.Labels)
		dep.Spec.Replicas = wantDep.Spec.Replicas
		if dep.CreationTimestamp.IsZero() {
			dep.Spec.Selector = wantDep.Spec.Selector // immutable after creation
		}
		dep.Spec.Template = wantDep.Spec.Template
	}); err != nil {
		return nil, err
	}
	return dep, nil
}

// createOrUpdate writes obj with the mutation and a controller reference to
// api. It refuses to adopt an existing object controlled by someone else.
func (r *DomainAPIReconciler) createOrUpdate(ctx context.Context, api *kabrv1.DomainAPI, obj client.Object, mutate func()) error {
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
		if ts := obj.GetCreationTimestamp(); !ts.IsZero() && !metav1.IsControlledBy(obj, api) {
			return fmt.Errorf("%w: %T %q is not controlled by DomainAPI %q", errNotControlled, obj, obj.GetName(), api.Name)
		}
		mutate()
		return controllerutil.SetControllerReference(api, obj, r.Scheme)
	})
	return err
}

func mergeLabels(have, want map[string]string) map[string]string {
	if have == nil {
		have = make(map[string]string, len(want))
	}
	for k, v := range want {
		have[k] = v
	}
	return have
}

// deleteChildren deletes the workload of api (only objects it controls).
// The Deployment goes first and the NetworkPolicy last.
func (r *DomainAPIReconciler) deleteChildren(ctx context.Context, api *kabrv1.DomainAPI) error {
	objs := []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: api.Name, Namespace: api.Namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: api.Name, Namespace: api.Namespace}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName(api.Name), Namespace: api.Namespace}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: api.Name, Namespace: api.Namespace}},
	}
	for _, o := range objs {
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(o), o); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if !metav1.IsControlledBy(o, api) {
			continue
		}
		if err := r.Client.Delete(ctx, o, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return err
		}
		log.FromContext(ctx).Info("deleted workload object", "kind", fmt.Sprintf("%T", o), "name", o.GetName())
	}
	return nil
}

// SetupWithManager registers the reconciler with mgr.
func (r *DomainAPIReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kabrv1.DomainAPI{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Watches(&kabrv1.DataSource{}, handler.EnqueueRequestsFromMapFunc(r.apisFor(indexDataSourceRef))).
		Watches(&kabrv1.DomainPlan{}, handler.EnqueueRequestsFromMapFunc(r.apisFor(indexPlanRef))).
		Named("domainapi").
		Complete(r)
}

// apisFor maps a DataSource or DomainPlan to the DomainAPIs of its
// namespace that reference it through field.
func (r *DomainAPIReconciler) apisFor(field string) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		var list kabrv1.DomainAPIList
		if err := r.Client.List(ctx, &list, client.InNamespace(o.GetNamespace()),
			client.MatchingFields{field: o.GetName()}); err != nil {
			log.FromContext(ctx).Error(err, "list domain APIs", "index", field)
			return nil
		}
		out := make([]reconcile.Request, 0, len(list.Items))
		for i := range list.Items {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
		return out
	}
}
