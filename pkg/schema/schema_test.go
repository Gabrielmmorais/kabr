package schema

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

// fixture returns a small valid catalog with a composite FK, a
// self-reference, a nullable FK and a comment, deliberately unsorted.
func fixture() *Catalog {
	return &Catalog{
		SchemaVersion: SchemaVersion,
		Dialect:       DialectPostgres,
		Database:      "shop",
		Schemas:       []string{"public", "audit", "public"},
		Tables: []Table{
			{
				Schema: "public", Name: "order_line",
				Columns: []Column{
					{Name: "product_id", Ordinal: 3, DataType: "integer"},
					{Name: "order_id", Ordinal: 1, DataType: "integer"},
					{Name: "line_no", Ordinal: 2, DataType: "integer"},
				},
				PrimaryKey: &PrimaryKey{Name: "order_line_pkey", Columns: []string{"order_id", "line_no"}},
				ForeignKeys: []ForeignKey{
					{Name: "order_line_product_fkey", Columns: []string{"product_id"}, RefSchema: "public", RefTable: "product", RefColumns: []string{"id"}},
					{Name: "order_line_order_fkey", Columns: []string{"order_id"}, RefSchema: "public", RefTable: "orders", RefColumns: []string{"id"}},
				},
			},
			{
				Schema: "public", Name: "product", Comment: "Catalog items",
				Columns: []Column{
					{Name: "id", Ordinal: 1, DataType: "integer", Stats: ColumnStats{NDistinct: ptr(-1.0)}},
					{Name: "parent_id", Ordinal: 2, DataType: "integer", Nullable: true, Default: ptr("NULL")},
				},
				PrimaryKey:  &PrimaryKey{Name: "product_pkey", Columns: []string{"id"}},
				ForeignKeys: []ForeignKey{{Name: "product_parent_fkey", Columns: []string{"parent_id"}, RefSchema: "public", RefTable: "product", RefColumns: []string{"id"}, OnDelete: "SET NULL"}},
				Indexes: []Index{
					{Name: "product_pkey", Columns: []string{"id"}, Unique: true, Primary: true, Method: "btree"},
					{Name: "product_lower_idx", Columns: []string{"lower(name)"}, Method: "btree"},
				},
				Stats: TableStats{EstimatedRows: ptr(int64(42))},
			},
			{
				Schema: "public", Name: "orders",
				Columns:           []Column{{Name: "id", Ordinal: 1, DataType: "integer"}, {Name: "code", Ordinal: 2, DataType: "text"}},
				PrimaryKey:        &PrimaryKey{Name: "orders_pkey", Columns: []string{"id"}},
				UniqueConstraints: []UniqueConstraint{{Name: "orders_code_key", Columns: []string{"code"}}},
			},
			{
				Schema: "audit", Name: "shipment",
				Columns: []Column{{Name: "order_id", Ordinal: 1, DataType: "integer"}, {Name: "line_no", Ordinal: 2, DataType: "integer"}},
				ForeignKeys: []ForeignKey{{
					Name: "shipment_line_fkey", Columns: []string{"order_id", "line_no"},
					RefSchema: "public", RefTable: "order_line", RefColumns: []string{"order_id", "line_no"},
				}},
			},
		},
	}
}

