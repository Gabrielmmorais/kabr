package graph

import (
	"fmt"
	"strings"

	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

// Mermaid renders the catalog as a Mermaid "erDiagram". The catalog is not
// modified; a normalized copy is rendered, so output is deterministic.
//
// Entities: one per table. The entity name is the table name when every
// table lives in the same schema, otherwise "schema__table". Names are
// sanitized with [MermaidIdent]; on a collision a numeric suffix ("_2", ...)
// is appended in table order. Each column becomes an attribute
// "<type> <name> [PK|FK|UK...] ["comment"]" where the type is sanitized with
// [MermaidType], PK marks primary key columns, FK foreign key columns and UK
// columns of a single-column unique constraint/index. Comments have double
// quotes replaced by single quotes and whitespace collapsed.
//
// Relationships: one per foreign key, written "parent <card> child : "fk"".
// Cardinality is inferred from the child side:
//
//	|o--o{  nullable FK (any FK column nullable), one-to-many
//	||--o{  NOT NULL FK, one-to-many
//	|o--o|  nullable FK whose columns form a unique key (PK/unique), one-to-one
//	||--o|  NOT NULL FK whose columns form a unique key, one-to-one
//
// The child side of a one-to-one is "o|" (zero or one) because a parent row
// does not require a referencing row. Foreign keys to tables outside the
// catalog are emitted as "%%" comments only.
func Mermaid(c *schema.Catalog) string {
	n := normalized(c)
	var b strings.Builder
	b.WriteString("erDiagram\n")

	names := entityNames(n)
	for i := range n.Tables {
		t := &n.Tables[i]
		name := names[t.ID()]
		if len(t.Columns) == 0 {
			fmt.Fprintf(&b, "    %s\n", name)
			continue
		}
		pk, fk, uk := keyColumns(t)
		fmt.Fprintf(&b, "    %s {\n", name)
		for _, col := range t.Columns {
			fmt.Fprintf(&b, "        %s %s", MermaidType(col.DataType), MermaidIdent(col.Name))
			var keys []string
			if pk[col.Name] {
				keys = append(keys, "PK")
			}
			if fk[col.Name] {
				keys = append(keys, "FK")
			}
			if uk[col.Name] && !pk[col.Name] {
				keys = append(keys, "UK")
			}
			if len(keys) > 0 {
				b.WriteString(" " + strings.Join(keys, ", "))
			}
			if cm := mermaidComment(col.Comment); cm != "" {
				fmt.Fprintf(&b, " \"%s\"", cm)
			}
			b.WriteString("\n")
		}
		b.WriteString("    }\n")
	}

	for i := range n.Tables {
		t := &n.Tables[i]
		for _, fk := range t.ForeignKeys {
			label := mermaidComment(fk.Name)
			ref := n.Table(fk.RefSchema, fk.RefTable)
			if ref == nil {
				fmt.Fprintf(&b, "    %%%% external reference %s -> %s : %s\n", t.ID(), fk.RefID(), label)
				continue
			}
			parent := "||"
			for _, cn := range fk.Columns {
				if col := t.Column(cn); col == nil || col.Nullable {
					parent = "|o"
					break
				}
			}
			child := "o{"
			if t.IsUniqueKey(fk.Columns) {
				child = "o|"
			}
			fmt.Fprintf(&b, "    %s %s--%s %s : \"%s\"\n", names[ref.ID()], parent, child, names[t.ID()], label)
		}
	}
	return b.String()
}

// DOT renders the catalog as a Graphviz digraph. Nodes are tables (ID and
// label "schema.table"); there is one edge per foreign key, from the
// referencing to the referenced table, labeled with the foreign key name.
// Self-references become self-loops. Foreign keys to tables outside the
// catalog are emitted as comments only. All IDs and labels are quoted and
// escaped. Output is deterministic.
func DOT(c *schema.Catalog) string {
	n := normalized(c)
	var b strings.Builder
	b.WriteString("digraph kabr {\n")
	b.WriteString("    rankdir=LR;\n")
	b.WriteString("    node [shape=box];\n")
	for i := range n.Tables {
		id := n.Tables[i].ID()
		fmt.Fprintf(&b, "    %s [label=%s];\n", dotQuote(id), dotQuote(id))
	}
	for i := range n.Tables {
		t := &n.Tables[i]
		for _, fk := range t.ForeignKeys {
			if n.Table(fk.RefSchema, fk.RefTable) == nil {
				fmt.Fprintf(&b, "    // external reference %s -> %s : %s\n",
					oneLine(t.ID()), oneLine(fk.RefID()), oneLine(fk.Name))
				continue
			}
			fmt.Fprintf(&b, "    %s -> %s [label=%s];\n",
				dotQuote(t.ID()), dotQuote(fk.RefID()), dotQuote(fk.Name))
		}
	}
	b.WriteString("}\n")
	return b.String()
}

// MermaidIdent sanitizes an identifier for Mermaid: every character outside
// [A-Za-z0-9_] becomes "_", and an identifier that is empty or does not start
// with a letter or "_" is prefixed with "_". Example: "order-items" ->
// "order_items", "2fa" -> "_2fa".
func MermaidIdent(s string) string {
	var b strings.Builder
	for _, r := range s {
		if isWord(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" || (out[0] >= '0' && out[0] <= '9') {
		out = "_" + out
	}
	return out
}

// typeAliases maps verbose SQL type names to their short forms before
// sanitization. Matching is on the lower-cased base type (without modifiers).
var typeAliases = map[string]string{
	"character varying":           "varchar",
	"character":                   "char",
	"timestamp without time zone": "timestamp",
	"timestamp with time zone":    "timestamptz",
	"time without time zone":      "time",
	"time with time zone":         "timetz",
	"double precision":            "float8",
	"bit varying":                 "varbit",
}

// MermaidType turns a SQL data type into a Mermaid attribute type token:
//
//  1. lower-case and trim; a trailing "[]" becomes the suffix "_array";
//  2. a known verbose base name is shortened ("character varying" ->
//     "varchar", "timestamp with time zone" -> "timestamptz", ...);
//  3. every character outside [a-z0-9_] becomes "_" and runs of "_" collapse;
//     leading underscores are dropped, a leading digit gets a "t_" prefix and
//     an empty result becomes "unknown".
//
// Examples: "character varying(45)" -> "varchar_45_", "numeric(10,2)" ->
// "numeric_10_2_", "integer[]" -> "integer_array", "USER-DEFINED" ->
// "user_defined".
func MermaidType(dataType string) string {
	s := strings.ToLower(strings.TrimSpace(dataType))
	suffix := ""
	for strings.HasSuffix(s, "[]") {
		s = strings.TrimSpace(strings.TrimSuffix(s, "[]"))
		suffix += "_array"
	}
	base, mods := s, ""
	if i := strings.IndexByte(s, '('); i >= 0 {
		base, mods = strings.TrimSpace(s[:i]), s[i:]
	}
	if alias, ok := typeAliases[base]; ok {
		base = alias
	}
	s = base + mods + suffix

	var b strings.Builder
	lastUnderscore := false
	for _, r := range s {
		if isWord(r) && r != '_' {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	out := strings.TrimLeft(b.String(), "_")
	switch {
	case out == "":
		return "unknown"
	case out[0] >= '0' && out[0] <= '9':
		return "t_" + out
	}
	return out
}

func normalized(c *schema.Catalog) *schema.Catalog {
	if c == nil {
		return schema.New("", "")
	}
	n := c.Clone()
	n.Normalize()
	return n
}

// entityNames assigns a unique Mermaid entity name to each table.
func entityNames(c *schema.Catalog) map[string]string {
	schemas := map[string]bool{}
	for i := range c.Tables {
		schemas[c.Tables[i].Schema] = true
	}
	names := make(map[string]string, len(c.Tables))
	used := map[string]bool{}
	for i := range c.Tables {
		t := &c.Tables[i]
		raw := t.Name
		if len(schemas) > 1 {
			raw = t.Schema + "__" + t.Name
		}
		base := MermaidIdent(raw)
		name := base
		for k := 2; used[name]; k++ {
			name = fmt.Sprintf("%s_%d", base, k)
		}
		used[name] = true
		names[t.ID()] = name
	}
	return names
}

func keyColumns(t *schema.Table) (pk, fk, uk map[string]bool) {
	pk, fk, uk = map[string]bool{}, map[string]bool{}, map[string]bool{}
	if t.PrimaryKey != nil {
		for _, c := range t.PrimaryKey.Columns {
			pk[c] = true
		}
	}
	for _, f := range t.ForeignKeys {
		for _, c := range f.Columns {
			fk[c] = true
		}
	}
	for _, u := range t.UniqueConstraints {
		if len(u.Columns) == 1 {
			uk[u.Columns[0]] = true
		}
	}
	for _, ix := range t.Indexes {
		if ix.Unique && !ix.Primary && len(ix.Columns) == 1 && t.Column(ix.Columns[0]) != nil {
			uk[ix.Columns[0]] = true
		}
	}
	return pk, fk, uk
}

func mermaidComment(s string) string {
	s = strings.ReplaceAll(s, `"`, "'")
	return strings.Join(strings.Fields(s), " ")
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func dotQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\r", "", "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

func isWord(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}
