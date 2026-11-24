package segment

import (
	"cmp"
	"slices"
)

// eps absorbs floating point noise when comparing modularity gains.
const eps = 1e-12

// maxPasses bounds the local-moving passes of one Louvain level and maxLevels
// the number of aggregation levels. Both are safety nets: every accepted move
// strictly increases modularity, so the algorithm terminates long before.
const (
	maxPasses = 1000
	maxLevels = 64
)

type arc struct {
	to int
	w  float64
}

// wgraph is a weighted undirected graph in the shape Louvain needs. adj holds
// both directions of every edge between distinct nodes, sorted by target;
// self[i] is the internal weight of node i counted as adjacency matrix entries
// (A_ii, i.e. twice the undirected internal weight); k[i] is the weighted
// degree (row sum of A including A_ii).
type wgraph struct {
	adj  [][]arc
	self []float64
	k    []float64
}

// newWGraph builds a wgraph over ids (whose order defines node indexes) from
// a symmetric adjacency keyed by id. Unknown ids, self-loops and non-positive
// weights are ignored.
func newWGraph(ids []string, adj map[string]map[string]float64) *wgraph {
	idx := make(map[string]int, len(ids))
	for i, id := range ids {
		idx[id] = i
	}
	g := &wgraph{
		adj:  make([][]arc, len(ids)),
		self: make([]float64, len(ids)),
		k:    make([]float64, len(ids)),
	}
	for i, id := range ids {
		nb := adj[id]
		for _, other := range sortedKeys(nb) {
			j, ok := idx[other]
			w := nb[other]
			if !ok || j == i || w <= 0 {
				continue
			}
			g.adj[i] = append(g.adj[i], arc{to: j, w: w})
			g.k[i] += w
		}
	}
	return g
}

func (g *wgraph) n() int { return len(g.k) }

func (g *wgraph) totalWeight() float64 { // 2m
	var s float64
	for _, k := range g.k {
		s += k
	}
	return s
}

// modularity returns the (generalized, resolution gamma) Newman modularity of
// a partition: Q = Σ_c [ in_c/2m − γ (tot_c/2m)² ], where in_c is the sum of
// adjacency entries inside c and tot_c the sum of degrees in c. A graph with
// no edges has Q = 0.
func modularity(g *wgraph, comm []int, gamma float64) float64 {
	m2 := g.totalWeight()
	if m2 == 0 {
		return 0
	}
	in := map[int]float64{}
	tot := map[int]float64{}
	for i, c := range comm[:g.n()] {
		tot[c] += g.k[i]
		in[c] += g.self[i]
		for _, a := range g.adj[i] {
			if comm[a.to] == c {
				in[c] += a.w
			}
		}
	}
	var q float64
	for _, c := range sortedKeys(tot) {
		q += in[c]/m2 - gamma*(tot[c]/m2)*(tot[c]/m2)
	}
	return q
}

// louvain partitions g with the Louvain method (Blondel et al. 2008) and
// returns one community label per node. Labels are canonical: communities are
// numbered 0..k-1 in order of their smallest node index.
//
// The run is fully deterministic: nodes are visited in index order, a node
// only leaves its community for a strictly better gain, and ties between
// candidate communities go to the smallest community label. Nodes are only
// ever moved towards communities of their neighbors, so disconnected parts of
// the graph are never merged.
func louvain(g *wgraph, gamma float64) []int {
	member := make([]int, g.n())
	for i := range member {
		member[i] = i
	}
	cur := g
	for range maxLevels {
		comm, moved := localMove(cur, gamma)
		if !moved {
			break
		}
		labels, k := relabel(comm)
		for v := range member {
			member[v] = labels[member[v]]
		}
		cur = aggregate(cur, labels, k)
	}
	canon, _ := relabel(member)
	return canon
}

// localMove is Louvain phase 1. It reports whether any node changed
// community.
func localMove(g *wgraph, gamma float64) (comm []int, moved bool) {
	n := g.n()
	comm = make([]int, n)
	tot := make([]float64, n)
	for i := range n {
		comm[i] = i
		tot[i] = g.k[i]
	}
	m2 := g.totalWeight()
	if m2 == 0 {
		return comm, false
	}
	neighW := make([]float64, n)
	mark := make([]bool, n)
	var touched []int
	for range maxPasses {
		improved := false
		for i := range n {
			ci := comm[i]
			touched = touched[:0]
			for _, a := range g.adj[i] {
				c := comm[a.to]
				if !mark[c] {
					mark[c] = true
					touched = append(touched, c)
				}
				neighW[c] += a.w
			}
			tot[ci] -= g.k[i]
			best := ci
			bestGain := neighW[ci] - gamma*tot[ci]*g.k[i]/m2
			for _, c := range touched {
				if c == ci {
					continue
				}
				gain := neighW[c] - gamma*tot[c]*g.k[i]/m2
				switch {
				case gain > bestGain+eps:
					best, bestGain = c, gain
				case gain >= bestGain-eps && best != ci && c < best:
					best, bestGain = c, gain
				}
			}
			tot[best] += g.k[i]
			if best != ci {
				comm[i] = best
				improved, moved = true, true
			}
			for _, c := range touched {
				mark[c] = false
				neighW[c] = 0
			}
			neighW[ci] = 0
		}
		if !improved {
			break
		}
	}
	return comm, moved
}

// relabel renumbers arbitrary labels to 0..k-1 by first appearance.
func relabel(comm []int) (labels []int, k int) {
	seen := map[int]int{}
	labels = make([]int, len(comm))
	for i, c := range comm {
		l, ok := seen[c]
		if !ok {
			l = len(seen)
			seen[c] = l
		}
		labels[i] = l
	}
	return labels, len(seen)
}

// aggregate is Louvain phase 2: every community becomes one node.
func aggregate(g *wgraph, labels []int, k int) *wgraph {
	out := &wgraph{
		adj:  make([][]arc, k),
		self: make([]float64, k),
		k:    make([]float64, k),
	}
	between := make([]map[int]float64, k)
	for i := range g.n() {
		c := labels[i]
		out.self[c] += g.self[i]
		out.k[c] += g.k[i]
		for _, a := range g.adj[i] {
			d := labels[a.to]
			if d == c {
				out.self[c] += a.w
				continue
			}
			if between[c] == nil {
				between[c] = map[int]float64{}
			}
			between[c][d] += a.w
		}
	}
	for c := range k {
		for _, d := range sortedKeys(between[c]) {
			out.adj[c] = append(out.adj[c], arc{to: d, w: between[c][d]})
		}
	}
	return out
}

func sortedKeys[K cmp.Ordered, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
