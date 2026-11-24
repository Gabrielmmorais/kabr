package plan

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

var update = flag.Bool("update", false, "rewrite golden files")

// smallCatalog: customer -> address, rental -> customer, rental -> film,
// film_actor junction between film and actor.
func smallCatalog() *schema.Catalog {
	rows := int64(42)
	c := schema.New(schema.DialectPostgres, "shop")
	col := func(name, typ string, ord int, nullable bool) schema.Column {
		return schema.Column{Name: name, DataType: typ, Ordinal: ord, Nullable: nullable}
	}
	pk := func(cols ...string) *schema.PrimaryKey { return &schema.PrimaryKey{Name: "pk", Columns: cols} }
	fk := func(name, colName, ref string) schema.ForeignKey {
		return schema.ForeignKey{Name: name, Columns: []string{colName}, RefSchema: "public", RefTable: ref, RefColumns: []string{ref + "_id"}}
	}
	c.Tables = []schema.Table{
		{Schema: "public", Name: "address", Columns: []schema.Column{col("address_id", "integer", 1, false), col("street", "text", 2, false)}, PrimaryKey: pk("address_id")},
		{
			Schema: "public", Name: "customer",
			Columns: []schema.Column{
				col("customer_id", "integer", 1, false), col("email", "text", 2, true),
				col("password_hash", "text", 3, true), col("address_id", "integer", 4, false),
			},
			PrimaryKey:        pk("customer_id"),
			ForeignKeys:       []schema.ForeignKey{fk("customer_address_fk", "address_id", "address")},
			UniqueConstraints: []schema.UniqueConstraint{{Name: "customer_email_key", Columns: []string{"email"}}},
			Stats:             schema.TableStats{EstimatedRows: &rows},
		},
		{Schema: "public", Name: "film", Columns: []schema.Column{col("film_id", "integer", 1, false), col("title", "text", 2, false)}, PrimaryKey: pk("film_id")},
		{Schema: "public", Name: "actor", Columns: []schema.Column{col("actor_id", "integer", 1, false), col("name", "text", 2, false)}, PrimaryKey: pk("actor_id")},
		{
			Schema: "public", Name: "film_actor",
			Columns:     []schema.Column{col("film_id", "integer", 1, false), col("actor_id", "integer", 2, false)},
			PrimaryKey:  pk("film_id", "actor_id"),
			ForeignKeys: []schema.ForeignKey{fk("fa_film_fk", "film_id", "film"), fk("fa_actor_fk", "actor_id", "actor")},
		},
		{
			Schema: "public", Name: "rental",
			Columns:     []schema.Column{col("rental_id", "integer", 1, false), col("customer_id", "integer", 2, false), col("film_id", "integer", 3, false), col("amount", "numeric(5,2)", 4, false)},
			PrimaryKey:  pk("rental_id"),
			ForeignKeys: []schema.ForeignKey{fk("rental_customer_fk", "customer_id", "customer"), fk("rental_film_fk", "film_id", "film")},
		},
	}
	return c
}

// smallPlan is a valid plan for smallCatalog.
func smallPlan(c *schema.Catalog) *DomainPlan {
	p := New("shop", c)
	p.Status.Generator = Generator{Mode: ModeGraph, Modularity: 0.42}
	p.Spec.Domains = []Domain{
		{
			Name: "customers", Description: "Customer accounts",
			Tables: []TableAssignment{
				{Name: "public.customer", Role: RoleCore},
				{Name: "public.address", Role: RoleLookup, Shared: true},
			},
			SensitiveColumns: []SensitiveColumn{
				{Table: "public.customer", Column: "password_hash", Class: ClassCredential, Action: ActionDrop, Source: SourceHeuristic},
				{Table: "public.customer", Column: "email", Class: ClassPII, Action: ActionMask, Source: SourceHeuristic},
			},
			Rationale: "connected component around customer",
		},
		{
			Name: "catalog",
			Tables: []TableAssignment{
				{Name: "public.film_actor", Role: RoleJunction},
				{Name: "public.film", Role: RoleCore},
				{Name: "public.actor", Role: RoleCore},
			},
		},
		{
			Name:   "rentals",
			Tables: []TableAssignment{{Name: "public.rental", Role: RoleCore}},
			SensitiveColumns: []SensitiveColumn{
				{Table: "public.rental", Column: "amount", Class: ClassFinancial, Action: ActionMask},
			},
		},
	}
	return p
}

