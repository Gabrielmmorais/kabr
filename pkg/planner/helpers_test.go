package planner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

func loadPagila(t *testing.T) *schema.Catalog {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "pagila.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := schema.Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
