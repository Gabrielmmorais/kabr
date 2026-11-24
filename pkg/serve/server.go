package serve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Gabrielmmorais/kabr/pkg/introspect"
	"github.com/Gabrielmmorais/kabr/pkg/introspect/postgres"
	"github.com/Gabrielmmorais/kabr/pkg/plan"
	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

// Defaults of [Options].
const (
	// DefaultAddr is the listen address when [Options.Addr] is empty.
	DefaultAddr = ":8080"
	// DefaultRecheckInterval is the catalog recheck interval used by the
	// CLI ([Options.RecheckInterval] has no implicit default: 0 disables).
	DefaultRecheckInterval = 5 * time.Minute
	// DefaultMaxConns is the pool size when [Options.MaxConns] is zero.
	DefaultMaxConns = 4
	// MinHashKeyBytes is the shortest accepted hash key.
	MinHashKeyBytes = 16

	// retryInterval is how often the catalog check is retried while the
	// domain is not ready (database not up yet, or a transient failure of
	// a periodic recheck), when shorter than the recheck interval.
	retryInterval = 15 * time.Second
	// refreshTimeout bounds one catalog check.
	refreshTimeout = 2 * time.Minute
	// requestSlack is added to the statement timeout for the whole request
	// (acquire, begin, rollback).
	requestSlack = 5 * time.Second
	// readyPingAfter: /readyz pings the database only when no query
	// succeeded for this long.
	readyPingAfter   = 10 * time.Second
	readyPingTimeout = 2 * time.Second
	shutdownTimeout  = 15 * time.Second
)

// Options configures a [Server].
type Options struct {
	// Config is the validated domain configuration (required).
	Config *Config
	// DSN is the PostgreSQL connection string (required). It is never
	// logged; read it from $KABR_DSN or a file, never from a flag.
	DSN string
	// HashKey is the HMAC key of the hash action. Empty means no key:
	// hashed columns are dropped (with a warning). When set it must be at
	// least [MinHashKeyBytes] long.
	HashKey []byte
	// Addr is the listen address of [Server.Run] (default [DefaultAddr]).
	Addr string
	// RecheckInterval is how often the catalog fingerprint is re-checked
	// while the domain is ready; 0 disables rechecks once ready. While the
	// domain is not ready the check is retried every 15 seconds (or every
	// RecheckInterval, when shorter).
	RecheckInterval time.Duration
	// MaxConns bounds the database connections (default [DefaultMaxConns]).
	MaxConns int
	// Logger receives structured logs (default: discard).
	Logger *slog.Logger
}

// catalogLoader introspects the live catalog of the configured schemas.
type catalogLoader func(ctx context.Context) (*schema.Catalog, *introspect.PrivilegeReport, error)

// Server is the runtime of one domain. Create it with [New] and start it
// with [Server.Run] or [Server.Serve].
type Server struct {
	cfg         *Config
	addr        string
	recheck     time.Duration
	retry       time.Duration // fast retry while not ready (tests shorten it)
	stmtTimeout time.Duration
	rowLimit    int
	hashKey     []byte
	log         *slog.Logger
	db          database
	secrets     []string // scrubbed from logged errors
	loadCatalog catalogLoader
	handler     http.Handler

	state    atomic.Pointer[state]
	lastDBOK atomic.Int64 // unix nanoseconds of the last successful database use

	refreshMu  sync.Mutex
	privWarned bool
	lastWarn   string
}

// Not-ready categories: the only reason /readyz discloses. The detailed
// reason (fingerprints, table and column names) goes to the log only.
const (
	notReadyStarting      = "starting"
	notReadyDatabase      = "database_unavailable"
	notReadyCatalogDrift  = "catalog_drift"
	notReadyConfig        = "config_error"
	notReadyTable         = "table_unreadable"
	notReadyInternalError = "internal_error"
)

// state is an immutable snapshot of readiness and of the served model.
type state struct {
	ready    bool
	category string // coarse not-ready category, safe to disclose
	reason   string // detailed not-ready reason, for the log only
	model    *model // non-nil only when ready
	openapi  []byte
}

