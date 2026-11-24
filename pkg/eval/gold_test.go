package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pagilaGold = "../../evals/pagila/gold.yaml"

func TestLoadGoldPagila(t *testing.T) {
	t.Parallel()
	g, err := LoadGold(pagilaGold)
	if err != nil {
		t.Fatal(err)
	}
	if g.Name != "pagila" || g.Catalog != "examples/pagila.schema.json" {
		t.Errorf("name/catalog = %q/%q", g.Name, g.Catalog)
	}
	a := g.Assignment()
	if len(a) != 15 {
		t.Errorf("assignment has %d tables, want 15", len(a))
	}
	want := map[string]int{"catalog": 6, "customers": 4, "rentals": 3, "stores": 2}
	for label, n := range want {
		if len(g.Domains[label]) != n {
			t.Errorf("domain %q has %d tables, want %d", label, len(g.Domains[label]), n)
		}
	}
	if len(g.Sensitive) != 11 {
		t.Errorf("sensitive has %d entries, want 11", len(g.Sensitive))
	}
	if a["public.film_actor"] != "catalog" || a["public.payment"] != "rentals" {
		t.Errorf("unexpected assignment: %v", a)
	}
}

// TestPagilaGoldMatchesCatalog guards against typos: every gold table and
// sensitive column must exist in the referenced catalog.
func TestPagilaGoldMatchesCatalog(t *testing.T) {
	t.Parallel()
	g, err := LoadGold(pagilaGold)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(g.Catalog)))
	if err != nil {
		t.Fatal(err)
	}
	var cat struct {
		Tables []struct {
			Schema  string `json:"schema"`
			Name    string `json:"name"`
			Columns []struct {
				Name string `json:"name"`
			} `json:"columns"`
		} `json:"tables"`
	}
	if err := json.Unmarshal(data, &cat); err != nil {
		t.Fatal(err)
	}
	cols := map[string]map[string]bool{}
	for _, tb := range cat.Tables {
		id := tb.Schema + "." + tb.Name
		cols[id] = map[string]bool{}
		for _, c := range tb.Columns {
			cols[id][c.Name] = true
		}
	}
	a := g.Assignment()
	for id := range a {
		if cols[id] == nil {
			t.Errorf("gold table %q not in catalog", id)
		}
	}
	for id := range cols {
		if _, ok := a[id]; !ok {
			t.Errorf("catalog table %q not covered by gold", id)
		}
	}
	for _, s := range g.Sensitive {
		if !cols[s.Table][s.Column] {
			t.Errorf("gold sensitive column %s.%s not in catalog", s.Table, s.Column)
		}
	}
}

func TestLoadGoldInvalid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, yaml, wantErr string
	}{
		{
			name:    "duplicate table across domains",
			yaml:    "name: x\ndomains:\n  a: [public.t1]\n  b: [public.t1]\n",
			wantErr: `table "public.t1" is in domains "a" and "b"`,
		},
		{
			name:    "duplicate table within a domain",
			yaml:    "name: x\ndomains:\n  a: [public.t1, public.t1]\n",
			wantErr: `table "public.t1"`,
		},
		{
			name:    "unknown field",
			yaml:    "name: x\nbogus: 1\ndomains:\n  a: [public.t1]\n",
			wantErr: "bogus",
		},
		{
			name:    "unknown sensitive field",
			yaml:    "name: x\ndomains:\n  a: [public.t1]\nsensitive:\n  - {table: public.t1, column: c, class: pii, why: x}\n",
			wantErr: "why",
		},
		{
			name:    "no domains",
			yaml:    "name: x\n",
			wantErr: "no domains",
		},
		{
			name:    "empty domain",
			yaml:    "name: x\ndomains:\n  a: []\n",
			wantErr: `domain "a" has no tables`,
		},
		{
			name:    "missing name",
			yaml:    "domains:\n  a: [public.t1]\n",
			wantErr: "name is empty",
		},
		{
			name:    "bad class",
			yaml:    "name: x\ndomains:\n  a: [public.t1]\nsensitive:\n  - {table: public.t1, column: c, class: secret}\n",
			wantErr: `unknown class "secret"`,
		},
		{
			name:    "sensitive table outside gold",
			yaml:    "name: x\ndomains:\n  a: [public.t1]\nsensitive:\n  - {table: public.t2, column: c, class: pii}\n",
			wantErr: `table "public.t2" is not in any domain`,
		},
		{
			name:    "duplicate sensitive",
			yaml:    "name: x\ndomains:\n  a: [public.t1]\nsensitive:\n  - {table: public.t1, column: c, class: pii}\n  - {table: public.t1, column: c, class: other}\n",
			wantErr: "duplicate column",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "gold.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadGold(path)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("LoadGold error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadGoldMissingFile(t *testing.T) {
	t.Parallel()
	if _, err := LoadGold(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected error for missing file")
	}
}
