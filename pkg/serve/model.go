package serve

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Gabrielmmorais/kabr/pkg/plan"
	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

// treatment is what the runtime does with an exposed column.
type treatment int

const (
	treatPlain treatment = iota
	treatMask            // '***' (or NULL), computed in SQL
	treatHash            // HMAC-SHA256 hex of the text form, computed here
)

// column is one exposed column of a served table. Dropped columns are not
// represented at all.
type column struct {
	name     string // catalog name, also the JSON property name
	ident    string // quoted identifier (pgx.Identifier.Sanitize)
	dataType string // catalog data type: documentation only, never SQL
	kind     kind
	elem     kind // element kind of an array
	nullable bool
	treat    treatment
	// sensitive marks any column listed in sensitiveColumns (masked or
	// hashed when exposed): never filterable.
	sensitive bool
}

// filterable reports whether ?<name>=value is accepted for the column.
func (c *column) filterable() bool {
	return !c.sensitive && c.treat == treatPlain && c.kind.filterable() &&
		c.name != paramLimit && c.name != paramCursor
}

// table is one served table: allowlisted, with a usable primary key.
type table struct {
	schema, name string
	id           string // "schema.table"
	ident        string // quoted "schema"."table"
	columns      []column
	byName       map[string]int // column name -> index in columns
	pk           []int          // indexes of the primary key columns, key order
}

func (t *table) column(name string) *column {
	if i, ok := t.byName[name]; ok {
		return &t.columns[i]
	}
	return nil
}

// model is the resolved, immutable allowlist the handlers serve.
type model struct {
	cfg      *Config
	rowLimit int
	tables   []*table             // sorted by id
	byName   map[[2]string]*table // {schema, table} -> table
	excluded []string             // table IDs left out (warnings explain why)
	hashKey  []byte
}

// table returns the allowlisted table with exactly this schema and name.
// Lookups are by the (schema, table) pair, not by "schema.table", so a
// name containing a dot cannot alias another table.
func (m *model) table(sch, name string) *table {
	return m.byName[[2]string{sch, name}]
}

// resolve matches the configuration against a catalog (whose fingerprint
// was already verified) and returns the served model and warnings. Errors
// are configuration problems that make the domain unservable: a configured
// table or sensitive column missing from the catalog, or no servable table.
// hashKey enables the hash action; without it hashed columns are dropped.
func resolve(cfg *Config, cat *schema.Catalog, hashKey []byte, rowLimit int) (*model, []string, error) {
	m := &model{cfg: cfg, rowLimit: rowLimit, byName: map[[2]string]*table{}, hashKey: hashKey}
	var warnings []string
	var errs []error

	sensitive := map[string]map[string]plan.SensitiveColumn{}
	for _, sc := range cfg.SensitiveColumns {
		if sensitive[sc.Table] == nil {
			sensitive[sc.Table] = map[string]plan.SensitiveColumn{}
		}
		sensitive[sc.Table][sc.Column] = sc
	}

	for _, id := range cfg.Tables {
		sch, name, _ := splitTableID(id)
		ct := cat.Table(sch, name)
		if ct == nil {
			errs = append(errs, fmt.Errorf("table %s not found in the catalog", id))
			continue
		}
		for col := range sensitive[id] {
			if ct.Column(col) == nil {
				errs = append(errs, fmt.Errorf("sensitive column %s.%s not found in the catalog", id, col))
			}
		}
		t, why := buildTable(ct, sensitive[id], hashKey != nil, &warnings)
		if t == nil {
			m.excluded = append(m.excluded, id)
			warnings = append(warnings, fmt.Sprintf("table %s is not served: %s", id, why))
			continue
		}
		m.tables = append(m.tables, t)
		m.byName[[2]string{t.schema, t.name}] = t
	}
	if len(errs) > 0 {
		return nil, warnings, errors.Join(errs...)
	}
	if len(m.tables) == 0 {
		return nil, warnings, errors.New("no servable table (see the warnings in the log)")
	}
	slices.SortFunc(m.tables, func(a, b *table) int { return strings.Compare(a.id, b.id) })
	return m, warnings, nil
}

// buildTable applies the sensitive-column rules to one catalog table. It
// returns nil and the reason when the table cannot be served.
func buildTable(ct *schema.Table, sens map[string]plan.SensitiveColumn, haveHashKey bool, warnings *[]string) (*table, string) {
	if ct.PrimaryKey == nil || len(ct.PrimaryKey.Columns) == 0 {
		return nil, "it has no primary key (keyset pagination needs one)"
	}
	t := &table{
		schema: ct.Schema,
		name:   ct.Name,
		id:     ct.ID(),
		ident:  pgx.Identifier{ct.Schema, ct.Name}.Sanitize(),
		byName: map[string]int{},
	}
	cols := slices.Clone(ct.Columns)
	slices.SortStableFunc(cols, func(a, b schema.Column) int { return a.Ordinal - b.Ordinal })
	for _, cc := range cols {
		k, elem := kindOf(cc.DataType)
		c := column{
			name:     cc.Name,
			ident:    pgx.Identifier{cc.Name}.Sanitize(),
			dataType: cc.DataType,
			kind:     k,
			elem:     elem,
			nullable: cc.Nullable,
		}
		if sc, ok := sens[cc.Name]; ok {
			c.sensitive = true
			switch {
			case sc.Class == plan.ClassCredential:
				continue // always dropped, whatever the action
			case sc.Action == plan.ActionMask:
				c.treat = treatMask
			case sc.Action == plan.ActionHash && haveHashKey:
				c.treat = treatHash
			case sc.Action == plan.ActionHash:
				*warnings = append(*warnings, fmt.Sprintf(
					"column %s.%s (action hash) is dropped: no hash key configured", t.id, cc.Name))
				continue
			default: // drop, or anything unexpected: fail closed
				continue
			}
		}
		t.byName[c.name] = len(t.columns)
		t.columns = append(t.columns, c)
	}
	for _, pkName := range ct.PrimaryKey.Columns {
		if _, ok := sens[pkName]; ok {
			return nil, fmt.Sprintf("primary key column %q is sensitive (a key or cursor would expose it)", pkName)
		}
		i, ok := t.byName[pkName]
		if !ok {
			return nil, fmt.Sprintf("primary key column %q is not in the catalog", pkName)
		}
		if !t.columns[i].kind.keyable() {
			return nil, fmt.Sprintf("primary key column %q has type %s, which cannot be paginated", pkName, t.columns[i].dataType)
		}
		t.pk = append(t.pk, i)
	}
	return t, ""
}
