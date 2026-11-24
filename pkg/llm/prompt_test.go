package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Gabrielmmorais/kabr/pkg/plan"
	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

// allowedKeys is the complete set of JSON keys the user prompt may contain.
// Adding a key here is a privacy decision: see the package documentation.
var allowedKeys = map[string]bool{
	// document
	"tables": true, "baseline": true,
	// table metadata
	"id": true, "comment": true, "columns": true, "primaryKey": true,
	"foreignKeys": true, "uniqueConstraints": true,
	// column metadata
	"name": true, "type": true, "nullable": true,
	// foreign key
	"references": true, "referencedColumns": true,
	// baseline
	"domains": true, "role": true, "shared": true,
}

// walkKeys calls f for every object key in v, recursively.
func walkKeys(v any, path string, f func(path, key string)) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			f(path, k)
			walkKeys(child, path+"."+k, f)
		}
	case []any:
		for _, child := range x {
			walkKeys(child, path+"[]", f)
		}
	}
}

func decodeUser(t *testing.T, p Prompt) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(p.User), &doc); err != nil {
		t.Fatalf("user prompt is not JSON: %v", err)
	}
	return doc
}

func TestBuildPromptKeysAllowlisted(t *testing.T) {
	tests := []struct {
		name    string
		catalog func(t *testing.T) *schema.Catalog
	}{
		{"pagila", pagila},
		{"sentinel", sentinelCatalog},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.catalog(t)
			p, err := BuildPrompt(c, baselineFor(t, c))
			if err != nil {
				t.Fatalf("BuildPrompt: %v", err)
			}
			seen := 0
			walkKeys(decodeUser(t, p), "$", func(path, key string) {
				seen++
				if !allowedKeys[key] {
					t.Errorf("forbidden key %q at %s", key, path)
				}
			})
			if seen == 0 {
				t.Fatal("no keys walked")
			}
		})
	}
}

