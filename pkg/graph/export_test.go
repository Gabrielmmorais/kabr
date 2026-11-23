package graph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run `go test ./pkg/graph -update` to create): %v", err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch (run `go test ./pkg/graph -update` if intended)\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func TestGolden(t *testing.T) {
	c := loadFixture(t)
	tests := []struct {
		file   string
		render func(*schema.Catalog) string
	}{
		{"catalog.mermaid.golden", Mermaid},
		{"catalog.dot.golden", DOT},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			before, _ := schema.Marshal(c)
			got := tt.render(c)
			after, _ := schema.Marshal(c)
			if string(before) != string(after) {
				t.Error("exporter modified the catalog")
			}
			// Determinism: shuffled input must render identically.
			shuffled := c.Clone()
			for i, j := 0, len(shuffled.Tables)-1; i < j; i, j = i+1, j-1 {
				shuffled.Tables[i], shuffled.Tables[j] = shuffled.Tables[j], shuffled.Tables[i]
			}
			if again := tt.render(shuffled); again != got {
				t.Error("output depends on input order")
			}
			golden(t, tt.file, got)
		})
	}
}

func TestMermaidCardinality(t *testing.T) {
	parent := schema.Table{
		Schema: "s", Name: "parent",
		Columns:    []schema.Column{{Name: "id", Ordinal: 1, DataType: "integer"}},
		PrimaryKey: &schema.PrimaryKey{Name: "parent_pkey", Columns: []string{"id"}},
	}
	child := func(nullable, unique bool) schema.Table {
		t := schema.Table{
			Schema: "s", Name: "child",
			Columns: []schema.Column{
				{Name: "id", Ordinal: 1, DataType: "integer"},
				{Name: "parent_id", Ordinal: 2, DataType: "integer", Nullable: nullable},
			},
			PrimaryKey:  &schema.PrimaryKey{Name: "child_pkey", Columns: []string{"id"}},
			ForeignKeys: []schema.ForeignKey{{Name: "child_parent_fkey", Columns: []string{"parent_id"}, RefSchema: "s", RefTable: "parent", RefColumns: []string{"id"}}},
		}
		if unique {
			t.UniqueConstraints = []schema.UniqueConstraint{{Name: "child_parent_key", Columns: []string{"parent_id"}}}
		}
		return t
	}
	tests := []struct {
		name             string
		nullable, unique bool
		want             string
	}{
		{"not null one-to-many", false, false, `parent ||--o{ child : "child_parent_fkey"`},
		{"nullable one-to-many", true, false, `parent |o--o{ child : "child_parent_fkey"`},
		{"not null one-to-one", false, true, `parent ||--o| child : "child_parent_fkey"`},
		{"nullable one-to-one", true, true, `parent |o--o| child : "child_parent_fkey"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := Mermaid(cat(parent, child(tt.nullable, tt.unique)))
			if !strings.Contains(out, tt.want) {
				t.Errorf("missing %q in:\n%s", tt.want, out)
			}
		})
	}
}

func TestMermaidType(t *testing.T) {
	tests := []struct{ in, want string }{
		{"character varying(45)", "varchar_45_"},
		{"character varying", "varchar"},
		{"numeric(10,2)", "numeric_10_2_"},
		{"integer[]", "integer_array"},
		{"timestamp with time zone", "timestamptz"},
		{"timestamp(3) without time zone", "timestamp_3_without_time_zone"},
		{"double precision", "float8"},
		{"USER-DEFINED", "user_defined"},
		{`"MyType"`, "mytype_"},
		{"", "unknown"},
		{"(", "unknown"},
		{"2d", "t_2d"},
	}
	for _, tt := range tests {
		if got := MermaidType(tt.in); got != tt.want {
			t.Errorf("MermaidType(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestMermaidIdent(t *testing.T) {
	tests := []struct{ in, want string }{
		{"users", "users"},
		{"order-items", "order_items"},
		{"my table", "my_table"},
		{"2fa", "_2fa"},
		{"", "_"},
		{"public.users", "public_users"},
	}
	for _, tt := range tests {
		if got := MermaidIdent(tt.in); got != tt.want {
			t.Errorf("MermaidIdent(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestMermaidEntityNames(t *testing.T) {
	// Single schema: unqualified. Collision after sanitizing gets a suffix.
	out := Mermaid(cat(tbl("s", "a-b"), tbl("s", "a_b")))
	if !strings.Contains(out, "    a_b {") || !strings.Contains(out, "    a_b_2 {") {
		t.Errorf("unexpected entity names:\n%s", out)
	}
	// Columnless tables render without braces.
	empty := schema.New(schema.DialectPostgres, "db")
	empty.Tables = []schema.Table{{Schema: "s", Name: "t"}}
	if got := Mermaid(empty); got != "erDiagram\n    t\n" {
		t.Errorf("columnless table: %q", got)
	}
	if got := Mermaid(nil); got != "erDiagram\n" {
		t.Errorf("nil catalog: %q", got)
	}
}

func TestDOTQuotingAndExternal(t *testing.T) {
	c := cat(
		tbl("s", `we"ird\name`, fk("fk \"q\"", `we"ird\name`)),
		tbl("s", "a", fk("a_ext", "missing")),
	)
	out := DOT(c)
	for _, want := range []string{
		`"s.we\"ird\\name" [label="s.we\"ird\\name"];`,
		`"s.we\"ird\\name" -> "s.we\"ird\\name" [label="fk \"q\""];`,
		`// external reference s.a -> s.missing : a_ext`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if got := DOT(nil); got != "digraph kabr {\n    rankdir=LR;\n    node [shape=box];\n}\n" {
		t.Errorf("nil catalog: %q", got)
	}
	if !strings.Contains(Mermaid(c), "%% external reference s.a -> s.missing : a_ext") {
		t.Error("mermaid external reference comment missing")
	}
}
