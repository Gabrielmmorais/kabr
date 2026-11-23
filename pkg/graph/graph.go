package graph

import (
	"cmp"
	"slices"

	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

// Edge is a directed edge from the referencing table (From) to the
// referenced table (To).
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Weight is the number of foreign keys from From to To.
	Weight int `json:"weight"`
	// ForeignKeys holds the names of those foreign keys, sorted.
	ForeignKeys []string `json:"foreignKeys"`
	// SelfReference is true when From == To.
	SelfReference bool `json:"selfReference,omitempty"`
}

// Graph is an immutable directed table graph. Build it with [Build].
type Graph struct {
	nodes       []string
	nodeSet     map[string]bool
	edges       []Edge
	in, out     map[string]int
	adj         map[string]map[string]float64 // undirected, no self-loops
	externalFKs int
}

// Metrics summarizes a graph.
type Metrics struct {
	Nodes int `json:"nodes"`
	// Edges is the number of distinct directed table pairs (self-loops
	// included).
	Edges int `json:"edges"`
	// ForeignKeys is the total number of foreign keys represented by edges
	// (sum of edge weights).
	ForeignKeys int `json:"foreignKeys"`
	// SelfReferences is the number of tables with a self-loop.
	SelfReferences int `json:"selfReferences"`
	// ExternalForeignKeys counts foreign keys whose referenced table is not
	// in the catalog; they are not part of the graph.
	ExternalForeignKeys int `json:"externalForeignKeys"`
	Components          int `json:"components"`
	Orphans             int `json:"orphans"`
	// MaxDegree is the highest in+out degree and MaxDegreeTable the table
	// holding it (lexicographically smallest on ties; empty for an empty
	// graph).
	MaxDegree      int    `json:"maxDegree"`
	MaxDegreeTable string `json:"maxDegreeTable,omitempty"`
}

// Build constructs the graph of a catalog. The catalog is not modified and
// need not be normalized. A nil catalog yields an empty graph.
func Build(c *schema.Catalog) *Graph {
	g := &Graph{
		nodeSet: map[string]bool{},
		in:      map[string]int{},
		out:     map[string]int{},
		adj:     map[string]map[string]float64{},
	}
	if c == nil {
		return g
	}
	for i := range c.Tables {
		id := c.Tables[i].ID()
		if !g.nodeSet[id] {
			g.nodeSet[id] = true
			g.nodes = append(g.nodes, id)
			g.adj[id] = map[string]float64{}
		}
	}
	slices.Sort(g.nodes)

	type pair struct{ from, to string }
	byPair := map[pair]*Edge{}
	for i := range c.Tables {
		t := &c.Tables[i]
		from := t.ID()
		for _, fk := range t.ForeignKeys {
			to := fk.RefID()
			if !g.nodeSet[to] {
				g.externalFKs++
				continue
			}
			p := pair{from, to}
			e, ok := byPair[p]
			if !ok {
				e = &Edge{From: from, To: to, SelfReference: from == to}
				byPair[p] = e
			}
			e.Weight++
			e.ForeignKeys = append(e.ForeignKeys, fk.Name)
			g.out[from]++
			g.in[to]++
			if from != to {
				g.adj[from][to]++
				g.adj[to][from]++
			}
		}
	}
	for _, e := range byPair {
		slices.Sort(e.ForeignKeys)
		g.edges = append(g.edges, *e)
	}
	slices.SortFunc(g.edges, func(a, b Edge) int {
		return cmp.Or(cmp.Compare(a.From, b.From), cmp.Compare(a.To, b.To))
	})
	return g
}

// Nodes returns all table IDs, sorted.
func (g *Graph) Nodes() []string { return slices.Clone(g.nodes) }

// HasNode reports whether the table ID is in the graph.
func (g *Graph) HasNode(id string) bool { return g.nodeSet[id] }

// Edges returns all edges sorted by From, then To.
func (g *Graph) Edges() []Edge {
	out := make([]Edge, len(g.edges))
	for i, e := range g.edges {
		e.ForeignKeys = slices.Clone(e.ForeignKeys)
		out[i] = e
	}
	return out
}

// Degree returns the number of foreign keys pointing to (in) and from (out)
// the table. A self-reference counts once in each. Unknown IDs return 0, 0.
func (g *Graph) Degree(id string) (in, out int) {
	return g.in[id], g.out[id]
}

// UndirectedNeighbors returns the weighted undirected adjacency of a table:
// for every OTHER table linked to it in either direction, the total number of
// foreign keys between the two. Self-loops are excluded. The result is a
// fresh map the caller may modify; it is nil for an unknown ID.
//
// This symmetric weighted view is the input format expected by modularity
// based community detection (Louvain/Leiden) used for domain segmentation.
func (g *Graph) UndirectedNeighbors(id string) map[string]float64 {
	nb, ok := g.adj[id]
	if !ok {
		return nil
	}
	out := make(map[string]float64, len(nb))
	for k, v := range nb {
		out[k] = v
	}
	return out
}

// ConnectedComponents returns the weakly connected components. Each
// component is sorted; components are ordered by size (largest first), then
// by their first table ID.
func (g *Graph) ConnectedComponents() [][]string {
	seen := map[string]bool{}
	var comps [][]string
	for _, start := range g.nodes {
		if seen[start] {
			continue
		}
		seen[start] = true
		comp := []string{}
		stack := []string{start}
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			comp = append(comp, n)
			for m := range g.adj[n] {
				if !seen[m] {
					seen[m] = true
					stack = append(stack, m)
				}
			}
		}
		slices.Sort(comp)
		comps = append(comps, comp)
	}
	slices.SortStableFunc(comps, func(a, b []string) int {
		return cmp.Or(cmp.Compare(len(b), len(a)), cmp.Compare(a[0], b[0]))
	})
	return comps
}

// Orphans returns, sorted, the tables with no foreign key to or from another
// table. Tables whose only foreign keys are self-references are orphans.
func (g *Graph) Orphans() []string {
	out := []string{}
	for _, n := range g.nodes {
		if len(g.adj[n]) == 0 {
			out = append(out, n)
		}
	}
	return out
}

// Metrics computes summary metrics for the graph.
func (g *Graph) Metrics() Metrics {
	m := Metrics{
		Nodes:               len(g.nodes),
		Edges:               len(g.edges),
		ExternalForeignKeys: g.externalFKs,
		Components:          len(g.ConnectedComponents()),
		Orphans:             len(g.Orphans()),
	}
	for _, e := range g.edges {
		m.ForeignKeys += e.Weight
		if e.SelfReference {
			m.SelfReferences++
		}
	}
	for _, n := range g.nodes { // sorted: first max wins ties
		if d := g.in[n] + g.out[n]; d > m.MaxDegree || m.MaxDegreeTable == "" {
			m.MaxDegree, m.MaxDegreeTable = d, n
		}
	}
	return m
}
