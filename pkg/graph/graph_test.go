package graph

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata/")

func loadFixture(t *testing.T) *schema.Catalog {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := schema.Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// tbl builds a table whose FKs reference "id" columns.
func tbl(schemaName, name string, fks ...schema.ForeignKey) schema.Table {
	t := schema.Table{
		Schema: schemaName, Name: name,
		Columns:     []schema.Column{{Name: "id", Ordinal: 1, DataType: "integer"}},
		ForeignKeys: fks,
	}
	return t
}

func fk(name, refTable string) schema.ForeignKey {
	return schema.ForeignKey{Name: name, Columns: []string{"id"}, RefSchema: "s", RefTable: refTable, RefColumns: []string{"id"}}
}

func cat(tables ...schema.Table) *schema.Catalog {
	c := schema.New(schema.DialectPostgres, "db")
	c.Tables = tables
	return c
}

func TestBuildCases(t *testing.T) {
	tests := []struct {
		name       string
		catalog    *schema.Catalog
		wantEdges  []Edge
		wantComps  [][]string
		wantOrphan []string
		wantExt    int
	}{
		{
			name:       "nil catalog",
			catalog:    nil,
			wantComps:  nil,
			wantOrphan: []string{},
		},
		{
			name:       "two FKs between the same pair give weight 2",
			catalog:    cat(tbl("s", "b", fk("b_x", "a"), fk("b_y", "a")), tbl("s", "a")),
			wantEdges:  []Edge{{From: "s.b", To: "s.a", Weight: 2, ForeignKeys: []string{"b_x", "b_y"}}},
			wantComps:  [][]string{{"s.a", "s.b"}},
			wantOrphan: []string{},
		},
		{
			name:       "self reference only is an orphan",
			catalog:    cat(tbl("s", "emp", fk("emp_mgr", "emp"))),
			wantEdges:  []Edge{{From: "s.emp", To: "s.emp", Weight: 1, ForeignKeys: []string{"emp_mgr"}, SelfReference: true}},
			wantComps:  [][]string{{"s.emp"}},
			wantOrphan: []string{"s.emp"},
		},
		{
			name: "two components and an orphan",
			catalog: cat(
				tbl("s", "a"), tbl("s", "b", fk("b_a", "a")),
				tbl("s", "c"), tbl("s", "d", fk("d_c", "c")), tbl("s", "e", fk("e_d", "d")),
				tbl("s", "z"),
			),
			wantEdges: []Edge{
				{From: "s.b", To: "s.a", Weight: 1, ForeignKeys: []string{"b_a"}},
				{From: "s.d", To: "s.c", Weight: 1, ForeignKeys: []string{"d_c"}},
				{From: "s.e", To: "s.d", Weight: 1, ForeignKeys: []string{"e_d"}},
			},
			wantComps:  [][]string{{"s.c", "s.d", "s.e"}, {"s.a", "s.b"}, {"s.z"}},
			wantOrphan: []string{"s.z"},
		},
		{
			name:       "external reference is skipped",
			catalog:    cat(tbl("s", "a", fk("a_ext", "missing"))),
			wantComps:  [][]string{{"s.a"}},
			wantOrphan: []string{"s.a"},
			wantExt:    1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := Build(tt.catalog)
			if got := g.Edges(); !reflect.DeepEqual(got, tt.wantEdges) && !(len(got) == 0 && len(tt.wantEdges) == 0) {
				t.Errorf("edges = %+v, want %+v", got, tt.wantEdges)
			}
			if got := g.ConnectedComponents(); !reflect.DeepEqual(got, tt.wantComps) {
				t.Errorf("components = %v, want %v", got, tt.wantComps)
			}
			if got := g.Orphans(); !reflect.DeepEqual(got, tt.wantOrphan) {
				t.Errorf("orphans = %v, want %v", got, tt.wantOrphan)
			}
			if got := g.Metrics().ExternalForeignKeys; got != tt.wantExt {
				t.Errorf("external FKs = %d, want %d", got, tt.wantExt)
			}
		})
	}
}

