// Package postgres implements [introspect.Introspector] for PostgreSQL using
// pgx v5. Importing it registers the "postgres" dialect with
// [introspect.Open].
//
// Safety model (defense in depth):
//   - The session is opened with default_transaction_read_only=on, a
//     statement_timeout and application_name=kabr.
//   - Every call runs inside BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY
//     (one consistent catalog snapshot) and is rolled back afterwards.
//   - Queries only read pg_catalog (see queries.go); no user table row is
//     ever selected.
//
// Scope decisions for M1:
//   - Ordinary and partitioned tables are reported. Partitions are omitted
//     unless [introspect.Options.IncludePartitions] is set, in which case they
//     appear as ordinary tables (the catalog model has no partition field).
//   - Views, materialized views, foreign tables, sequences and types are not
//     reported.
//   - Index columns are the key columns only (INCLUDE columns are omitted);
//     expression keys are rendered with pg_get_indexdef.
//   - Column data types come from format_type, which schema-qualifies types
//     not visible on the session search_path (left at the server default).
package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Gabrielmmorais/kabr/pkg/introspect"
	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

func init() {
	introspect.Register(schema.DialectPostgres, func(ctx context.Context, dsn string, cfg introspect.Config) (introspect.Introspector, error) {
		return Open(ctx, dsn, cfg)
	})
}

// Introspector reads a PostgreSQL catalog over a single connection. It is
// not safe for concurrent use.
type Introspector struct {
	conn     *pgx.Conn
	password string // used only to scrub error messages
}

var _ introspect.Introspector = (*Introspector)(nil)

// Open connects to PostgreSQL with a read-only session. Errors never
// contain the DSN password.
func Open(ctx context.Context, dsn string, cfg introspect.Config) (*Introspector, error) {
	cfg = cfg.WithDefaults()
	pcfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		// pgx already redacts, but the raw DSN may be malformed in ways its
		// redaction misses; never echo it.
		return nil, fmt.Errorf("postgres: invalid DSN %q: %s", introspect.RedactDSN(dsn),
			introspect.RedactText(err.Error()))
	}
	pw := pcfg.Password
	if pcfg.RuntimeParams == nil {
		pcfg.RuntimeParams = map[string]string{}
	}
	pcfg.RuntimeParams["default_transaction_read_only"] = "on"
	pcfg.RuntimeParams["statement_timeout"] = strconv.FormatInt(cfg.StatementTimeout.Milliseconds(), 10)
	pcfg.RuntimeParams["application_name"] = "kabr"
	pcfg.ConnectTimeout = cfg.ConnectTimeout

	conn, err := pgx.ConnectConfig(ctx, pcfg)
	if err != nil {
		return nil, introspect.SanitizeError("postgres: connect to "+introspect.RedactDSN(dsn), err, pw)
	}
	return &Introspector{conn: conn, password: pw}, nil
}

// Close closes the connection.
func (p *Introspector) Close() error {
	if p == nil || p.conn == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.wrap("close", p.conn.Close(ctx))
}

func (p *Introspector) wrap(what string, err error) error {
	if err == nil {
		return nil
	}
	return introspect.SanitizeError("postgres: "+what, err, p.password)
}

