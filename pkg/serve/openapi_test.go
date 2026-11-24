package serve

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"testing"
)

func TestBuildOpenAPI(t *testing.T) {
	m, _ := mustResolve(t, catalogConfig(), unitHashKey)
	data, err := buildOpenAPI(m)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := buildOpenAPI(m)
	if !bytes.Equal(data, again) {
		t.Error("OpenAPI document is not deterministic")
	}
	var doc struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"info"`
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
			Parameters  []struct {
				Name   string         `json:"name"`
				In     string         `json:"in"`
				Schema map[string]any `json:"schema"`
			} `json:"parameters"`
			Responses map[string]json.RawMessage `json:"responses"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Required   []string                  `json:"required"`
				Properties map[string]map[string]any `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.OpenAPI != "3.0.3" || doc.Info.Title != "Kabr domain film" || doc.Info.Version != "pagila@"+testFingerprint {
		t.Errorf("header: %+v %+v", doc.OpenAPI, doc.Info)
	}
	var paths []string
	for p := range doc.Paths {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	want := []string{
		"/v1/public/customer", "/v1/public/customer/{pk}", "/v1/public/film", "/v1/public/film/{pk}",
		"/v1/public/rating_info", "/v1/public/rating_info/{pk}", "/v1/public/rental_event",
		"/v1/public/we%22ird", "/v1/public/we%22ird/{pk}",
	}
	if !slices.Equal(paths, want) {
		t.Errorf("paths = %v\nwant %v", paths, want)
	}

	// Component keys are OpenAPI-valid and unique.
	keyRE := regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	for name := range doc.Components.Schemas {
		if !keyRE.MatchString(name) {
			t.Errorf("invalid component key %q", name)
		}
	}
	weird := doc.Components.Schemas["public.we_ird"]
	if _, ok := weird.Properties["café"]; !ok || len(weird.Properties) != 2 {
		t.Errorf("we\"ird schema: %+v", weird)
	}

	cust := doc.Components.Schemas["public.customer"]
	if !slices.Equal(cust.Required, []string{"customer_id", "email", "card"}) {
		t.Errorf("customer required = %v", cust.Required)
	}
	for _, dropped := range []string{"password", "first_name"} {
		if _, ok := cust.Properties[dropped]; ok || bytes.Contains(data, []byte(`"`+dropped+`"`)) {
			t.Errorf("dropped column %s in the document", dropped)
		}
	}
	if p := cust.Properties["email"]; p["type"] != "string" || p["nullable"] != true {
		t.Errorf("masked email = %v", p)
	}
	if p := cust.Properties["card"]; p["type"] != "string" || p["pattern"] != "^[0-9a-f]{64}$" {
		t.Errorf("hashed card = %v", p)
	}
	if p := cust.Properties["customer_id"]; p["format"] != "int64" || p["nullable"] != nil {
		t.Errorf("bigint key = %v", p)
	}

	film := doc.Components.Schemas["public.film"].Properties
	for col, typ := range map[string]string{
		"film_id": "integer", "rental_rate": "string", "ratio": "number", "active": "boolean",
		"last_update": "string", "released_on": "string", "poster": "string", "features": "array", "uid": "string",
		"rating": "string",
	} {
		if film[col]["type"] != typ {
			t.Errorf("film.%s type = %v, want %s", col, film[col]["type"], typ)
		}
	}
	if _, typed := film["meta"]["type"]; typed {
		t.Errorf("jsonb must accept any JSON: %v", film["meta"])
	}

	list := doc.Paths["/v1/public/customer"]["get"]
	var params []string
	for _, p := range list.Parameters {
		params = append(params, p.Name)
		if p.Name == "limit" && (p.Schema["maximum"] != float64(100) || p.Schema["default"] != float64(100)) {
			t.Errorf("limit schema = %v", p.Schema)
		}
	}
	if !slices.Equal(params, []string{"limit", "cursor", "customer_id"}) {
		t.Errorf("customer filters = %v (sensitive columns must not be filters)", params)
	}
	for _, code := range []string{"200", "400", "500", "503", "504"} {
		if _, ok := list.Responses[code]; !ok {
			t.Errorf("list lacks response %s", code)
		}
	}
	get := doc.Paths["/v1/public/film/{pk}"]["get"]
	if get.OperationID != "get_public.film" || len(get.Parameters) != 1 || get.Parameters[0].In != "path" {
		t.Errorf("get operation: %+v", get)
	}
	if _, ok := get.Responses["404"]; !ok {
		t.Error("get lacks 404")
	}
}

func TestComponentNamesAreUnique(t *testing.T) {
	names := componentNames([]*table{{id: "a.b c"}, {id: "a.b_c"}, {id: "a.b?c"}, {id: "Error"}})
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] || n == "Error" {
			t.Errorf("duplicate or reserved component name %q in %v", n, names)
		}
		seen[n] = true
	}
}
