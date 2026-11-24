package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrl "sigs.k8s.io/controller-runtime/pkg/manager"

	kabrv1 "github.com/Gabrielmmorais/kabr/api/v1alpha1"
)

// Labels set by the operator on every object it creates.
const (
	LabelManagedBy  = "app.kubernetes.io/managed-by"
	ManagedByValue  = "kabr-operator"
	LabelPlan       = "kabr.dev/plan"
	LabelDomain     = "kabr.dev/domain"
	LabelDomainAPI  = "kabr.dev/domainapi"
	LabelName       = "app.kubernetes.io/name"
	LabelNameValue  = "kabr-domain-api"
	LabelInstance   = "app.kubernetes.io/instance"
	LabelComponent  = "app.kubernetes.io/component"
	ComponentServer = "api-server"
)

// AnnotationConfigChecksum is the pod template annotation holding the SHA-256
// of the rendered domain.json, so that a config change rolls the pods.
const AnnotationConfigChecksum = "kabr.dev/config-sha256"

// Condition reasons (metav1.Condition.Reason) of the Ready condition.
const (
	// Shared.
	ReasonReconciled = "Reconciled"

	// DataSource.
	ReasonInvalidSpec      = "InvalidSpec"
	ReasonSecretNotFound   = "SecretNotFound"
	ReasonSecretKeyMissing = "SecretKeyMissing"
	ReasonSecretUnreadable = "SecretUnreadable"

	// DomainPlan (and DomainAPI, about its plan).
	ReasonNotApproved           = "NotApproved"
	ReasonApprovalDigestMissing = "ApprovalDigestMissing"
	ReasonEditedAfterApproval   = "EditedAfterApproval"
	ReasonInvalidPlan           = "InvalidPlan"
	ReasonDataSourceNotFound    = "DataSourceNotFound"

	// DomainAPI.
	ReasonPlanNotFound   = "PlanNotFound"
	ReasonNotOwnedByPlan = "NotOwnedByPlan"
	ReasonOutOfSync      = "OutOfSync"
	ReasonNoRuntimeImage = "NoRuntimeImage"
	ReasonConflict       = "Conflict"
	ReasonScaledToZero   = "ScaledToZero"
	ReasonProgressing    = "Progressing"
	ReasonDegraded       = "Degraded"
	ReasonAvailable      = "Available"
)

// Field indexes registered by [Setup].
const (
	// indexDataSourceRef indexes DomainPlans and DomainAPIs by
	// spec.dataSourceRef.name.
	indexDataSourceRef = "spec.dataSourceRef.name"
	// indexPlanRef indexes DomainAPIs by spec.planRef.name.
	indexPlanRef = "spec.planRef.name"
)

// errNotControlled: an object with the name the operator needs exists but is
// controlled by someone else; it is reported (reason Conflict), not adopted.
var errNotControlled = errors.New("object exists and is not controlled by the operator")

// RuntimeImageEnv is the environment variable read for the default runtime
// image of the DomainAPI pods (the --runtime-image flag of "kabr operator").
const RuntimeImageEnv = "KABR_RUNTIME_IMAGE"

// Owner references set with controllerutil.SetControllerReference carry
// blockOwnerDeletion, which clusters with the OwnerReferencesPermissionEnforcement
// admission plugin only allow with "update" on the owner's finalizers.
// +kubebuilder:rbac:groups=kabr.dev,resources=domainplans/finalizers;domainapis/finalizers,verbs=update

// Options configures the reconcilers.
type Options struct {
	// RuntimeImage is the container image of every DomainAPI pod (a
	// DomainAPI cannot override it). Usually the operator's own image.
	RuntimeImage string
	// SecretRecheckInterval is how often DataSources re-check their Secrets
	// (Secrets are not watched); zero means DefaultSecretRecheckInterval.
	SecretRecheckInterval time.Duration
}

// DefaultSecretRecheckInterval is the default of
// [Options.SecretRecheckInterval].
const DefaultSecretRecheckInterval = 60 * time.Second

// Setup registers the field indexes and the three reconcilers with mgr.
func Setup(ctx context.Context, mgr ctrl.Manager, opts Options) error {
	if err := setupIndexes(ctx, mgr); err != nil {
		return err
	}
	if err := (&DataSourceReconciler{
		Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), RecheckInterval: opts.SecretRecheckInterval,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("controller: DataSource: %w", err)
	}
	if err := (&DomainPlanReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Recorder: mgr.GetEventRecorder("kabr-operator"),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("controller: DomainPlan: %w", err)
	}
	if err := (&DomainAPIReconciler{
		Client: mgr.GetClient(), Scheme: mgr.GetScheme(), RuntimeImage: opts.RuntimeImage,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("controller: DomainAPI: %w", err)
	}
	return nil
}

func setupIndexes(ctx context.Context, mgr ctrl.Manager) error {
	fi := mgr.GetFieldIndexer()
	idx := []struct {
		obj   client.Object
		field string
		fn    client.IndexerFunc
	}{
		{&kabrv1.DomainPlan{}, indexDataSourceRef, func(o client.Object) []string {
			return nonEmpty(o.(*kabrv1.DomainPlan).Spec.DataSourceRef.Name)
		}},
		{&kabrv1.DomainAPI{}, indexDataSourceRef, func(o client.Object) []string {
			return nonEmpty(o.(*kabrv1.DomainAPI).Spec.DataSourceRef.Name)
		}},
		{&kabrv1.DomainAPI{}, indexPlanRef, func(o client.Object) []string {
			return nonEmpty(o.(*kabrv1.DomainAPI).Spec.PlanRef.Name)
		}},
	}
	for _, i := range idx {
		if err := fi.IndexField(ctx, i.obj, i.field, i.fn); err != nil {
			return fmt.Errorf("controller: index %T %s: %w", i.obj, i.field, err)
		}
	}
	return nil
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

// setReady sets the Ready condition of conds for generation gen.
func setReady(conds *[]metav1.Condition, gen int64, ok bool, reason, msg string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(conds, metav1.Condition{
		Type:               kabrv1.ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            truncate(msg, 1024),
		ObservedGeneration: gen,
	})
}

// truncate bounds condition messages (the API allows 32768 bytes; a short
// message reads better in kubectl).
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n-3], "") + "..."
}
