package segment

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/Gabrielmmorais/kabr/pkg/graph"
	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

// Role is the structural role of a table inside its domain.
type Role string

// Table roles.
const (
	// RoleCore is a regular table placed by modularity clustering.
	RoleCore Role = "core"
	// RoleLookup is a small reference table (no outgoing references, several
	// incoming ones) attached to the domain holding most of its links.
	RoleLookup Role = "lookup"
	// RoleJunction is an association table whose composite primary key is
	// made of references to two or more tables; it joins its owner's domain.
	RoleJunction Role = "junction"
	// RoleOrphan is a table with no declared or inferred reference at all.
	RoleOrphan Role = "orphan"
)

// Options tunes [Segment]. Start from [DefaultOptions].
type Options struct {
	// Resolution is the Louvain resolution γ (> 0). Values above 1 favor
	// more, smaller domains; below 1 fewer, larger ones. Default 1.0.
	Resolution float64
	// InferEdges enables references inferred from column naming conventions
	// where no foreign key is declared (see [InferredEdge]). Default true.
	InferEdges bool
	// InferredWeight is the edge weight of one inferred reference; a declared
	// foreign key weighs 1.0. Must be > 0 when InferEdges is set. Default 0.5.
	InferredWeight float64
	// LookupMaxColumns is the maximum column count of a lookup candidate: a
	// table with at most this many columns, no outgoing reference and at
	// least two incoming references (foreign keys plus inferred) is a
	// lookup. Zero or negative disables lookup detection. Default 4.
	LookupMaxColumns int
}

// DefaultOptions returns the recommended options. See the package
// documentation for the reasoning behind each default.
func DefaultOptions() Options {
	return Options{
		Resolution:       1.0,
		InferEdges:       true,
		InferredWeight:   0.5,
		LookupMaxColumns: 4,
	}
}

// Assignment places one table in a community.
type Assignment struct {
	// Table is the table ID ("schema.name").
	Table string `json:"table"`
	Role  Role   `json:"role"`
	// Shared is true for a lookup referenced from two or more communities.
	Shared bool `json:"shared,omitempty"`
	// Rationale explains, from the structural evidence, why the table has
	// this role and community.
	Rationale string `json:"rationale"`
}

// Community is one proposed domain.
type Community struct {
	// ID is the index of the community in [Result].Communities.
	ID int `json:"id"`
	// Anchor is the most central core table of the community (highest
	// weighted degree, ties by smallest ID), used to name the domain. For the
	// orphan bucket it is the first orphan.
	Anchor string `json:"anchor"`
	// OrphanBucket is true for the single community gathering all orphans.
	OrphanBucket bool `json:"orphanBucket,omitempty"`
	// Tables is sorted by table ID.
	Tables []Assignment `json:"tables"`
}

// InferredEdge is a reference deduced from a column name, not declared as a
// foreign key: From.Column is assumed to reference To's primary key.
type InferredEdge struct {
	From   string  `json:"from"`
	To     string  `json:"to"`
	Column string  `json:"column"`
	Weight float64 `json:"weight"`
}

// Result is the proposed segmentation.
type Result struct {
	// Communities is ordered by size (largest first), then by Anchor.
	Communities []Community `json:"communities"`
	// Modularity is the standard Newman modularity (γ = 1) of the final
	// partition, measured on the full weighted graph (declared foreign keys
	// plus inferred edges), after the lookup/junction heuristics.
	Modularity float64 `json:"modularity"`
	// InferredEdges is sorted by From, then Column.
	InferredEdges []InferredEdge `json:"inferredEdges"`
	// Orphans lists, sorted, the tables with no declared or inferred
	// reference to or from another table.
	Orphans []string `json:"orphans"`
}

// CommunityOf returns the ID of the community holding table (a table ID).
func (r *Result) CommunityOf(table string) (int, bool) {
	if r == nil {
		return 0, false
	}
	for _, c := range r.Communities {
		for _, a := range c.Tables {
			if a.Table == table {
				return c.ID, true
			}
		}
	}
	return 0, false
}

// Segment proposes a split of the catalog into domains. The catalog is not
// modified. It returns an error for a nil or invalid catalog, or invalid
// options; an empty catalog yields an empty result. The output depends only
// on the catalog content, never on the order of its tables or constraints.
func Segment(c *schema.Catalog, opts Options) (*Result, error) {
	if c == nil {
		return nil, errors.New("segment: catalog is nil")
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("segment: invalid catalog: %w", err)
	}
	if err := opts.validate(); err != nil {
		return nil, err
	}
	n := c.Clone()
	n.Normalize()
	s := newSegmenter(n, opts)
	return s.run(), nil
}

