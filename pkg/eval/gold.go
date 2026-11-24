package eval

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// Sensitive column classes accepted in a gold file.
const (
	ClassPII        = "pii"
	ClassCredential = "credential"
	ClassFinancial  = "financial"
	ClassOther      = "other"
)

var validClasses = map[string]bool{
	ClassPII:        true,
	ClassCredential: true,
	ClassFinancial:  true,
	ClassOther:      true,
}

// GoldSensitive is one column the gold standard says MUST be flagged.
type GoldSensitive struct {
	Table  string `json:"table"`  // table ID, e.g. "public.staff"
	Column string `json:"column"` // column name, e.g. "password"
	Class  string `json:"class"`  // pii | credential | financial | other
}

// Gold is a human-curated reference plan for one catalog.
type Gold struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Catalog is the repository-relative path of the catalog the gold applies
	// to, e.g. "examples/pagila.schema.json".
	Catalog string `json:"catalog,omitempty"`
	// Domains maps a domain label to the table IDs ("schema.name") in it.
	Domains map[string][]string `json:"domains"`
	// Sensitive is the must-flag set of sensitive columns.
	Sensitive []GoldSensitive `json:"sensitive,omitempty"`
}

// LoadGold reads and validates a gold file. Decoding is strict: unknown
// fields and duplicate YAML keys are rejected.
func LoadGold(path string) (*Gold, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is chosen by the caller (eval datasets)
	if err != nil {
		return nil, fmt.Errorf("eval: read gold %q: %w", path, err)
	}
	var g Gold
	if err := yaml.UnmarshalStrict(data, &g); err != nil {
		return nil, fmt.Errorf("eval: decode gold %q: %w", path, err)
	}
	if err := g.Validate(); err != nil {
		return nil, fmt.Errorf("eval: invalid gold %q: %w", path, err)
	}
	return &g, nil
}

// Validate checks the structural invariants of a gold standard: a name, at
// least one non-empty domain, no table in two domains (or twice in one), and
// sensitive entries that reference gold tables with a known class and no
// duplicates.
func (g *Gold) Validate() error {
	var errs []error
	if strings.TrimSpace(g.Name) == "" {
		errs = append(errs, errors.New("name is empty"))
	}
	if len(g.Domains) == 0 {
		errs = append(errs, errors.New("no domains"))
	}
	owner := map[string]string{}
	for _, label := range sortedKeys(g.Domains) {
		tables := g.Domains[label]
		if strings.TrimSpace(label) == "" {
			errs = append(errs, errors.New("empty domain label"))
		}
		if len(tables) == 0 {
			errs = append(errs, fmt.Errorf("domain %q has no tables", label))
		}
		for _, t := range tables {
			if strings.TrimSpace(t) == "" {
				errs = append(errs, fmt.Errorf("domain %q has an empty table ID", label))
				continue
			}
			if prev, ok := owner[t]; ok {
				errs = append(errs, fmt.Errorf("table %q is in domains %q and %q", t, prev, label))
				continue
			}
			owner[t] = label
		}
	}
	seen := map[string]bool{}
	for i, s := range g.Sensitive {
		key := s.Table + "." + s.Column
		switch {
		case s.Table == "" || s.Column == "":
			errs = append(errs, fmt.Errorf("sensitive[%d]: table and column are required", i))
		case owner[s.Table] == "":
			errs = append(errs, fmt.Errorf("sensitive[%d]: table %q is not in any domain", i, s.Table))
		}
		if !validClasses[s.Class] {
			errs = append(errs, fmt.Errorf("sensitive[%d]: unknown class %q", i, s.Class))
		}
		if seen[key] {
			errs = append(errs, fmt.Errorf("sensitive[%d]: duplicate column %q", i, key))
		}
		seen[key] = true
	}
	return errors.Join(errs...)
}

// Assignment returns the gold grouping as table ID -> domain label.
func (g *Gold) Assignment() Assignment {
	a := Assignment{}
	for label, tables := range g.Domains {
		for _, t := range tables {
			a[t] = label
		}
	}
	return a
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
