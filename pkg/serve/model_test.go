package serve

import (
	"slices"
	"strings"
	"testing"

	"github.com/Gabrielmmorais/kabr/pkg/plan"
)

func mustResolve(t *testing.T, cfg *Config, hashKey []byte) (*model, []string) {
	t.Helper()
	m, warnings, err := resolve(cfg, testCatalog(), hashKey, cfg.EffectiveRowLimit())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return m, warnings
}

func columnNames(t *table) []string {
	out := make([]string, len(t.columns))
	for i := range t.columns {
		out[i] = t.columns[i].name
	}
	return out
}

func TestResolveAllowlistAndExclusions(t *testing.T) {
	m, warnings := mustResolve(t, catalogConfig(), unitHashKey)
	var served []string
	for _, tb := range m.tables {
		served = append(served, tb.id)
	}
	want := []string{"public.customer", "public.film", "public.rating_info", "public.rental_event", `public.we"ird`}
	if !slices.Equal(served, want) {
		t.Errorf("served = %v, want %v", served, want)
	}
	if !slices.Equal(m.excluded, []string{"public.staging", "public.doc"}) {
		t.Errorf("excluded = %v", m.excluded)
	}
	joined := strings.Join(warnings, "\n")
	for _, w := range []string{"public.staging is not served: it has no primary key", `public.doc is not served: primary key column "body" has type jsonb`} {
		if !strings.Contains(joined, w) {
			t.Errorf("missing warning %q in:\n%s", w, joined)
		}
	}
	if m.table("public", "staging") != nil || m.table("public", "nope") != nil || m.table("pg_catalog", "pg_authid") != nil {
		t.Error("lookup outside the allowlist succeeded")
	}
	if m.table("public", `we"ird`) == nil {
		t.Error("allowlisted table with a quote not found")
	}
}

func TestTableLookupIsExact(t *testing.T) {
	cat := testCatalog()
	cat.Tables = append(cat.Tables, tbl("public", "x.y", []string{"id"}, col("id", "integer", false)))
	cfg := catalogConfig()
	cfg.Tables = append(cfg.Tables, "public.x.y")
	m, _, err := resolve(cfg, cat, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if m.table("public", "x.y") == nil {
		t.Fatal("table with a dot in its name not found")
	}
	if m.table("public.x", "y") != nil {
		t.Error("schema \"public.x\" + table \"y\" aliased table public.\"x.y\"")
	}
}

func TestResolveSensitiveColumns(t *testing.T) {
	m, _ := mustResolve(t, catalogConfig(), unitHashKey)
	cust := m.table("public", "customer")
	if got := columnNames(cust); !slices.Equal(got, []string{"customer_id", "email", "card"}) {
		t.Fatalf("customer columns = %v (credential and drop must be absent)", got)
	}
	if c := cust.column("email"); c.treat != treatMask || !c.sensitive || c.filterable() {
		t.Errorf("email: %+v", *c)
	}
	if c := cust.column("card"); c.treat != treatHash || !c.sensitive || c.filterable() {
		t.Errorf("card: %+v", *c)
	}
	if c := cust.column("customer_id"); c.treat != treatPlain || c.sensitive || !c.filterable() {
		t.Errorf("customer_id: %+v", *c)
	}

	// Without a hash key, hashed columns are dropped with a warning.
	m, warnings := mustResolve(t, catalogConfig(), nil)
	if got := columnNames(m.table("public", "customer")); !slices.Equal(got, []string{"customer_id", "email"}) {
		t.Errorf("customer columns without hash key = %v", got)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "column public.customer.card (action hash) is dropped: no hash key configured") {
		t.Errorf("warnings: %v", warnings)
	}

	// A credential is dropped whatever its action.
	for _, action := range []plan.Action{plan.ActionMask, plan.ActionHash, plan.ActionDrop} {
		cfg := catalogConfig()
		cfg.SensitiveColumns = []plan.SensitiveColumn{{Table: "public.film", Column: "title", Class: plan.ClassCredential, Action: action}}
		m, _ := mustResolve(t, cfg, unitHashKey)
		if m.table("public", "film").column("title") != nil {
			t.Errorf("credential with action %s exposed", action)
		}
	}
}

func TestResolveFilterability(t *testing.T) {
	m, _ := mustResolve(t, catalogConfig(), unitHashKey)
	film := m.table("public", "film")
	for name, want := range map[string]bool{
		"film_id": true, "title": true, "rating": true, "rental_rate": true, "ratio": true, "active": true,
		"last_update": true, "released_on": true, "uid": true,
		"poster": false, "features": false, "meta": false,
		"limit": false, // reserved query parameter name
	} {
		if got := film.column(name).filterable(); got != want {
			t.Errorf("%s filterable = %v, want %v", name, got, want)
		}
	}
	if re := m.table("public", "rental_event"); len(re.pk) != 3 || re.columns[re.pk[1]].name != "happened_at" {
		t.Errorf("composite key = %v", re.pk)
	}
}

func TestResolveErrors(t *testing.T) {
	cfg := catalogConfig()
	cfg.Tables = append(cfg.Tables, "public.ghost")
	if _, _, err := resolve(cfg, testCatalog(), nil, 100); err == nil || !strings.Contains(err.Error(), "table public.ghost not found") {
		t.Errorf("missing table: %v", err)
	}
	cfg = catalogConfig()
	cfg.SensitiveColumns = append(cfg.SensitiveColumns, plan.SensitiveColumn{
		Table: "public.film", Column: "secret", Class: plan.ClassPII, Action: plan.ActionMask})
	if _, _, err := resolve(cfg, testCatalog(), nil, 100); err == nil || !strings.Contains(err.Error(), "sensitive column public.film.secret not found") {
		t.Errorf("missing sensitive column: %v", err)
	}
	cfg = catalogConfig()
	cfg.Tables, cfg.SensitiveColumns = []string{"public.staging", "public.doc"}, nil
	if _, _, err := resolve(cfg, testCatalog(), nil, 100); err == nil || !strings.Contains(err.Error(), "no servable table") {
		t.Errorf("nothing servable: %v", err)
	}
	cfg = catalogConfig()
	cfg.Tables = []string{"public.rating_info"}
	cfg.SensitiveColumns = []plan.SensitiveColumn{{Table: "public.rating_info", Column: "rating", Class: plan.ClassOther, Action: plan.ActionMask}}
	_, warnings, err := resolve(cfg, testCatalog(), nil, 100)
	if err == nil || !strings.Contains(strings.Join(warnings, " "), `primary key column "rating" is sensitive`) {
		t.Errorf("sensitive key: err %v, warnings %v", err, warnings)
	}
}
