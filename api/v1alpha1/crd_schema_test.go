package v1alpha1

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/Gabrielmmorais/kabr/pkg/plan"
)

// crdSchema returns spec.versions[0].schema.openAPIV3Schema of a generated
// CRD under config/crd/bases.
func crdSchema(t *testing.T, file string) map[string]any {
	t.Helper()
	var crd map[string]any
	if err := yaml.Unmarshal(readTestFile(t, filepath.Join("..", "..", "config", "crd", "bases", file)), &crd); err != nil {
		t.Fatal(err)
	}
	versions := crd["spec"].(map[string]any)["versions"].([]any)
	if len(versions) != 1 {
		t.Fatalf("%s: %d versions, want 1", file, len(versions))
	}
	v := versions[0].(map[string]any)
	if v["name"] != Version || v["served"] != true || v["storage"] != true {
		t.Fatalf("%s: unexpected version %v", file, v["name"])
	}
	return v["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)
}

// checkCovers fails when a JSON field of typ has no property in the schema
// (the API server would prune it, breaking the approval digest), or when
// the schema sets a default inside it (the API server would add a field the
// approver never saw). It recurses into structs and slices.
func checkCovers(t *testing.T, path string, typ reflect.Type, s map[string]any) {
	t.Helper()
	if _, ok := s["default"]; ok {
		t.Errorf("%s: schema sets a default inside the plan spec", path)
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Slice:
		items, ok := s["items"].(map[string]any)
		if !ok {
			t.Errorf("%s: schema has no items", path)
			return
		}
		checkCovers(t, path+"[]", typ.Elem(), items)
	case reflect.Struct:
		props, _ := s["properties"].(map[string]any)
		for f := range typ.Fields() {
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" || name == "-" {
				continue
			}
			child, ok := props[name].(map[string]any)
			if !ok {
				t.Errorf("%s.%s: field missing from the CRD schema (it would be pruned)", path, name)
				continue
			}
			checkCovers(t, path+"."+name, f.Type, child)
		}
	default:
	}
}

// TestDomainPlanCRDCoversPlanSpec guards the approval digest without a
// running API server: every field of the embedded plan.Spec must survive a
// round trip through the CRD's structural schema unchanged. The envtest
// suite (make envtest) checks the same against a real kube-apiserver.
func TestDomainPlanCRDCoversPlanSpec(t *testing.T) {
	spec := crdSchema(t, "kabr.dev_domainplans.yaml")["properties"].(map[string]any)["spec"].(map[string]any)
	checkCovers(t, "spec", reflect.TypeFor[plan.Spec](), spec)
	if spec["x-kubernetes-preserve-unknown-fields"] != nil {
		t.Error("spec must be fully structural")
	}

	sc := crdSchema(t, "kabr.dev_domainapis.yaml")["properties"].(map[string]any)["spec"].(map[string]any)["properties"].(map[string]any)["sensitiveColumns"].(map[string]any)
	checkCovers(t, "spec.sensitiveColumns", reflect.TypeFor[[]plan.SensitiveColumn](), sc)
}

// TestCRDsAreNamespacedWithStatus checks the generated CRDs against the
// kubebuilder markers they come from.
func TestCRDsAreNamespacedWithStatus(t *testing.T) {
	for file, kind := range map[string]string{
		"kabr.dev_datasources.yaml": KindDataSource,
		"kabr.dev_domainplans.yaml": KindDomainPlan,
		"kabr.dev_domainapis.yaml":  KindDomainAPI,
	} {
		var crd map[string]any
		if err := yaml.Unmarshal(readTestFile(t, filepath.Join("..", "..", "config", "crd", "bases", file)), &crd); err != nil {
			t.Fatal(err)
		}
		spec := crd["spec"].(map[string]any)
		if spec["group"] != Group || spec["scope"] != "Namespaced" || spec["names"].(map[string]any)["kind"] != kind {
			t.Errorf("%s: group %v scope %v kind %v", file, spec["group"], spec["scope"], spec["names"].(map[string]any)["kind"])
		}
		v := spec["versions"].([]any)[0].(map[string]any)
		if _, ok := v["subresources"].(map[string]any)["status"]; !ok {
			t.Errorf("%s: no status subresource", file)
		}
	}
}