func (o Options) validate() error {
	if math.IsNaN(o.Resolution) || math.IsInf(o.Resolution, 0) || o.Resolution <= 0 {
		return fmt.Errorf("segment: resolution must be a positive number, got %v", o.Resolution)
	}
	if o.InferEdges && (math.IsNaN(o.InferredWeight) || math.IsInf(o.InferredWeight, 0) || o.InferredWeight <= 0) {
		return fmt.Errorf("segment: inferred weight must be a positive number, got %v", o.InferredWeight)
	}
	return nil
}

type tableInfo struct {
	t        *schema.Table
	role     Role
	refsOut  map[string]float64 // outgoing references to other tables: FK count + inferred weight
	refsIn   map[string]int     // incoming references from other tables (count of FKs + inferred)
	inferred []InferredEdge     // inferred edges originating here
	parts    []string           // junction: referenced tables through PK columns, sorted
	comm     int                // community label, -1 while unplaced
}

type segmenter struct {
	c        *schema.Catalog
	opts     Options
	ids      []string // sorted
	info     map[string]*tableInfo
	adj      map[string]map[string]float64 // full weighted undirected graph
	reduced  map[string]map[string]float64 // core-only graph clustered by Louvain
	inferred []InferredEdge
	nextComm int
}

func newSegmenter(c *schema.Catalog, opts Options) *segmenter {
	g := graph.Build(c)
	s := &segmenter{
		c:        c,
		opts:     opts,
		ids:      g.Nodes(),
		info:     map[string]*tableInfo{},
		adj:      map[string]map[string]float64{},
		inferred: []InferredEdge{},
	}
	for i := range c.Tables {
		t := &c.Tables[i]
		s.info[t.ID()] = &tableInfo{t: t, refsOut: map[string]float64{}, refsIn: map[string]int{}, comm: -1}
	}
	for _, id := range s.ids {
		s.adj[id] = g.UndirectedNeighbors(id)
	}
	for i := range c.Tables {
		t := &c.Tables[i]
		for _, fk := range t.ForeignKeys {
			to := fk.RefID()
			if to == t.ID() || !g.HasNode(to) {
				continue
			}
			s.info[t.ID()].refsOut[to]++
			s.info[to].refsIn[t.ID()]++
		}
	}
	if opts.InferEdges {
		s.inferred = inferEdges(c, opts.InferredWeight)
		for _, e := range s.inferred {
			s.adj[e.From][e.To] += e.Weight
			s.adj[e.To][e.From] += e.Weight
			s.info[e.From].refsOut[e.To] += e.Weight
			s.info[e.From].inferred = append(s.info[e.From].inferred, e)
			s.info[e.To].refsIn[e.From]++
		}
	}
	return s
}

func (s *segmenter) weightedDegree(id string) float64 {
	var d float64
	nb := s.adj[id]
	for _, k := range sortedKeys(nb) {
		d += nb[k]
	}
	return d
}

