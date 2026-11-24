// Package segment proposes a deterministic split of a [schema.Catalog] into
// business domains, using only the catalog structure (no AI). It is the
// baseline that later refinement stages start from.
//
// # Pipeline
//
//  1. Graph. The weighted undirected table graph is the foreign key graph of
//     package graph (one unit of weight per foreign key between two tables,
//     self-references ignored) plus, when [Options].InferEdges is set,
//     references inferred from column names ([InferredEdge]) weighing
//     [Options].InferredWeight each. A column "X_id" (or "prefix_X_id") not
//     covered by a declared foreign key references table X (or a simple
//     plural/singular form of it) of the same schema when X has the single
//     column primary key "X_id" or "id" of a compatible type family.
//  2. Roles. Tables with no edge at all are orphans. A table whose composite
//     primary key consists only of referencing columns reaching two or more
//     tables is a junction. A small table ([Options].LookupMaxColumns) with no
//     outgoing reference and at least two incoming references is a lookup.
//     Everything else is core.
//  3. Clustering. Louvain (local moving + aggregation, repeated until no
//     modularity gain) runs on the core tables only. Each junction is
//     replaced by edges between the core tables it associates (weight: the
//     smaller of its two links), so that, for example, the two sides of a
//     many-to-many relation stay together without the junction itself acting
//     as a hub. Lookups are excluded so that a widely referenced reference
//     table cannot glue unrelated domains together.
//  4. Placement. Each junction joins the community of its owner, the
//     referenced core table with the highest weighted degree. Each lookup
//     joins the community holding most of its edge weight and is marked
//     Shared when referenced from two or more communities. All orphans share
//     one orphan bucket community.
//
// Every assignment carries a Rationale naming the structural evidence, and
// [Result].Modularity reports the standard Newman modularity (γ = 1) of the
// final partition on the full graph.
//
// # Determinism
//
// The catalog is cloned and normalized first, nodes are visited in sorted ID
// order, a node only changes community for a strictly better gain, gain ties
// go to the smallest community label, and every map is iterated through its
// sorted keys. No randomness is used. The same catalog content therefore
// yields a byte-identical [Result] whatever the order of its tables, columns
// or constraints.
//
// # Defaults
//
//   - Resolution 1.0: the standard modularity definition, so the clustering
//     optimizes exactly the quantity it reports; no dataset-specific tuning.
//     Raise it (for example to 1.5) to obtain finer domains.
//   - InferEdges true, InferredWeight 0.5: missing foreign keys are common
//     (partitioned tables, legacy schemas, ORMs), yet a name match is weaker
//     evidence than a declared constraint, hence half the weight of an FK.
//   - LookupMaxColumns 4: a key, a label and one or two audit columns (for
//     example id, name, last_update) is the typical shape of a reference
//     table; larger tables carry business data of their own.
//
// The disconnected parts of the graph are never merged: Louvain only moves a
// table towards the communities of its neighbors.
package segment
