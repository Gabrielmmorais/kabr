# ADR 0006: Operator and CRD model

- **Status:** Accepted
- **Date:** 2026-11-24

## Context

Milestone M3 turns an approved `DomainPlan` (M2,
[ADR 0004](0004-segmentation-baseline-llm-approval.md)) into running,
read-only REST APIs, one per domain, on Kubernetes. Until M2 the plan was a
YAML file approved with `kabr plan approve`; nothing consumed it.

The operator sits between a human decision (the approved plan) and a
workload that holds a database credential. That shapes the requirements:

1. **Nothing beyond what was approved.** A plan edited after approval, or a
   hand-made object that claims to come from a plan, must serve nothing.
2. **The plan model must not fork.** The CLI, the evals and the operator
   must read the same `pkg/plan` types; a second, Kubernetes-flavoured copy
   of the model would drift.
3. **Least privilege in the cluster.** The operator should not need the
   database, should not keep Secret data in memory, and a user who can edit
   a Kabr object must not be able to run arbitrary code next to the DSN.
4. **Fail closed.** When anything is missing or inconsistent, the result is
   no workload (or a workload that answers 503), never a widened one.

## Decision

### Three namespaced CRDs (`kabr.dev/v1alpha1`)

| Kind | Short name | Written by | Role |
|------|------------|------------|------|
| `DataSource` | `kds` | a user | How the APIs reach one database: `driver` (`postgres`), `secretRef` (a Secret key holding the DSN of a read-only role), `network` (TCP port plus `cidrs` and/or `podSelector`/`namespaceSelector`), optional `hashKeySecretRef` (HMAC key for `hash` columns; without it they are dropped). No credential is ever inline. |
| `DomainPlan` | `kdp` | `kabr plan manifest`, applied by a user | The approved plan bound to a `DataSource`. `spec` embeds `plan.Spec` **inline and unchanged** (same Go type, same JSON fields), plus `dataSourceRef`, `approval` and the optional knobs `rowLimit` (1..1000, default 100) and `replicas` (0..10, default 1). |
| `DomainAPI` | `kapi` | the operator only | One domain of one approved plan: `domain`, `planRef`, `dataSourceRef`, `catalogFingerprint`, `tables`, `schemas`, `sensitiveColumns`, `rowLimit`, `replicas`. |

All three are namespaced and every reference (`dataSourceRef`, `planRef`,
Secret names) is to an object in the same namespace. Each kind has a
`status.conditions` list with a `Ready` condition (reasons such as
`NotApproved`, `EditedAfterApproval`, `DataSourceNotFound`,
`SecretKeyMissing`, `NotOwnedByPlan`, `Progressing`, `Available`) and
`status.observedGeneration`. `DomainPlan.status.domainAPIs` lists the
children; `DomainAPI.status` carries `readyReplicas`, `serviceName` and the
in-cluster `url`.

Embedding `plan.Spec` keeps one model: `DomainPlan.ToPlan()` rebuilds a
plan document from the custom resource, and the operator checks it with the
same `plan.VerifyApproval` and `ValidateStructure` the CLI uses on a file.

### From plan file to custom resource: `kabr plan manifest`

DomainPlans are not written by hand. `kabr plan manifest FILE --datasource
NAME [--namespace NS] [--out FILE]` takes an **approved** plan file, refuses
it (exit 1, nothing written) unless it validates and its approval verifies,
and prints the custom resource: `metadata.name` is the plan name, `spec`
holds the canonical plan spec, `spec.dataSourceRef` names the DataSource and
`spec.approval` copies `approvedBy`, `approvedAt` and the approval digest.
Annotations, generator details and warnings of the file are dropped; they
were not part of what was approved.

### The approval digest, and its limit

`kabr plan approve` now records `status.approvalDigest` in the plan file,
copied to `spec.approval.digest` in the custom resource. It is
`plan.SpecDigest`: `sha256:` over a versioned prefix
(`kabr-spec-digest-v1`) and the canonical JSON of the spec, that is the
source (catalog fingerprint, database, dialect) and every domain with its
tables, sensitive columns, descriptions and rationales. The order of
domains, tables and sensitive columns does not change it; any other edit
does. A plan whose spec no longer hashes to its digest fails
`VerifyApproval` (`ErrEditedAfterApproval`); a plan approved before digests
existed fails with `ErrApprovalDigestMissing` and must be re-approved.