// readOnly runs fn inside a read-only, repeatable-read transaction that is
// always rolled back.
func (p *Introspector) readOnly(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := p.conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return p.wrap("begin read-only transaction", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	return fn(tx)
}

// Introspect implements [introspect.Introspector].
func (p *Introspector) Introspect(ctx context.Context, opts introspect.Options) (*schema.Catalog, error) {
	schemas := opts.EffectiveSchemas()
	var cat *schema.Catalog
	err := p.readOnly(ctx, func(tx pgx.Tx) error {
		var db string
		if err := tx.QueryRow(ctx, qCurrentDatabase).Scan(&db); err != nil {
			return p.wrap("read current database", err)
		}
		cat = schema.New(schema.DialectPostgres, db)
		cat.Schemas = schemas

		tables, oids, err := p.readTables(ctx, tx, schemas, opts)
		if err != nil {
			return err
		}
		if err := p.readColumns(ctx, tx, oids, tables); err != nil {
			return err
		}
		if err := p.readConstraints(ctx, tx, oids, tables); err != nil {
			return err
		}
		if err := p.readIndexes(ctx, tx, oids, tables); err != nil {
			return err
		}
		if opts.IncludeStats {
			if err := p.readColumnStats(ctx, tx, schemas, tables); err != nil {
				return err
			}
		}
		for _, oid := range oids {
			cat.Tables = append(cat.Tables, *tables[oid])
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	cat.Normalize()
	if err := cat.Validate(); err != nil {
		return nil, fmt.Errorf("postgres: introspected catalog is invalid: %w", err)
	}
	return cat, nil
}

func (p *Introspector) readTables(ctx context.Context, tx pgx.Tx, schemas []string, opts introspect.Options) (map[uint32]*schema.Table, []uint32, error) {
	rows, err := tx.Query(ctx, qTables, schemas, opts.IncludePartitions)
	if err != nil {
		return nil, nil, p.wrap("list tables", err)
	}
	tables := map[uint32]*schema.Table{}
	var oids []uint32
	for rows.Next() {
		var (
			oid      uint32
			t        schema.Table
			reltuple float64
			relpages int64
		)
		if err := rows.Scan(&oid, &t.Schema, &t.Name, &t.Comment, &reltuple, &relpages); err != nil {
			rows.Close()
			return nil, nil, p.wrap("scan table", err)
		}
		if opts.IncludeStats {
			t.Stats.EstimatedRows = estimatedRows(reltuple, relpages)
		}
		tables[oid] = &t
		oids = append(oids, oid)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, p.wrap("list tables", err)
	}
	return tables, oids, nil
}

// estimatedRows maps pg_class.reltuples to a row estimate. PostgreSQL 14+
// uses -1 for "never vacuumed/analyzed"; 0 with no pages is also unknown.
func estimatedRows(reltuples float64, relpages int64) *int64 {
	if reltuples < 0 || (reltuples == 0 && relpages == 0) {
		return nil
	}
	n := int64(math.Round(reltuples))
	return &n
}

func (p *Introspector) readColumns(ctx context.Context, tx pgx.Tx, oids []uint32, tables map[uint32]*schema.Table) error {
	rows, err := tx.Query(ctx, qColumns, oids)
	if err != nil {
		return p.wrap("list columns", err)
	}
	for rows.Next() {
		var (
			oid uint32
			c   schema.Column
		)
		if err := rows.Scan(&oid, &c.Name, &c.Ordinal, &c.DataType, &c.Nullable, &c.Default, &c.Comment); err != nil {
			rows.Close()
			return p.wrap("scan column", err)
		}
		if t := tables[oid]; t != nil {
			t.Columns = append(t.Columns, c)
		}
	}
	return p.wrap("list columns", rows.Err())
}

func (p *Introspector) readConstraints(ctx context.Context, tx pgx.Tx, oids []uint32, tables map[uint32]*schema.Table) error {
	rows, err := tx.Query(ctx, qConstraints, oids)
	if err != nil {
		return p.wrap("list constraints", err)
	}
	for rows.Next() {
		var (
			oid                 uint32
			name, typ           string
			cols, refCols       []string
			refSchema, refTable string
			onDelete, onUpdate  string
		)
		if err := rows.Scan(&oid, &name, &typ, &cols, &refSchema, &refTable, &refCols, &onDelete, &onUpdate); err != nil {
			rows.Close()
			return p.wrap("scan constraint", err)
		}
		t := tables[oid]
		if t == nil {
			continue
		}
		switch typ {
		case "p":
			t.PrimaryKey = &schema.PrimaryKey{Name: name, Columns: cols}
		case "u":
			t.UniqueConstraints = append(t.UniqueConstraints, schema.UniqueConstraint{Name: name, Columns: cols})
		case "f":
			t.ForeignKeys = append(t.ForeignKeys, schema.ForeignKey{
				Name: name, Columns: cols,
				RefSchema: refSchema, RefTable: refTable, RefColumns: refCols,
				OnDelete: fkAction(onDelete), OnUpdate: fkAction(onUpdate),
			})
		}
	}
	return p.wrap("list constraints", rows.Err())
}

// fkAction maps pg_constraint.confdeltype/confupdtype codes to SQL.
func fkAction(code string) string {
	switch code {
	case "a":
		return "NO ACTION"
	case "r":
		return "RESTRICT"
	case "c":
		return "CASCADE"
	case "n":
		return "SET NULL"
	case "d":
		return "SET DEFAULT"
	}
	return ""
}

func (p *Introspector) readIndexes(ctx context.Context, tx pgx.Tx, oids []uint32, tables map[uint32]*schema.Table) error {
	rows, err := tx.Query(ctx, qIndexes, oids)
	if err != nil {
		return p.wrap("list indexes", err)
	}
	for rows.Next() {
		var (
			oid uint32
			ix  schema.Index
		)
		if err := rows.Scan(&oid, &ix.Name, &ix.Unique, &ix.Primary, &ix.Method, &ix.Columns); err != nil {
			rows.Close()
			return p.wrap("scan index", err)
		}
		if t := tables[oid]; t != nil {
			t.Indexes = append(t.Indexes, ix)
		}
	}
	return p.wrap("list indexes", rows.Err())
}

func (p *Introspector) readColumnStats(ctx context.Context, tx pgx.Tx, schemas []string, tables map[uint32]*schema.Table) error {
	byID := make(map[string]*schema.Table, len(tables))
	for _, t := range tables {
		byID[t.ID()] = t
	}
	rows, err := tx.Query(ctx, qColumnStats, schemas)
	if err != nil {
		return p.wrap("read column statistics", err)
	}
	for rows.Next() {
		var (
			sch, tbl, col string
			nd            float64
		)
		if err := rows.Scan(&sch, &tbl, &col, &nd); err != nil {
			rows.Close()
			return p.wrap("scan column statistics", err)
		}
		if t := byID[schema.TableID(sch, tbl)]; t != nil {
			if c := t.Column(col); c != nil {
				v := nd
				c.Stats.NDistinct = &v
			}
		}
	}
	return p.wrap("read column statistics", rows.Err())
}

// errNoRole is returned when current_user is not found in pg_roles.
var errNoRole = errors.New("current role not found in pg_roles")