func mustValid(t *testing.T, p *DomainPlan, c *schema.Catalog) {
	t.Helper()
	if err := p.Validate(c); err != nil {
		t.Fatalf("plan should be valid: %v", err)
	}
}

func TestNew(t *testing.T) {
	c := smallCatalog()
	p := New("shop", c)
	if p.APIVersion != APIVersion || p.Kind != Kind || p.Metadata.Name != "shop" || p.Status.Phase != PhaseProposed {
		t.Fatalf("unexpected header: %+v", p)
	}
	want := Source{Dialect: "postgres", Database: "shop", SchemaVersion: schema.SchemaVersion, CatalogFingerprint: Fingerprint(c)}
	if p.Spec.Source != want {
		t.Fatalf("source = %+v, want %+v", p.Spec.Source, want)
	}
	if p.Spec.Domains == nil || len(p.Spec.Domains) != 0 {
		t.Fatalf("domains = %#v, want empty non-nil", p.Spec.Domains)
	}
	if New("x", nil).Spec.Source != (Source{}) {
		t.Fatal("nil catalog should yield empty source")
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	c := smallCatalog()
	approved := smallPlan(c)
	approved.Metadata.Annotations = map[string]string{"kabr.dev/note": "hello: world"}
	approved.Status.Warnings = []string{"w1"}
	if err := Approve(approved, "alice", time.Date(2026, 11, 23, 10, 0, 0, 999, time.FixedZone("x", 3600))); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		plan *DomainPlan
	}{
		{"proposed", smallPlan(c)},
		{"approved with annotations", approved},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := Marshal(tt.plan)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Unmarshal(data)
			if err != nil {
				t.Fatalf("unmarshal: %v\n%s", err, data)
			}
			want := tt.plan.Clone()
			want.canonicalize()
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("round trip mismatch\n got: %+v\nwant: %+v\n%s", got, want, data)
			}
			again, err := Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(data, again) {
				t.Fatalf("second marshal differs:\n%s\n---\n%s", data, again)
			}
			mustValid(t, got, c)
		})
	}
}

func TestMarshalDeterministicAndNonMutating(t *testing.T) {
	c := smallCatalog()
	p := smallPlan(c)
	before := p.Clone()
	a, err := Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p, before) {
		t.Fatal("Marshal mutated its input")
	}
	shuffled := p.Clone()
	slices.Reverse(shuffled.Spec.Domains)
	for i := range shuffled.Spec.Domains {
		slices.Reverse(shuffled.Spec.Domains[i].Tables)
		slices.Reverse(shuffled.Spec.Domains[i].SensitiveColumns)
	}
	b, err := Marshal(shuffled)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("shuffled input produced different bytes:\n%s\n---\n%s", a, b)
	}
	if _, err := Marshal(nil); err == nil {
		t.Fatal("Marshal(nil) should fail")
	}
}

