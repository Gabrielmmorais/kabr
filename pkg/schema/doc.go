// Package schema defines Kabr's canonical, versioned model of a relational
// database catalog.
//
// A [Catalog] is the single hand-off format between the stages of Kabr:
// introspectors (for example the PostgreSQL introspector) fill it from the
// database catalog using read-only queries, the graph package turns it into a
// table graph, and later milestones segment it into domains. The model only
// describes structure and planner statistics; it never carries row data.
//
// The document is versioned through [SchemaVersion]. Consumers should call
// [Catalog.Validate] after building or loading a catalog and
// [Catalog.Normalize] before emitting it, so that the output is byte-for-byte
// deterministic. [Marshal] and [Unmarshal] wrap both steps for JSON.
//
// Conventions:
//   - Tables are identified by [TableID], i.e. "schema.name".
//   - Column lists inside keys, foreign keys, indexes and unique constraints
//     are ordered as declared in the database and are never re-sorted.
//   - Index column entries may be expressions (for example "lower(email)")
//     when the index is an expression index; they are kept verbatim.
//   - Optional values (defaults, statistics) are pointers so that "unknown"
//     is distinguishable from a zero value.
package schema