func TestTableID(t *testing.T) {
	if got := TableID("public", "users"); got != "public.users" {
		t.Fatalf("TableID = %q", got)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Catalog)
		wantErr []string // substrings; empty means valid
	}{
		{name: "valid fixture", mutate: func(*Catalog) {}},
		{name: "external FK reference is allowed", mutate: func(c *Catalog) {
			c.Tables[0].ForeignKeys[0].RefSchema = "other"
		}},
		{name: "expression index entry is allowed", mutate: func(*Catalog) {}},
		{name: "unsupported version", mutate: func(c *Catalog) { c.SchemaVersion = "kabr.dev/v9" }, wantErr: []string{"unsupported schemaVersion"}},
		{name: "unsupported dialect", mutate: func(c *Catalog) { c.Dialect = "oracle" }, wantErr: []string{"unsupported dialect"}},
		{name: "empty database", mutate: func(c *Catalog) { c.Database = " " }, wantErr: []string{"database name is empty"}},
		{name: "empty table name", mutate: func(c *Catalog) { c.Tables[2].Name = "" }, wantErr: []string{"schema and name must not be empty"}},
		{name: "duplicate table", mutate: func(c *Catalog) { c.Tables = append(c.Tables, c.Tables[2]) }, wantErr: []string{"duplicate table"}},
		{name: "empty column name", mutate: func(c *Catalog) { c.Tables[2].Columns[1].Name = "" }, wantErr: []string{"name is empty", "unknown column \"code\""}},
		{name: "duplicate column", mutate: func(c *Catalog) { c.Tables[2].Columns[1].Name = "id" }, wantErr: []string{"duplicate column \"id\""}},
		{name: "empty data type", mutate: func(c *Catalog) { c.Tables[2].Columns[0].DataType = "" }, wantErr: []string{"dataType is empty"}},
		{name: "pk unknown column", mutate: func(c *Catalog) { c.Tables[2].PrimaryKey.Columns = []string{"nope"} }, wantErr: []string{"primary key \"orders_pkey\" references unknown column \"nope\""}},
		{name: "empty pk name", mutate: func(c *Catalog) { c.Tables[2].PrimaryKey.Name = "" }, wantErr: []string{"primary key name is empty"}},
		{name: "unique unknown column", mutate: func(c *Catalog) { c.Tables[2].UniqueConstraints[0].Columns = []string{"x"} }, wantErr: []string{"unique constraint \"orders_code_key\" references unknown column"}},
		{name: "index without columns", mutate: func(c *Catalog) { c.Tables[1].Indexes[1].Columns = nil }, wantErr: []string{"has no columns"}},
		{name: "fk arity mismatch", mutate: func(c *Catalog) {
			c.Tables[3].ForeignKeys[0].RefColumns = []string{"order_id"}
		}, wantErr: []string{"2 columns but 1 refColumns"}},
		{name: "fk unknown local column", mutate: func(c *Catalog) {
			c.Tables[0].ForeignKeys[0].Columns = []string{"ghost"}
		}, wantErr: []string{"references unknown column \"ghost\""}},
		{name: "fk unknown referenced column", mutate: func(c *Catalog) {
			c.Tables[0].ForeignKeys[0].RefColumns = []string{"ghost"}
		}, wantErr: []string{"references unknown column \"ghost\" in public.product"}},
		{name: "fk empty name and ref", mutate: func(c *Catalog) {
			c.Tables[0].ForeignKeys[0].Name = ""
			c.Tables[0].ForeignKeys[0].RefTable = ""
		}, wantErr: []string{"foreignKeys[0]: name is empty", "refSchema and refTable must not be empty"}},
		{name: "fk without columns", mutate: func(c *Catalog) {
			c.Tables[0].ForeignKeys[0].Columns = nil
			c.Tables[0].ForeignKeys[0].RefColumns = nil
		}, wantErr: []string{"has no columns"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fixture()
			tt.mutate(c)
			err := c.Validate()
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErr)
			}
			for _, w := range tt.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
		})
	}
	var nilCat *Catalog
	if nilCat.Validate() == nil {
		t.Error("nil catalog should not validate")
	}
}

func TestNormalize(t *testing.T) {
	c := fixture()
	c.Normalize()

	var ids []string
	for _, tb := range c.Tables {
		ids = append(ids, tb.ID())
	}
	wantIDs := []string{"audit.shipment", "public.order_line", "public.orders", "public.product"}
	if !reflect.DeepEqual(ids, wantIDs) {
		t.Errorf("table order = %v, want %v", ids, wantIDs)
	}
	if want := []string{"audit", "public"}; !reflect.DeepEqual(c.Schemas, want) {
		t.Errorf("schemas = %v, want %v", c.Schemas, want)
	}
	ol := c.Table("public", "order_line")
	var cols []string
	for _, col := range ol.Columns {
		cols = append(cols, col.Name)
	}
	if want := []string{"order_id", "line_no", "product_id"}; !reflect.DeepEqual(cols, want) {
		t.Errorf("column order = %v, want %v", cols, want)
	}
	if ol.ForeignKeys[0].Name != "order_line_order_fkey" {
		t.Errorf("fk order: first = %q", ol.ForeignKeys[0].Name)
	}
	if got := c.Table("public", "product").Indexes[0].Name; got != "product_lower_idx" {
		t.Errorf("index order: first = %q", got)
	}
	// Composite FK column order must be preserved.
	if got := c.Table("audit", "shipment").ForeignKeys[0].Columns; !reflect.DeepEqual(got, []string{"order_id", "line_no"}) {
		t.Errorf("composite fk columns reordered: %v", got)
	}

	// Idempotent.
	again := c.Clone()
	again.Normalize()
	if !reflect.DeepEqual(c, again) {
		t.Error("Normalize is not idempotent")
	}

	// Nil slices become empty.
	empty := &Catalog{Tables: []Table{{Schema: "s", Name: "t"}}}
	empty.Normalize()
	if empty.Tables[0].Columns == nil {
		t.Error("nil Columns should become empty slice")
	}
	var nilCat *Catalog
	nilCat.Normalize() // must not panic
}

