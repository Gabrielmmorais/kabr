package serve

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Gabrielmmorais/kabr/pkg/plan"
)

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(c *Config)
		want   string // substring of the error; "" means valid
	}{
		{"valid", func(*Config) {}, ""},
		{"defaults", func(c *Config) { c.RowLimit, c.StatementTimeout, c.Schemas = 0, 0, nil }, ""},
		{"row limit 1", func(c *Config) { c.RowLimit = 1 }, ""},
		{"row limit max", func(c *Config) { c.RowLimit = MaxRowLimit }, ""},
		{"row limit too high", func(c *Config) { c.RowLimit = MaxRowLimit + 1 }, "rowLimit"},
		{"row limit negative", func(c *Config) { c.RowLimit = -1 }, "rowLimit"},
		{"timeout min", func(c *Config) { c.StatementTimeout = Duration(MinStatementTimeout) }, ""},
		{"timeout max", func(c *Config) { c.StatementTimeout = Duration(MaxStatementTimeout) }, ""},
		{"timeout too short", func(c *Config) { c.StatementTimeout = Duration(99 * time.Millisecond) }, "statementTimeout"},
		{"timeout too long", func(c *Config) { c.StatementTimeout = Duration(61 * time.Second) }, "statementTimeout"},
		{"timeout negative", func(c *Config) { c.StatementTimeout = Duration(-time.Second) }, "statementTimeout"},
		{"bad domain", func(c *Config) { c.Domain = "Film_1" }, "domain"},
		{"empty domain", func(c *Config) { c.Domain = "" }, "domain"},
		{"bad plan", func(c *Config) { c.Plan = "-x" }, "plan"},
		{"bad fingerprint", func(c *Config) { c.CatalogFingerprint = "sha256:abc" }, "catalogFingerprint"},
		{"no fingerprint", func(c *Config) { c.CatalogFingerprint = "" }, "catalogFingerprint"},
		{"no tables", func(c *Config) { c.Tables = nil; c.SensitiveColumns = nil }, "at least one table"},
		{"table without schema", func(c *Config) { c.Tables = append(c.Tables, "film") }, "not a table ID"},
		{"table empty name", func(c *Config) { c.Tables = append(c.Tables, "public.") }, "not a table ID"},
		{"table empty schema", func(c *Config) { c.Tables = append(c.Tables, ".film") }, "not a table ID"},
		{"table with spaces", func(c *Config) { c.Tables = append(c.Tables, " public.x") }, "not a table ID"},
		{"duplicate table", func(c *Config) { c.Tables = append(c.Tables, "public.film") }, "duplicate table"},
		{"table schema not listed", func(c *Config) { c.Tables = append(c.Tables, "sales.order") }, "not listed in schemas"},
		{"duplicate schema", func(c *Config) { c.Schemas = []string{"public", "public"} }, "duplicate schema"},
		{"empty schema", func(c *Config) { c.Schemas = []string{""} }, "schemas[0]"},
		{"sensitive unknown table", func(c *Config) { c.SensitiveColumns[0].Table = "public.other" }, "not listed in tables"},
		{"sensitive no column", func(c *Config) { c.SensitiveColumns[0].Column = " " }, "column: must not be empty"},
		{"sensitive bad class", func(c *Config) { c.SensitiveColumns[0].Class = "secret" }, "invalid class"},
		{"sensitive bad action", func(c *Config) { c.SensitiveColumns[0].Action = "show" }, "invalid action"},
		{"sensitive empty action", func(c *Config) { c.SensitiveColumns[0].Action = "" }, "invalid action"},
		{"sensitive duplicate", func(c *Config) {
			c.SensitiveColumns = append(c.SensitiveColumns, c.SensitiveColumns[0])
		}, "duplicate entry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.mutate(c)
			err := c.Validate()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
	var nilCfg *Config
	if nilCfg.Validate() == nil {
		t.Error("nil config must be invalid")
	}
}

func TestConfigEffectiveValues(t *testing.T) {
	c := validConfig()
	c.RowLimit, c.StatementTimeout, c.Schemas = 0, 0, nil
	c.Tables = []string{"sales.order", "public.film", "sales.item"}
	c.SensitiveColumns = nil
	if c.EffectiveRowLimit() != DefaultRowLimit || c.EffectiveStatementTimeout() != DefaultStatementTimeout {
		t.Errorf("defaults: %d %s", c.EffectiveRowLimit(), c.EffectiveStatementTimeout())
	}
	if got := c.EffectiveSchemas(); !reflect.DeepEqual(got, []string{"public", "sales"}) {
		t.Errorf("schemas = %v", got)
	}
	c.RowLimit, c.StatementTimeout, c.Schemas = 7, Duration(time.Second), []string{"x"}
	if c.EffectiveRowLimit() != 7 || c.EffectiveStatementTimeout() != time.Second || c.EffectiveSchemas()[0] != "x" {
		t.Error("explicit values not returned")
	}
}

func TestDecodeConfig(t *testing.T) {
	good, err := json.Marshal(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(good, []byte(`"statementTimeout":"5s"`)) {
		t.Errorf("duration not encoded as a string: %s", good)
	}
	c, err := DecodeConfig(bytes.NewReader(good))
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if !reflect.DeepEqual(c, validConfig()) {
		t.Errorf("round trip changed the config: %+v", c)
	}

	minimal := `{"domain":"film","plan":"pagila","catalogFingerprint":"` + testFingerprint + `","tables":["public.film"]}`
	if c, err := DecodeConfig(strings.NewReader(minimal)); err != nil || c.EffectiveRowLimit() != 100 {
		t.Fatalf("minimal config: %v", err)
	}
	bad := map[string]string{
		"unknown key":            strings.Replace(minimal, `"plan"`, `"extra":1,"plan"`, 1),
		"case variant":           strings.Replace(minimal, `"domain"`, `"Domain"`, 1),
		"duplicate key":          strings.Replace(minimal, `"domain":"film"`, `"domain":"film","domain":"other"`, 1),
		"sensitive unknown key":  strings.Replace(minimal, `"tables"`, `"sensitiveColumns":[{"table":"public.film","column":"a","class":"pii","action":"mask","x":1}],"tables"`, 1),
		"sensitive case variant": strings.Replace(minimal, `"tables"`, `"sensitiveColumns":[{"Table":"public.film","column":"a","class":"pii","action":"mask"}],"tables"`, 1),
		"sensitive duplicate":    strings.Replace(minimal, `"tables"`, `"sensitiveColumns":[{"table":"public.film","table":"public.film","column":"a","class":"pii","action":"mask"}],"tables"`, 1),
		"nested duplicate":       strings.Replace(minimal, `"tables"`, `"x":{"a":1,"a":2},"tables"`, 1),
		"trailing data":          minimal + `{}`,
		"not an object":          `["film"]`,
		"empty":                  ``,
		"duration number":        strings.Replace(minimal, `"tables"`, `"statementTimeout":5,"tables"`, 1),
		"duration invalid":       strings.Replace(minimal, `"tables"`, `"statementTimeout":"soon","tables"`, 1),
		"invalid values":         strings.Replace(minimal, `"film"`, `"Not A Label"`, 1),
		"row limit too high":     strings.Replace(minimal, `"tables"`, `"rowLimit":5000,"tables"`, 1),
	}
	for name, in := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeConfig(strings.NewReader(in)); err == nil {
				t.Errorf("accepted %s", in)
			}
		})
	}
	huge := `{"domain":"` + strings.Repeat("a", maxConfigBytes) + `"}`
	if _, err := DecodeConfig(strings.NewReader(huge)); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("oversized config: %v", err)
	}
}

func TestLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "domain.json")
	data, _ := json.Marshal(validConfig())
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path + ".missing"); err == nil {
		t.Error("missing file accepted")
	}
}

func approvedPlan(t *testing.T) *plan.DomainPlan {
	t.Helper()
	p := &plan.DomainPlan{
		APIVersion: plan.APIVersion,
		Kind:       plan.Kind,
		Metadata:   plan.Metadata{Name: "shop"},
		Spec: plan.Spec{
			Source: plan.Source{Dialect: "postgres", Database: "shop", SchemaVersion: "kabr.dev/v1alpha1",
				CatalogFingerprint: testFingerprint},
			Domains: []plan.Domain{
				{Name: "sales", Tables: []plan.TableAssignment{{Name: "sales.order"}, {Name: "sales.item"}},
					SensitiveColumns: []plan.SensitiveColumn{
						{Table: "sales.order", Column: "card", Class: plan.ClassFinancial, Action: plan.ActionHash},
					}},
				{Name: "people", Tables: []plan.TableAssignment{{Name: "public.customer"}}},
				{Name: "audit", Tables: []plan.TableAssignment{{Name: "logs.event"}}},
			},
		},
	}
	if err := plan.Approve(p, "reviewer", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConfigFromPlan(t *testing.T) {
	p := approvedPlan(t)
	c, err := ConfigFromPlan(p, "sales")
	if err != nil {
		t.Fatal(err)
	}
	want := &Config{
		Domain:             "sales",
		Plan:               "shop",
		CatalogFingerprint: testFingerprint,
		Schemas:            []string{"logs", "public", "sales"},
		RowLimit:           DefaultRowLimit,
		StatementTimeout:   Duration(DefaultStatementTimeout),
		Tables:             []string{"sales.order", "sales.item"},
		SensitiveColumns:   p.Spec.Domains[0].SensitiveColumns,
	}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("config = %+v\nwant %+v", c, want)
	}
	c.SensitiveColumns[0].Action = plan.ActionMask
	if p.Spec.Domains[0].SensitiveColumns[0].Action != plan.ActionHash {
		t.Error("ConfigFromPlan must not alias the plan")
	}

	if _, err := ConfigFromPlan(p, "nope"); err == nil || !strings.Contains(err.Error(), "domains: audit, people, sales") {
		t.Errorf("unknown domain: %v", err)
	}
	proposed := approvedPlan(t)
	proposed.Status.Phase = plan.PhaseProposed
	if _, err := ConfigFromPlan(proposed, "sales"); err == nil || !strings.Contains(err.Error(), "not approved") {
		t.Errorf("proposed plan: %v", err)
	}
	unset := approvedPlan(t)
	unset.Status = plan.Status{}
	if _, err := ConfigFromPlan(unset, "sales"); err == nil || !strings.Contains(err.Error(), "phase unset") {
		t.Errorf("plan without phase: %v", err)
	}
	broken := approvedPlan(t)
	broken.Spec.Source.CatalogFingerprint = "nope"
	if _, err := ConfigFromPlan(broken, "sales"); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Errorf("invalid plan: %v", err)
	}
	if _, err := ConfigFromPlan(nil, "sales"); err == nil {
		t.Error("nil plan accepted")
	}
}