func (s *segmenter) run() *Result {
	s.classify()

	// Reduced graph: core tables only, plus the projection of every junction
	// onto the pairs of core tables it associates.
	var core []string
	for _, id := range s.ids {
		if s.info[id].role == RoleCore {
			core = append(core, id)
		}
	}
	reduced := map[string]map[string]float64{}
	add := func(a, b string, w float64) {
		if reduced[a] == nil {
			reduced[a] = map[string]float64{}
		}
		reduced[a][b] += w
	}
	for _, a := range core {
		nb := s.adj[a]
		for _, b := range sortedKeys(nb) {
			if s.info[b].role == RoleCore {
				add(a, b, nb[b])
			}
		}
	}
	for _, j := range s.ids {
		ti := s.info[j]
		if ti.role != RoleJunction {
			continue
		}
		for x, a := range ti.parts {
			for _, b := range ti.parts[x+1:] {
				if s.info[a].role != RoleCore || s.info[b].role != RoleCore {
					continue
				}
				w := min(s.adj[j][a], s.adj[j][b])
				add(a, b, w)
				add(b, a, w)
			}
		}
	}
	s.reduced = reduced
	labels := louvain(newWGraph(core, reduced), s.opts.Resolution)
	for i, id := range core {
		s.info[id].comm = labels[i]
	}
	s.nextComm = 0
	for _, l := range labels {
		s.nextComm = max(s.nextComm, l+1)
	}

	rationale := map[string]string{}
	shared := map[string]bool{}
	for _, id := range core {
		rationale[id] = s.coreRationale(id)
	}
	for _, id := range s.ids {
		if s.info[id].role == RoleJunction {
			rationale[id] = s.placeJunction(id)
		}
	}
	for _, id := range s.ids {
		if s.info[id].role == RoleLookup {
			rationale[id], shared[id] = s.placeLookup(id)
		}
	}
	orphans := []string{}
	for _, id := range s.ids {
		if s.info[id].role == RoleOrphan {
			orphans = append(orphans, id)
			rationale[id] = "orphan: no foreign key and no inferred reference to or from another table"
		}
	}
	orphanComm := -1
	if len(orphans) > 0 {
		orphanComm = s.nextComm
		s.nextComm++
		for _, id := range orphans {
			s.info[id].comm = orphanComm
		}
	}

	// Build communities.
	members := map[int][]string{}
	for _, id := range s.ids {
		members[s.info[id].comm] = append(members[s.info[id].comm], id)
	}
	comms := []Community{}
	for _, label := range sortedKeys(members) {
		ids := members[label]
		cm := Community{Tables: make([]Assignment, 0, len(ids))}
		for _, id := range ids {
			cm.Tables = append(cm.Tables, Assignment{Table: id, Role: s.info[id].role, Shared: shared[id], Rationale: rationale[id]})
		}
		if label == orphanComm {
			cm.OrphanBucket = true
			cm.Anchor = ids[0]
		} else {
			cm.Anchor = s.anchor(ids)
		}
		comms = append(comms, cm)
	}
	slices.SortStableFunc(comms, func(a, b Community) int {
		return cmp.Or(cmp.Compare(len(b.Tables), len(a.Tables)), cmp.Compare(a.Anchor, b.Anchor))
	})
	final := make(map[string]int, len(s.ids))
	for i := range comms {
		comms[i].ID = i
		for _, a := range comms[i].Tables {
			final[a.Table] = i
		}
	}
	part := make([]int, len(s.ids))
	for i, id := range s.ids {
		part[i] = final[id]
	}
	q := modularity(newWGraph(s.ids, s.adj), part, 1.0)
	if math.Abs(q) < eps {
		q = 0
	}
	return &Result{
		Communities:   comms,
		Modularity:    q,
		InferredEdges: slices.Clone(s.inferred),
		Orphans:       orphans,
	}
}

// classify assigns orphan, junction, lookup and core roles.
func (s *segmenter) classify() {
	for _, id := range s.ids {
		ti := s.info[id]
		switch {
		case len(s.adj[id]) == 0:
			ti.role = RoleOrphan
		case s.isJunction(ti):
			ti.role = RoleJunction
		case s.isLookup(ti):
			ti.role = RoleLookup
		default:
			ti.role = RoleCore
		}
	}
}

// isJunction: composite primary key whose every column is a referencing
// column (declared FK to another table or inferred), the key columns
// reaching at least two distinct tables. It also records ti.parts.
func (s *segmenter) isJunction(ti *tableInfo) bool {
	pk := ti.t.PrimaryKey
	if pk == nil || len(pk.Columns) < 2 {
		return false
	}
	colTargets := map[string][]string{}
	for _, fk := range ti.t.ForeignKeys {
		to := fk.RefID()
		if to == ti.t.ID() || s.info[to] == nil {
			continue
		}
		for _, col := range fk.Columns {
			colTargets[col] = append(colTargets[col], to)
		}
	}
	for _, e := range ti.inferred {
		colTargets[e.Column] = append(colTargets[e.Column], e.To)
	}
	var parts []string
	for _, col := range pk.Columns {
		ts := colTargets[col]
		if len(ts) == 0 {
			return false
		}
		parts = append(parts, ts...)
	}
	slices.Sort(parts)
	parts = slices.Compact(parts)
	if len(parts) < 2 {
		return false
	}
	ti.parts = parts
	return true
}

func (s *segmenter) isLookup(ti *tableInfo) bool {
	if s.opts.LookupMaxColumns <= 0 || len(ti.t.Columns) > s.opts.LookupMaxColumns || len(ti.refsOut) > 0 {
		return false
	}
	total := 0
	for _, n := range ti.refsIn {
		total += n
	}
	return total >= 2
}

// strongest returns the community holding most of id's edge weight among
// already placed neighbors (ties: smallest community label), with that
// weight. ok is false when no neighbor is placed.
func (s *segmenter) strongest(id string) (comm int, w float64, ok bool) {
	byComm := map[int]float64{}
	nb := s.adj[id]
	for _, other := range sortedKeys(nb) {
		if c := s.info[other].comm; c >= 0 {
			byComm[c] += nb[other]
		}
	}
	comm = -1
	for _, c := range sortedKeys(byComm) {
		if byComm[c] > w+eps {
			comm, w = c, byComm[c]
		}
	}
	return comm, w, comm >= 0
}

func (s *segmenter) newComm() int {
	c := s.nextComm
	s.nextComm++
	return c
}

