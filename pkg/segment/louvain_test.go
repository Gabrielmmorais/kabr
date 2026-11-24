package segment

import (
	"math"
	"slices"
	"testing"
)

// undirected builds a symmetric adjacency from "a-b" pairs of weight 1.
func undirected(pairs ...[2]string) map[string]map[string]float64 {
	adj := map[string]map[string]float64{}
	for _, p := range pairs {
		for _, d := range [][2]string{{p[0], p[1]}, {p[1], p[0]}} {
			if adj[d[0]] == nil {
				adj[d[0]] = map[string]float64{}
			}
			adj[d[0]][d[1]]++
		}
	}
	return adj
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestModularityHandComputed(t *testing.T) {
	twoTriangles := newWGraph(
		[]string{"a", "b", "c", "d", "e", "f"},
		undirected([2]string{"a", "b"}, [2]string{"b", "c"}, [2]string{"a", "c"},
			[2]string{"d", "e"}, [2]string{"e", "f"}, [2]string{"d", "f"},
			[2]string{"c", "d"}),
	)
	triangle := newWGraph([]string{"a", "b", "c"},
		undirected([2]string{"a", "b"}, [2]string{"b", "c"}, [2]string{"a", "c"}))

	tests := []struct {
		name  string
		g     *wgraph
		part  []int
		gamma float64
		want  float64
	}{
		// m = 7; each triangle: in = 6 (ordered), tot = 7 → 2·(6/14 − ¼) = 5/14.
		{"two triangles split", twoTriangles, []int{0, 0, 0, 1, 1, 1}, 1, 5.0 / 14},
		// one community holding everything always has Q = 0.
		{"two triangles merged", twoTriangles, []int{0, 0, 0, 0, 0, 0}, 1, 0},
		{"triangle singletons", triangle, []int{0, 1, 2}, 1, -1.0 / 3},
		{"triangle one community", triangle, []int{0, 0, 0}, 1, 0},
		// γ = 2: 2·(6/14 − 2·¼) = −1/7.
		{"two triangles split gamma 2", twoTriangles, []int{0, 0, 0, 1, 1, 1}, 2, -1.0 / 7},
		{"no edges", newWGraph([]string{"a", "b"}, nil), []int{0, 1}, 1, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := modularity(tt.g, tt.part, tt.gamma); !near(got, tt.want) {
				t.Errorf("Q = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLouvainTwoTriangles(t *testing.T) {
	g := newWGraph(
		[]string{"a", "b", "c", "d", "e", "f"},
		undirected([2]string{"a", "b"}, [2]string{"b", "c"}, [2]string{"a", "c"},
			[2]string{"d", "e"}, [2]string{"e", "f"}, [2]string{"d", "f"},
			[2]string{"c", "d"}),
	)
	got := louvain(g, 1)
	if want := []int{0, 0, 0, 1, 1, 1}; !slices.Equal(got, want) {
		t.Fatalf("partition = %v, want %v", got, want)
	}
	if q := modularity(g, got, 1); !near(q, 5.0/14) {
		t.Errorf("Q = %v, want 5/14", q)
	}
}

func TestLouvainNeverMergesComponents(t *testing.T) {
	// Two disconnected edges, one isolated node, one heavy star.
	g := newWGraph(
		[]string{"a", "b", "c", "d", "e", "s1", "s2", "s3", "s4"},
		undirected([2]string{"a", "b"}, [2]string{"c", "d"},
			[2]string{"s1", "s2"}, [2]string{"s1", "s3"}, [2]string{"s1", "s4"}),
	)
	for _, gamma := range []float64{0.01, 0.5, 1, 3} {
		got := louvain(g, gamma)
		comp := map[string]int{"a": 0, "b": 0, "c": 1, "d": 1, "e": 2, "s1": 3, "s2": 3, "s3": 3, "s4": 3}
		ids := []string{"a", "b", "c", "d", "e", "s1", "s2", "s3", "s4"}
		for i := range ids {
			for j := range ids {
				if comp[ids[i]] != comp[ids[j]] && got[i] == got[j] {
					t.Errorf("gamma %v: %s and %s merged across components: %v", gamma, ids[i], ids[j], got)
				}
			}
		}
	}
}

func TestAggregatePreservesModularity(t *testing.T) {
	g := newWGraph(
		[]string{"a", "b", "c", "d", "e", "f"},
		undirected([2]string{"a", "b"}, [2]string{"b", "c"}, [2]string{"a", "c"},
			[2]string{"d", "e"}, [2]string{"e", "f"}, [2]string{"d", "f"},
			[2]string{"c", "d"}, [2]string{"c", "d"}),
	)
	part := []int{0, 0, 0, 1, 1, 1}
	agg := aggregate(g, part, 2)
	if a, b := modularity(g, part, 1), modularity(agg, []int{0, 1}, 1); !near(a, b) {
		t.Errorf("aggregation changed Q: %v vs %v", a, b)
	}
	if !near(agg.totalWeight(), g.totalWeight()) {
		t.Errorf("aggregation changed total weight")
	}
}

func TestLouvainRingOfCliques(t *testing.T) {
	// 6 cliques of 4 nodes connected in a ring: the canonical Louvain case.
	var ids []string
	var pairs [][2]string
	name := func(c, i int) string { return string(rune('a'+c)) + string(rune('0'+i)) }
	for c := range 6 {
		for i := range 4 {
			ids = append(ids, name(c, i))
			for j := i + 1; j < 4; j++ {
				pairs = append(pairs, [2]string{name(c, i), name(c, j)})
			}
		}
		pairs = append(pairs, [2]string{name(c, 3), name((c+1)%6, 0)})
	}
	slices.Sort(ids)
	got := louvain(newWGraph(ids, undirected(pairs...)), 1)
	for i, id := range ids {
		if want := int(id[0] - 'a'); got[i] != want {
			t.Fatalf("partition = %v, want one community per clique", got)
		}
	}
}
