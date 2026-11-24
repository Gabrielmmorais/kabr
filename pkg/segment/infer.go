package segment

import (
	"cmp"
	"slices"
	"strings"

	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

// inferEdges proposes references that the catalog does not declare as
// foreign keys, from naming conventions only. The catalog must be normalized.
//
// For every column named "X_id" (case-insensitive) that is not part of any
// declared foreign key of its table, the stem X and every suffix of it obtained
// by dropping leading "prefix_" segments (manager_staff → staff) are tried,
// longest first. A stem matches a table of the SAME schema named X, Xs, Xes,
// Xies (for X ending in "y") or X without a trailing "s", in that order, whose
// primary key is the single column "X_id" or "id". The referencing column and
// the key must belong to compatible type families (integers, strings, UUIDs)
// when both types are recognized. A table never references itself.
func inferEdges(c *schema.Catalog, weight float64) []InferredEdge {
	bySchema := map[string]map[string]*schema.Table{}
	for i := range c.Tables {
		t := &c.Tables[i]
		if bySchema[t.Schema] == nil {
			bySchema[t.Schema] = map[string]*schema.Table{}
		}
		key := strings.ToLower(t.Name)
		if _, dup := bySchema[t.Schema][key]; !dup { // tables are sorted: first wins
			bySchema[t.Schema][key] = t
		}
	}

	out := []InferredEdge{}
	for i := range c.Tables {
		t := &c.Tables[i]
		covered := map[string]bool{}
		for _, fk := range t.ForeignKeys {
			for _, col := range fk.Columns {
				covered[col] = true
			}
		}
		for _, col := range t.Columns {
			if covered[col.Name] {
				continue
			}
			target := matchColumn(bySchema[t.Schema], t, col)
			if target == nil {
				continue
			}
			out = append(out, InferredEdge{From: t.ID(), To: target.ID(), Column: col.Name, Weight: weight})
		}
	}
	slices.SortFunc(out, func(a, b InferredEdge) int {
		return cmp.Or(cmp.Compare(a.From, b.From), cmp.Compare(a.Column, b.Column))
	})
	return out
}

func matchColumn(tables map[string]*schema.Table, self *schema.Table, col schema.Column) *schema.Table {
	name := strings.ToLower(col.Name)
	stem, ok := strings.CutSuffix(name, "_id")
	if !ok || stem == "" {
		return nil
	}
	for {
		for _, cand := range nameVariants(stem) {
			t := tables[cand]
			if t == nil || t == self {
				continue
			}
			if pk := singlePK(t); pk != nil && (strings.EqualFold(pk.Name, stem+"_id") || strings.EqualFold(pk.Name, "id")) &&
				compatibleTypes(col.DataType, pk.DataType) {
				return t
			}
		}
		_, rest, found := strings.Cut(stem, "_")
		if !found || rest == "" {
			return nil
		}
		stem = rest
	}
}

func nameVariants(stem string) []string {
	v := []string{stem, stem + "s", stem + "es"}
	if base, ok := strings.CutSuffix(stem, "y"); ok && base != "" {
		v = append(v, base+"ies")
	}
	if base, ok := strings.CutSuffix(stem, "s"); ok && base != "" {
		v = append(v, base)
	}
	return v
}

func singlePK(t *schema.Table) *schema.Column {
	if t.PrimaryKey == nil || len(t.PrimaryKey.Columns) != 1 {
		return nil
	}
	return t.Column(t.PrimaryKey.Columns[0])
}

// typeFamily maps a SQL type to a coarse family used to reject implausible
// inferred references; unknown types map to "".
func typeFamily(dataType string) string {
	d := strings.ToLower(strings.TrimSpace(dataType))
	if i := strings.IndexByte(d, '('); i >= 0 {
		d = strings.TrimSpace(d[:i])
	}
	switch d {
	case "smallint", "integer", "int", "bigint", "int2", "int4", "int8", "tinyint", "mediumint",
		"serial", "smallserial", "bigserial", "serial4", "serial8", "numeric", "decimal", "number":
		return "int"
	case "uuid", "uniqueidentifier":
		return "uuid"
	case "text", "varchar", "character varying", "char", "character", "nvarchar", "nchar", "string", "citext":
		return "str"
	}
	return ""
}

func compatibleTypes(a, b string) bool {
	fa, fb := typeFamily(a), typeFamily(b)
	return fa == "" || fb == "" || fa == fb
}
