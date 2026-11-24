package planner

import (
	"bytes"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/Gabrielmmorais/kabr/pkg/plan"
	"github.com/Gabrielmmorais/kabr/pkg/schema"
	"github.com/Gabrielmmorais/kabr/pkg/segment"
)

func TestBaselinePagila(t *testing.T) {
	c := loadPagila(t)
	p1, err := Baseline(c, "", segment.Options{})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := Baseline(loadPagila(t), "", segment.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	b1, err := plan.Marshal(p1)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := plan.Marshal(p2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatalf("baseline is not deterministic:\n%s\n---\n%s", b1, b2)
	}
	if err := p1.Validate(c); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if p1.Metadata.Name != "pagila" {
		t.Errorf("metadata.name = %q, want pagila", p1.Metadata.Name)
	}
	if g := p1.Status.Generator; g.Mode != plan.ModeGraph || g.Modularity != 0.4087 {
		t.Errorf("generator = %+v, want graph/0.4087", g)
	}

	want := map[string][]string{
		"film":  {"public.actor", "public.category", "public.film", "public.film_actor", "public.film_category", "public.inventory", "public.language"},
		"store": {"public.address", "public.customer", "public.payment", "public.rental", "public.staff", "public.store"},
		"city":  {"public.city", "public.country"},
	}
	if len(p1.Spec.Domains) != len(want) {
		t.Fatalf("got %d domains, want %d", len(p1.Spec.Domains), len(want))
	}
	for _, d := range p1.Spec.Domains {
		if !plan.IsDNSLabel(d.Name) {
			t.Errorf("domain name %q is not a DNS label", d.Name)
		}
		var got []string
		for _, ta := range d.Tables {
			got = append(got, ta.Name)
		}
		slices.Sort(got)
		if !slices.Equal(got, want[d.Name]) {
			t.Errorf("domain %q tables = %v, want %v", d.Name, got, want[d.Name])
		}
		if d.Description == "" || !strings.HasPrefix(d.Rationale, "graph community anchored at public.") {
			t.Errorf("domain %q: description %q, rationale %q", d.Name, d.Description, d.Rationale)
		}
		for _, sc := range d.SensitiveColumns {
			if p1.DomainOf(sc.Table) != d.Name {
				t.Errorf("sensitive %s.%s attached to %q, table lives in %q", sc.Table, sc.Column, d.Name, p1.DomainOf(sc.Table))
			}
		}
	}
	film := p1.Domain("film")
	if !strings.Contains(film.Rationale, "7 tables, 7 internal links (0 inferred), 2 links to other domains") {
		t.Errorf("film rationale = %q", film.Rationale)
	}
	for _, ta := range film.Tables {
		if ta.Name == "public.film_actor" && ta.Role != plan.RoleJunction {
			t.Errorf("film_actor role = %q, want junction", ta.Role)
		}
		if ta.Name == "public.language" && ta.Role != plan.RoleLookup {
			t.Errorf("language role = %q, want lookup", ta.Role)
		}
	}
}

func TestModularityContributionsSumToQ(t *testing.T) {
	c := loadPagila(t)
	for _, gamma := range []float64{0.5, 1, 1.5, 3} {
		opts := segment.DefaultOptions()
		opts.Resolution = gamma
		res, err := segment.Segment(c, opts)
		if err != nil {
			t.Fatal(err)
		}
		s := newLinkStats(c, res)
		var sum float64
		for _, comm := range res.Communities {
			sum += s.contribution(comm)
		}
		if math.Abs(sum-res.Modularity) > 1e-9 {
			t.Errorf("γ=%v: sum of contributions %v != modularity %v", gamma, sum, res.Modularity)
		}
	}
}

func table(name string, cols ...string) schema.Table {
	t := schema.Table{Schema: "public", Name: name, PrimaryKey: &schema.PrimaryKey{Name: name + "_pkey", Columns: []string{"id"}}}
	t.Columns = append(t.Columns, schema.Column{Name: "id", Ordinal: 1, DataType: "integer"})
	for i, c := range cols {
		t.Columns = append(t.Columns, schema.Column{Name: c, Ordinal: i + 2, DataType: "text"})
	}
	return t
}

func TestBaselineNamingAndOrphans(t *testing.T) {
	c := schema.New(schema.DialectPostgres, "My_Shop.Prod DB")
	child := table("unassigned", "parent_id")
	child.Columns[1].DataType = "integer"
	child.ForeignKeys = []schema.ForeignKey{{Name: "fk", Columns: []string{"parent_id"}, RefSchema: "public", RefTable: "Ünïcode_Ω", RefColumns: []string{"id"}}}
	c.Tables = []schema.Table{table("Ünïcode_Ω"), child, table("lonely_one"), table("lonely_two")}
	p, err := Baseline(c, "", segment.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Metadata.Name != "my-shop.prod-db" {
		t.Errorf("metadata.name = %q", p.Metadata.Name)
	}
	var names []string
	for _, d := range p.Spec.Domains {
		names = append(names, d.Name)
	}
	slices.Sort(names)
	// The connected pair is anchored at the table named "unassigned", but the
	// orphan bucket always owns that name, so the anchor gets the suffix.
	if want := []string{"unassigned", "unassigned-2"}; !slices.Equal(names, want) {
		t.Errorf("domains = %v, want %v", names, want)
	}
	if d := p.Domain(UnassignedDomain); d == nil || !strings.HasPrefix(d.Rationale, "orphan bucket: 2 tables") {
		t.Errorf("orphan bucket domain = %+v", d)
	}
}

func TestBaselineErrors(t *testing.T) {
	tests := []struct {
		name string
		c    *schema.Catalog
		plan string
		opts segment.Options
		want string
	}{
		{"nil catalog", nil, "", segment.Options{}, "catalog is nil"},
		{"empty catalog", schema.New(schema.DialectPostgres, "x"), "", segment.Options{}, "no tables"},
		{"bad name", &schema.Catalog{SchemaVersion: schema.SchemaVersion, Dialect: schema.DialectPostgres, Database: "x", Tables: []schema.Table{table("a")}}, "Bad_Name", segment.Options{}, "DNS-1123"},
		{"bad options", &schema.Catalog{SchemaVersion: schema.SchemaVersion, Dialect: schema.DialectPostgres, Database: "x", Tables: []schema.Table{table("a")}}, "", segment.Options{Resolution: -1}, "resolution"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Baseline(tt.c, tt.plan, tt.opts)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
			if p != nil {
				t.Error("plan must be nil on error")
			}
		})
	}
}

func TestDeriveName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"pagila", "pagila"},
		{"My_DB", "my-db"},
		{"a..b", "a.b"},
		{"__", DefaultName},
		{"", DefaultName},
		{"Ωmega", "mega"},
		{strings.Repeat("x", 70), strings.Repeat("x", 63)},
		{strings.Repeat(strings.Repeat("y", 63)+".", 5), strings.TrimSuffix(strings.Repeat(strings.Repeat("y", 63)+".", 3), ".")},
	}
	for _, tt := range tests {
		got := DeriveName(tt.in)
		if got != tt.want {
			t.Errorf("DeriveName(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if !plan.IsDNSSubdomain(got) {
			t.Errorf("DeriveName(%q) = %q is not a DNS subdomain", tt.in, got)
		}
	}
}

