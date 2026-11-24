package segment

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

// table builds s.<name> with integer columns, an optional primary key and
// foreign keys.
func table(name string, cols, pk []string, fks ...schema.ForeignKey) schema.Table {
	t := schema.Table{Schema: "s", Name: name, ForeignKeys: fks}
	for i, c := range cols {
		t.Columns = append(t.Columns, schema.Column{Name: c, Ordinal: i + 1, DataType: "integer"})
	}
	if len(pk) > 0 {
		t.PrimaryKey = &schema.PrimaryKey{Name: name + "_pkey", Columns: pk}
	}
	return t
}

func ref(col, refTable, refCol string) schema.ForeignKey {
	return schema.ForeignKey{
		Name: col + "_" + refTable + "_fkey", Columns: []string{col},
		RefSchema: "s", RefTable: refTable, RefColumns: []string{refCol},
	}
}

func cat(tables ...schema.Table) *schema.Catalog {
	c := schema.New(schema.DialectPostgres, "db")
	c.Tables = tables
	return c
}

func mustSegment(t *testing.T, c *schema.Catalog, opts Options) *Result {
	t.Helper()
	r, err := Segment(c, opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func assignment(t *testing.T, r *Result, id string) (Assignment, int) {
	t.Helper()
	for _, c := range r.Communities {
		for _, a := range c.Tables {
			if a.Table == id {
				return a, c.ID
			}
		}
	}
	t.Fatalf("table %s not assigned", id)
	return Assignment{}, -1
}

func dump(r *Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Q=%.4f orphans=%v\n", r.Modularity, r.Orphans)
	for _, c := range r.Communities {
		fmt.Fprintf(&b, "  #%d anchor=%s orphanBucket=%v\n", c.ID, c.Anchor, c.OrphanBucket)
		for _, a := range c.Tables {
			fmt.Fprintf(&b, "      %-22s %-8s shared=%-5v %s\n", a.Table, a.Role, a.Shared, a.Rationale)
		}
	}
	for _, e := range r.InferredEdges {
		fmt.Fprintf(&b, "  inferred %s.%s -> %s (%v)\n", e.From, e.Column, e.To, e.Weight)
	}
	return b.String()
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

func TestSegmentErrors(t *testing.T) {
	if _, err := Segment(nil, DefaultOptions()); err == nil {
		t.Error("nil catalog: want error")
	}
	bad := cat(table("a", []string{"id"}, nil), table("a", []string{"id"}, nil))
	if _, err := Segment(bad, DefaultOptions()); err == nil {
		t.Error("duplicate tables: want error")
	}
	for _, o := range []Options{
		{Resolution: 0, InferEdges: true, InferredWeight: 0.5},
		{Resolution: -1},
		{Resolution: 1, InferEdges: true, InferredWeight: 0},
	} {
		if _, err := Segment(cat(), o); err == nil {
			t.Errorf("options %+v: want error", o)
		}
	}
	if _, err := Segment(cat(), Options{Resolution: 1}); err != nil {
		t.Errorf("inference off with zero weight should be valid: %v", err)
	}
}

func TestSegmentEmpty(t *testing.T) {
	r := mustSegment(t, cat(), DefaultOptions())
	got, _ := json.Marshal(r)
	if want := `{"communities":[],"modularity":0,"inferredEdges":[],"orphans":[]}`; string(got) != want {
		t.Errorf("empty result = %s, want %s", got, want)
	}
	if _, ok := r.CommunityOf("s.x"); ok {
		t.Error("CommunityOf on empty result")
	}
	var nilRes *Result
	if _, ok := nilRes.CommunityOf("s.x"); ok {
		t.Error("CommunityOf on nil result")
	}
}

func TestSegmentDoesNotModifyCatalog(t *testing.T) {
	c := loadPagila(t)
	slices.Reverse(c.Tables)
	before, _ := json.Marshal(c)
	mustSegment(t, c, DefaultOptions())
	after, _ := json.Marshal(c)
	if string(before) != string(after) {
		t.Error("Segment modified its input")
	}
}

func TestInference(t *testing.T) {
	people := func(name string) schema.Table {
		return table(name, []string{name + "_id", "name"}, []string{name + "_id"})
	}
	tests := []struct {
		name    string
		catalog *schema.Catalog
		want    []string // "from.column->to"
	}{
		{
			name: "payment-like table without FKs",
			catalog: cat(people("customer"), people("staff"), people("rental"),
				table("payment", []string{"payment_id", "customer_id", "staff_id", "rental_id", "amount"}, []string{"payment_id"})),
			want: []string{"s.payment.customer_id->s.customer", "s.payment.rental_id->s.rental", "s.payment.staff_id->s.staff"},
		},
		{
			name: "prefixed column",
			catalog: cat(people("staff"),
				table("store", []string{"store_id", "manager_staff_id"}, []string{"store_id"})),
			want: []string{"s.store.manager_staff_id->s.staff"},
		},
		{
			name: "declared FK suppresses inference",
			catalog: cat(people("customer"),
				table("orders", []string{"order_id", "customer_id"}, []string{"order_id"}, ref("customer_id", "customer", "customer_id"))),
			want: nil,
		},
		{
			name:    "never self",
			catalog: cat(table("node", []string{"node_id", "parent_node_id"}, []string{"node_id"})),
			want:    nil,
		},
		{
			name: "plural table with id key",
			catalog: cat(table("users", []string{"id", "name"}, []string{"id"}),
				table("categories", []string{"id"}, []string{"id"}),
				table("post", []string{"post_id", "user_id", "category_id"}, []string{"post_id"})),
			want: []string{"s.post.category_id->s.categories", "s.post.user_id->s.users"},
		},
		{
			name: "composite key target is not matched",
			catalog: cat(table("account", []string{"account_id", "region"}, []string{"account_id", "region"}),
				table("invoice", []string{"invoice_id", "account_id"}, []string{"invoice_id"})),
			want: nil,
		},
		{
			name: "type family mismatch rejected",
			catalog: func() *schema.Catalog {
				c := cat(people("customer"), table("note", []string{"note_id", "customer_id"}, []string{"note_id"}))
				c.Tables[1].Columns[1].DataType = "uuid"
				return c
			}(),
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := mustSegment(t, tt.catalog, DefaultOptions())
			var got []string
			for _, e := range r.InferredEdges {
				got = append(got, e.From+"."+e.Column+"->"+e.To)
				if e.Weight != 0.5 {
					t.Errorf("weight = %v", e.Weight)
				}
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("inferred = %v, want %v", got, tt.want)
			}
			off := DefaultOptions()
			off.InferEdges = false
			if r := mustSegment(t, tt.catalog, off); len(r.InferredEdges) != 0 {
				t.Errorf("InferEdges=false still inferred %v", r.InferredEdges)
			}
		})
	}
}

func TestClassification(t *testing.T) {
	// Two business clusters sharing a small lookup; a junction; an orphan.
	c := cat(
		table("status", []string{"status_id", "label"}, []string{"status_id"}),
		table("author", []string{"author_id", "name", "bio", "email", "x"}, []string{"author_id"}),
		table("book", []string{"book_id", "title", "status_id", "a", "b"}, []string{"book_id"},
			ref("status_id", "status", "status_id")),
		table("book_author", []string{"book_id", "author_id"}, []string{"book_id", "author_id"},
			ref("book_id", "book", "book_id"), ref("author_id", "author", "author_id")),
		table("chapter", []string{"chapter_id", "book_id", "c", "d", "e"}, []string{"chapter_id"},
			ref("book_id", "book", "book_id")),
		table("client", []string{"client_id", "name", "status_id", "a", "b"}, []string{"client_id"},
			ref("status_id", "status", "status_id")),
		table("invoice", []string{"invoice_id", "client_id", "a", "b", "c"}, []string{"invoice_id"},
			ref("client_id", "client", "client_id")),
		table("invoice_line", []string{"line_id", "invoice_id", "a", "b", "c"}, []string{"line_id"},
			ref("invoice_id", "invoice", "invoice_id")),
		table("audit_log", []string{"log_id", "message"}, []string{"log_id"}),
		table("settings", []string{"key", "value"}, []string{"key"}),
	)
	r := mustSegment(t, c, DefaultOptions())
	t.Log("\n" + dump(r))

	want := map[string]Role{
		"s.status": RoleLookup, "s.book_author": RoleJunction, "s.audit_log": RoleOrphan, "s.settings": RoleOrphan,
		"s.book": RoleCore, "s.author": RoleCore, "s.chapter": RoleCore, "s.client": RoleCore, "s.invoice": RoleCore, "s.invoice_line": RoleCore,
	}
	for id, role := range want {
		if a, _ := assignment(t, r, id); a.Role != role {
			t.Errorf("%s role = %s, want %s", id, a.Role, role)
		}
	}
	// The lookup must not merge the two clusters.
	_, book := assignment(t, r, "s.book")
	_, client := assignment(t, r, "s.client")
	if book == client {
		t.Errorf("lookup glued book and client together:\n%s", dump(r))
	}
	status, statusComm := assignment(t, r, "s.status")
	if !status.Shared || !strings.Contains(status.Rationale, "lookup: 2 columns") {
		t.Errorf("status: %+v", status)
	}
	if statusComm != book && statusComm != client {
		t.Errorf("lookup placed away from its referencers")
	}
	ja, jComm := assignment(t, r, "s.book_author")
	if jComm != book || !strings.Contains(ja.Rationale, "owner s.book") {
		t.Errorf("junction %+v in community %d, want book's %d", ja, jComm, book)
	}
	if _, a := assignment(t, r, "s.author"); a != book {
		t.Errorf("author not with book")
	}
	if !slices.Equal(r.Orphans, []string{"s.audit_log", "s.settings"}) {
		t.Errorf("orphans = %v", r.Orphans)
	}
	_, o1 := assignment(t, r, "s.audit_log")
	_, o2 := assignment(t, r, "s.settings")
	if o1 != o2 || !r.Communities[o1].OrphanBucket || r.Communities[o1].Anchor != "s.audit_log" {
		t.Errorf("orphan bucket wrong: %+v", r.Communities[o1])
	}
	if got := r.Communities[book].Anchor; got != "s.book" {
		t.Errorf("anchor = %s, want s.book", got)
	}
	for i, cm := range r.Communities {
		if cm.ID != i {
			t.Errorf("community %d has ID %d", i, cm.ID)
		}
		if id, ok := r.CommunityOf(cm.Tables[0].Table); !ok || id != i {
			t.Errorf("CommunityOf(%s) = %d,%v", cm.Tables[0].Table, id, ok)
		}
		if i > 0 && len(r.Communities[i-1].Tables) < len(cm.Tables) {
			t.Errorf("communities not ordered by size")
		}
	}

	// Lookup detection disabled ⇒ status becomes core.
	noLookup := DefaultOptions()
	noLookup.LookupMaxColumns = 0
	if a, _ := assignment(t, mustSegment(t, c, noLookup), "s.status"); a.Role != RoleCore {
		t.Errorf("LookupMaxColumns=0: status role %s", a.Role)
	}
}

func TestLookupNeedsTwoReferences(t *testing.T) {
	c := cat(
		table("country", []string{"country_id", "name"}, []string{"country_id"}),
		table("city", []string{"city_id", "country_id"}, []string{"city_id"}, ref("country_id", "country", "country_id")),
	)
	r := mustSegment(t, c, DefaultOptions())
	if a, _ := assignment(t, r, "s.country"); a.Role != RoleCore {
		t.Errorf("single reference must not make a lookup: %+v", a)
	}
}

func TestDisconnectedClustersStaySeparate(t *testing.T) {
	c := cat(
		table("a", []string{"a_id"}, []string{"a_id"}),
		table("b", []string{"b_id", "a_id"}, []string{"b_id"}, ref("a_id", "a", "a_id")),
		table("x", []string{"x_id"}, []string{"x_id"}),
		table("y", []string{"y_id", "x_id"}, []string{"y_id"}, ref("x_id", "x", "x_id")),
	)
	r := mustSegment(t, c, DefaultOptions())
	if len(r.Communities) != 2 {
		t.Fatalf("want 2 communities:\n%s", dump(r))
	}
	// Two equal components: Q = 2·(2/4 − ¼) = 0.5.
	if !near(r.Modularity, 0.5) {
		t.Errorf("Q = %v, want 0.5", r.Modularity)
	}
}

// shuffle returns a copy of c with tables, columns and constraints permuted.
func shuffle(c *schema.Catalog, rng *rand.Rand) *schema.Catalog {
	s := c.Clone()
	rng.Shuffle(len(s.Tables), func(i, j int) { s.Tables[i], s.Tables[j] = s.Tables[j], s.Tables[i] })
	for i := range s.Tables {
		t := &s.Tables[i]
		rng.Shuffle(len(t.ForeignKeys), func(a, b int) { t.ForeignKeys[a], t.ForeignKeys[b] = t.ForeignKeys[b], t.ForeignKeys[a] })
		rng.Shuffle(len(t.Columns), func(a, b int) { t.Columns[a], t.Columns[b] = t.Columns[b], t.Columns[a] })
		rng.Shuffle(len(t.UniqueConstraints), func(a, b int) {
			t.UniqueConstraints[a], t.UniqueConstraints[b] = t.UniqueConstraints[b], t.UniqueConstraints[a]
		})
	}
	return s
}

func TestDeterminismUnderShuffling(t *testing.T) {
	catalogs := map[string]*schema.Catalog{
		"pagila":    loadPagila(t),
		"synthetic": synthetic(300, 7),
	}
	for name, c := range catalogs {
		t.Run(name, func(t *testing.T) {
			base := mustSegment(t, c, DefaultOptions())
			wantJSON, _ := json.Marshal(base)
			wantFmt := fmt.Sprintf("%+v", *base)
			rng := rand.New(rand.NewPCG(42, 2026))
			for i := range 20 {
				r := mustSegment(t, shuffle(c, rng), DefaultOptions())
				gotJSON, _ := json.Marshal(r)
				if string(gotJSON) != string(wantJSON) || fmt.Sprintf("%+v", *r) != wantFmt {
					t.Fatalf("shuffle %d produced a different result:\n%s\nvs\n%s", i, gotJSON, wantJSON)
				}
			}
		})
	}
}

// synthetic builds n tables in clusters of 20: a chain plus a few random
// intra-cluster FKs, rare inter-cluster FKs, a shared lookup and some
// orphans. Generation is seeded, hence reproducible.
func synthetic(n int, seed uint64) *schema.Catalog {
	rng := rand.New(rand.NewPCG(seed, 1))
	c := schema.New(schema.DialectPostgres, "db")
	name := func(i int) string { return fmt.Sprintf("t%05d", i) }
	c.Tables = append(c.Tables, table("zz_lookup", []string{"zz_lookup_id", "label"}, []string{"zz_lookup_id"}))
	for i := range n {
		cols := []string{"id", "payload", "lookup_ref"}
		tb := table(name(i), cols, []string{"id"})
		cluster := i / 20
		if i%20 != 0 {
			tb.Columns = append(tb.Columns, schema.Column{Name: "prev", Ordinal: 10, DataType: "integer"})
			tb.ForeignKeys = append(tb.ForeignKeys, schema.ForeignKey{
				Name: name(i) + "_prev", Columns: []string{"prev"}, RefSchema: "s", RefTable: name(i - 1), RefColumns: []string{"id"},
			})
		}
		for k := range 2 {
			j := cluster*20 + rng.IntN(20)
			if j >= n || j == i {
				continue
			}
			col := fmt.Sprintf("r%d", k)
			tb.Columns = append(tb.Columns, schema.Column{Name: col, Ordinal: 20 + k, DataType: "integer"})
			tb.ForeignKeys = append(tb.ForeignKeys, schema.ForeignKey{
				Name: fmt.Sprintf("%s_r%d", name(i), k), Columns: []string{col}, RefSchema: "s", RefTable: name(j), RefColumns: []string{"id"},
			})
		}
		if rng.IntN(25) == 0 {
			j := rng.IntN(n)
			if j != i {
				tb.Columns = append(tb.Columns, schema.Column{Name: "x", Ordinal: 30, DataType: "integer"})
				tb.ForeignKeys = append(tb.ForeignKeys, schema.ForeignKey{
					Name: name(i) + "_x", Columns: []string{"x"}, RefSchema: "s", RefTable: name(j), RefColumns: []string{"id"},
				})
			}
		}
		if i%7 == 0 {
			tb.ForeignKeys = append(tb.ForeignKeys, schema.ForeignKey{
				Name: name(i) + "_lk", Columns: []string{"lookup_ref"}, RefSchema: "s", RefTable: "zz_lookup", RefColumns: []string{"zz_lookup_id"},
			})
		}
		c.Tables = append(c.Tables, tb)
	}
	for i := range n / 100 {
		c.Tables = append(c.Tables, table(fmt.Sprintf("orphan%03d", i), []string{"id"}, []string{"id"}))
	}
	return c
}

func TestLargeCatalogPerformance(t *testing.T) {
	c := synthetic(2000, 1)
	start := time.Now()
	r := mustSegment(t, c, DefaultOptions())
	elapsed := time.Since(start)
	t.Logf("2000 tables: %d communities, Q=%.4f, %v", len(r.Communities), r.Modularity, elapsed)
	if elapsed > 2*time.Second {
		t.Errorf("segmentation took %v, want < 2s", elapsed)
	}
	if len(r.Communities) < 50 {
		t.Errorf("expected roughly one community per cluster, got %d", len(r.Communities))
	}
	if a, _ := assignment(t, r, "s.zz_lookup"); a.Role != RoleLookup || !a.Shared {
		t.Errorf("synthetic lookup: %+v", a)
	}
}

func BenchmarkSegment2000(b *testing.B) {
	c := synthetic(2000, 1)
	for b.Loop() {
		if _, err := Segment(c, DefaultOptions()); err != nil {
			b.Fatal(err)
		}
	}
}

func TestPagila(t *testing.T) {
	c := loadPagila(t)
	r := mustSegment(t, c, DefaultOptions())
	t.Log("Pagila, default options:\n" + dump(r))

	pay, _ := assignment(t, r, "public.payment")
	if pay.Role == RoleOrphan || slices.Contains(r.Orphans, "public.payment") {
		t.Errorf("payment must not be an orphan: %+v", pay)
	}
	var payRefs []string
	for _, e := range r.InferredEdges {
		if e.From == "public.payment" {
			payRefs = append(payRefs, e.To)
		}
	}
	if !slices.Equal(payRefs, []string{"public.customer", "public.rental", "public.staff"}) {
		t.Errorf("payment inferred refs = %v", payRefs)
	}
	_, film := assignment(t, r, "public.film")
	for _, j := range []string{"public.film_actor", "public.film_category"} {
		a, comm := assignment(t, r, j)
		if a.Role != RoleJunction || comm != film {
			t.Errorf("%s: role %s community %d, want junction in film's %d", j, a.Role, comm, film)
		}
	}
	if a, _ := assignment(t, r, "public.language"); a.Role != RoleLookup {
		t.Errorf("language role = %s, want lookup", a.Role)
	}
	if len(r.Communities) < 3 {
		t.Errorf("want ≥ 3 communities, got %d", len(r.Communities))
	}
	if len(r.Orphans) != 0 {
		t.Errorf("orphans = %v", r.Orphans)
	}
	if r.Modularity <= 0.3 {
		t.Errorf("modularity %v unexpectedly low", r.Modularity)
	}

	finer := DefaultOptions()
	finer.Resolution = 1.5
	t.Log("Pagila, resolution 1.5:\n" + dump(mustSegment(t, c, finer)))
}