func TestCloneIsDeep(t *testing.T) {
	c := fixture()
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	c.IntrospectedAt = &ts
	cl := c.Clone()
	if !reflect.DeepEqual(c, cl) {
		t.Fatal("clone differs from original")
	}
	cl.Tables[1].Columns[0].Name = "changed"
	*cl.Tables[1].Stats.EstimatedRows = 7
	*cl.Tables[1].Columns[1].Default = "changed"
	cl.Tables[3].ForeignKeys[0].Columns[0] = "changed"
	cl.Tables[0].PrimaryKey.Columns[0] = "changed"
	*cl.IntrospectedAt = time.Time{}
	if c.Tables[1].Columns[0].Name == "changed" || *c.Tables[1].Stats.EstimatedRows != 42 ||
		*c.Tables[1].Columns[1].Default != "NULL" || c.Tables[3].ForeignKeys[0].Columns[0] == "changed" ||
		c.Tables[0].PrimaryKey.Columns[0] == "changed" || !c.IntrospectedAt.Equal(ts) {
		t.Error("clone shares memory with original")
	}
	var nilCat *Catalog
	if nilCat.Clone() != nil {
		t.Error("Clone(nil) should be nil")
	}
}

func TestJSONRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		at   *time.Time
	}{
		{name: "without introspectedAt"},
		{name: "with introspectedAt", at: ptr(time.Date(2026, 11, 23, 14, 0, 0, 0, time.FixedZone("BRT", -3*3600)))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fixture()
			c.IntrospectedAt = tt.at
			orig := c.Clone()

			data, err := Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(c, orig) {
				t.Error("Marshal modified its input")
			}
			if !bytes.HasSuffix(data, []byte("}\n")) || !bytes.Contains(data, []byte("\n  \"dialect\": \"postgres\"")) {
				t.Errorf("unexpected formatting:\n%s", data)
			}
			if tt.at == nil && bytes.Contains(data, []byte("introspectedAt")) {
				t.Error("nil introspectedAt should be omitted")
			}
			if bytes.Contains(data, []byte(`"stats": {}`)) {
				t.Error("empty stats should be omitted")
			}

			back, err := Unmarshal(data)
			if err != nil {
				t.Fatal(err)
			}
			want := orig.Clone()
			want.Normalize()
			if !reflect.DeepEqual(back, want) {
				t.Errorf("round trip mismatch\n got: %#v\nwant: %#v", back, want)
			}
			data2, err := Marshal(back)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(data, data2) {
				t.Errorf("marshal not stable:\n%s\n---\n%s", data, data2)
			}
		})
	}
}

func TestMarshalDeterministicAcrossInputOrder(t *testing.T) {
	a := fixture()
	b := fixture()
	for i, j := 0, len(b.Tables)-1; i < j; i, j = i+1, j-1 {
		b.Tables[i], b.Tables[j] = b.Tables[j], b.Tables[i]
	}
	da, _ := Marshal(a)
	db, _ := Marshal(b)
	if !bytes.Equal(da, db) {
		t.Error("output depends on input table order")
	}
}

func TestUnmarshalErrors(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"malformed", `{`, "unmarshal catalog"},
		{"unknown field", `{"schemaVersion":"kabr.dev/v1alpha1","dialect":"postgres","database":"x","tables":[],"bogus":1}`, "unknown field"},
		{"trailing data", `{"schemaVersion":"kabr.dev/v1alpha1","dialect":"postgres","database":"x","tables":[]} {}`, "trailing data"},
		{"invalid version", `{"schemaVersion":"v0","dialect":"postgres","database":"x","tables":[]}`, "unsupported schemaVersion"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Unmarshal([]byte(tt.in))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
	if _, err := Marshal(nil); err == nil {
		t.Error("Marshal(nil) should fail")
	}
}

func TestIsUniqueKey(t *testing.T) {
	c := fixture()
	tests := []struct {
		table string
		cols  []string
		want  bool
	}{
		{"order_line", []string{"line_no", "order_id"}, true}, // PK, order-insensitive
		{"order_line", []string{"order_id"}, false},
		{"orders", []string{"code"}, true}, // unique constraint
		{"product", []string{"id"}, true},  // PK
		{"product", []string{"parent_id"}, false},
		{"product", nil, false},
	}
	for _, tt := range tests {
		if got := c.Table("public", tt.table).IsUniqueKey(tt.cols); got != tt.want {
			t.Errorf("%s.IsUniqueKey(%v) = %v, want %v", tt.table, tt.cols, got, tt.want)
		}
	}
	if New(DialectPostgres, "db").Validate() != nil {
		t.Error("New() catalog should be valid")
	}
}