**The digest is integrity, not authentication.** It proves that the spec
was not changed after the approval was recorded. It does not prove who
approved it: anyone allowed to edit DomainPlans can recompute a matching
digest and write any `approvedBy`. The real control is Kubernetes RBAC on
`domainplans` (and on `domainapis`, `datasources` and the Secrets they
reference), plus review of the plan file before it is applied. The
`DomainPlan` `Ready` message says so explicitly ("not authenticated"). Signed
approvals are future work (M4+).

`spec.dataSourceRef`, `rowLimit` and `replicas` are outside the digest: they
are deployment choices, not part of the segmentation decision. Pointing an
approved plan at a different database is therefore not caught by the digest.
It is caught by the runtime: every API pod introspects the live catalog and
refuses to serve while its fingerprint differs from the approved
`catalogFingerprint` (see [ADR 0007](0007-runtime-query-safety.md)).

### The operator never connects to the database

`kabr operator` has no database credential and opens no database
connection. The `DataSource` reconciler checks only what it can see in the
cluster: a supported driver, network CIDRs that are valid and narrower than
`/0`, and Secrets that exist and hold non-empty values under the referenced
keys. It never reads the DSN into logs, status or events. Whether the
database is reachable, whether the role can read the tables and whether the
catalog still matches the plan is decided by the runtime pods (`kabr serve`)
themselves, which stay NotReady until it does.

### Reconciliation chain and fail-closed rules

`DataSource` + `DomainPlan` -> `DomainAPI` -> `ConfigMap`, `Deployment`,
`Service`, `NetworkPolicy`. Every child gets a controller owner reference,
so deleting a DomainPlan garbage-collects its DomainAPIs and their
workloads.

- **DomainPlan reconciler.** Serves nothing unless `VerifyApproval` and
  `ValidateStructure` accept `ToPlan()`. For an approved plan it creates or
  updates one DomainAPI per domain, named `<plan>-<domain>` (must be a
  DNS-1035 label), and deletes owned DomainAPIs of domains that left the
  plan. When the approval breaks (for example the spec was edited in the
  cluster) it **deletes all of the plan's DomainAPIs** and emits a Warning
  event. If the DataSource is missing, existing DomainAPIs are kept (their
  pods cannot start without it) and nothing new is created. Owned
  DomainAPIs are found by owner reference, not by label (labels are
  editable).
- **DomainAPI reconciler.** Does **not** trust `DomainAPI.spec` for what is
  served: anyone with write access to DomainAPIs could edit it, and it
  carries no approval binding. On every reconcile it resolves
  `spec.planRef`, requires that DomainPlan to be the DomainAPI's controller,
  re-verifies the plan's approval, requires the domain to exist in the plan
  and the DomainAPI's name to be exactly `<plan>-<domain>` (an owner
  reference is only a UID that anyone can copy into an object they create),
  and then renders the runtime config from the verified plan with
  `serve.ConfigFromPlan`. Only `rowLimit` (capped at the runtime maximum)
  and `replicas` are taken from the DomainAPI. A missing, foreign or
  unapproved plan means `Ready=False` and the workload is deleted.
- **No image field on DomainAPI.** The pods always run the operator's
  runtime image (`--runtime-image` / `KABR_RUNTIME_IMAGE`, operator
  configuration). Adversarial review of an earlier draft found that an
  editable image field would let anyone who can edit a DomainAPI run an
  arbitrary image with the DSN Secret mounted; the field was removed.
- The rendered `domain.json` is decoded back with the runtime's strict
  decoder before it is written, so the operator never ships a config the
  runtime would refuse. Its checksum is a pod-template annotation, so a
  config change rolls the Deployment.

Children are written with `controllerutil.CreateOrUpdate`, not server-side
apply: the operator is the only writer, the mutate functions set every
field the operator cares about, and unchanged updates are no-ops. The
operator refuses to adopt an existing object controlled by someone else
(`Ready=False`, reason `Conflict`).

### Secrets

- The manager's cache is disabled for Secrets: they are read with an
  uncached `get`, keys only, and are never watched or cached. DataSources
  are re-checked periodically instead of on Secret events.
- The generated ClusterRole grants `get` on Secrets and nothing else (no
  `list`, `watch` or writes). The DSN and, only for a domain that hashes a
  non-credential column, the hash key reach the runtime pod
  as files mounted from the Secrets (mode 0400, `fsGroup` so the non-root
  user can read them), never as environment variables or flags, and never
  in the ConfigMap.
- RBAC is a single **ClusterRole**, so the operator can `get` any Secret in
  the cluster. `--watch-namespace` restricts what the operator reconciles,
  not what it is allowed to read. A namespaced Role mode is planned for M4.

