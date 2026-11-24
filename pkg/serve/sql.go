package serve

import (
	"strconv"
	"strings"
)

// Reserved query parameter names of the list endpoint. A column with one of
// these names is not filterable.
const (
	paramLimit  = "limit"
	paramCursor = "cursor"
)

// Every SQL statement of the runtime is assembled in this file from three
// kinds of fragments only:
//
//   - fixed text written here (keywords, pg_catalog-qualified functions and
//     casts, the '***' mask literal, $n placeholders);
//   - identifiers of allowlisted, catalog-resolved tables and columns,
//     quoted with pgx.Identifier.Sanitize when the model is resolved
//     (table.ident, column.ident);
//   - placeholder numbers computed here.
//
// Request input (path segments, query keys and values, cursors, limits) is
// never written into the text: query keys only select an allowlisted column
// by exact name, and every value is appended to the argument list.

// projection is the SELECT-list item of a column.
func (c *column) projection() string {
	switch {
	case c.treat == treatMask:
		return "CASE WHEN " + c.ident + " IS NULL THEN NULL ELSE '***' END AS " + c.ident
	case c.treat == treatHash, c.kind.textProjected():
		return c.ident + "::pg_catalog.text AS " + c.ident
	case c.kind == kindArray:
		return "pg_catalog.array_to_json(" + c.ident + ")::pg_catalog.text AS " + c.ident
	default:
		return c.ident
	}
}

// compareExpr is the expression that keys and filters compare with a
// parameter: the column itself for built-in types (pg_catalog operators,
// the server infers the parameter type from it), its text form otherwise.
// ORDER BY uses the same expression, so pagination order and the keyset
// predicate always agree.
func (c *column) compareExpr() string {
	if c.kind.nativeCompare() {
		return c.ident
	}
	return c.ident + "::pg_catalog.text"
}

func (t *table) selectList() string {
	items := make([]string, len(t.columns))
	for i := range t.columns {
		items[i] = t.columns[i].projection()
	}
	return strings.Join(items, ", ")
}

func (t *table) keyExprs() []string {
	keys := make([]string, len(t.pk))
	for i, ci := range t.pk {
		keys[i] = t.columns[ci].compareExpr()
	}
	return keys
}

// filterArg is one validated equality filter: a column index and the raw
// request value (bound as a parameter, never written into SQL).
type filterArg struct {
	col   int
	value string
}

// listQuery is a validated list request.
type listQuery struct {
	filters []filterArg // sorted by column index
	after   []string    // key values of the cursor; nil on the first page
	limit   int         // rows to return, 1..rowLimit
}

// sqlArgs accumulates bind parameters and hands out their placeholders.
type sqlArgs struct{ values []any }

func (a *sqlArgs) add(v any) string {
	a.values = append(a.values, v)
	return "$" + strconv.Itoa(len(a.values))
}

// buildList returns the keyset-paginated list statement of t and its
// arguments. It fetches limit+1 rows so the caller can tell whether a next
// page exists without returning more than limit rows.
func buildList(t *table, q listQuery) (string, []any) {
	var b strings.Builder
	var args sqlArgs
	b.WriteString("SELECT ")
	b.WriteString(t.selectList())
	b.WriteString(" FROM ")
	b.WriteString(t.ident)
	var conds []string
	for _, f := range q.filters {
		conds = append(conds, t.columns[f.col].compareExpr()+" = "+args.add(f.value))
	}
	keys := t.keyExprs()
	if q.after != nil {
		params := make([]string, len(q.after))
		for i, v := range q.after {
			params[i] = args.add(v)
		}
		if len(keys) == 1 {
			conds = append(conds, keys[0]+" > "+params[0])
		} else {
			conds = append(conds, "("+strings.Join(keys, ", ")+") > ("+strings.Join(params, ", ")+")")
		}
	}
	if len(conds) > 0 {
		b.WriteString(" WHERE ")
		b.WriteString(strings.Join(conds, " AND "))
	}
	b.WriteString(" ORDER BY ")
	b.WriteString(strings.Join(keys, " ASC, "))
	b.WriteString(" ASC LIMIT ")
	b.WriteString(args.add(q.limit + 1))
	return b.String(), args.values
}

// buildGet returns the statement that reads one row of t by the value of its
// single-column primary key.
func buildGet(t *table, key string) (string, []any) {
	var args sqlArgs
	sql := "SELECT " + t.selectList() + " FROM " + t.ident +
		" WHERE " + t.columns[t.pk[0]].compareExpr() + " = " + args.add(key) + " LIMIT 1"
	return sql, args.values
}

// buildProbe returns a statement that reads no row but needs SELECT on every
// selected column, so a missing privilege shows up in readiness.
func buildProbe(t *table) string {
	return "SELECT " + t.selectList() + " FROM " + t.ident + " LIMIT 0"
}