func TestFixtureGraph(t *testing.T) {
	g := Build(loadFixture(t))

	wantNodes := []string{
		"audit.event_log", "audit.retention_policy",
		"public.assignment", "public.department", "public.employee",
		"public.employee_badge", "public.project", "public.timesheet",
	}
	if got := g.Nodes(); !reflect.DeepEqual(got, wantNodes) {
		t.Fatalf("nodes = %v", got)
	}
	wantEdges := []Edge{
		{From: "audit.retention_policy", To: "audit.retention_policy", Weight: 1, ForeignKeys: []string{"retention_policy_parent_id_fkey"}, SelfReference: true},
		{From: "public.assignment", To: "public.employee", Weight: 1, ForeignKeys: []string{"assignment_employee_id_fkey"}},
		{From: "public.assignment", To: "public.project", Weight: 1, ForeignKeys: []string{"assignment_project_id_fkey"}},
		{From: "public.employee", To: "public.department", Weight: 1, ForeignKeys: []string{"employee_department_id_fkey"}},
		{From: "public.employee", To: "public.employee", Weight: 1, ForeignKeys: []string{"employee_manager_id_fkey"}, SelfReference: true},
		{From: "public.employee_badge", To: "public.employee", Weight: 1, ForeignKeys: []string{"employee_badge_employee_id_fkey"}},
		{From: "public.project", To: "public.department", Weight: 1, ForeignKeys: []string{"project_department_id_fkey"}},
		{From: "public.project", To: "public.employee", Weight: 2, ForeignKeys: []string{"project_owner_id_fkey", "project_reviewer_id_fkey"}},
		// Composite FK (employee_id, project_id) is ONE edge of weight 1.
		{From: "public.timesheet", To: "public.assignment", Weight: 1, ForeignKeys: []string{"timesheet_assignment_fkey"}},
	}
	if got := g.Edges(); !reflect.DeepEqual(got, wantEdges) {
		t.Errorf("edges:\n got %+v\nwant %+v", got, wantEdges)
	}

	degrees := []struct {
		id      string
		in, out int
	}{
		{"public.employee", 5, 2}, // assignment, badge, project x2, self | department, self
		{"public.project", 1, 3},
		{"public.timesheet", 0, 1},
		{"audit.retention_policy", 1, 1},
		{"audit.event_log", 0, 0},
		{"nope.nope", 0, 0},
	}
	for _, d := range degrees {
		if in, out := g.Degree(d.id); in != d.in || out != d.out {
			t.Errorf("Degree(%s) = (%d,%d), want (%d,%d)", d.id, in, out, d.in, d.out)
		}
	}

	wantNb := map[string]float64{
		"public.assignment": 1, "public.department": 1,
		"public.employee_badge": 1, "public.project": 2,
	}
	if got := g.UndirectedNeighbors("public.employee"); !reflect.DeepEqual(got, wantNb) {
		t.Errorf("UndirectedNeighbors(employee) = %v, want %v", got, wantNb)
	}
	if got := g.UndirectedNeighbors("audit.retention_policy"); len(got) != 0 || got == nil {
		t.Errorf("self-loop-only neighbors = %v, want empty non-nil map", got)
	}
	if g.UndirectedNeighbors("nope.nope") != nil {
		t.Error("unknown node should return nil neighbors")
	}

	wantComps := [][]string{
		{"public.assignment", "public.department", "public.employee", "public.employee_badge", "public.project", "public.timesheet"},
		{"audit.event_log"},
		{"audit.retention_policy"},
	}
	if got := g.ConnectedComponents(); !reflect.DeepEqual(got, wantComps) {
		t.Errorf("components = %v", got)
	}
	if got, want := g.Orphans(), []string{"audit.event_log", "audit.retention_policy"}; !reflect.DeepEqual(got, want) {
		t.Errorf("orphans = %v, want %v", got, want)
	}

	wantM := Metrics{
		Nodes: 8, Edges: 9, ForeignKeys: 10, SelfReferences: 2,
		Components: 3, Orphans: 2, MaxDegree: 7, MaxDegreeTable: "public.employee",
	}
	if got := g.Metrics(); got != wantM {
		t.Errorf("metrics = %+v, want %+v", got, wantM)
	}
	if !g.HasNode("public.employee") || g.HasNode("public.nope") {
		t.Error("HasNode mismatch")
	}
}

func TestEdgesReturnsCopies(t *testing.T) {
	g := Build(cat(tbl("s", "b", fk("b_a", "a")), tbl("s", "a")))
	e := g.Edges()
	e[0].ForeignKeys[0] = "mutated"
	n := g.Nodes()
	n[0] = "mutated"
	if g.Edges()[0].ForeignKeys[0] != "b_a" || g.Nodes()[0] != "s.a" {
		t.Error("graph state leaked through returned slices")
	}
}

func TestMetricsEmpty(t *testing.T) {
	if got := Build(nil).Metrics(); got != (Metrics{}) {
		t.Errorf("empty metrics = %+v", got)
	}
}