func (s *segmenter) coreRationale(id string) string {
	var b strings.Builder
	ti := s.info[id]
	best, bestW := "", 0.0
	nb := s.reduced[id]
	for _, other := range sortedKeys(nb) {
		if s.info[other].comm == ti.comm && nb[other] > bestW+eps {
			best, bestW = other, nb[other]
		}
	}
	switch {
	case best == "" && len(nb) == 0:
		b.WriteString("core: linked only to lookup or junction tables, kept as its own domain")
	case best == "":
		b.WriteString("core: modularity clustering kept it apart from its core neighbors")
	default:
		fmt.Fprintf(&b, "core: grouped by modularity clustering; strongest in-domain link %s (weight %s", best, fmtW(bestW))
		switch direct := s.adj[id][best]; {
		case direct < eps:
			b.WriteString(", through a junction table")
		case direct < bestW-eps:
			b.WriteString(", partly through a junction table")
		}
		b.WriteString(")")
	}
	if len(ti.inferred) > 0 {
		refs := make([]string, len(ti.inferred))
		for i, e := range ti.inferred {
			refs[i] = e.Column + "→" + e.To
		}
		fmt.Fprintf(&b, "; inferred references: %s", strings.Join(refs, ", "))
	}
	return b.String()
}

// placeJunction puts a junction in the community of its owner: the
// referenced core table with the highest weighted degree (ties by ID).
func (s *segmenter) placeJunction(id string) string {
	ti := s.info[id]
	owner, ownerDeg := "", -1.0
	for _, p := range ti.parts {
		if s.info[p].role != RoleCore {
			continue
		}
		if d := s.weightedDegree(p); d > ownerDeg+eps {
			owner, ownerDeg = p, d
		}
	}
	sides := strings.Join(ti.parts, " and ")
	pk := strings.Join(ti.t.PrimaryKey.Columns, ", ")
	if owner != "" {
		ti.comm = s.info[owner].comm
		return fmt.Sprintf("junction: composite primary key (%s) references %s; placed with owner %s (highest weighted degree %s)",
			pk, sides, owner, fmtW(ownerDeg))
	}
	if c, _, ok := s.strongest(id); ok {
		ti.comm = c
		return fmt.Sprintf("junction: composite primary key (%s) references %s; no core owner, placed with its strongest neighbors", pk, sides)
	}
	ti.comm = s.newComm()
	return fmt.Sprintf("junction: composite primary key (%s) references %s; no placed neighbor, kept as its own domain", pk, sides)
}

// placeLookup attaches a lookup to the community holding most of its edge
// weight and reports whether it is shared across communities.
func (s *segmenter) placeLookup(id string) (string, bool) {
	ti := s.info[id]
	var refs []string
	total := 0
	for _, from := range sortedKeys(ti.refsIn) {
		n := ti.refsIn[from]
		total += n
		refs = append(refs, fmt.Sprintf("%s (%d)", from, n))
	}
	comms := map[int]bool{}
	for from := range ti.refsIn {
		if c := s.info[from].comm; c >= 0 {
			comms[c] = true
		}
	}
	head := fmt.Sprintf("lookup: %d columns, no outgoing reference, %d incoming references from %s",
		len(ti.t.Columns), total, strings.Join(refs, ", "))
	c, _, ok := s.strongest(id)
	if !ok {
		ti.comm = s.newComm()
		return head + "; no placed neighbor, kept as its own domain", false
	}
	ti.comm = c
	// Name the strongest neighbor inside the chosen community.
	best, bestW := "", 0.0
	nb := s.adj[id]
	for _, other := range sortedKeys(nb) {
		if s.info[other].comm == c && nb[other] > bestW+eps {
			best, bestW = other, nb[other]
		}
	}
	msg := fmt.Sprintf("%s; placed with its strongest neighbor %s", head, best)
	isShared := len(comms) >= 2
	if isShared {
		msg += fmt.Sprintf("; shared: referenced from %d domains", len(comms))
	}
	return msg, isShared
}

// anchor returns the core member with the highest weighted degree (ties by
// smallest ID); communities without core members fall back to all members.
func (s *segmenter) anchor(ids []string) string {
	pick := func(onlyCore bool) string {
		best, bestDeg := "", -1.0
		for _, id := range ids {
			if onlyCore && s.info[id].role != RoleCore {
				continue
			}
			if d := s.weightedDegree(id); d > bestDeg+eps {
				best, bestDeg = id, d
			}
		}
		return best
	}
	if a := pick(true); a != "" {
		return a
	}
	return pick(false)
}

func fmtW(w float64) string { return strconv.FormatFloat(w, 'f', -1, 64) }
