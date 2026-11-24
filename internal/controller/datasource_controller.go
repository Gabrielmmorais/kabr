package controller

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kabrv1 "github.com/Gabrielmmorais/kabr/api/v1alpha1"
)

// DataSourceReconciler validates DataSources. It checks that the referenced
// Secrets exist and contain the referenced keys, and NEVER connects to the
// database (only the runtime pods do).
//
// The key check needs "get" on Secrets, and nothing else: the operator reads
// the Secret with an uncached reader (Reader), looks only at which keys exist
// and drops the object; values are never copied, logged or written to the
// status or an event. Secrets are NOT watched (no list/watch, no informer: a
// metadata informer would still cache annotations such as kubectl's
// last-applied-configuration, which can hold the Secret data). Instead every
// DataSource is re-checked every RecheckInterval and on its own changes.
//
// The ClusterRole grants "get" on Secrets in every namespace; a namespaced
// Role mode (one Role per watched namespace) is planned for M4.
type DataSourceReconciler struct {
	Client client.Client
	// Reader reads Secrets directly from the API server (mgr.GetAPIReader()).
	Reader client.Reader
	// RecheckInterval is how often the Secrets are re-checked; zero means
	// DefaultSecretRecheckInterval.
	RecheckInterval time.Duration
}

// +kubebuilder:rbac:groups=kabr.dev,resources=datasources,verbs=get;list;watch
// +kubebuilder:rbac:groups=kabr.dev,resources=datasources/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

// Reconcile implements [reconcile.Reconciler].
func (r *DataSourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ds kabrv1.DataSource
	if err := r.Client.Get(ctx, req.NamespacedName, &ds); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	ok, reason, msg, err := r.check(ctx, &ds)
	if err != nil {
		return ctrl.Result{}, err
	}
	before := ds.DeepCopy()
	ds.Status.ObservedGeneration = ds.Generation
	setReady(&ds.Status.Conditions, ds.Generation, ok, reason, msg)
	if err := r.Client.Status().Patch(ctx, &ds, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !ok {
		log.FromContext(ctx).V(1).Info("data source not ready", "reason", reason)
	}
	interval := r.RecheckInterval
	if interval <= 0 {
		interval = DefaultSecretRecheckInterval
	}
	return ctrl.Result{RequeueAfter: interval}, nil
}

// check returns the Ready verdict of ds. Transient API errors are returned
// as err (retried with backoff); everything else is a verdict.
func (r *DataSourceReconciler) check(ctx context.Context, ds *kabrv1.DataSource) (ok bool, reason, msg string, err error) {
	if reason, msg := validateDataSourceSpec(&ds.Spec); reason != "" {
		return false, reason, msg, nil
	}
	key := ds.Spec.SecretRef.Key
	if key == "" {
		key = kabrv1.DefaultDSNSecretKey
	}
	if reason, msg, err := r.checkSecretKey(ctx, ds.Namespace, ds.Spec.SecretRef.Name, key, "secretRef"); reason != "" || err != nil {
		return false, reason, msg, err
	}
	if h := ds.Spec.HashKeySecretRef; h != nil {
		key := h.Key
		if key == "" {
			key = kabrv1.DefaultHashKeySecretKey
		}
		if reason, msg, err := r.checkSecretKey(ctx, ds.Namespace, h.Name, key, "hashKeySecretRef"); reason != "" || err != nil {
			return false, reason, msg, err
		}
	}
	return true, ReasonReconciled, "Secrets found; the operator does not connect to the database (the API pods check it at startup)", nil
}

// validateDataSourceSpec repeats the checks of the CRD schema that the
// operator relies on (defense in depth against an outdated CRD). It returns
// an empty reason when the spec is valid.
func validateDataSourceSpec(s *kabrv1.DataSourceSpec) (reason, msg string) {
	switch {
	case s.Driver != kabrv1.DriverPostgres:
		return ReasonInvalidSpec, fmt.Sprintf("spec.driver %q is not supported (supported: %s)", s.Driver, kabrv1.DriverPostgres)
	case s.SecretRef.Name == "":
		return ReasonInvalidSpec, "spec.secretRef.name is required"
	case s.Network.Port < 1 || s.Network.Port > 65535:
		return ReasonInvalidSpec, fmt.Sprintf("spec.network.port %d is out of range", s.Network.Port)
	case len(s.Network.CIDRs) == 0 && s.Network.PodSelector == nil:
		return ReasonInvalidSpec, "spec.network needs cidrs or podSelector"
	case s.Network.NamespaceSelector != nil && s.Network.PodSelector == nil:
		return ReasonInvalidSpec, "spec.network.namespaceSelector requires podSelector"
	case s.HashKeySecretRef != nil && s.HashKeySecretRef.Name == "":
		return ReasonInvalidSpec, "spec.hashKeySecretRef.name is required"
	}
	for _, c := range s.Network.CIDRs {
		pfx, err := netip.ParsePrefix(c)
		switch {
		case err != nil:
			return ReasonInvalidSpec, fmt.Sprintf("spec.network.cidrs: %q is not a valid CIDR", c)
		case pfx.Bits() == 0:
			// A /0 block would turn the database egress rule into "anywhere".
			return ReasonInvalidSpec, fmt.Sprintf("spec.network.cidrs: %q allows every address; name the database's address blocks", c)
		}
	}
	return "", ""
}

// checkSecretKey reports whether Secret name in ns has a non-empty key. The
// Secret is read uncached and only its key set is inspected.
func (r *DataSourceReconciler) checkSecretKey(ctx context.Context, ns, name, key, field string) (reason, msg string, err error) {
	var s corev1.Secret
	switch err := r.Reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &s); {
	case apierrors.IsNotFound(err):
		return ReasonSecretNotFound, fmt.Sprintf("%s: Secret %q not found", field, name), nil
	case apierrors.IsForbidden(err):
		return ReasonSecretUnreadable, fmt.Sprintf("%s: the operator may not get Secret %q (RBAC)", field, name), nil
	case err != nil:
		return "", "", err
	}
	v, found := s.Data[key]
	n := len(v)
	clear(v) // drop the value as early as possible
	s.Data, s.StringData = nil, nil
	if !found || n == 0 {
		return ReasonSecretKeyMissing, fmt.Sprintf("%s: Secret %q has no non-empty key %q", field, name, key), nil
	}
	return "", "", nil
}

// SetupWithManager registers the reconciler with mgr. Secrets are not
// watched; see [DataSourceReconciler].
func (r *DataSourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kabrv1.DataSource{}).
		Named("datasource").
		Complete(r)
}