// New validates the options and prepares the server. It opens no
// connection: the first catalog check happens in [Server.Serve].
func New(opts Options) (*Server, error) {
	if opts.Config == nil {
		return nil, errors.New("serve: config is required")
	}
	if err := opts.Config.Validate(); err != nil {
		return nil, fmt.Errorf("serve: invalid config: %w", err)
	}
	if strings.TrimSpace(opts.DSN) == "" {
		return nil, errors.New("serve: DSN is required")
	}
	if len(opts.HashKey) > 0 && len(opts.HashKey) < MinHashKeyBytes {
		return nil, fmt.Errorf("serve: hash key must be at least %d bytes", MinHashKeyBytes)
	}
	if opts.RecheckInterval < 0 {
		return nil, errors.New("serve: recheck interval must not be negative")
	}
	if opts.MaxConns < 0 {
		return nil, errors.New("serve: max connections must not be negative")
	}
	cfg := *opts.Config
	s := &Server{
		cfg:         &cfg,
		addr:        opts.Addr,
		recheck:     opts.RecheckInterval,
		stmtTimeout: cfg.EffectiveStatementTimeout(),
		rowLimit:    cfg.EffectiveRowLimit(),
		log:         opts.Logger,
	}
	if s.addr == "" {
		s.addr = DefaultAddr
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if len(opts.HashKey) > 0 {
		s.hashKey = append([]byte(nil), opts.HashKey...)
	}
	maxConns := opts.MaxConns
	if maxConns == 0 {
		maxConns = DefaultMaxConns
	}
	db, err := newPGDatabase(opts.DSN, maxConns, s.stmtTimeout)
	if err != nil {
		return nil, fmt.Errorf("serve: %w", err)
	}
	s.db = db
	s.secrets = []string{opts.DSN, db.password()}
	s.loadCatalog = s.introspector(opts.DSN)
	s.init()
	return s, nil
}

// init sets the initial state and the handler (shared with tests).
func (s *Server) init() {
	if s.retry == 0 {
		s.retry = retryInterval
	}
	s.state.Store(&state{category: notReadyStarting, reason: "starting: the catalog has not been checked yet"})
	s.handler = s.routes()
}

// introspector returns the default catalog loader: the M1 read-only
// introspector on its own connection, statistics off.
func (s *Server) introspector(dsn string) catalogLoader {
	return func(ctx context.Context) (*schema.Catalog, *introspect.PrivilegeReport, error) {
		in, err := postgres.Open(ctx, dsn, introspect.Config{})
		if err != nil {
			return nil, nil, err
		}
		defer func() { _ = in.Close() }()
		opts := introspect.Options{Schemas: s.cfg.EffectiveSchemas()}
		rep, err := in.CheckPrivileges(ctx, opts)
		if err != nil {
			return nil, nil, err
		}
		cat, err := in.Introspect(ctx, opts)
		if err != nil {
			return nil, nil, err
		}
		return cat, &rep, nil
	}
}

// Handler returns the HTTP handler of the server.
func (s *Server) Handler() http.Handler { return s.handler }

// Run listens on the configured address and calls [Server.Serve].
func (s *Server) Run(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.addr)
	if err != nil {
		s.db.close()
		return fmt.Errorf("serve: listen: %w", err)
	}
	return s.Serve(ctx, ln)
}

// Serve answers HTTP on ln until ctx is canceled, then shuts down
// gracefully. It checks the catalog right away (the server answers
// /healthz meanwhile, /readyz says 503) and then every recheck interval.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      s.stmtTimeout + requestSlack + 10*time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	s.log.Info("listening", "addr", ln.Addr().String(), "domain", s.cfg.Domain, "plan", s.cfg.Plan,
		"rowLimit", s.rowLimit, "statementTimeout", s.stmtTimeout.String(), "recheckInterval", s.recheck.String())

	watchCtx, stopWatch := context.WithCancel(ctx)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		s.watch(watchCtx)
	}()

	var err error
	select {
	case <-ctx.Done():
		s.log.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		err = srv.Shutdown(shutCtx)
		cancel()
	case err = <-errc:
	}
	stopWatch()
	<-watchDone
	s.db.close()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// watch runs the first catalog check, then keeps checking: on the fast
// retry interval while the domain is not ready, every recheck interval (if
// any) while it is ready.
func (s *Server) watch(ctx context.Context) {
	s.refreshWithTimeout(ctx)
	s.recheckLoop(ctx)
}

// nextInterval is the delay before the next catalog check; 0 means stop.
// A not-ready domain (never ready, or a transient failure of a periodic
// recheck) is retried on the fast interval, still failing closed meanwhile.
func (s *Server) nextInterval() time.Duration {
	interval := s.recheck
	if !s.state.Load().ready && (interval == 0 || interval > s.retry) {
		interval = s.retry
	}
	return interval
}