func TestUniqueName(t *testing.T) {
	used := map[string]bool{}
	long := strings.Repeat("a", 63)
	got := []string{uniqueName("x", used), uniqueName("x", used), uniqueName("x", used), uniqueName(long, used), uniqueName(long, used)}
	want := []string{"x", "x-2", "x-3", long, strings.Repeat("a", 61) + "-2"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	for _, n := range got {
		if !plan.IsDNSLabel(n) {
			t.Errorf("%q is not a DNS label", n)
		}
	}
}

func TestDomainBase(t *testing.T) {
	tests := []struct {
		comm segment.Community
		want string
	}{
		{segment.Community{Anchor: "public.film_actor"}, "film-actor"},
		{segment.Community{Anchor: "sales.Order Items"}, "orderitems"},
		{segment.Community{Anchor: "public._x_"}, "x"},
		{segment.Community{Anchor: "public.ÜÖ"}, fallbackDomain},
		{segment.Community{Anchor: "public." + strings.Repeat("b", 80)}, strings.Repeat("b", 63)},
		{segment.Community{Anchor: "public.t", OrphanBucket: true}, UnassignedDomain},
	}
	for _, tt := range tests {
		if got := domainBase(tt.comm); got != tt.want {
			t.Errorf("domainBase(%q) = %q, want %q", tt.comm.Anchor, got, tt.want)
		}
	}
}
