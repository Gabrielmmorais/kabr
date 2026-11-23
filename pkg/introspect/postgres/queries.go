package postgres

// Every query in this file reads only system catalogs: relations in
// pg_catalog (including the pg_stats view) and set-returning functions.
// None of them reads a user table. TestQueriesReadOnlySystemCatalogs
// enforces that every FROM/JOIN target is pg_catalog-qualified or a function
// call, and the integration test proves introspection works for a role with
// no SELECT privilege on any user table.
//
// $1 is always either the list of target schema names (text[]) or the list
// of table OIDs (oid[]) obtained from qTables.

const qCurrentDatabase = `SELECT pg_catalog.current_database()::text`

// qTables lists ordinary (r) and partitioned (p) tables. Views and
// materialized views are out of scope for M1. $2 includes partitions.
const qTables = `
SELECT c.oid, n.nspname::text, c.relname::text,
       coalesce(pg_catalog.obj_description(c.oid, 'pg_class'), ''),
       c.reltuples::float8, c.relpages::int8
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p')
  AND n.nspname = ANY($1::text[])
  AND ($2::bool OR NOT c.relispartition)`

const qColumns = `
SELECT a.attrelid, a.attname::text, a.attnum::int,
       pg_catalog.format_type(a.atttypid, a.atttypmod),
       NOT a.attnotnull,
       pg_catalog.pg_get_expr(d.adbin, d.adrelid),
       coalesce(pg_catalog.col_description(a.attrelid, a.attnum), '')
FROM pg_catalog.pg_attribute a
LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
WHERE a.attrelid = ANY($1::oid[]) AND a.attnum > 0 AND NOT a.attisdropped`

// qConstraints returns primary keys (p), unique constraints (u) and foreign
// keys (f) with column names in key order. For a foreign key that
// references a partitioned table, PostgreSQL also stores one internal child
// constraint per referenced partition (conparentid <> 0, confrelid = the
// partition); those are skipped so the FK appears once, pointing at the
// partitioned table.
const qConstraints = `
SELECT con.conrelid, con.conname::text, con.contype::text,
       ARRAY(SELECT a.attname::text
             FROM unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord)
             JOIN pg_catalog.pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.attnum
             ORDER BY k.ord),
       coalesce(rn.nspname::text, ''), coalesce(rc.relname::text, ''),
       ARRAY(SELECT a.attname::text
             FROM unnest(con.confkey) WITH ORDINALITY AS k(attnum, ord)
             JOIN pg_catalog.pg_attribute a ON a.attrelid = con.confrelid AND a.attnum = k.attnum
             ORDER BY k.ord),
       con.confdeltype::text, con.confupdtype::text
FROM pg_catalog.pg_constraint con
LEFT JOIN pg_catalog.pg_class rc ON rc.oid = con.confrelid
LEFT JOIN pg_catalog.pg_namespace rn ON rn.oid = rc.relnamespace
WHERE con.conrelid = ANY($1::oid[])
  AND con.contype IN ('p', 'u', 'f')
  AND (con.contype <> 'f' OR con.conparentid = 0 OR NOT rc.relispartition)`

// qIndexes lists indexes with their key columns (INCLUDE columns are left
// out): a column name, or the expression text for expression keys.
const qIndexes = `
SELECT i.indrelid, ic.relname::text, i.indisunique, i.indisprimary,
       coalesce(am.amname::text, ''),
       ARRAY(SELECT CASE WHEN i.indkey[k.n - 1] <> 0 THEN a.attname::text
                         ELSE pg_catalog.pg_get_indexdef(i.indexrelid, k.n, true) END
             FROM generate_series(1, i.indnkeyatts) AS k(n)
             LEFT JOIN pg_catalog.pg_attribute a
               ON a.attrelid = i.indrelid AND a.attnum = i.indkey[k.n - 1]
             ORDER BY k.n)
FROM pg_catalog.pg_index i
JOIN pg_catalog.pg_class ic ON ic.oid = i.indexrelid
LEFT JOIN pg_catalog.pg_am am ON am.oid = ic.relam
WHERE i.indrelid = ANY($1::oid[])`

// qColumnStats reads n_distinct from the pg_stats view. pg_stats only shows
// columns the current role may SELECT, so a strictly catalog-only role gets
// no rows here (reported as unknown, not as an error). When both exist,
// non-inherited stats win; partitioned tables only have inherited ones.
const qColumnStats = `
SELECT DISTINCT ON (s.schemaname, s.tablename, s.attname)
       s.schemaname::text, s.tablename::text, s.attname::text, s.n_distinct::float8
FROM pg_catalog.pg_stats s
WHERE s.schemaname = ANY($1::text[]) AND s.n_distinct IS NOT NULL
ORDER BY s.schemaname, s.tablename, s.attname, s.inherited`

const qRole = `
SELECT r.rolname::text, r.rolsuper, r.rolbypassrls, r.rolcreaterole,
       r.rolcreatedb, r.rolreplication,
       pg_catalog.current_database()::text,
       pg_catalog.has_database_privilege(pg_catalog.current_database(), 'CREATE')
FROM pg_catalog.pg_roles r
WHERE r.rolname = current_user`

const qSchemaPrivileges = `
SELECT n.nspname::text, pg_catalog.has_schema_privilege(n.oid, 'CREATE')
FROM pg_catalog.pg_namespace n
WHERE n.nspname = ANY($1::text[])`

// qTablePrivileges checks every table and partition in the target schemas
// in one round trip. has_table_privilege accounts for PUBLIC and inherited
// role membership.
const qTablePrivileges = `
SELECT n.nspname::text, c.relname::text,
       pg_catalog.has_table_privilege(c.oid, 'INSERT'),
       pg_catalog.has_table_privilege(c.oid, 'UPDATE'),
       pg_catalog.has_table_privilege(c.oid, 'DELETE'),
       pg_catalog.has_table_privilege(c.oid, 'TRUNCATE'),
       pg_catalog.has_table_privilege(c.oid, 'REFERENCES'),
       pg_catalog.has_table_privilege(c.oid, 'TRIGGER')
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p') AND n.nspname = ANY($1::text[])`

// allQueries is used by the static catalog-only test.
var allQueries = map[string]string{
	"qCurrentDatabase":  qCurrentDatabase,
	"qTables":           qTables,
	"qColumns":          qColumns,
	"qConstraints":      qConstraints,
	"qIndexes":          qIndexes,
	"qColumnStats":      qColumnStats,
	"qRole":             qRole,
	"qSchemaPrivileges": qSchemaPrivileges,
	"qTablePrivileges":  qTablePrivileges,
}
