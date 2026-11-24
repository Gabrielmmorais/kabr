// Package serve is the generic, read-only REST runtime of one domain of an
// approved [plan.DomainPlan]: "kabr serve". It has no Kubernetes dependency;
// the M3 operator runs one process per domain and renders its [Config] into
// a mounted JSON file.
//
// # Endpoints
//
//	GET /v1/{schema}/{table}        list rows: keyset pagination over the
//	                                primary key (?limit=, ?cursor=) and
//	                                equality filters (?column=value, ANDed)
//	GET /v1/{schema}/{table}/{pk}   one row by primary key; only tables with a
//	                                single-column primary key (composite-key
//	                                tables are list-only: filter on the key
//	                                columns instead)
//	GET /openapi.json               OpenAPI 3.0.3 document of the served tables
//	GET /healthz                    the process is alive
//	GET /readyz                     the database answers and the catalog still
//	                                has the approved fingerprint
//
// Only GET and HEAD are accepted. Errors are JSON objects
// {"error": "<code>", "message": "..."}; they never contain SQL text, the
// DSN or driver internals.
//
// # Safety model
//
// Catalog: at startup and every recheck interval the catalog is introspected
// with the M1 introspector and its [plan.Fingerprint] compared with
// [Config.CatalogFingerprint]. The served model (routes, columns, OpenAPI) is
// only ever built from a catalog whose fingerprint matches; on drift, on a
// configured table that does not exist or on a database error the process
// keeps running but /readyz and every data endpoint answer 503. While not
// ready the check is retried on a short interval. /readyz discloses only a
// coarse category (starting, database_unavailable, catalog_drift,
// config_error, table_unreadable, internal_error); the detailed reason is
// logged once per change.
//
// Allowlist: only the configured tables are served, and only their columns
// that survive the sensitive-column rules. Tables without a primary key (no
// key to paginate on), with a sensitive or non-orderable key column are left
// out with a logged warning.
//
// Sensitive columns fail closed: class credential is always dropped,
// whatever its action; action drop is never selected; action mask is applied
// in SQL (CASE WHEN c IS NULL THEN NULL ELSE '***' END) so the raw value
// never leaves the database; action hash is HMAC-SHA256 (hex) with a
// configured key, and is treated as drop when no key is configured.
// Sensitive columns are never filterable (a filter would be an oracle).
//
// SQL: statements are assembled only from catalog-resolved identifiers of
// allowlisted tables and columns, quoted with [pgx.Identifier.Sanitize], and
// fixed fragments of this package (pg_catalog-qualified functions and
// casts). No request string and no catalog type name is ever part of the SQL
// text: every request value (filter, key, cursor, limit) is a bind
// parameter. String parameters are sent in text format and parsed by the
// server according to the type it infers from the compared column, so a
// value that is not valid for the column type is a client error (400).
//
// Execution: the connection pool opens sessions with
// default_transaction_read_only=on and search_path=pg_catalog; every request
// runs in BEGIN READ ONLY with a transaction-local statement_timeout and is
// always rolled back. Responses never hold more than the row limit.
//
// # Value encoding
//
// Integers, floats and booleans are JSON numbers and booleans (NaN and
// infinities as strings); numeric is a string (exact); timestamps are
// RFC 3339 strings in UTC ("infinity" and "-infinity" kept); bytea is base64;
// json and jsonb are embedded as JSON; arrays are JSON arrays (elements as
// PostgreSQL's array_to_json renders them); every other type (text, uuid,
// date, enums, domains, tsvector, ...) is its PostgreSQL text form. Tables
// whose key column has a non-built-in type (enum, domain, ...) are paginated
// in the text order of that column.
package serve
