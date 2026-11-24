package llm

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/Gabrielmmorais/kabr/pkg/plan"
	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

// Prompt is the provider-agnostic request: a constant system prompt and a
// user message holding the metadata JSON document.
type Prompt struct {
	System string
	User   string
}

// systemPrompt is constant: it must never interpolate catalog content.
const systemPrompt = `You refine how the tables of a relational database are grouped into business domains. Each domain will become one API.

The user message is a JSON document with two parts:
- "tables": the catalog METADATA only (table IDs "schema.table", comments, columns with name, type, nullability and comment, primary keys, foreign keys, unique constraints). No row data is available.
- "baseline": a grouping computed from the foreign key graph: domains, each with its tables (ID, role core|lookup|junction|orphan, shared flag).

Propose only these operations on top of the baseline:
1. "renames": give a baseline domain a meaningful business name and a one-sentence description. "from" is the baseline name; "to" must be a DNS-1123 label: lowercase letters, digits and "-", starting and ending with a letter or digit, at most 63 characters. Renames are applied in order.
2. "moves": move a table to another domain when it clearly belongs there. "to" is a domain name after renames; a new valid name creates a new domain. Do not leave a domain empty unless that is intended.
3. "sensitiveColumns": flag columns whose values must not be exposed verbatim. "class" is pii (personal data), credential (passwords, tokens, secrets), financial (payment or account data) or other. "action" is mask, drop or hash: prefer drop for credentials, hash for identifiers that must stay joinable, mask otherwise.

Rules:
- Use only table IDs and column names that appear in the input. Never invent tables, columns or domains you do not create through renames or moves.
- Every item needs a short "rationale" based on the metadata.
- Return empty lists when nothing should change.
- Answer with a single JSON object matching the provided JSON schema, with no other text.`

// promptDoc is the user message. Every field here is reviewed against the
// privacy invariant (see the package documentation); never embed schema
// types directly.
type promptDoc struct {
	Tables   []promptTable  `json:"tables"`
	Baseline promptBaseline `json:"baseline"`
}

type promptTable struct {
	ID                string             `json:"id"`
	Comment           string             `json:"comment,omitempty"`
	Columns           []promptColumn     `json:"columns"`
	PrimaryKey        []string           `json:"primaryKey,omitempty"`
	ForeignKeys       []promptForeignKey `json:"foreignKeys,omitempty"`
	UniqueConstraints [][]string         `json:"uniqueConstraints,omitempty"`
}

type promptColumn struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	Comment  string `json:"comment,omitempty"`
}

type promptForeignKey struct {
	Columns           []string `json:"columns"`
	References        string   `json:"references"`
	ReferencedColumns []string `json:"referencedColumns"`
}

type promptBaseline struct {
	Domains []promptDomain `json:"domains"`
}

type promptDomain struct {
	Name   string             `json:"name"`
	Tables []promptAssignment `json:"tables"`
}

// promptAssignment is a baseline table assignment.
type promptAssignment struct {
	ID     string `json:"id"`
	Role   string `json:"role,omitempty"`
	Shared bool   `json:"shared,omitempty"`
}

// BuildPrompt returns the prompt for refining baseline against c. The
// output is deterministic: tables and domains are sorted.
func BuildPrompt(c *schema.Catalog, baseline *plan.DomainPlan) (Prompt, error) {
	if c == nil {
		return Prompt{}, errors.New("llm: catalog is nil")
	}
	if baseline == nil {
		return Prompt{}, errors.New("llm: baseline plan is nil")
	}
	doc := promptDoc{Tables: make([]promptTable, 0, len(c.Tables))}
	for i := range c.Tables {
		doc.Tables = append(doc.Tables, metadataOf(&c.Tables[i]))
	}
	slices.SortFunc(doc.Tables, func(a, b promptTable) int { return cmp.Compare(a.ID, b.ID) })

	doc.Baseline.Domains = make([]promptDomain, 0, len(baseline.Spec.Domains))
	for _, d := range baseline.Spec.Domains {
		pd := promptDomain{Name: d.Name, Tables: make([]promptAssignment, 0, len(d.Tables))}
		for _, t := range d.Tables {
			pd.Tables = append(pd.Tables, promptAssignment{ID: t.Name, Role: t.Role, Shared: t.Shared})
		}
		slices.SortFunc(pd.Tables, func(a, b promptAssignment) int { return cmp.Compare(a.ID, b.ID) })
		doc.Baseline.Domains = append(doc.Baseline.Domains, pd)
	}
	slices.SortFunc(doc.Baseline.Domains, func(a, b promptDomain) int { return cmp.Compare(a.Name, b.Name) })

	user, err := json.Marshal(doc)
	if err != nil {
		return Prompt{}, fmt.Errorf("llm: encode prompt: %w", err)
	}
	return Prompt{System: systemPrompt, User: string(user)}, nil
}

// metadataOf copies the allowed metadata of t, field by field.
func metadataOf(t *schema.Table) promptTable {
	pt := promptTable{
		ID:      t.ID(),
		Comment: t.Comment,
		Columns: make([]promptColumn, 0, len(t.Columns)),
	}
	cols := slices.Clone(t.Columns)
	slices.SortStableFunc(cols, func(a, b schema.Column) int { return cmp.Compare(a.Ordinal, b.Ordinal) })
	for _, col := range cols {
		pt.Columns = append(pt.Columns, promptColumn{
			Name:     col.Name,
			Type:     col.DataType,
			Nullable: col.Nullable,
			Comment:  col.Comment,
		})
	}
	if t.PrimaryKey != nil {
		pt.PrimaryKey = slices.Clone(t.PrimaryKey.Columns)
	}
	for _, fk := range t.ForeignKeys {
		pt.ForeignKeys = append(pt.ForeignKeys, promptForeignKey{
			Columns:           slices.Clone(fk.Columns),
			References:        fk.RefID(),
			ReferencedColumns: slices.Clone(fk.RefColumns),
		})
	}
	for _, u := range t.UniqueConstraints {
		pt.UniqueConstraints = append(pt.UniqueConstraints, slices.Clone(u.Columns))
	}
	return pt
}
