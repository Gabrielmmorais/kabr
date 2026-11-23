package postgres

import (
	"regexp"
	"strings"
	"testing"
)

// TestQueriesReadOnlySystemCatalogs statically checks that every relation a
// query reads from is in pg_catalog. Function calls in FROM (unnest,
// generate_series) are allowed. The integration test complements this by
// running introspection as a role with no SELECT on any user table.
func TestQueriesReadOnlySystemCatalogs(t *testing.T) {
	target := regexp.MustCompile(`(?i)\b(?:FROM|JOIN)\s+([A-Za-z_][A-Za-z0-9_.]*)(\s*\()?`)
	allowedFuncs := map[string]bool{"unnest": true, "generate_series": true}
	for name, q := range allQueries {
		matches := target.FindAllStringSubmatch(q, -1)
		for _, m := range matches {
			obj, isCall := m[1], m[2] != ""
			switch {
			case isCall && allowedFuncs[strings.ToLower(obj)]:
			case !isCall && strings.HasPrefix(obj, "pg_catalog."):
			default:
				t.Errorf("%s reads from %q, which is not a pg_catalog relation or an allowed function", name, m[0])
			}
		}
		if strings.Contains(strings.ToLower(q), "information_schema") {
			// information_schema is allowed by policy but unused; keep the
			// static check simple by failing loudly if that changes.
			t.Errorf("%s uses information_schema; extend this test first", name)
		}
	}
	if len(allQueries) == 0 {
		t.Fatal("no queries registered")
	}
}

func TestFKAction(t *testing.T) {
	for in, want := range map[string]string{"a": "NO ACTION", "r": "RESTRICT", "c": "CASCADE", "n": "SET NULL", "d": "SET DEFAULT", " ": "", "z": ""} {
		if got := fkAction(in); got != want {
			t.Errorf("fkAction(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEstimatedRows(t *testing.T) {
	cases := []struct {
		tuples float64
		pages  int64
		want   *int64
	}{
		{-1, 0, nil},
		{0, 0, nil},
		{0, 3, ptr(int64(0))},
		{41.6, 1, ptr(int64(42))},
	}
	for _, c := range cases {
		got := estimatedRows(c.tuples, c.pages)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("estimatedRows(%v,%v) = %v, want %v", c.tuples, c.pages, got, c.want)
		}
	}
}

func ptr[T any](v T) *T { return &v }
