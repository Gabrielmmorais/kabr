package schema

import (
	"errors"
	"fmt"
	"slices"
)

// Validate checks the structural integrity of the catalog and returns all
// problems found, joined with [errors.Join]. It returns nil for a valid
// catalog. Validate does not modify the catalog.
//
// Rules:
//   - SchemaVersion is one of [SupportedVersions]; Dialect is one of
//     [SupportedDialects]; Database is not blank.
//   - Every table, column and constraint has a non-blank name; table IDs and
//     column names within a table are unique.
//   - Primary key and unique constraint columns exist in their table.
//   - Index column lists are non-empty with non-blank entries (entries are
//     not checked against columns because they may be expressions).
//   - Foreign keys have at least one column, len(Columns) == len(RefColumns),
//     their local columns exist, and, when the referenced table is part of
//     the catalog, the referenced columns exist in it. References to tables
//     outside the catalog are allowed (partial introspection).
func (c *Catalog) Validate() error {
	if c == nil {
		return errors.New("catalog is nil")
	}
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if !slices.Contains(SupportedVersions, c.SchemaVersion) {
		add("unsupported schemaVersion %q (supported: %v)", c.SchemaVersion, SupportedVersions)
	}
	if !slices.Contains(SupportedDialects, c.Dialect) {
		add("unsupported dialect %q (supported: %v)", c.Dialect, SupportedDialects)
	}
	if blank(c.Database) {
		add("database name is empty")
	}
	for i, s := range c.Schemas {
		if blank(s) {
			add("schemas[%d]: name is empty", i)
		}
	}

	tables := make(map[string]*Table, len(c.Tables))
	for i := range c.Tables {
		t := &c.Tables[i]
		if blank(t.Schema) || blank(t.Name) {
			add("tables[%d]: schema and name must not be empty (got %q)", i, t.ID())
			continue
		}
		if _, dup := tables[t.ID()]; dup {
			add("table %s: duplicate table", t.ID())
			continue
		}
		tables[t.ID()] = t
	}

	for i := range c.Tables {
		t := &c.Tables[i]
		id := t.ID()
		cols := make(map[string]bool, len(t.Columns))
		for j, col := range t.Columns {
			if blank(col.Name) {
				add("table %s: columns[%d]: name is empty", id, j)
				continue
			}
			if cols[col.Name] {
				add("table %s: duplicate column %q", id, col.Name)
			}
			cols[col.Name] = true
			if blank(col.DataType) {
				add("table %s: column %q: dataType is empty", id, col.Name)
			}
		}
		checkCols := func(what string, list []string) {
			if len(list) == 0 {
				add("table %s: %s has no columns", id, what)
			}
			for _, name := range list {
				if !cols[name] {
					add("table %s: %s references unknown column %q", id, what, name)
				}
			}
		}

		if pk := t.PrimaryKey; pk != nil {
			if blank(pk.Name) {
				add("table %s: primary key name is empty", id)
			}
			checkCols("primary key "+quote(pk.Name), pk.Columns)
		}
		for j, u := range t.UniqueConstraints {
			if blank(u.Name) {
				add("table %s: uniqueConstraints[%d]: name is empty", id, j)
			}
			checkCols("unique constraint "+quote(u.Name), u.Columns)
		}
		for j, ix := range t.Indexes {
			if blank(ix.Name) {
				add("table %s: indexes[%d]: name is empty", id, j)
			}
			if len(ix.Columns) == 0 {
				add("table %s: index %q has no columns", id, ix.Name)
			}
			for _, e := range ix.Columns {
				if blank(e) {
					add("table %s: index %q has an empty column entry", id, ix.Name)
				}
			}
		}
		for j, fk := range t.ForeignKeys {
			label := "foreign key " + quote(fk.Name)
			if blank(fk.Name) {
				add("table %s: foreignKeys[%d]: name is empty", id, j)
			}
			if blank(fk.RefSchema) || blank(fk.RefTable) {
				add("table %s: %s: refSchema and refTable must not be empty", id, label)
			}
			if len(fk.Columns) != len(fk.RefColumns) {
				add("table %s: %s: %d columns but %d refColumns", id, label, len(fk.Columns), len(fk.RefColumns))
			}
			checkCols(label, fk.Columns)
			ref, ok := tables[fk.RefID()]
			if !ok {
				continue
			}
			for _, rc := range fk.RefColumns {
				if ref.Column(rc) == nil {
					add("table %s: %s references unknown column %q in %s", id, label, rc, ref.ID())
				}
			}
		}
	}
	return errors.Join(errs...)
}

func quote(s string) string { return fmt.Sprintf("%q", s) }
