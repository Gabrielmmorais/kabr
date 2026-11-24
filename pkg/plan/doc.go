// Package plan defines the DomainPlan document: Kabr's reviewable proposal of
// how the tables of a database catalog are grouped into business domains.
//
// A DomainPlan is a Kubernetes-style document (apiVersion kabr.dev/v1alpha1,
// kind DomainPlan) serialized as YAML. It is produced by the segmenter (from
// the foreign key graph, optionally refined by an LLM), reviewed and approved
// by a human, and later consumed by the operator that turns every domain into
// an API.
//
// Lifecycle:
//
//	p := plan.New("pagila", catalog)          // Phase Proposed
//	p.Spec.Domains = ...                      // filled by the segmenter
//	p, warnings, err := plan.Apply(p, r, c)   // optional refinement
//	err = p.Validate(catalog)                 // structure + catalog coverage
//	err = plan.Approve(p, "alice", time.Now()) // records the spec digest
//	data, err := plan.Marshal(p)              // deterministic YAML
//	err = plan.VerifyApproval(p)              // approved and unchanged since
//
// The plan is pinned to the exact catalog structure it was generated for via
// [Fingerprint]; [DomainPlan.Validate] rejects a plan whose fingerprint does
// not match the catalog it is validated against.
//
// Approval is pinned to the exact spec that was reviewed via [SpecDigest],
// stored in status.approvalDigest by [Approve]. [VerifyApproval] and
// [DomainPlan.ValidateStructure] reject an approved plan whose spec was
// edited afterwards (reordering domains, tables or sensitive columns is not
// an edit) and an approved plan without a digest, which must be re-approved.
//
// A [Refinement] is the only thing an LLM (or another tool) may propose on
// top of a baseline plan: domain renames, table moves and sensitive column
// annotations. [Apply] applies it defensively, skipping invalid items with a
// warning and refusing results that no longer validate.
//
// The JSON Schema of the document is embedded and available via [Schema]; an
// identical copy is published at api/v1alpha1/domainplan.schema.json.
package plan
