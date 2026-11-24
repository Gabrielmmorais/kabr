package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// specDigestVersion is mixed into the hash so that a future change of the
// canonical form cannot collide with an old digest.
const specDigestVersion = "kabr-spec-digest-v1"

// Errors wrapped by [VerifyApproval] and [DomainPlan.ValidateStructure] so
// callers (the CLI, the operator) can tell the cases apart with errors.Is.
var (
	// ErrNotApproved: the plan is not in phase Approved, or its approver or
	// approval time is missing.
	ErrNotApproved = errors.New("plan is not approved")
	// ErrApprovalDigestMissing: the plan is Approved but has no
	// status.approvalDigest, i.e. it was approved by a Kabr version that did
	// not record one. It must be re-approved.
	ErrApprovalDigestMissing = errors.New("plan was approved before approval digests existed")
	// ErrEditedAfterApproval: the spec no longer hashes to
	// status.approvalDigest, i.e. it was edited after approval.
	ErrEditedAfterApproval = errors.New("approved plan was edited after approval")
)

const reapproveHint = `re-approve it with "kabr plan approve"`

// CanonicalSpec returns a deep copy of s in its serialized order: domains
// sorted by name, tables by name and sensitive columns by (table, column); a
// nil domain or table list becomes an empty one. s is not modified.
func CanonicalSpec(s Spec) Spec {
	p := (&DomainPlan{Spec: s}).clone()
	p.canonicalize()
	return p.Spec
}

// SpecDigest returns "sha256:<hex>" over the canonical JSON encoding of the
// spec (see [CanonicalSpec]): the source and every domain with its tables,
// sensitive columns, descriptions and rationales. The order of domains,
// tables and sensitive columns does not change the digest; any other edit
// does. [Approve] records it in status.approvalDigest.
//
// The hashed bytes are the line "kabr-spec-digest-v1\n" followed by the
// canonical spec as written by a default encoding/json Encoder (struct field
// order, empty optional fields omitted, trailing newline), so an optional
// field added to the format later does not change the digest of plans that
// leave it empty.
func SpecDigest(s Spec) string {
	h := sha256.New()
	h.Write([]byte(specDigestVersion + "\n"))
	// Encoding plain strings, bools and slices cannot fail.
	_ = json.NewEncoder(h).Encode(CanonicalSpec(s))
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// approvalDigestProblem compares status.approvalDigest with the digest of
// the spec. It returns the current digest of the spec ("" when the status
// has no digest) and nil, [ErrApprovalDigestMissing] or
// [ErrEditedAfterApproval].
func approvalDigestProblem(st Status, s Spec) (current string, problem error) {
	if st.ApprovalDigest == "" {
		return "", ErrApprovalDigestMissing
	}
	if current = SpecDigest(s); current != st.ApprovalDigest {
		return current, ErrEditedAfterApproval
	}
	return current, nil
}

// VerifyApproval checks that the plan is approved and was not changed since:
// phase Approved, a non-empty approvedBy, an approvedAt time and an
// approvalDigest equal to [SpecDigest] of the current spec. The error wraps
// [ErrNotApproved], [ErrApprovalDigestMissing] (approved before digests
// existed: re-approve) or [ErrEditedAfterApproval]. It does not validate the
// structure of the plan; see [DomainPlan.ValidateStructure].
func VerifyApproval(p *DomainPlan) error {
	if p == nil {
		return fmt.Errorf("plan: %w", errNilPlan)
	}
	name, st := p.Metadata.Name, p.Status
	switch {
	case st.Phase != PhaseApproved:
		phase := string(st.Phase)
		if phase == "" {
			phase = "<unset>"
		}
		return fmt.Errorf("%w: %q has phase %s; approve it with \"kabr plan approve\"", ErrNotApproved, name, phase)
	case strings.TrimSpace(st.ApprovedBy) == "":
		return fmt.Errorf("%w: %q has phase Approved but no status.approvedBy; %s", ErrNotApproved, name, reapproveHint)
	case st.ApprovedAt == nil || st.ApprovedAt.IsZero():
		return fmt.Errorf("%w: %q has phase Approved but no status.approvedAt; %s", ErrNotApproved, name, reapproveHint)
	}
	switch current, problem := approvalDigestProblem(st, p.Spec); {
	case problem == nil:
		return nil
	case errors.Is(problem, ErrApprovalDigestMissing):
		return fmt.Errorf("%w: %q has no status.approvalDigest; %s", problem, name, reapproveHint)
	default:
		return fmt.Errorf("%w: the spec of %q now hashes to %q, not the approved %q; review the changes and %s",
			problem, name, current, st.ApprovalDigest, reapproveHint)
	}
}