func (s *Server) recheckLoop(ctx context.Context) {
	for {
		interval := s.nextInterval()
		if interval == 0 {
			return
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		s.refreshWithTimeout(ctx)
	}
}

func (s *Server) refreshWithTimeout(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	_ = s.refresh(rctx)
}

// refresh introspects the catalog, verifies its fingerprint, resolves the
// model, probes every table and publishes the new state. It returns nil
// when the domain is ready, otherwise an error holding the reason.
func (s *Server) refresh(ctx context.Context) error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	cat, rep, err := s.loadCatalog(ctx)
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			return err
		}
		s.log.Error("catalog check failed", "error", s.scrub(err))
		return s.setNotReady(notReadyDatabase, "database unavailable or catalog introspection failed")
	}
	s.lastDBOK.Store(time.Now().UnixNano())
	if rep != nil && !rep.ReadOnly && !s.privWarned {
		s.privWarned = true
		privs := make([]string, 0, len(rep.Findings))
		for _, f := range rep.Findings {
			privs = append(privs, f.Object+": "+f.Privilege)
		}
		s.log.Warn("the database role holds privileges beyond read-only; every request still runs in a READ ONLY transaction",
			"role", rep.Role, "superuser", rep.Superuser, "privileges", privs)
	}
	if fp := plan.Fingerprint(cat); fp != s.cfg.CatalogFingerprint {
		return s.setNotReady(notReadyCatalogDrift, fmt.Sprintf("catalog drift: expected %s, got %s", s.cfg.CatalogFingerprint, fp))
	}
	m, warnings, err := resolve(s.cfg, cat, s.hashKey, s.rowLimit)
	if w := strings.Join(warnings, "\n"); w != s.lastWarn {
		s.lastWarn = w
		for _, msg := range warnings {
			s.log.Warn(msg)
		}
	}
	if err != nil {
		return s.setNotReady(notReadyConfig, "configuration does not match the catalog: "+oneLine(err))
	}
	if err = s.probe(ctx, m); err != nil {
		category := notReadyTable
		if errors.Is(err, errDatabaseUnavailable) {
			category = notReadyDatabase
		}
		return s.setNotReady(category, err.Error())
	}
	doc, err := buildOpenAPI(m)
	if err != nil {
		s.log.Error("build OpenAPI document", "error", err.Error())
		return s.setNotReady(notReadyInternalError, "internal error building the OpenAPI document")
	}
	prev := s.state.Load()
	s.state.Store(&state{ready: true, model: m, openapi: doc})
	if !prev.ready {
		s.log.Info("ready", "tables", len(m.tables), "excluded", len(m.excluded))
	}
	return nil
}

// probe reads zero rows of every served table with the exact projection
// of the data queries: a missing SELECT privilege makes the domain unready
// instead of failing requests.
func (s *Server) probe(ctx context.Context, m *model) error {
	var failed string
	err := s.db.readOnly(ctx, func(tx pgx.Tx) error {
		for _, t := range m.tables {
			rows, err := tx.Query(ctx, buildProbe(t))
			if err == nil {
				rows.Close()
				err = rows.Err()
			}
			if err != nil {
				failed = t.id
				return err
			}
		}
		return nil
	})
	if err == nil {
		s.lastDBOK.Store(time.Now().UnixNano())
		return nil
	}
	s.log.Error("table probe failed", "table", failed, "error", s.scrub(err))
	var pgErr *pgconn.PgError
	if failed != "" && errors.As(err, &pgErr) {
		why := "query failed (see the log)"
		switch pgErr.Code {
		case "42501":
			why = "permission denied"
		case "42P01", "42703":
			why = "table or column not found"
		case "57014":
			why = "statement timeout"
		}
		return fmt.Errorf("cannot read table %s: %s", failed, why)
	}
	return errDatabaseUnavailable
}

var errDatabaseUnavailable = errors.New("database unavailable")

// setNotReady publishes a not-ready state and returns the detailed reason
// as an error. Only the category is disclosed by /readyz; transitions and
// new reasons are logged once (not per request) with the detailed reason.
func (s *Server) setNotReady(category, reason string) error {
	prev := s.state.Load()
	s.state.Store(&state{category: category, reason: reason})
	if prev.ready || prev.reason != reason {
		s.log.Warn("not ready", "category", category, "reason", reason)
	}
	return errors.New(reason)
}

// scrub renders an error for the log without the DSN or its password. A
// server error of class 22 (bad input value) is reduced to its SQLSTATE:
// its message quotes the offending value, which may be personal data.
func (s *Server) scrub(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "22") {
		return "SQLSTATE " + pgErr.Code
	}
	return introspect.RedactText(err.Error(), s.secrets...)
}

func oneLine(err error) string {
	return strings.Join(strings.Split(err.Error(), "\n"), "; ")
}
