// Package graph builds a directed table graph from a [schema.Catalog] and
// exports catalogs as Mermaid ER diagrams and Graphviz DOT.
//
// # Model
//
// Nodes are the catalog's tables, identified by [schema.TableID]
// ("schema.table"). There is one directed [Edge] per ordered pair of tables
// linked by at least one foreign key, pointing from the referencing (child)
// table to the referenced (parent) table. A composite foreign key counts as
// ONE foreign key. Edge.Weight is the number of foreign keys between the
// pair, and Edge.ForeignKeys lists their names. A table referencing itself
// yields a self-loop flagged with Edge.SelfReference.
//
// Foreign keys that reference a table absent from the catalog (for example
// when only some schemas were introspected) produce no edge and no node; they
// are counted in [Metrics].ExternalForeignKeys.
//
// # Conventions
//
//   - Degree counts foreign keys (edge weights), not distinct neighbors. A
//     self-reference contributes to both the in- and the out-degree.
//   - Orphans are tables with no edge to or from ANOTHER table. A table whose
//     only foreign keys are self-references is therefore an orphan, because
//     it is isolated with respect to the rest of the schema.
//   - Connected components are weakly connected (edge direction ignored).
//
// [Graph.UndirectedNeighbors] exposes the symmetric weighted adjacency
// (self-loops excluded) and is the intended input for community detection
// (for example Louvain/Leiden) when segmenting the schema into domains.
//
// Every list returned by this package is sorted, and both exporters produce
// byte-for-byte deterministic output for the same catalog content.
package graph
