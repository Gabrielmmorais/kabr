package planner

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/Gabrielmmorais/kabr/pkg/graph"
	"github.com/Gabrielmmorais/kabr/pkg/plan"
	"github.com/Gabrielmmorais/kabr/pkg/schema"
	"github.com/Gabrielmmorais/kabr/pkg/segment"
)

// DefaultName is metadata.name when none is given and none can be derived
// from the catalog's database name.
const DefaultName = "kabr-plan"

// UnassignedDomain is the name of the domain holding the orphan bucket.
const UnassignedDomain = "unassigned"

// fallbackDomain names a community whose anchor yields no valid label.
const fallbackDomain = "domain"

// Baseline builds the graph-only plan for c: one domain per community of
// [segment.Segment] (segOpts; the zero value means
// [segment.DefaultOptions]), named after the community anchor table, with the
// sensitive columns of [ClassifySensitive]. An empty name is derived from the
// catalog database (see [Options].Name). The result passes
// [plan.DomainPlan.Validate] against c.
func Baseline(c *schema.Catalog, name string, segOpts segment.Options) (*plan.DomainPlan, error) {
	if c == nil {
		return nil, errors.New("planner: catalog is nil")
	}
	if len(c.Tables) == 0 {
		return nil, errors.New("planner: catalog has no tables")
	}
	if name == "" {
		name = DeriveName(c.Database)
	}
	if !plan.IsDNSSubdomain(name) {
		return nil, fmt.Errorf("planner: plan name %q is not a valid DNS-1123 subdomain", name)
	}
	if segOpts == (segment.Options{}) {
		segOpts = segment.DefaultOptions()
	}
	res, err := segment.Segment(c, segOpts)
	if err != nil {
		return nil, fmt.Errorf("planner: %w", err)
	}

	p := plan.New(name, c)
	stats := newLinkStats(c, res)
	// The orphan bucket always owns "unassigned"; an anchor table of that
	// name gets a suffix instead.
	used := map[string]bool{}
	for _, comm := range res.Communities {
		if comm.OrphanBucket {
			used[UnassignedDomain] = true
		}
	}
	for _, comm := range res.Communities {
		name := UnassignedDomain
		if !comm.OrphanBucket {
			name = uniqueName(domainBase(comm), used)
		}
		d := plan.Domain{Name: name}
		if comm.OrphanBucket {
			d.Description = "Tables with no declared or inferred foreign-key relationship."
			d.Rationale = fmt.Sprintf("orphan bucket: %d %s with no declared or inferred reference to or from another table",
				len(comm.Tables), plural(len(comm.Tables), "table", "tables"))
		} else {
			d.Description = fmt.Sprintf("Tables grouped around %s by foreign-key structure.", tablePart(comm.Anchor))
			d.Rationale = stats.rationale(comm)
		}
		for _, a := range comm.Tables {
			d.Tables = append(d.Tables, plan.TableAssignment{
				Name:      a.Table,
				Shared:    a.Shared,
				Role:      string(a.Role),
				Rationale: a.Rationale,
			})
		}
		p.Spec.Domains = append(p.Spec.Domains, d)
	}
	for _, sc := range ClassifySensitive(c) {
		if d := p.Domain(p.DomainOf(sc.Table)); d != nil {
			d.SensitiveColumns = append(d.SensitiveColumns, sc)
		}
	}
	p.Status.Generator = plan.Generator{Mode: plan.ModeGraph, Modularity: round4(res.Modularity)}

	if err := p.Validate(c); err != nil {
		return nil, fmt.Errorf("planner: baseline plan is invalid: %w", err)
	}
	return p, nil
}

// DeriveName turns a database name into a DNS-1123 subdomain usable as
// metadata.name: lowercased, every character outside [a-z0-9.-] replaced by
// "-", labels trimmed of leading/trailing "-" and cut to 63 characters, empty
// labels dropped, total cut to 253 characters. It returns [DefaultName] when
// nothing valid remains.
func DeriveName(database string) string {
	var labels []string
	total := 0
	for raw := range strings.SplitSeq(strings.ToLower(database), ".") {
		l := strings.Trim(sanitizeLabel(raw), "-")
		if l == "" {
			continue
		}
		if total+len(l)+1 > 254 { // +1 for the separating dot
			break
		}
		labels = append(labels, l)
		total += len(l) + 1
	}
	name := strings.Join(labels, ".")
	if !plan.IsDNSSubdomain(name) {
		return DefaultName
	}
	return name
}

