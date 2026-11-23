package schema

import (
	"strings"
	"time"
)

// SchemaVersion is the version identifier written into every [Catalog]
// produced by this package.
const SchemaVersion = "kabr.dev/v1alpha1"

// DialectPostgres is the dialect identifier for PostgreSQL catalogs.
const DialectPostgres = "postgres"

// SupportedVersions lists the catalog versions this package can read.
var SupportedVersions = []string{SchemaVersion}

// SupportedDialects lists the dialects this package accepts.
var SupportedDialects = []string{DialectPostgres}

// Catalog is the root document describing the structure of one database.
type Catalog struct {
	// SchemaVersion is the document version, normally [SchemaVersion].
	SchemaVersion string `json:"schemaVersion"`
	// Dialect is the database engine, for example [DialectPostgres].
	Dialect string `json:"dialect"`
	// Database is the name of the introspected database.
	Database string `json:"database"`
	// IntrospectedAt is when the catalog was read. It is optional so that
	// callers can omit it to obtain fully deterministic output.
	IntrospectedAt *time.Time `json:"introspectedAt,omitempty"`
	// Schemas lists the database schemas (namespaces) that were included in
	// the introspection, even those that turned out to be empty.
	Schemas []string `json:"schemas,omitempty"`
	// Tables holds every introspected table.
	Tables []Table `json:"tables"`
}

// Table describes one table.
type Table struct {
	Schema            string             `json:"schema"`
	Name              string             `json:"name"`
	Comment           string             `json:"comment,omitempty"`
	Columns           []Column           `json:"columns"`
	PrimaryKey        *PrimaryKey        `json:"primaryKey,omitempty"`
	ForeignKeys       []ForeignKey       `json:"foreignKeys,omitempty"`
	Indexes           []Index            `json:"indexes,omitempty"`
	UniqueConstraints []UniqueConstraint `json:"uniqueConstraints,omitempty"`
	Stats             TableStats         `json:"stats,omitzero"`
}

// TableStats holds planner statistics for a table. All fields are optional.
type TableStats struct {
	// EstimatedRows is the planner's row estimate (e.g. pg_class.reltuples).
	// Nil means unknown (for example, a never-analyzed table).
	EstimatedRows *int64 `json:"estimatedRows,omitempty"`
}

// Column describes one column of a table.
type Column struct {
	Name string `json:"name"`
	// Ordinal is the 1-based position of the column in the table.
	Ordinal int `json:"ordinal"`
	// DataType is the type as reported by the database, for example
	// "character varying(45)" or "integer[]".
	DataType string `json:"dataType"`
	Nullable bool   `json:"nullable"`
	// Default is the default expression as text; nil means no default.
	Default *string     `json:"default,omitempty"`
	Comment string      `json:"comment,omitempty"`
	Stats   ColumnStats `json:"stats,omitzero"`
}

// ColumnStats holds planner statistics for a column. All fields are optional.
type ColumnStats struct {
	// NDistinct follows pg_stats.n_distinct semantics: a positive value is an
	// estimated count of distinct values, a negative value is the negated
	// fraction of rows that are distinct (-1 means unique). Nil means unknown.
	NDistinct *float64 `json:"nDistinct,omitempty"`
}

// PrimaryKey describes a table's primary key constraint.
type PrimaryKey struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
}

// ForeignKey describes a foreign key constraint. A composite foreign key is
// one ForeignKey with several columns; Columns[i] references RefColumns[i].
type ForeignKey struct {
	Name       string   `json:"name"`
	Columns    []string `json:"columns"`
	RefSchema  string   `json:"refSchema"`
	RefTable   string   `json:"refTable"`
	RefColumns []string `json:"refColumns"`
	// OnDelete and OnUpdate are referential actions in upper case SQL form,
	// for example "NO ACTION", "CASCADE", "SET NULL". Empty means unknown.
	OnDelete string `json:"onDelete,omitempty"`
	OnUpdate string `json:"onUpdate,omitempty"`
}

// Index describes an index. Indexes backing primary keys or unique
// constraints are included as well and flagged accordingly.
type Index struct {
	Name string `json:"name"`
	// Columns are column names or, for expression indexes, the expression
	// text, in index order.
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique,omitempty"`
	Primary bool     `json:"primary,omitempty"`
	// Method is the access method, for example "btree", "hash", "gin".
	Method string `json:"method,omitempty"`
}

// UniqueConstraint describes a UNIQUE constraint.
type UniqueConstraint struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
}

// TableID returns the canonical identifier of a table: "schema.name".
func TableID(schema, name string) string {
	return schema + "." + name
}

// ID returns the canonical identifier of the table (see [TableID]).
func (t *Table) ID() string {
	return TableID(t.Schema, t.Name)
}

// RefID returns the canonical identifier of the referenced table.
func (fk *ForeignKey) RefID() string {
	return TableID(fk.RefSchema, fk.RefTable)
}

// Column returns the column with the given name, or nil.
func (t *Table) Column(name string) *Column {
	for i := range t.Columns {
		if t.Columns[i].Name == name {
			return &t.Columns[i]
		}
	}
	return nil
}

// IsUniqueKey reports whether the given column set (order-insensitive) is
// guaranteed unique in the table: it equals the primary key, a unique
// constraint, or a unique index.
func (t *Table) IsUniqueKey(cols []string) bool {
	if len(cols) == 0 {
		return false
	}
	if t.PrimaryKey != nil && sameSet(t.PrimaryKey.Columns, cols) {
		return true
	}
	for _, u := range t.UniqueConstraints {
		if sameSet(u.Columns, cols) {
			return true
		}
	}
	for _, ix := range t.Indexes {
		if (ix.Unique || ix.Primary) && sameSet(ix.Columns, cols) {
			return true
		}
	}
	return false
}

// Table returns the table with the given schema and name, or nil.
func (c *Catalog) Table(schema, name string) *Table {
	for i := range c.Tables {
		if c.Tables[i].Schema == schema && c.Tables[i].Name == name {
			return &c.Tables[i]
		}
	}
	return nil
}

// New returns an empty catalog with the current [SchemaVersion] set.
func New(dialect, database string) *Catalog {
	return &Catalog{
		SchemaVersion: SchemaVersion,
		Dialect:       dialect,
		Database:      database,
		Tables:        []Table{},
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		if seen[s] == 0 {
			return false
		}
		seen[s]--
	}
	return true
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }
