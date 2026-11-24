package llm

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Gabrielmmorais/kabr/pkg/plan"
	"github.com/Gabrielmmorais/kabr/pkg/schema"
	"github.com/Gabrielmmorais/kabr/pkg/segment"
)

// pagila loads the example catalog shipped with the repository.
func pagila(t *testing.T) *schema.Catalog {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "pagila.schema.json"))
	if err != nil {
		t.Fatalf("read pagila: %v", err)
	}
	c, err := schema.Unmarshal(data)
	if err != nil {
		t.Fatalf("unmarshal pagila: %v", err)
	}
	return c
}

// baselineFor segments c into a graph-only plan with generic domain names.
func baselineFor(t *testing.T, c *schema.Catalog) *plan.DomainPlan {
	t.Helper()
	res, err := segment.Segment(c, segment.DefaultOptions())
	if err != nil {
		t.Fatalf("segment: %v", err)
	}
	p := plan.New("test", c)
	for _, com := range res.Communities {
		d := plan.Domain{
			Name:        fmt.Sprintf("domain-%d", com.ID),
			Description: "baseline description",
			Rationale:   "baseline rationale",
		}
		for _, a := range com.Tables {
			d.Tables = append(d.Tables, plan.TableAssignment{
				Name:      a.Table,
				Role:      string(a.Role),
				Shared:    a.Shared,
				Rationale: a.Rationale,
			})
		}
		p.Spec.Domains = append(p.Spec.Domains, d)
	}
	if err := p.Validate(c); err != nil {
		t.Fatalf("baseline invalid: %v", err)
	}
	return p
}

// canned is a valid model answer for the Pagila catalog.
const canned = `{
  "renames": [{"from": "domain-0", "to": "rentals", "description": "Film rentals.", "rationale": "rental and payment tables"}],
  "moves": [],
  "sensitiveColumns": [{"table": "public.customer", "column": "email", "class": "pii", "action": "mask", "rationale": "email address"}]
}`

// mustTime returns a fixed, recognizable introspection timestamp.
func mustTime(t *testing.T) time.Time {
	t.Helper()
	return time.Date(2031, time.January, 2, 3, 4, 5, 0, time.UTC)
}