### Workload shape

- **One binary, one image.** `kabr operator` and `kabr serve` are
  subcommands of the same `kabr` binary, shipped as one distroless,
  non-root image. The runtime pod runs `kabr serve --config ... --dsn-file
  ...` from that image.
- **Pods.** Non-root (UID/GID 65532), read-only root filesystem, no
  privilege escalation, all capabilities dropped, `RuntimeDefault` seccomp,
  no service-account token, no service links, resource requests and limits,
  liveness on `/healthz` and readiness on `/readyz`.
- **Service.** ClusterIP on TCP 8080.
- **NetworkPolicy**, created before the Deployment so the pods never run
  without it. It selects only the pods of one DomainAPI and allows:
  - egress to the DataSource's network on its TCP port (one peer per CIDR,
    plus the pod/namespace selector), and to cluster DNS (UDP and TCP 53 to
    `kube-system` pods labeled `k8s-app=kube-dns`); nothing else;
  - ingress on TCP 8080 **from any source**. Restricting callers is M4
    (Gateway API and per-consumer policy).

  A DataSource must name at least one CIDR or a pod selector, and a `/0`
  CIDR is rejected both by CRD validation (CEL) and by the reconciler, so the
  database rule can never become "allow every destination". A
  NetworkPolicy only takes effect on a CNI that enforces it.

## Alternatives considered

- **The operator introspects the database itself.** It could verify the
  fingerprint before creating anything and report drift in the DomainPlan
  status. Rejected: the operator would need network access to every
  database and a credential for each, concentrating access in one
  cluster-wide component. The runtime has to check the live catalog anyway
  (drift can happen after deployment), so the check lives once, where the
  credential already is.
- **A sidecar or per-pod init container for the checks.** Rejected: one
  process that both verifies the catalog and serves is simpler, and its
  readiness gates traffic directly. A separate checker would need its own
  credential mount and a way to signal the server.
- **Helm chart only, no operator.** Render the Deployments from the plan at
  install time. Rejected for M3: nothing would re-verify the approval when a
  plan is edited in the cluster, delete workloads when it breaks, or keep
  children consistent with the plan. A Helm chart for installing the
  operator is still planned (M4+).
- **Admission webhooks** to reject unapproved DomainPlans and hand-made
  DomainAPIs at write time. Deferred, not rejected: a webhook needs
  certificates and becomes a cluster-wide availability dependency. In M3
  the reconcilers fail closed instead, so a bad object can exist but serves
  nothing. A validating webhook is a candidate for M4.
- **A Kubernetes-specific copy of the plan types.** Rejected: two models
  would drift. The CRD embeds `plan.Spec`; the cost is that CRD validation
  mirrors only part of `ValidateStructure` (see Consequences).
- **Server-side apply for children.** Not needed while the operator is the
  only writer; `CreateOrUpdate` with complete mutate functions is simpler to
  test.

## Consequences

**Positive**

- One plan model from CLI to cluster; the approval check in the operator is
  the same code as in `kabr plan manifest` and `kabr serve --plan`.
- Editing an approved DomainPlan, hand-making a DomainAPI, or editing a
  DomainAPI's tables or sensitive columns cannot widen what is served:
  either nothing runs, or the runtime config is rebuilt from the verified
  plan.
- The operator holds no database credential, opens no database connection
  and keeps no Secret data in its cache.
- The whole chain is exercised by envtest (real kube-apiserver and etcd) in
  CI, and end to end on kind with `make e2e` (also in CI).

**Negative / trade-offs**

- The approval digest is not an authentication mechanism. Whoever can write
  DomainPlans is effectively an approver; RBAC must be set accordingly.
- `dataSourceRef`, `rowLimit` and `replicas` can be changed without
  re-approval. A changed `dataSourceRef` is caught only at runtime by the
  fingerprint check, and only if the other database's catalog differs.
- The ClusterRole can read any Secret in the cluster (`get` only).
- Ingress to the API pods is open on TCP 8080 within the cluster, and the
  optional operator metrics endpoint has no authentication; both are M4
  items.
- CRD validation does not mirror every rule of `ValidateStructure` (for
  example uniqueness of sensitive columns is not enforced by CEL). Such a
  plan is accepted by the API server but rejected by the operator
  (`InvalidPlan`, nothing served).
- The runtime image is pulled with `IfNotPresent`; pin it by digest or a
  unique tag.