func TestMarshalGolden(t *testing.T) {
	c := smallCatalog()
	p := smallPlan(c)
	if err := Approve(p, "alice@example.com", time.Date(2026, 11, 23, 12, 30, 45, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	got, err := Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "shop.golden.yaml")
	if *update {
		if werr := os.WriteFile(golden, got, 0o600); werr != nil {
			t.Fatal(werr)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("golden mismatch (run with -update):\n%s", got)
	}
	parsed, err := Unmarshal(want)
	if err != nil {
		t.Fatal(err)
	}
	mustValid(t, parsed, c)
}

func TestUnmarshalErrors(t *testing.T) {
	c := smallCatalog()
	valid, err := Marshal(smallPlan(c))
	if err != nil {
		t.Fatal(err)
	}
	s := string(valid)
	tests := []struct {
		name string
		data string
		want string
	}{
		{"unknown top-level field", s + "extra: 1\n", "unknown field"},
		{"unknown nested field", strings.Replace(s, "  name: shop\n", "  name: shop\n  labels: {}\n", 1), "unknown field"},
		{"wrong apiVersion", strings.Replace(s, "apiVersion: kabr.dev/v1alpha1", "apiVersion: kabr.dev/v1", 1), "apiVersion"},
		{"wrong kind", strings.Replace(s, "kind: DomainPlan", "kind: Plan", 1), "kind"},
		{"duplicate key", s + "kind: DomainPlan\n", "already set"},
		{"not yaml", "{[", "unmarshal"},
		{"empty", "", "apiVersion"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Unmarshal([]byte(tt.data))
			if err == nil {
				t.Fatalf("expected error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestValidateStructure(t *testing.T) {
	c := smallCatalog()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(p *DomainPlan)
		want   []string // substrings that must all appear; nil = valid
	}{
		{"valid", func(*DomainPlan) {}, nil},
		{"valid subdomain name", func(p *DomainPlan) { p.Metadata.Name = "shop.prod-1" }, nil},
		{"approved valid", func(p *DomainPlan) {
			p.Status.Phase, p.Status.ApprovedBy, p.Status.ApprovedAt = PhaseApproved, "bob", &now
		}, nil},
		{"wrong apiVersion and kind", func(p *DomainPlan) { p.APIVersion, p.Kind = "v1", "Plan" }, []string{"apiVersion:", "kind:"}},
		{"bad metadata name", func(p *DomainPlan) { p.Metadata.Name = "Shop_DB" }, []string{"metadata.name"}},
		{"empty metadata name", func(p *DomainPlan) { p.Metadata.Name = "" }, []string{"metadata.name"}},
		{"bad fingerprint", func(p *DomainPlan) { p.Spec.Source.CatalogFingerprint = "md5:abc" }, []string{"spec.source.catalogFingerprint"}},
		{"empty source", func(p *DomainPlan) { p.Spec.Source.Dialect, p.Spec.Source.Database = "", " " }, []string{"spec.source.dialect", "spec.source.database"}},
		{"no domains", func(p *DomainPlan) { p.Spec.Domains = nil }, []string{"spec.domains: at least one"}},
		{"uppercase domain", func(p *DomainPlan) { p.Spec.Domains[2].Name = "Rentals" }, []string{"spec.domains[2].name"}},
		{"leading dash", func(p *DomainPlan) { p.Spec.Domains[1].Name = "-catalog" }, []string{"spec.domains[1].name"}},
		{"too long label", func(p *DomainPlan) { p.Spec.Domains[1].Name = strings.Repeat("a", 64) }, []string{"spec.domains[1].name"}},
		{"duplicate domain", func(p *DomainPlan) { p.Spec.Domains[2].Name = "catalog" }, []string{"spec.domains[2].name: duplicate"}},
		{"empty domain", func(p *DomainPlan) {
			p.Spec.Domains = append(p.Spec.Domains, Domain{Name: "empty"})
		}, []string{"spec.domains[3].tables"}},
		{"table in two domains", func(p *DomainPlan) {
			p.Spec.Domains[2].Tables = append(p.Spec.Domains[2].Tables, TableAssignment{Name: "public.film"})
		}, []string{"spec.domains[2].tables[1].name", "already listed at spec.domains[1].tables[1]"}},
		{"table twice in one domain", func(p *DomainPlan) {
			p.Spec.Domains[2].Tables = append(p.Spec.Domains[2].Tables, TableAssignment{Name: "public.rental"})
		}, []string{"already listed"}},
		{"bad table id", func(p *DomainPlan) { p.Spec.Domains[2].Tables[0].Name = "rental" }, []string{"spec.domains[2].tables[0].name"}},
		{"bad role", func(p *DomainPlan) { p.Spec.Domains[1].Tables[0].Role = "bridge" }, []string{"spec.domains[1].tables[0].role"}},
		{"bad class action source", func(p *DomainPlan) {
			sc := &p.Spec.Domains[0].SensitiveColumns[0]
			sc.Class, sc.Action, sc.Source = "secret", "encrypt", "guess"
		}, []string{"sensitiveColumns[0].class", "sensitiveColumns[0].action", "sensitiveColumns[0].source"}},
		{"sensitive column on table of another domain", func(p *DomainPlan) {
			p.Spec.Domains[2].SensitiveColumns[0].Table = "public.customer"
		}, []string{"spec.domains[2].sensitiveColumns[0].table", "not listed in domain"}},
		{"duplicate sensitive column", func(p *DomainPlan) {
			d := &p.Spec.Domains[0]
			d.SensitiveColumns = append(d.SensitiveColumns, d.SensitiveColumns[0])
		}, []string{"duplicate sensitive column"}},
		{"bad phase", func(p *DomainPlan) { p.Status.Phase = "Done" }, []string{"status.phase"}},
		{"approved without approver", func(p *DomainPlan) { p.Status.Phase = PhaseApproved }, []string{"status.approvedBy", "status.approvedAt"}},
		{"bad generator mode", func(p *DomainPlan) { p.Status.Generator.Mode = "magic" }, []string{"status.generator.mode"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := smallPlan(c)
			tt.mutate(p)
			err := p.ValidateStructure()
			if tt.want == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected errors %v", tt.want)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error does not contain %q:\n%v", w, err)
				}
			}
		})
	}
	var nilPlan *DomainPlan
	if nilPlan.ValidateStructure() == nil {
		t.Fatal("nil plan should be invalid")
	}
}

func TestValidateStructureReportsAllProblems(t *testing.T) {
	p := smallPlan(smallCatalog())
	p.Metadata.Name = "BAD"
	p.Spec.Domains[0].Name = "BAD"
	p.Status.Phase = PhaseApproved
	err := p.ValidateStructure()
	if err == nil {
		t.Fatal("expected errors")
	}
	if n := strings.Count(err.Error(), "\n") + 1; n < 4 {
		t.Fatalf("expected at least 4 joined errors, got %d:\n%v", n, err)
	}
}

func TestValidateAgainstCatalog(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(p *DomainPlan, c *schema.Catalog)
		want   []string
	}{
		{"valid", func(*DomainPlan, *schema.Catalog) {}, nil},
		{"missing table", func(p *DomainPlan, _ *schema.Catalog) {
			p.Spec.Domains[1].Tables = p.Spec.Domains[1].Tables[:2] // drop public.actor
		}, []string{`catalog table "public.actor" is not assigned`}},
		{"unknown table", func(p *DomainPlan, _ *schema.Catalog) {
			p.Spec.Domains[2].Tables = append(p.Spec.Domains[2].Tables, TableAssignment{Name: "public.payment"})
		}, []string{`spec.domains[2].tables[1].name: unknown table "public.payment"`}},
		{"nonexistent sensitive column", func(p *DomainPlan, _ *schema.Catalog) {
			p.Spec.Domains[2].SensitiveColumns[0].Column = "price"
		}, []string{`spec.domains[2].sensitiveColumns[0].column: table "public.rental" has no column "price"`}},
		{"sensitive column on wrong domain", func(p *DomainPlan, _ *schema.Catalog) {
			p.Spec.Domains[2].SensitiveColumns[0] = SensitiveColumn{Table: "public.customer", Column: "email", Class: ClassPII, Action: ActionMask}
		}, []string{"not listed in domain \"rentals\""}},
		{"fingerprint mismatch", func(_ *DomainPlan, c *schema.Catalog) {
			c.Tables[0].Columns[1].DataType = "varchar(10)"
		}, []string{"plan was generated for a different catalog"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := smallCatalog()
			p := smallPlan(c)
			tt.mutate(p, c)
			err := p.Validate(c)
			if tt.want == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected errors %v", tt.want)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error does not contain %q:\n%v", w, err)
				}
			}
			if strings.Contains(tt.name, "fingerprint") && !errors.Is(err, ErrCatalogMismatch) {
				t.Errorf("fingerprint mismatch should wrap ErrCatalogMismatch: %v", err)
			}
		})
	}
	if err := smallPlan(smallCatalog()).Validate(nil); err == nil {
		t.Fatal("nil catalog should fail")
	}
}

