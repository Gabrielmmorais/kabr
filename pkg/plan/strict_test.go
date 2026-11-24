package plan

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestUnmarshalStrictKeysAndDocuments(t *testing.T) {
	valid, err := Marshal(smallPlan(smallCatalog()))
	if err != nil {
		t.Fatal(err)
	}
	s := string(valid)
	tests := []struct {
		name string
		data string
		want string // "" = must succeed
	}{
		{"valid", s, ""},
		{"leading document marker", "---\n" + s, ""},
		{"leading comment and marker", "# plan\n---\n" + s, ""},
		{"trailing end marker", s + "...\n", ""},
		{"annotations keep free-form keys", strings.Replace(s, "  name: shop\n", "  annotations:\n    Kabr.Dev/Note: x\n  name: shop\n", 1), ""},
		{"block scalar containing dashes", strings.Replace(s, "  - name: catalog\n", "  - name: catalog\n    rationale: |\n      a --- b\n", 1), ""},
		{"uppercase kind key", strings.Replace(s, "kind: DomainPlan", "KIND: DomainPlan", 1), `KIND: unknown field "KIND"`},
		{"shadow Kind key", strings.Replace(s, "kind: DomainPlan\n", "kind: DomainPlan\nKind: DomainPlan\n", 1), `Kind: unknown field "Kind"`},
		{"case variant nested", strings.Replace(s, "    catalogFingerprint:", "    CatalogFingerprint:", 1), `spec.source.CatalogFingerprint: unknown field`},
		{"case variant in list item", strings.Replace(s, "  - name: catalog\n", "  - Name: catalog\n", 1), `spec.domains[0].Name: unknown field`},
		{"case variant in sensitive column", strings.Replace(s, "      column: email\n", "      Column: email\n", 1), `spec.domains[1].sensitiveColumns[0].Column`},
		{"second document", s + "---\nfoo: bar\n", "multiple YAML documents"},
		{"second empty document", s + "---\n", "multiple YAML documents"},
		{"second plan", s + "---\n" + s, "multiple YAML documents"},
		{"content after end marker", s + "...\nfoo: bar\n", "content after end of YAML document"},
		{"two leading markers", "---\n---\n" + s, "multiple YAML documents"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Unmarshal([]byte(tt.data))
			if tt.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if p.Kind != Kind {
					t.Fatalf("kind = %q", p.Kind)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

// TestSchemaEnumsMatchGo keeps the JSON Schema enums and the Go validator
// in agreement, including "" for optional enums.
func TestSchemaEnumsMatchGo(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(Schema(), &doc); err != nil {
		t.Fatal(err)
	}
	defs := doc["$defs"].(map[string]any)
	enum := func(def, prop string) []string {
		p := defs[def].(map[string]any)["properties"].(map[string]any)[prop].(map[string]any)
		var out []string
		for _, v := range p["enum"].([]any) {
			out = append(out, v.(string))
		}
		slices.Sort(out)
		return out
	}
	keys := func(m map[string]bool) []string { return sortedKeys(m) }
	conv := func(m any) []string {
		var out []string
		switch mm := m.(type) {
		case map[SensitiveClass]bool:
			for k := range mm {
				out = append(out, string(k))
			}
		case map[Action]bool:
			for k := range mm {
				out = append(out, string(k))
			}
		case map[Phase]bool:
			for k := range mm {
				out = append(out, string(k))
			}
		}
		slices.Sort(out)
		return out
	}
	tests := []struct {
		name      string
		schema, g []string
	}{
		{"role", enum("TableAssignment", "role"), keys(validRoles)},
		{"source", enum("SensitiveColumn", "source"), keys(validSources)},
		{"mode", enum("Generator", "mode"), keys(validGenModes)},
		{"class", enum("SensitiveColumn", "class"), conv(validClasses)},
		{"action", enum("SensitiveColumn", "action"), conv(validActions)},
		{"phase", enum("Status", "phase"), conv(validPhases)},
	}
	for _, tt := range tests {
		if !slices.Equal(tt.schema, tt.g) {
			t.Errorf("%s: schema enum %q != Go %q", tt.name, tt.schema, tt.g)
		}
	}

	// An explicit empty optional enum is accepted by Go (as by the schema).
	p := smallPlan(smallCatalog())
	p.Spec.Domains[0].Tables[0].Role = ""
	p.Spec.Domains[0].SensitiveColumns[0].Source = ""
	p.Status.Generator.Mode = ""
	if err := p.ValidateStructure(); err != nil {
		t.Fatalf("empty optional enums should be valid: %v", err)
	}
}
