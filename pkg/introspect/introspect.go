// Package introspect defines the dialect-agnostic contract for reading the
// structure of a relational database from its system catalog.
//
// An [Introspector] only ever reads catalog metadata (for PostgreSQL:
// pg_catalog, information_schema and the pg_stats view). It never reads rows
// from user tables, and implementations must run in a read-only session.
//
// Dialect implementations live in sub-packages and register themselves with
// [Register] from an init function, in the style of database/sql drivers.
// To use [Open] with a PostgreSQL DSN, blank-import the driver once:
//
//	import _ "github.com/Gabrielmmorais/kabr/pkg/introspect/postgres"
//
//	in, err := introspect.Open(ctx, dsn, introspect.Config{})
//	if err != nil { ... }
//	defer in.Close()
//	cat, err := in.Introspect(ctx, introspect.DefaultOptions())
package introspect

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

// DefaultSchemas is used when [Options.Schemas] is empty.
var DefaultSchemas = []string{"public"}

// DefaultStatementTimeout is used when [Config.StatementTimeout] is zero.
const DefaultStatementTimeout = 15 * time.Second

// DefaultConnectTimeout is used when [Config.ConnectTimeout] is zero.
const DefaultConnectTimeout = 10 * time.Second

// Introspector reads the catalog of one database over one connection.
// Implementations are not safe for concurrent use.
type Introspector interface {
	// Introspect reads the structure of the selected schemas. The returned
	// catalog is normalized and validated; IntrospectedAt is left nil so the
	// caller decides whether to stamp it.
	Introspect(ctx context.Context, opts Options) (*schema.Catalog, error)
	// CheckPrivileges reports privileges of the connected role that go
	// beyond read-only access (write, DDL, dangerous role attributes) on the
	// schemas selected by opts.
	CheckPrivileges(ctx context.Context, opts Options) (PrivilegeReport, error)
	// Close releases the underlying connection.
	Close() error
}

// Options selects what [Introspector.Introspect] reads.
type Options struct {
	// Schemas lists the database schemas to read. Empty means
	// [DefaultSchemas].
	Schemas []string
	// IncludeStats adds planner statistics (estimated row counts and, where
	// the role may read them, per-column n_distinct). [DefaultOptions] sets
	// it to true.
	IncludeStats bool
	// IncludePartitions adds partitions of partitioned tables as ordinary
	// tables. By default only the partitioned (parent) table is reported.
	IncludePartitions bool
}

// DefaultOptions returns Options with the default schemas and statistics on.
func DefaultOptions() Options {
	return Options{Schemas: append([]string(nil), DefaultSchemas...), IncludeStats: true}
}

// EffectiveSchemas returns o.Schemas, or [DefaultSchemas] when it is empty.
func (o Options) EffectiveSchemas() []string {
	if len(o.Schemas) == 0 {
		return append([]string(nil), DefaultSchemas...)
	}
	return append([]string(nil), o.Schemas...)
}

// Config tunes how a connection is opened.
type Config struct {
	// StatementTimeout bounds every statement run by the introspector.
	// Zero means [DefaultStatementTimeout].
	StatementTimeout time.Duration
	// ConnectTimeout bounds connection establishment. Zero means
	// [DefaultConnectTimeout].
	ConnectTimeout time.Duration
}

// WithDefaults returns c with zero fields replaced by their defaults.
func (c Config) WithDefaults() Config {
	if c.StatementTimeout <= 0 {
		c.StatementTimeout = DefaultStatementTimeout
	}
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = DefaultConnectTimeout
	}
	return c
}

// PrivilegeReport describes privileges of the connected role that exceed
// what catalog introspection needs.
type PrivilegeReport struct {
	// Role is the effective role (current_user).
	Role string
	// Superuser reports whether the role is a superuser. A superuser
	// bypasses every privilege check and is never read-only.
	Superuser bool
	// Findings lists each excess privilege, sorted by Object then Privilege.
	Findings []Finding
	// ReadOnly is true when the role is not a superuser and has no findings.
	ReadOnly bool
}

// Finding is one privilege the role holds that it should not need.
//
// Object is prefixed with its kind: "role <name>" (dangerous role
// attributes such as SUPERUSER, BYPASSRLS, CREATEROLE, CREATEDB,
// REPLICATION), "database <name>" (CREATE), "schema <name>" (CREATE) or
// "table <schema>.<name>" (INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES,
// TRIGGER).
type Finding struct {
	Object    string
	Privilege string
}

// Opener opens an [Introspector] for a DSN. Dialect packages implement it.
type Opener func(ctx context.Context, dsn string, cfg Config) (Introspector, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Opener{}
)

// knownSchemes maps every DSN scheme Kabr recognizes to its dialect name.
var knownSchemes = map[string]string{
	"postgres":   schema.DialectPostgres,
	"postgresql": schema.DialectPostgres,
	// TODO(M2+): MySQL/MariaDB and SQL Server introspection.
	"mysql":     "mysql",
	"mariadb":   "mysql",
	"sqlserver": "sqlserver",
	"mssql":     "sqlserver",
}

// ErrDialectNotSupported is returned by [Open] for a recognized DSN scheme
// whose dialect has no implementation yet (MySQL, SQL Server).
var ErrDialectNotSupported = errors.New("dialect not supported yet")

// Register makes an [Opener] available to [Open] for the given dialect
// (for example [schema.DialectPostgres]). It panics if called twice for the
// same dialect or with a nil opener.
func Register(dialect string, open Opener) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if open == nil {
		panic("introspect: Register opener is nil")
	}
	if _, dup := registry[dialect]; dup {
		panic("introspect: Register called twice for dialect " + dialect)
	}
	registry[dialect] = open
}

// DialectForDSN returns the dialect name for a DSN based on its URL scheme.
// A keyword/value DSN ("host=... dbname=...") is treated as PostgreSQL.
func DialectForDSN(dsn string) (string, error) {
	s := strings.TrimSpace(dsn)
	if s == "" {
		return "", errors.New("introspect: empty DSN")
	}
	scheme, _, ok := strings.Cut(s, "://")
	if !ok {
		if strings.Contains(s, "=") {
			return schema.DialectPostgres, nil
		}
		return "", fmt.Errorf("introspect: cannot determine dialect of DSN %q", RedactDSN(dsn))
	}
	d, ok := knownSchemes[strings.ToLower(scheme)]
	if !ok {
		return "", fmt.Errorf("introspect: unknown DSN scheme %q", url.PathEscape(scheme))
	}
	return d, nil
}

// Open picks the dialect from the DSN scheme ("postgres://" or
// "postgresql://", or a keyword/value DSN) and opens a read-only
// [Introspector]. Error messages never contain the DSN password.
func Open(ctx context.Context, dsn string, cfg Config) (Introspector, error) {
	dialect, err := DialectForDSN(dsn)
	if err != nil {
		return nil, err
	}
	registryMu.RLock()
	open, ok := registry[dialect]
	registryMu.RUnlock()
	if !ok {
		if dialect == schema.DialectPostgres {
			return nil, errors.New(`introspect: postgres dialect is not registered; ` +
				`blank-import "github.com/Gabrielmmorais/kabr/pkg/introspect/postgres"`)
		}
		return nil, fmt.Errorf("introspect: %s: %w", dialect, ErrDialectNotSupported)
	}
	return open(ctx, dsn, cfg.WithDefaults())
}
