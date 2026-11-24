package serve

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Gabrielmmorais/kabr/pkg/introspect"
)

// database runs read-only work for the handlers. The production
// implementation is pgDatabase; tests substitute fakes.
type database interface {
	// readOnly runs fn in a READ ONLY transaction with a transaction-local
	// statement_timeout; the transaction is always rolled back.
	readOnly(ctx context.Context, fn func(pgx.Tx) error) error
	// ping checks that a connection can be used.
	ping(ctx context.Context) error
	close()
}

const (
	// connMaxLifetime recycles connections periodically.
	connMaxLifetime = 30 * time.Minute
	// cleanupTimeout bounds rollbacks and connection closes.
	cleanupTimeout = 5 * time.Second
)

var errPoolClosed = errors.New("connection pool is closed")

// setLocalTimeout is SET LOCAL statement_timeout with the value as a bind
// parameter (set_config with is_local = true is the same as SET LOCAL).
const setLocalTimeout = "SELECT pg_catalog.set_config('statement_timeout', $1, true)"

// pgDatabase is a small fixed-size pool of pgx connections. (pgxpool would
// need a new module requirement; this pool only needs what the runtime
// uses: bounded concurrency, reuse, and discarding broken connections.)
type pgDatabase struct {
	cfg       *pgx.ConnConfig
	timeoutMS string // statement_timeout in milliseconds
	slots     chan struct{}

	mu     sync.Mutex
	idle   []*pooledConn
	closed bool
}

type pooledConn struct {
	conn    *pgx.Conn
	created time.Time
}

// newPGDatabase parses the DSN and returns a pool of at most maxConns
// sessions, each opened with default_transaction_read_only=on and a fixed,
// safe session environment. No connection is opened yet. Errors never hold
// the DSN or its password.
func newPGDatabase(dsn string, maxConns int, stmtTimeout time.Duration) (*pgDatabase, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid DSN %q: %s", introspect.RedactDSN(dsn), introspect.RedactText(err.Error()))
	}
	ms := strconv.FormatInt(stmtTimeout.Milliseconds(), 10)
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	for k, v := range map[string]string{
		"default_transaction_read_only": "on",
		"statement_timeout":             ms,
		// A stuck transaction cannot hold a snapshot for long.
		"idle_in_transaction_session_timeout": strconv.FormatInt(2*stmtTimeout.Milliseconds()+5000, 10),
		// Every relation is schema-qualified and every function and cast is
		// pg_catalog-qualified; operators then resolve in pg_catalog only.
		"search_path":      "pg_catalog",
		"DateStyle":        "ISO, MDY",
		"IntervalStyle":    "postgres",
		"TimeZone":         "UTC",
		"application_name": "kabr-serve",
	} {
		cfg.RuntimeParams[k] = v
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement
	if maxConns < 1 {
		maxConns = 1
	}
	return &pgDatabase{cfg: cfg, timeoutMS: ms, slots: make(chan struct{}, maxConns)}, nil
}

// password returns the DSN password, used only to scrub log messages.
func (d *pgDatabase) password() string { return d.cfg.Password }

func (d *pgDatabase) acquire(ctx context.Context) (*pooledConn, error) {
	select {
	case d.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	pc, err := d.takeIdle()
	if err == nil && pc == nil {
		var conn *pgx.Conn
		if conn, err = pgx.ConnectConfig(ctx, d.cfg); err == nil {
			pc = &pooledConn{conn: conn, created: time.Now()}
		}
	}
	if err != nil {
		<-d.slots
		return nil, err
	}
	return pc, nil
}

// takeIdle pops a usable idle connection (closing stale ones on the way);
// it returns nil, nil when there is none.
func (d *pgDatabase) takeIdle() (*pooledConn, error) {
	for {
		d.mu.Lock()
		if d.closed {
			d.mu.Unlock()
			return nil, errPoolClosed
		}
		n := len(d.idle)
		if n == 0 {
			d.mu.Unlock()
			return nil, nil
		}
		pc := d.idle[n-1]
		d.idle = d.idle[:n-1]
		d.mu.Unlock()
		if !pc.conn.IsClosed() && time.Since(pc.created) <= connMaxLifetime {
			return pc, nil
		}
		closeConn(pc)
	}
}

// release returns a connection to the pool, or closes it when it is broken,
// not idle, expired or the pool is closed.
func (d *pgDatabase) release(pc *pooledConn, reusable bool) {
	defer func() { <-d.slots }()
	if !reusable || pc.conn.IsClosed() || pc.conn.PgConn().TxStatus() != 'I' ||
		time.Since(pc.created) > connMaxLifetime {
		closeConn(pc)
		return
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		closeConn(pc)
		return
	}
	d.idle = append(d.idle, pc)
	d.mu.Unlock()
}

func closeConn(pc *pooledConn) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	_ = pc.conn.Close(ctx)
}

func (d *pgDatabase) close() {
	d.mu.Lock()
	d.closed = true
	idle := d.idle
	d.idle = nil
	d.mu.Unlock()
	for _, pc := range idle {
		closeConn(pc)
	}
}

func (d *pgDatabase) ping(ctx context.Context) error {
	pc, err := d.acquire(ctx)
	if err != nil {
		return err
	}
	err = pc.conn.Ping(ctx)
	d.release(pc, err == nil)
	return err
}

func (d *pgDatabase) readOnly(ctx context.Context, fn func(pgx.Tx) error) error {
	for attempt := 0; ; attempt++ {
		pc, err := d.acquire(ctx)
		if err != nil {
			return err
		}
		tx, err := pc.conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if err != nil {
			d.release(pc, false)
			// An idle connection the server already dropped fails here
			// before anything was sent: retry once on a fresh one.
			if attempt == 0 && pgconn.SafeToRetry(err) && ctx.Err() == nil {
				continue
			}
			return err
		}
		return d.runAndRollback(ctx, pc, tx, fn)
	}
}

// runAndRollback sets the transaction-local statement timeout, runs fn and
// always rolls back and releases the connection, even if fn panics (the
// connection is then discarded and the panic goes on).
func (d *pgDatabase) runAndRollback(ctx context.Context, pc *pooledConn, tx pgx.Tx, fn func(pgx.Tx) error) error {
	completed := false
	defer func() {
		rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		rbErr := tx.Rollback(rbCtx)
		cancel()
		d.release(pc, completed && rbErr == nil)
	}()
	if _, err := tx.Exec(ctx, setLocalTimeout, d.timeoutMS); err != nil {
		completed = true
		return err
	}
	err := fn(tx)
	completed = true
	return err
}
