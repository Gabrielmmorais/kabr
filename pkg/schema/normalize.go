package schema

import (
	"cmp"
	"slices"
)

// Normalize sorts the catalog in place into its canonical order so that
// serialized output is deterministic regardless of the order in which an
// introspector discovered objects:
//
//   - Schemas: sorted, duplicates removed.
//   - Tables: by schema, then name.
//   - Columns: by ordinal, then name.
//   - Foreign keys, indexes and unique constraints: by name, then by their
//     column lists as a tie-breaker.
//   - A nil Tables or Columns slice becomes an empty slice (serialized as []).
//
// Column lists inside constraints and indexes are never reordered because
// their order is meaningful. Normalize is idempotent.
func (c *Catalog) Normalize() {
	if c == nil {
		return
	}
	if c.Schemas != nil {
		slices.Sort(c.Schemas)
		c.Schemas = slices.Compact(c.Schemas)
	}
	if c.Tables == nil {
		c.Tables = []Table{}
	}
	if c.IntrospectedAt != nil {
		utc := c.IntrospectedAt.UTC()
		c.IntrospectedAt = &utc
	}
	slices.SortStableFunc(c.Tables, func(a, b Table) int {
		return cmp.Or(cmp.Compare(a.Schema, b.Schema), cmp.Compare(a.Name, b.Name))
	})
	for i := range c.Tables {
		t := &c.Tables[i]
		if t.Columns == nil {
			t.Columns = []Column{}
		}
		slices.SortStableFunc(t.Columns, func(a, b Column) int {
			return cmp.Or(cmp.Compare(a.Ordinal, b.Ordinal), cmp.Compare(a.Name, b.Name))
		})
		slices.SortStableFunc(t.ForeignKeys, func(a, b ForeignKey) int {
			return cmp.Or(cmp.Compare(a.Name, b.Name), slices.Compare(a.Columns, b.Columns))
		})
		slices.SortStableFunc(t.Indexes, func(a, b Index) int {
			return cmp.Or(cmp.Compare(a.Name, b.Name), slices.Compare(a.Columns, b.Columns))
		})
		slices.SortStableFunc(t.UniqueConstraints, func(a, b UniqueConstraint) int {
			return cmp.Or(cmp.Compare(a.Name, b.Name), slices.Compare(a.Columns, b.Columns))
		})
	}
}

// Clone returns a deep copy of the catalog. It returns nil for a nil catalog.
func (c *Catalog) Clone() *Catalog {
	if c == nil {
		return nil
	}
	out := *c
	if c.IntrospectedAt != nil {
		ts := *c.IntrospectedAt
		out.IntrospectedAt = &ts
	}
	out.Schemas = slices.Clone(c.Schemas)
	if c.Tables != nil {
		out.Tables = make([]Table, len(c.Tables))
		for i := range c.Tables {
			out.Tables[i] = c.Tables[i].clone()
		}
	}
	return &out
}

func (t Table) clone() Table {
	out := t
	if t.Columns != nil {
		out.Columns = make([]Column, len(t.Columns))
		for i, col := range t.Columns {
			col.Default = clonePtr(col.Default)
			col.Stats.NDistinct = clonePtr(col.Stats.NDistinct)
			out.Columns[i] = col
		}
	}
	if t.PrimaryKey != nil {
		pk := *t.PrimaryKey
		pk.Columns = slices.Clone(pk.Columns)
		out.PrimaryKey = &pk
	}
	if t.ForeignKeys != nil {
		out.ForeignKeys = make([]ForeignKey, len(t.ForeignKeys))
		for i, fk := range t.ForeignKeys {
			fk.Columns = slices.Clone(fk.Columns)
			fk.RefColumns = slices.Clone(fk.RefColumns)
			out.ForeignKeys[i] = fk
		}
	}
	if t.Indexes != nil {
		out.Indexes = make([]Index, len(t.Indexes))
		for i, ix := range t.Indexes {
			ix.Columns = slices.Clone(ix.Columns)
			out.Indexes[i] = ix
		}
	}
	if t.UniqueConstraints != nil {
		out.UniqueConstraints = make([]UniqueConstraint, len(t.UniqueConstraints))
		for i, u := range t.UniqueConstraints {
			u.Columns = slices.Clone(u.Columns)
			out.UniqueConstraints[i] = u
		}
	}
	out.Stats.EstimatedRows = clonePtr(t.Stats.EstimatedRows)
	return out
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