// sanitizeLabel maps s to [a-z0-9-] characters, cut to 63 and trimmed of "-".
func sanitizeLabel(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-")
	}
	return out
}

// domainBase is the preferred domain name of a community (before collision
// suffixes).
func domainBase(comm segment.Community) string {
	if comm.OrphanBucket {
		return UnassignedDomain
	}
	// Table part only, "_" becomes "-", anything else invalid is removed.
	t := strings.ReplaceAll(strings.ToLower(tablePart(comm.Anchor)), "_", "-")
	var b strings.Builder
	for _, r := range t {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > 63 {
		name = strings.Trim(name[:63], "-")
	}
	if !plan.IsDNSLabel(name) {
		return fallbackDomain
	}
	return name
}

// uniqueName returns base, or base-2, base-3, ... when already used, keeping
// the result a valid DNS-1123 label, and marks it used.
func uniqueName(base string, used map[string]bool) string {
	name := base
	for n := 2; used[name]; n++ {
		suffix := "-" + strconv.Itoa(n)
		b := base
		if len(b)+len(suffix) > 63 {
			b = strings.TrimRight(b[:63-len(suffix)], "-")
		}
		name = b + suffix
	}
	used[name] = true
	return name
}

func tablePart(id string) string {
	if _, t, ok := strings.Cut(id, "."); ok {
		return t
	}
	return id
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func round4(f float64) float64 {
	r := math.Round(f*1e4) / 1e4
	if r == 0 {
		return 0 // avoid -0
	}
	return r
}

// linkStats recomputes, from the catalog and the segmentation result, the
// weighted undirected graph segment clustered on (declared foreign keys
// weigh 1 each, inferred references their InferredEdge.Weight).
type linkStats struct {
	declared []graph.Edge
	inferred []segment.InferredEdge
	degree   map[string]float64
	m2       float64 // sum of weighted degrees (2m)
	commOf   map[string]int
}

func newLinkStats(c *schema.Catalog, res *segment.Result) *linkStats {
	s := &linkStats{degree: map[string]float64{}, commOf: map[string]int{}, inferred: res.InferredEdges}
	for _, e := range graph.Build(c).Edges() {
		if e.SelfReference {
			continue
		}
		s.declared = append(s.declared, e)
		s.degree[e.From] += float64(e.Weight)
		s.degree[e.To] += float64(e.Weight)
		s.m2 += 2 * float64(e.Weight)
	}
	for _, e := range res.InferredEdges {
		s.degree[e.From] += e.Weight
		s.degree[e.To] += e.Weight
		s.m2 += 2 * e.Weight
	}
	for _, comm := range res.Communities {
		for _, a := range comm.Tables {
			s.commOf[a.Table] = comm.ID
		}
	}
	return s
}

// contribution is the community's term of the Newman modularity (γ = 1)
// reported in segment.Result.Modularity.
func (s *linkStats) contribution(comm segment.Community) float64 {
	if s.m2 == 0 {
		return 0
	}
	var in, tot float64
	for _, a := range comm.Tables {
		tot += s.degree[a.Table]
	}
	for _, e := range s.declared {
		if s.commOf[e.From] == comm.ID && s.commOf[e.To] == comm.ID {
			in += 2 * float64(e.Weight)
		}
	}
	for _, e := range s.inferred {
		if s.commOf[e.From] == comm.ID && s.commOf[e.To] == comm.ID {
			in += 2 * e.Weight
		}
	}
	return in/s.m2 - (tot/s.m2)*(tot/s.m2)
}

func (s *linkStats) rationale(comm segment.Community) string {
	var fkIn, fkOut, infIn, infOut int
	for _, e := range s.declared {
		a, b := s.commOf[e.From] == comm.ID, s.commOf[e.To] == comm.ID
		switch {
		case a && b:
			fkIn += e.Weight
		case a || b:
			fkOut += e.Weight
		}
	}
	for _, e := range s.inferred {
		a, b := s.commOf[e.From] == comm.ID, s.commOf[e.To] == comm.ID
		switch {
		case a && b:
			infIn++
		case a || b:
			infOut++
		}
	}
	n := len(comm.Tables)
	return fmt.Sprintf("graph community anchored at %s: %d %s, %d internal %s (%d inferred), %d %s to other domains (%d inferred); modularity contribution %.4f",
		comm.Anchor, n, plural(n, "table", "tables"),
		fkIn+infIn, plural(fkIn+infIn, "link", "links"), infIn,
		fkOut+infOut, plural(fkOut+infOut, "link", "links"), infOut,
		round4(s.contribution(comm)))
}
