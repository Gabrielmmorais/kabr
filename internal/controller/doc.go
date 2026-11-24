// Package controller holds the Kabr operator's reconcilers for the
// kabr.dev/v1alpha1 API (DataSource, DomainPlan and DomainAPI). Their
// +kubebuilder:rbac markers are the source of config/rbac ("make manifests").
//
// The chain is DataSource + DomainPlan -> DomainAPI -> ConfigMap, Deployment,
// Service and NetworkPolicy:
//
//   - [DataSourceReconciler] checks that a DataSource is usable: a supported
//     driver, database CIDRs narrower than /0, and Secrets that exist and
//     hold the referenced keys. It reads Secrets with an uncached "get" and
//     inspects keys only (never values into logs, status or events); Secrets
//     are not watched or cached, DataSources are re-checked periodically. It
//     NEVER connects to the database; only the runtime pods ("kabr serve")
//     do.
//   - [DomainPlanReconciler] serves nothing unless plan.VerifyApproval and
//     ValidateStructure accept the plan. For an approved plan it owns one
//     DomainAPI per domain; when the approval breaks (for example the spec was
//     edited after approval) it deletes them: fail closed.
//   - [DomainAPIReconciler] renders and owns the workload of one domain. It
//     re-verifies the approval of the owning DomainPlan and builds the runtime
//     config from that verified plan (not from the DomainAPI's copy), so a
//     hand-made or edited DomainAPI cannot widen what is served.
//
// The approval digest is integrity, not authentication: anyone allowed to
// edit DomainPlans can recompute a matching digest and write any approvedBy.
// Who may approve is decided by Kubernetes RBAC on DomainPlans (and by review
// of the plan file), never by the approvedBy string. spec.dataSourceRef,
// rowLimit and replicas are outside the digest; pointing a plan at another
// database is caught by the runtime, which refuses to serve while the live
// catalog fingerprint differs from the approved one.
//
// RBAC: config/rbac/role.yaml is a ClusterRole generated from the markers in
// this package. It grants "get" on Secrets cluster-wide (no list/watch, no
// writes); a namespaced Role mode is backlog (M4). The pods of a DomainAPI
// always run the operator's runtime image: DomainAPI has no image field, so
// editing a DomainAPI cannot run another image with the DSN Secret mounted.
//
// Documented M4 items: ingress to the API pods is open on TCP 8080 to any
// source; the optional metrics endpoint has no authentication; the runtime
// image uses imagePullPolicy IfNotPresent (pin images by digest or unique
// tag).
//
// Children are written with controllerutil.CreateOrUpdate (not server-side
// apply): the operator is the only writer of these objects, the mutate
// functions set every field the operator cares about, and the API server
// turns updates that change nothing into no-ops.
package controller
