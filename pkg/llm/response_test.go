package llm

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Gabrielmmorais/kabr/pkg/plan"
)

func TestParseResponse(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    plan.Refinement
		wantErr string
	}{
		{
			name: "valid",
			in:   canned,
			want: plan.Refinement{
				Renames: []plan.DomainRename{{From: "domain-0", To: "rentals", Description: "Film rentals.", Rationale: "rental and payment tables"}},
				SensitiveColumns: []plan.SensitiveColumn{{
					Table: "public.customer", Column: "email", Class: plan.ClassPII, Action: plan.ActionMask,
					Source: plan.SourceLLM, Rationale: "email address",
				}},
			},
		},
		{
			name: "trims strings",
			in:   `{"renames":[],"moves":[{"table":" public.film ","to":" catalog ","rationale":" r "}],"sensitiveColumns":[{"table":"public.staff","column":" password ","class":" credential ","action":"drop","rationale":""}]}`,
			want: plan.Refinement{
				Moves: []plan.TableMove{{Table: "public.film", To: "catalog", Rationale: "r"}},
				SensitiveColumns: []plan.SensitiveColumn{{
					Table: "public.staff", Column: "password", Class: plan.ClassCredential, Action: plan.ActionDrop, Source: plan.SourceLLM,
				}},
			},
		},
		{name: "empty lists", in: `{"renames":[],"moves":[],"sensitiveColumns":[]}`, want: plan.Refinement{}},
		{name: "missing lists tolerated", in: `{}`, want: plan.Refinement{}},
		{name: "unknown top-level field", in: `{"renames":[],"extra":1}`, wantErr: "unknown field"},
		{name: "model sets source", in: `{"sensitiveColumns":[{"table":"a.b","column":"c","class":"pii","action":"mask","rationale":"","source":"human"}]}`, wantErr: "unknown field"},
		{name: "invalid class", in: `{"sensitiveColumns":[{"table":"a.b","column":"c","class":"secret","action":"mask","rationale":""}]}`, wantErr: "invalid class"},
		{name: "invalid action", in: `{"sensitiveColumns":[{"table":"a.b","column":"c","class":"pii","action":"encrypt","rationale":""}]}`, wantErr: "invalid action"},
		{name: "garbage", in: `not json`, wantErr: "decode response"},
		{name: "empty", in: ``, wantErr: "decode response"},
		{name: "code fence", in: "```json\n{}\n```", wantErr: "decode response"},
		{name: "trailing data", in: `{} {}`, wantErr: "after the JSON object"},
		{name: "array", in: `[]`, wantErr: "decode response"},
		{name: "null", in: `null`, wantErr: "must be a JSON object"},
		{name: "case-variant top key", in: `{"Renames":[]}`, wantErr: "case-sensitive"},
		{name: "case-variant shadow key", in: `{"renames":[],"Renames":[]}`, wantErr: "case-sensitive"},
		{name: "case-variant item key", in: `{"moves":[{"Table":"a.b","to":"x","rationale":""}]}`, wantErr: "case-sensitive"},
		{name: "case-variant sensitive key", in: `{"sensitiveColumns":[{"table":"a.b","column":"c","CLASS":"pii","action":"mask","rationale":""}]}`, wantErr: "case-sensitive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseResponse([]byte(tt.in))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseResponse: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

// TestResponseSchemaClosed checks that every object is closed and requires
// all its properties (needed by structured outputs) and that enums match the
// plan constants.
func TestResponseSchemaClosed(t *testing.T) {
	s := ResponseSchema()
	var check func(path string, node map[string]any)
	objects := 0
	check = func(path string, node map[string]any) {
		switch node["type"] {
		case "object":
			objects++
			if node["additionalProperties"] != false {
				t.Errorf("%s: additionalProperties must be false", path)
			}
			props, _ := node["properties"].(map[string]any)
			req, _ := node["required"].([]string)
			if len(req) != len(props) {
				t.Errorf("%s: required %v does not cover properties", path, req)
			}
			for _, r := range req {
				child, ok := props[r].(map[string]any)
				if !ok {
					t.Errorf("%s: required %q has no property", path, r)
					continue
				}
				check(path+"."+r, child)
			}
		case "array":
			check(path+"[]", node["items"].(map[string]any))
		}
	}
	check("$", s)
	if objects != 4 {
		t.Errorf("got %d objects, want 4", objects)
	}
	sc := s["properties"].(map[string]any)["sensitiveColumns"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	if got := sc["class"].(map[string]any)["enum"]; !reflect.DeepEqual(got, []any{"pii", "credential", "financial", "other"}) {
		t.Errorf("class enum = %v", got)
	}
	if got := sc["action"].(map[string]any)["enum"]; !reflect.DeepEqual(got, []any{"mask", "drop", "hash"}) {
		t.Errorf("action enum = %v", got)
	}
	if _, err := json.Marshal(s); err != nil {
		t.Fatalf("schema not serializable: %v", err)
	}
	// A fresh map each call: callers may not corrupt each other.
	s["type"] = "mutated"
	if ResponseSchema()["type"] != "object" {
		t.Fatal("ResponseSchema shares state between calls")
	}
}