// sentinelCatalog plants recognizable values in every forbidden field.
func sentinelCatalog(t *testing.T) *schema.Catalog {
	t.Helper()
	def := "'SENTINEL_DEFAULT_9f3'::text"
	rows := int64(987654321)
	nd := 123456.789
	at := mustTime(t)
	c := schema.New(schema.DialectPostgres, "SENTINEL_DATABASE_77a")
	c.IntrospectedAt = &at
	c.Schemas = []string{"app"}
	c.Tables = []schema.Table{
		{
			Schema:  "app",
			Name:    "users",
			Comment: "application users",
			Columns: []schema.Column{
				{Name: "id", Ordinal: 1, DataType: "integer"},
				{Name: "email", Ordinal: 2, DataType: "text", Comment: "login e-mail"},
				{Name: "status", Ordinal: 3, DataType: "text", Nullable: true, Default: &def,
					Stats: schema.ColumnStats{NDistinct: &nd}},
			},
			PrimaryKey:        &schema.PrimaryKey{Name: "SENTINEL_PKNAME_c1", Columns: []string{"id"}},
			UniqueConstraints: []schema.UniqueConstraint{{Name: "SENTINEL_UQNAME_d4", Columns: []string{"email"}}},
			Indexes: []schema.Index{
				{Name: "SENTINEL_INDEX_e5", Columns: []string{"lower(email)"}, Method: "SENTINEL_METHOD_f6"},
			},
			Stats: schema.TableStats{EstimatedRows: &rows},
		},
		{
			Schema: "app",
			Name:   "orders",
			Columns: []schema.Column{
				{Name: "id", Ordinal: 1, DataType: "integer"},
				{Name: "user_id", Ordinal: 2, DataType: "integer"},
			},
			PrimaryKey: &schema.PrimaryKey{Name: "orders_pkey", Columns: []string{"id"}},
			ForeignKeys: []schema.ForeignKey{{
				Name: "SENTINEL_FKNAME_a2", Columns: []string{"user_id"},
				RefSchema: "app", RefTable: "users", RefColumns: []string{"id"},
				OnDelete: "SENTINEL_ONDELETE", OnUpdate: "SENTINEL_ONUPDATE",
			}},
			Stats: schema.TableStats{EstimatedRows: &rows},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("sentinel catalog invalid: %v", err)
	}
	return c
}

func TestBuildPromptOmitsForbiddenValues(t *testing.T) {
	c := sentinelCatalog(t)
	base := baselineFor(t, c)
	base.Spec.Domains[0].Description = "SENTINEL_DESCRIPTION_b8"
	base.Spec.Domains[0].Rationale = "SENTINEL_RATIONALE_b9"
	base.Spec.Domains[0].SensitiveColumns = []plan.SensitiveColumn{{
		Table: "app.users", Column: "email", Class: plan.ClassPII, Action: plan.ActionMask,
		Rationale: "SENTINEL_SENSITIVE_ba",
	}}
	p, err := BuildPrompt(c, base)
	if err != nil {
		t.Fatalf("BuildPrompt: %v", err)
	}
	forbidden := []string{
		"SENTINEL", "987654321", "123456", "9f3", "2031", // defaults, stats, timestamp
		"introspectedAt", "estimatedRows", "nDistinct", "default", "stats", "indexes",
		"lower(email)", "pagila", "dsn", "fingerprint", "sha256",
	}
	for _, s := range forbidden {
		if strings.Contains(p.User, s) {
			t.Errorf("user prompt contains forbidden %q", s)
		}
		if strings.Contains(p.System, s) && s != "default" {
			t.Errorf("system prompt contains forbidden %q", s)
		}
	}
	// Allowed metadata is present.
	for _, s := range []string{`"app.users"`, `"app.orders"`, `"login e-mail"`, `"application users"`, `"references":"app.users"`} {
		if !strings.Contains(p.User, s) {
			t.Errorf("user prompt lacks %s: %s", s, p.User)
		}
	}
}

func TestBuildPromptSystemConstant(t *testing.T) {
	a := pagila(t)
	pa, err := BuildPrompt(a, baselineFor(t, a))
	if err != nil {
		t.Fatal(err)
	}
	b := sentinelCatalog(t)
	pb, err := BuildPrompt(b, baselineFor(t, b))
	if err != nil {
		t.Fatal(err)
	}
	if pa.System != pb.System || pa.System != systemPrompt {
		t.Fatal("system prompt depends on the catalog")
	}
	for _, s := range []string{"renames", "moves", "sensitiveColumns", "DNS-1123", "lowercase",
		"pii", "credential", "financial", "other", "mask", "drop", "hash", "Never invent", "JSON schema"} {
		if !strings.Contains(systemPrompt, s) {
			t.Errorf("system prompt lacks %q", s)
		}
	}
}

func TestBuildPromptDeterministic(t *testing.T) {
	c := pagila(t)
	base := baselineFor(t, c)
	p1, err := BuildPrompt(c, base)
	if err != nil {
		t.Fatal(err)
	}
	// Reverse table and domain order: output must not change.
	c2 := c.Clone()
	for i, j := 0, len(c2.Tables)-1; i < j; i, j = i+1, j-1 {
		c2.Tables[i], c2.Tables[j] = c2.Tables[j], c2.Tables[i]
	}
	b2 := base.Clone()
	for i, j := 0, len(b2.Spec.Domains)-1; i < j; i, j = i+1, j-1 {
		b2.Spec.Domains[i], b2.Spec.Domains[j] = b2.Spec.Domains[j], b2.Spec.Domains[i]
	}
	p2, err := BuildPrompt(c2, b2)
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Fatal("prompt depends on input order")
	}
	doc := decodeUser(t, p1)
	if n := len(doc["tables"].([]any)); n != len(c.Tables) {
		t.Fatalf("got %d tables, want %d", n, len(c.Tables))
	}
}

func TestBuildPromptNil(t *testing.T) {
	c := pagila(t)
	if _, err := BuildPrompt(nil, plan.New("x", c)); err == nil {
		t.Error("nil catalog: want error")
	}
	if _, err := BuildPrompt(c, nil); err == nil {
		t.Error("nil baseline: want error")
	}
}
