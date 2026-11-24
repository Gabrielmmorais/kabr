package plan

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

// fingerprintVersion is mixed into the hash so that a future change of the
// canonical form cannot collide with an old fingerprint.
const fingerprintVersion = "kabr-catalog-fingerprint-v1"

type fpColumn struct {
	Name     string `json:"n"`
	Type     string `json:"t"`
	Nullable bool   `json:"z"`
}

type fpForeignKey struct {
	Columns    []string `json:"c"`
	Ref        string   `json:"r"`
	RefColumns []string `json:"rc"`
}

type fpTable struct {
	ID          string         `json:"id"`
	Columns     []fpColumn     `json:"cols"`
	PrimaryKey  []string       `json:"pk"`
	ForeignKeys []fpForeignKey `json:"fks"`
	Uniques     [][]string     `json:"uq"`
}

// Fingerprint returns "sha256:<hex>" over the structure of the catalog:
// table IDs, columns (name, data type, nullability), primary key columns,
// foreign keys (columns, referenced table and columns) and unique
// constraints. Everything else is ignored: statistics, introspection time,
// comments, defaults, column ordinals, indexes and constraint names. The
// result does not depend on the order of tables, columns or constraints, so
// catalogs with the same structure always share a fingerprint. A nil catalog
// yields "".
func Fingerprint(c *schema.Catalog) string {
	if c == nil {
		return ""
	}
	tables := make([]fpTable, 0, len(c.Tables))
	for i := range c.Tables {
		t := &c.Tables[i]
		ft := fpTable{
			ID:          t.ID(),
			Columns:     make([]fpColumn, 0, len(t.Columns)),
			PrimaryKey:  []string{},
			ForeignKeys: make([]fpForeignKey, 0, len(t.ForeignKeys)),
			Uniques:     make([][]string, 0, len(t.UniqueConstraints)),
		}
		for _, col := range t.Columns {
			ft.Columns = append(ft.Columns, fpColumn{Name: col.Name, Type: col.DataType, Nullable: col.Nullable})
		}
		slices.SortFunc(ft.Columns, func(a, b fpColumn) int { return cmp.Compare(a.Name, b.Name) })
		if t.PrimaryKey != nil {
			ft.PrimaryKey = slices.Clone(t.PrimaryKey.Columns)
		}
		for j := range t.ForeignKeys {
			fk := &t.ForeignKeys[j]
			ft.ForeignKeys = append(ft.ForeignKeys, fpForeignKey{
				Columns:    slices.Clone(fk.Columns),
				Ref:        fk.RefID(),
				RefColumns: slices.Clone(fk.RefColumns),
			})
		}
		slices.SortFunc(ft.ForeignKeys, func(a, b fpForeignKey) int {
			return cmp.Or(slices.Compare(a.Columns, b.Columns), cmp.Compare(a.Ref, b.Ref), slices.Compare(a.RefColumns, b.RefColumns))
		})
		for _, u := range t.UniqueConstraints {
			ft.Uniques = append(ft.Uniques, slices.Clone(u.Columns))
		}
		slices.SortFunc(ft.Uniques, func(a, b []string) int { return strings.Compare(strings.Join(a, "\x00"), strings.Join(b, "\x00")) })
		ft.Uniques = slices.CompactFunc(ft.Uniques, slices.Equal)
		tables = append(tables, ft)
	}
	slices.SortFunc(tables, func(a, b fpTable) int { return cmp.Compare(a.ID, b.ID) })

	h := sha256.New()
	h.Write([]byte(fingerprintVersion + "\n"))
	// Encoding plain strings, bools and slices cannot fail.
	enc := json.NewEncoder(h)
	_ = enc.Encode(tables)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