func loadPagila(t *testing.T) *schema.Catalog {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "pagila.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := schema.Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// pagilaPlan puts every pagila table in a domain named after its first
// letter group: a single "store" domain plus "people" for customer/staff.
func pagilaPlan(c *schema.Catalog) *DomainPlan {
	p := New("pagila", c)
	people := Domain{Name: "people"}
	store := Domain{Name: "store"}
	for i := range c.Tables {
		id := c.Tables[i].ID()
		if c.Tables[i].Name == "customer" || c.Tables[i].Name == "staff" {
			people.Tables = append(people.Tables, TableAssignment{Name: id, Role: RoleCore})
			continue
		}
		store.Tables = append(store.Tables, TableAssignment{Name: id})
	}
	people.SensitiveColumns = []SensitiveColumn{{Table: "public.customer", Column: "email", Class: ClassPII, Action: ActionMask}}
	p.Spec.Domains = []Domain{people, store}
	return p
}

func TestValidatePagila(t *testing.T) {
	c := loadPagila(t)
	mustValid(t, pagilaPlan(c), c)

	t.Run("missing and unknown", func(t *testing.T) {
		p := pagilaPlan(c)
		p.Spec.Domains[1].Tables[0].Name = "public.nope"
		err := p.Validate(c)
		if err == nil || !strings.Contains(err.Error(), `unknown table "public.nope"`) || !strings.Contains(err.Error(), "is not assigned to any domain") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("sensitive column wrong domain", func(t *testing.T) {
		p := pagilaPlan(c)
		p.Spec.Domains[1].SensitiveColumns = []SensitiveColumn{{Table: "public.staff", Column: "password", Class: ClassCredential, Action: ActionDrop}}
		if err := p.Validate(c); err == nil || !strings.Contains(err.Error(), "not listed in domain") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("fingerprint mismatch", func(t *testing.T) {
		other := c.Clone()
		other.Tables = other.Tables[1:]
		if err := pagilaPlan(c).Validate(other); !errors.Is(err, ErrCatalogMismatch) {
			t.Fatalf("expected ErrCatalogMismatch, got %v", err)
		}
	})
}

func TestFingerprint(t *testing.T) {
	base := smallCatalog()
	fp := Fingerprint(base)
	if !fingerprintRE.MatchString(fp) {
		t.Fatalf("bad fingerprint format %q", fp)
	}
	if Fingerprint(nil) != "" {
		t.Fatal("nil catalog should give empty fingerprint")
	}
	same := []struct {
		name   string
		mutate func(c *schema.Catalog)
	}{
		{"tables reordered", func(c *schema.Catalog) { slices.Reverse(c.Tables) }},
		{"columns reordered", func(c *schema.Catalog) {
			for i := range c.Tables {
				slices.Reverse(c.Tables[i].Columns)
			}
		}},
		{"foreign keys reordered", func(c *schema.Catalog) { slices.Reverse(c.Tables[5].ForeignKeys) }},
		{"stats changed", func(c *schema.Catalog) {
			n := int64(1_000_000)
			d := -1.0
			c.Tables[1].Stats.EstimatedRows = &n
			c.Tables[0].Columns[0].Stats.NDistinct = &d
		}},
		{"introspectedAt, comments and names changed", func(c *schema.Catalog) {
			now := time.Now()
			c.IntrospectedAt = &now
			c.Tables[0].Comment = "addresses"
			c.Tables[0].Columns[0].Comment = "id"
			c.Tables[1].ForeignKeys[0].Name = "renamed_fk"
			c.Tables[1].Indexes = []schema.Index{{Name: "ix", Columns: []string{"email"}}}
		}},
		{"database renamed", func(c *schema.Catalog) { c.Database = "shop_copy" }},
	}
	for _, tt := range same {
		t.Run("same/"+tt.name, func(t *testing.T) {
			c := smallCatalog()
			tt.mutate(c)
			if got := Fingerprint(c); got != fp {
				t.Fatalf("fingerprint changed: %s != %s", got, fp)
			}
		})
	}
	different := []struct {
		name   string
		mutate func(c *schema.Catalog)
	}{
		{"column type changed", func(c *schema.Catalog) { c.Tables[0].Columns[1].DataType = "varchar(80)" }},
		{"nullability changed", func(c *schema.Catalog) { c.Tables[0].Columns[1].Nullable = true }},
		{"column added", func(c *schema.Catalog) {
			c.Tables[0].Columns = append(c.Tables[0].Columns, schema.Column{Name: "zip", DataType: "text", Ordinal: 3})
		}},
		{"table removed", func(c *schema.Catalog) { c.Tables = c.Tables[1:] }},
		{"foreign key removed", func(c *schema.Catalog) { c.Tables[1].ForeignKeys = nil }},
		{"primary key changed", func(c *schema.Catalog) { c.Tables[4].PrimaryKey.Columns = []string{"actor_id", "film_id"} }},
		{"unique constraint removed", func(c *schema.Catalog) { c.Tables[1].UniqueConstraints = nil }},
	}
	for _, tt := range different {
		t.Run("different/"+tt.name, func(t *testing.T) {
			c := smallCatalog()
			tt.mutate(c)
			if got := Fingerprint(c); got == fp {
				t.Fatal("fingerprint should change")
			}
		})
	}
}

func TestApprove(t *testing.T) {
	c := smallCatalog()
	p := smallPlan(c)
	if err := Approve(p, "", time.Now()); err == nil {
		t.Fatal("empty approver should fail")
	}
	if err := Approve(p, "  ", time.Now()); err == nil {
		t.Fatal("blank approver should fail")
	}
	if err := Approve(p, "alice", time.Time{}); err == nil {
		t.Fatal("zero time should fail")
	}
	if err := Approve(nil, "alice", time.Now()); err == nil {
		t.Fatal("nil plan should fail")
	}
	bad := smallPlan(c)
	bad.Spec.Domains[0].Name = "BAD"
	if err := Approve(bad, "alice", time.Now()); err == nil || bad.Status.Phase == PhaseApproved {
		t.Fatalf("invalid plan should not be approved: %v", err)
	}

	at := time.Date(2026, 11, 23, 9, 8, 7, 654321, time.FixedZone("BRT", -3*3600))
	if err := Approve(p, "alice", at); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 23, 12, 8, 7, 0, time.UTC)
	if p.Status.Phase != PhaseApproved || p.Status.ApprovedBy != "alice" || !p.Status.ApprovedAt.Equal(want) || p.Status.ApprovedAt.Location() != time.UTC {
		t.Fatalf("unexpected status: %+v at %v", p.Status, p.Status.ApprovedAt)
	}
	mustValid(t, p, c)

	later := at.Add(time.Hour)
	if err := Approve(p, "bob", later); err != nil {
		t.Fatal(err)
	}
	if p.Status.ApprovedBy != "bob" || !p.Status.ApprovedAt.Equal(want.Add(time.Hour)) {
		t.Fatalf("re-approve did not update: %+v", p.Status)
	}
}

func TestApply(t *testing.T) {
	c := smallCatalog()
	tests := []struct {
		name     string
		r        Refinement
		check    func(t *testing.T, out *DomainPlan)
		warnings []string // substrings, one per expected warning (in order)
		wantErr  string
	}{
		{
			name: "empty refinement is a no-op",
			check: func(t *testing.T, out *DomainPlan) {
				if len(out.Spec.Domains) != 3 {
					t.Fatalf("domains = %d", len(out.Spec.Domains))
				}
			},
		},
		{
			name: "rename",
			r:    Refinement{Renames: []DomainRename{{From: "catalog", To: "films", Description: "Film catalog", Rationale: "film is the hub"}}},
			check: func(t *testing.T, out *DomainPlan) {
				d := out.Domain("films")
				if d == nil || out.Domain("catalog") != nil {
					t.Fatal("rename not applied")
				}
				if d.Description != "Film catalog" || d.Rationale != "refined: film is the hub" || len(d.Tables) != 3 {
					t.Fatalf("unexpected domain %+v", d)
				}
			},
		},
		{
			name: "chained renames",
			r:    Refinement{Renames: []DomainRename{{From: "catalog", To: "films"}, {From: "films", To: "movies"}}},
			check: func(t *testing.T, out *DomainPlan) {
				if out.Domain("movies") == nil || out.Domain("catalog") != nil || out.Domain("films") != nil {
					t.Fatal("chained rename not applied")
				}
			},
		},
		{
			name: "move into existing domain carries sensitive columns and removes empty domain",
			r:    Refinement{Moves: []TableMove{{Table: "public.rental", To: "customers", Rationale: "rentals belong to customers"}}},
			check: func(t *testing.T, out *DomainPlan) {
				if out.Domain("rentals") != nil {
					t.Fatal("empty domain should be removed")
				}
				if out.DomainOf("public.rental") != "customers" {
					t.Fatal("table not moved")
				}
				d := out.Domain("customers")
				var found bool
				for _, ta := range d.Tables {
					if ta.Name == "public.rental" {
						found = true
						if ta.Rationale != "refined: rentals belong to customers" || ta.Role != RoleCore {
							t.Fatalf("assignment = %+v", ta)
						}
					}
				}
				if !found {
					t.Fatal("assignment missing")
				}
				if len(d.SensitiveColumns) != 3 {
					t.Fatalf("sensitive columns did not follow the table: %+v", d.SensitiveColumns)
				}
			},
			warnings: []string{`domain "rentals" removed`},
		},
		{
			name: "move into new domain, after rename",
			r: Refinement{
				Renames: []DomainRename{{From: "catalog", To: "films"}},
				Moves:   []TableMove{{Table: "public.actor", To: "people", Rationale: "actors are people"}, {Table: "public.film", To: "films"}},
			},
			check: func(t *testing.T, out *DomainPlan) {
				d := out.Domain("people")
				if d == nil || len(d.Tables) != 1 || d.Tables[0].Name != "public.actor" || !strings.HasPrefix(d.Rationale, "refined: ") {
					t.Fatalf("new domain = %+v", d)
				}
				if out.DomainOf("public.film") != "films" {
					t.Fatal("no-op move lost the table")
				}
			},
		},
		{
			name: "sensitive columns merged, refinement wins",
			r: Refinement{SensitiveColumns: []SensitiveColumn{
				{Table: "public.customer", Column: "email", Class: ClassPII, Action: ActionHash, Source: SourceLLM, Rationale: "joinable"},
				{Table: "public.address", Column: "street", Class: ClassPII, Action: ActionMask, Source: SourceLLM},
			}},
			check: func(t *testing.T, out *DomainPlan) {
				d := out.Domain("customers")
				if len(d.SensitiveColumns) != 3 {
					t.Fatalf("sensitive = %+v", d.SensitiveColumns)
				}
				for _, sc := range d.SensitiveColumns {
					if sc.Column == "email" && (sc.Action != ActionHash || sc.Source != SourceLLM) {
						t.Fatalf("refinement did not win: %+v", sc)
					}
				}
			},
		},
		{
			name: "invalid items skipped with warnings",
			r: Refinement{
				Renames: []DomainRename{
					{From: "nope", To: "x"},
					{From: "catalog", To: "Bad_Name"},
					{From: "catalog", To: "rentals"},
				},
				Moves: []TableMove{
					{Table: "public.ghost", To: "catalog"},
					{Table: "public.film", To: "NOT VALID"},
				},
				SensitiveColumns: []SensitiveColumn{
					{Table: "public.ghost", Column: "x", Class: ClassPII, Action: ActionMask},
					{Table: "public.film", Column: "ghost", Class: ClassPII, Action: ActionMask},
					{Table: "public.film", Column: "title", Class: "weird", Action: ActionMask},
				},
			},
			check: func(t *testing.T, out *DomainPlan) {
				if len(out.Spec.Domains) != 3 || out.Domain("catalog") == nil {
					t.Fatalf("plan changed: %+v", out.Spec.Domains)
				}
			},
			warnings: []string{
				`renames[0]: skipped: unknown domain "nope"`,
				`renames[1]: skipped: "Bad_Name"`,
				`renames[2]: skipped: domain "rentals" already exists`,
				`moves[0]: skipped: unknown table "public.ghost"`,
				`moves[1]: skipped: "NOT VALID"`,
				`sensitiveColumns[0]: skipped: unknown table`,
				`sensitiveColumns[1]: skipped: table "public.film" has no column "ghost"`,
				`sensitiveColumns[2].class`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := smallPlan(c)
			if err := Approve(p, "alice", time.Now()); err != nil {
				t.Fatal(err)
			}
			before := p.Clone()
			out, warnings, err := Apply(p, tt.r, c)
			if !reflect.DeepEqual(p, before) {
				t.Fatal("Apply mutated its input")
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v (warnings %v)", err, warnings)
			}
			if len(warnings) != len(tt.warnings) {
				t.Fatalf("warnings = %q, want %d", warnings, len(tt.warnings))
			}
			for i, w := range tt.warnings {
				if !strings.Contains(warnings[i], w) {
					t.Errorf("warnings[%d] = %q, want substring %q", i, warnings[i], w)
				}
			}
			if out.Status.Phase != PhaseProposed || out.Status.ApprovedBy != "" || out.Status.ApprovedAt != nil {
				t.Fatalf("approval not reset: %+v", out.Status)
			}
			mustValid(t, out, c)
			if tt.check != nil {
				tt.check(t, out)
			}
		})
	}
}

func TestApplyErrors(t *testing.T) {
	c := smallCatalog()
	if _, _, err := Apply(nil, Refinement{}, c); err == nil {
		t.Fatal("nil plan should fail")
	}
	if _, _, err := Apply(smallPlan(c), Refinement{}, nil); err == nil {
		t.Fatal("nil catalog should fail")
	}
	// A baseline that does not cover the catalog cannot be fixed by a
	// refinement: the result fails Validate and Apply returns an error.
	p := smallPlan(c)
	p.Spec.Domains[1].Tables = p.Spec.Domains[1].Tables[:2]
	if _, _, err := Apply(p, Refinement{}, c); err == nil || !strings.Contains(err.Error(), "refined plan is invalid") {
		t.Fatalf("err = %v", err)
	}
	other := smallCatalog()
	other.Tables[0].Columns[1].DataType = "varchar(1)"
	if _, _, err := Apply(smallPlan(c), Refinement{}, other); !errors.Is(err, ErrCatalogMismatch) {
		t.Fatalf("err = %v, want ErrCatalogMismatch", err)
	}
}

func TestSchema(t *testing.T) {
	embedded := Schema()
	var doc map[string]any
	if err := json.Unmarshal(embedded, &doc); err != nil {
		t.Fatalf("embedded schema is not valid JSON: %v", err)
	}
	if doc["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Fatalf("$schema = %v", doc["$schema"])
	}
	published, err := os.ReadFile(filepath.Join("..", "..", "api", "v1alpha1", "domainplan.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(embedded, published) {
		t.Fatal("pkg/plan/domainplan.schema.json and api/v1alpha1/domainplan.schema.json differ; keep them identical")
	}
	embedded[0] = 'X'
	if Schema()[0] == 'X' {
		t.Fatal("Schema() must return a copy")
	}

	// Top-level keys of a marshaled plan must be schema properties.
	p := smallPlan(smallCatalog())
	if aerr := Approve(p, "alice", time.Now()); aerr != nil {
		t.Fatal(aerr)
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatal(err)
	}
	props := doc["properties"].(map[string]any)
	if got, want := sortedKeys(top), sortedKeys(props); !slices.Equal(got, want) {
		t.Fatalf("marshaled keys %v != schema properties %v", got, want)
	}

	// Every struct's JSON field names must match its $defs properties.
	defs := doc["$defs"].(map[string]any)
	for _, typ := range []any{Metadata{}, Spec{}, Source{}, Domain{}, TableAssignment{}, SensitiveColumn{}, Status{}, Generator{}} {
		rt := reflect.TypeOf(typ)
		def, ok := defs[rt.Name()].(map[string]any)
		if !ok {
			t.Errorf("$defs.%s missing", rt.Name())
			continue
		}
		var fields []string
		for f := range rt.Fields() {
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			fields = append(fields, name)
		}
		slices.Sort(fields)
		if want := sortedKeys(def["properties"].(map[string]any)); !slices.Equal(fields, want) {
			t.Errorf("%s: struct fields %v != schema properties %v", rt.Name(), fields, want)
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func TestDNSHelpers(t *testing.T) {
	tests := []struct {
		in            string
		label, subdom bool
	}{
		{"a", true, true},
		{"customers", true, true},
		{"a-b-1", true, true},
		{"a.b", false, true},
		{"", false, false},
		{"-a", false, false},
		{"a-", false, false},
		{"A", false, false},
		{"a_b", false, false},
		{"a..b", false, false},
		{strings.Repeat("a", 63), true, true},
		{strings.Repeat("a", 64), false, false},
	}
	for _, tt := range tests {
		if got := IsDNSLabel(tt.in); got != tt.label {
			t.Errorf("IsDNSLabel(%q) = %v", tt.in, got)
		}
		if got := IsDNSSubdomain(tt.in); got != tt.subdom {
			t.Errorf("IsDNSSubdomain(%q) = %v", tt.in, got)
		}
	}
}
