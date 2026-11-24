package plan

import (
	_ "embed"
	"slices"
)

//go:embed domainplan.schema.json
var jsonSchema []byte

// Schema returns the JSON Schema (draft 2020-12) of the DomainPlan document.
// An identical copy is published at api/v1alpha1/domainplan.schema.json. The
// returned slice is a copy and may be modified by the caller.
func Schema() []byte {
	return slices.Clone(jsonSchema)
}
