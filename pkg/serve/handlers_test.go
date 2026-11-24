package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Gabrielmmorais/kabr/internal/testutil"
	"github.com/Gabrielmmorais/kabr/pkg/introspect"
	"github.com/Gabrielmmorais/kabr/pkg/plan"
	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

// Test credentials are generated per run; none is written in the source.
var (
	unitPassword = testutil.Secret()
	unitDSN      = testutil.PostgresURL("kabr", unitPassword, "db.invalid", "5432", "app")
)

// fakeDB counts calls so a test can prove that a request was rejected
// before any database access. Without queryErr it never runs fn and returns
// err (nil means "no rows"); with queryErr it runs fn with a transaction
// whose Query fails with queryErr.
type fakeDB struct {
	mu       sync.Mutex
	calls    int
	err      error
	queryErr error
	pingErr  error
	panic    bool
}

// fakeTx implements only Query; any other method would panic.
type fakeTx struct {
	pgx.Tx
	err error
}

func (f fakeTx) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, f.err }

func (f *fakeDB) readOnly(_ context.Context, fn func(pgx.Tx) error) error {
	f.mu.Lock()
	f.calls++
	queryErr, err, doPanic := f.queryErr, f.err, f.panic
	f.mu.Unlock()
	if doPanic {
		panic("boom")
	}
	if queryErr != nil {
		return fn(fakeTx{err: queryErr})
	}
	return err
}

func (f *fakeDB) ping(context.Context) error { return f.pingErr }
func (f *fakeDB) close()                     {}

func (f *fakeDB) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type unitServer struct {
	*Server
	db   *fakeDB
	cat  *schema.Catalog
	logs *bytes.Buffer
	mu   sync.Mutex
}

// newUnitServer builds a server around a fake database and a fixed catalog;
// the config fingerprint matches the catalog.
func newUnitServer(t *testing.T) *unitServer {
	t.Helper()
	cfg := catalogConfig()
	u := &unitServer{db: &fakeDB{}, cat: testCatalog(), logs: &bytes.Buffer{}}
	cfg.CatalogFingerprint = plan.Fingerprint(u.cat)
	u.Server = &Server{
		cfg:         cfg,
		stmtTimeout: cfg.EffectiveStatementTimeout(),
		rowLimit:    cfg.EffectiveRowLimit(),
		hashKey:     unitHashKey,
		log:         slog.New(slog.NewJSONHandler(u.logs, nil)),
		db:          u.db,
		secrets:     []string{unitDSN, unitPassword},
	}
	u.loadCatalog = func(context.Context) (*schema.Catalog, *introspect.PrivilegeReport, error) {
		u.mu.Lock()
		defer u.mu.Unlock()
		return u.cat.Clone(), &introspect.PrivilegeReport{Role: "kabr_api", ReadOnly: true}, nil
	}
	u.init()
	return u
}

func (u *unitServer) do(t *testing.T, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	u.Handler().ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func wantJSONError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) string {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d: %s", rec.Code, status, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body) != 2 || body["error"] != code {
		t.Fatalf("body = %s, want {error: %s, message}", rec.Body, code)
	}
	return body["message"]
}

func TestNotReadyUntilChecked(t *testing.T) {
	u := newUnitServer(t)
	if msg := wantJSONError(t, u.do(t, "GET", "/readyz"), 503, codeNotReady); msg != notReadyStarting {
		t.Errorf("readyz message = %q", msg)
	}
	for _, p := range []string{"/v1/public/film", "/v1/public/film/1", "/v1/public/nope", "/openapi.json"} {
		wantJSONError(t, u.do(t, "GET", p), 503, codeNotReady)
	}
	if rec := u.do(t, "GET", "/healthz"); rec.Code != 200 || rec.Body.String() != "{\"status\":\"ok\"}\n" {
		t.Errorf("healthz: %d %s", rec.Code, rec.Body)
	}
	if u.db.count() != 0 {
		t.Error("database used before readiness")
	}
}

func TestReadinessFollowsTheCatalog(t *testing.T) {
	u := newUnitServer(t)
	ctx := context.Background()
	if err := u.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if rec := u.do(t, "GET", "/readyz"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"ready"`) {
		t.Fatalf("readyz: %d %s", rec.Code, rec.Body)
	}
	if rec := u.do(t, "GET", "/openapi.json"); rec.Code != 200 || !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("openapi: %d", rec.Code)
	}

	// Drift: a column appears.
	u.mu.Lock()
	u.cat.Tables[0].Columns = append(u.cat.Tables[0].Columns, schema.Column{Name: "new_col", Ordinal: 99, DataType: "text"})
	drifted := plan.Fingerprint(u.cat)
	u.mu.Unlock()
	err := u.refresh(ctx)
	want := "catalog drift: expected " + u.cfg.CatalogFingerprint + ", got " + drifted
	if err == nil || err.Error() != want {
		t.Fatalf("refresh after drift: %v", err)
	}
	// /readyz discloses only the category; the detail is logged once.
	for range 3 {
		if msg := wantJSONError(t, u.do(t, "GET", "/readyz"), 503, codeNotReady); msg != notReadyCatalogDrift {
			t.Errorf("readyz message = %q, want %q", msg, notReadyCatalogDrift)
		}
	}
	if n := strings.Count(u.logs.String(), drifted); n != 1 {
		t.Errorf("detailed drift reason logged %d times, want 1:\n%s", n, u.logs)
	}
	calls := u.db.count()
	wantJSONError(t, u.do(t, "GET", "/v1/public/film"), 503, codeNotReady)
	wantJSONError(t, u.do(t, "GET", "/openapi.json"), 503, codeNotReady)
	if u.db.count() != calls {
		t.Error("data query issued while not ready")
	}

	// Back to the approved structure.
	u.mu.Lock()
	u.cat = testCatalog()
	u.mu.Unlock()
	if err := u.refresh(ctx); err != nil {
		t.Fatalf("refresh after revert: %v", err)
	}
	if rec := u.do(t, "GET", "/readyz"); rec.Code != 200 {
		t.Errorf("readyz after revert: %d", rec.Code)
	}

	// A stale database check triggers a ping.
	u.lastDBOK.Store(time.Now().Add(-time.Hour).UnixNano())
	u.db.pingErr = errors.New("connection refused")
	if msg := wantJSONError(t, u.do(t, "GET", "/readyz"), 503, codeNotReady); msg != notReadyDatabase {
		t.Errorf("readyz message after failed ping = %q", msg)
	}
	u.db.pingErr = nil
	if rec := u.do(t, "GET", "/readyz"); rec.Code != 200 {
		t.Errorf("readyz after ping recovers: %d", rec.Code)
	}
}

func TestRefreshFailures(t *testing.T) {
	u := newUnitServer(t)
	ctx := context.Background()
	u.loadCatalog = func(context.Context) (*schema.Catalog, *introspect.PrivilegeReport, error) {
		return nil, nil, errors.New("dial " + unitDSN + ": connection refused")
	}
	if err := u.refresh(ctx); err == nil || err.Error() != "database unavailable or catalog introspection failed" {
		t.Errorf("introspection failure: %v", err)
	}
	if strings.Contains(u.logs.String(), unitPassword) {
		t.Errorf("DSN password logged: %s", u.logs)
	}

	u = newUnitServer(t)
	u.db.queryErr = &pgconn.PgError{Code: "42501", Message: "permission denied for table customer"}
	if err := u.refresh(ctx); err == nil || err.Error() != "cannot read table public.customer: permission denied" {
		t.Errorf("probe failure: %v", err)
	}

	u = newUnitServer(t)
	u.loadCatalog = func(context.Context) (*schema.Catalog, *introspect.PrivilegeReport, error) {
		return testCatalog(), &introspect.PrivilegeReport{Role: "owner", Findings: []introspect.Finding{
			{Object: "table public.film", Privilege: "INSERT"}}}, nil
	}
	if err := u.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u.logs.String(), "privileges beyond read-only") {
		t.Error("over-privileged role not reported")
	}
}

func TestRequestValidationHappensBeforeTheDatabase(t *testing.T) {
	u := newUnitServer(t)
	if err := u.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := u.db.count()
	cases := []struct {
		target string
		status int
		code   string
	}{
		{"/v1/public/nope", 404, codeNotFound},
		{"/v1/public/staging", 404, codeNotFound},
		{"/v1/public/doc", 404, codeNotFound},
		{"/v1/pg_catalog/pg_authid", 404, codeNotFound},
		{"/v1/public/film/1/2", 404, codeNotFound},
		{"/nope", 404, codeNotFound},
		{"/v1/public/customer?email=x", 400, codeInvalidFilter},
		{"/v1/public/customer?password=x", 400, codeInvalidFilter},
		{"/v1/public/customer?first_name=x", 400, codeInvalidFilter},
		{"/v1/public/customer?card=x", 400, codeInvalidFilter},
		{"/v1/public/customer?unknown=x", 400, codeInvalidFilter},
		{"/v1/public/film?poster=x", 400, codeInvalidFilter},
		{"/v1/public/film?film_id=1.5", 400, codeInvalidValue},
		{"/v1/public/film?title=" + strings.Repeat("a", maxValueBytes+1), 400, codeInvalidValue},
		{"/v1/public/film?title=a%00b", 400, codeInvalidValue},
		{"/v1/public/film?cursor=bad", 400, codeInvalidCursor},
		{"/v1/public/film?limit=ten", 400, codeInvalidLimit},
		{"/v1/public/film?title=a&title=b", 400, codeInvalidQuery},
		{"/v1/public/film?a;b", 400, codeInvalidQuery},
		{"/v1/public/rental_event/1", 404, codeNotFound},
		{"/v1/public/film/x", 400, codeInvalidValue},
		{"/v1/public/film/1?limit=1", 400, codeInvalidQuery},
	}
	for _, tc := range cases {
		wantJSONError(t, u.do(t, "GET", tc.target), tc.status, tc.code)
	}
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		rec := u.do(t, m, "/v1/public/film")
		wantJSONError(t, rec, 405, codeMethodNotAllowed)
		if rec.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s: Allow = %q", m, rec.Header().Get("Allow"))
		}
	}
	if u.db.count() != before {
		t.Errorf("%d database calls for invalid requests", u.db.count()-before)
	}
	for _, target := range []string{"/v1/public/film/secret-key-1", "/v1/public/nope/secret-key-2",
		"/v1/public/film/secret-key-3/more", "/v1/public/film/"} {
		rec := u.do(t, "GET", target)
		if rec.Code != 400 && rec.Code != 404 {
			t.Errorf("%s: status %d", target, rec.Code)
		}
	}
	logs := u.logs.String()
	if strings.Contains(logs, "secret-key") {
		t.Errorf("a key value reached the log:\n%s", logs)
	}
	for _, want := range []string{`"path":"/v1/public/film/{pk}"`, `"path":"/v1/public/nope/{pk}"`, `"path":"/v1/{unmatched}"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %s", want)
		}
	}
	if u.db.count() != before {
		t.Errorf("%d database calls for invalid keys", u.db.count()-before)
	}
}

func TestDatabaseErrorsAreMapped(t *testing.T) {
	u := newUnitServer(t)
	if err := u.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	u.db.err = &pgconn.PgError{Code: "22P02", Message: `invalid input syntax for type integer: "x"`}
	wantJSONError(t, u.do(t, "GET", "/v1/public/film?cursor="+encodeCursor([]string{"x"})), 400, codeInvalidValue)
	u.db.err = &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}
	wantJSONError(t, u.do(t, "GET", "/v1/public/film"), 504, codeTimeout)
	u.db.err = nil
	u.db.queryErr = &pgconn.PgError{Code: "42501", Message: "permission denied for table top_secret_table"}
	rec := u.do(t, "GET", "/v1/public/film/1")
	if msg := wantJSONError(t, rec, 500, codeInternal); msg != "internal error" {
		t.Errorf("500 message = %q", msg)
	}
	if !strings.Contains(u.logs.String(), "top_secret_table") {
		t.Error("the 500 cause belongs in the log")
	}
	if strings.Contains(u.logs.String(), `syntax for type integer: \"x\"`) {
		t.Error("a class 22 message (it quotes the value) reached the log")
	}
	u.db.queryErr = errors.New("unexpected")
	wantJSONError(t, u.do(t, "GET", "/v1/public/film"), 500, codeInternal)
	u.db.queryErr, u.db.panic = nil, true
	wantJSONError(t, u.do(t, "GET", "/v1/public/film"), 500, codeInternal)
	u.db.panic = false

	// No rows: an empty page, and 404 for a key.
	rec = u.do(t, "GET", "/v1/public/film?title=none")
	if rec.Code != 200 || rec.Body.String() != "{\"items\":[],\"nextCursor\":null}\n" {
		t.Errorf("empty page: %d %s", rec.Code, rec.Body)
	}
	wantJSONError(t, u.do(t, "GET", "/v1/public/film/1"), 404, codeNotFound)
	for _, h := range []string{"Cache-Control", "X-Content-Type-Options"} {
		if rec.Header().Get(h) == "" {
			t.Errorf("missing header %s", h)
		}
	}
}

func TestParseListQuery(t *testing.T) {
	m, _ := mustResolve(t, catalogConfig(), unitHashKey)
	film := m.table("public", "film")
	q, names, aerr := parseListQuery(film, "uid=x&title=t&limit=7&film_id=3&cursor=", 50)
	if aerr != nil {
		t.Fatal(aerr)
	}
	if q.limit != 7 || q.after != nil || len(q.filters) != 3 {
		t.Fatalf("query = %+v", q)
	}
	// Filters are ordered by column position, whatever the query order.
	for i, want := range []string{"film_id", "title", "uid"} {
		if got := film.columns[q.filters[i].col].name; got != want {
			t.Errorf("filter %d = %s, want %s", i, got, want)
		}
	}
	if strings.Join(names, ",") != "film_id,title,uid" {
		t.Errorf("logged names = %v", names)
	}
	for in, want := range map[string]int{"limit=0": 1, "limit=-1": 1, "limit=51": 50, "limit=99999999999999999999": 0} {
		q, _, perr := parseListQuery(film, in, 50)
		if want == 0 {
			if perr == nil {
				t.Errorf("%s accepted", in)
			}
			continue
		}
		if perr != nil || q.limit != want {
			t.Errorf("%s: limit %d, %v; want %d", in, q.limit, perr, want)
		}
	}
	_, _, aerr = parseListQuery(film, "limit=1&"+strings.Repeat("x", 100)+"=1", 50)
	if aerr == nil || !strings.Contains(aerr.Message, "...") {
		t.Errorf("long unknown key: %v", aerr)
	}
}

func TestNewValidatesOptions(t *testing.T) {
	cfg := validConfig()
	dsn := unitDSN
	bad := map[string]Options{
		"no config":      {DSN: dsn},
		"invalid config": {Config: &Config{}, DSN: dsn},
		"no DSN":         {Config: cfg},
		"short hash key": {Config: cfg, DSN: dsn, HashKey: []byte("short")},
		"negative check": {Config: cfg, DSN: dsn, RecheckInterval: -time.Second},
		"negative conns": {Config: cfg, DSN: dsn, MaxConns: -1},
		"bad DSN":        {Config: cfg, DSN: testutil.PostgresURL("kabr", unitPassword, "localhost", "notaport", "app")},
	}
	for name, opts := range bad {
		_, err := New(opts)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), unitPassword) {
			t.Errorf("%s: error leaks the password: %v", name, err)
		}
	}
	s, err := New(Options{Config: cfg, DSN: dsn, HashKey: unitHashKey})
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.close()
	if s.addr != DefaultAddr || s.rowLimit != 100 || s.stmtTimeout != 5*time.Second {
		t.Errorf("defaults: %s %d %s", s.addr, s.rowLimit, s.stmtTimeout)
	}
	cfg.RowLimit = 1 // New copies the config
	if s.rowLimit != 100 || s.cfg.RowLimit != 100 {
		t.Error("server aliases the caller's config")
	}
	pg := s.db.(*pgDatabase)
	for k, v := range map[string]string{"default_transaction_read_only": "on", "search_path": "pg_catalog", "statement_timeout": "5000"} {
		if pg.cfg.RuntimeParams[k] != v {
			t.Errorf("runtime param %s = %q, want %q", k, pg.cfg.RuntimeParams[k], v)
		}
	}
}

func TestServeShutsDownGracefully(t *testing.T) {
	u := newUnitServer(t)
	u.addr = "127.0.0.1:0"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- u.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !u.state.Load().ready {
		if time.Now().After(deadline) {
			t.Fatalf("never became ready; logs:\n%s", u.logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if !strings.Contains(u.logs.String(), `"msg":"shutting down"`) {
		t.Errorf("logs: %s", u.logs)
	}
}

func TestRunReportsListenErrors(t *testing.T) {
	u := newUnitServer(t)
	u.addr = "127.0.0.1:-1"
	if err := u.Run(context.Background()); err == nil {
		t.Fatal("invalid address accepted")
	}
}

func TestHeadAndBodies(t *testing.T) {
	u := newUnitServer(t)
	if err := u.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec := u.do(t, "HEAD", "/openapi.json")
	if rec.Code != 200 {
		t.Errorf("HEAD: %d", rec.Code)
	}
	srv := httptest.NewServer(u.Handler())
	defer srv.Close()
	res, err := http.Head(srv.URL + "/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != 200 || len(body) != 0 {
		t.Errorf("HEAD over HTTP: %d %d bytes", res.StatusCode, len(body))
	}
}

// A transient failure of a periodic recheck after the domain was ready must
// be retried on the fast interval, not after the full recheck interval.
func TestTransientRecheckFailureRetriesFast(t *testing.T) {
	u := newUnitServer(t)
	u.recheck = time.Hour
	u.retry = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := u.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := u.nextInterval(); got != time.Hour {
		t.Errorf("next interval while ready = %v, want 1h", got)
	}

	// The periodic recheck hits a transient introspection failure.
	load := u.loadCatalog
	var mu sync.Mutex
	failing := true
	u.loadCatalog = func(ctx context.Context) (*schema.Catalog, *introspect.PrivilegeReport, error) {
		mu.Lock()
		f := failing
		mu.Unlock()
		if f {
			return nil, nil, errors.New("connection reset by peer")
		}
		return load(ctx)
	}
	if err := u.refresh(ctx); err == nil {
		t.Fatal("refresh with a failing loader succeeded")
	}
	if msg := wantJSONError(t, u.do(t, "GET", "/readyz"), 503, codeNotReady); msg != notReadyDatabase {
		t.Errorf("readyz message = %q, want %q", msg, notReadyDatabase)
	}
	if got := u.nextInterval(); got != u.retry {
		t.Errorf("next interval while not ready = %v, want %v", got, u.retry)
	}

	// The database recovers: the loop must notice on the fast interval.
	mu.Lock()
	failing = false
	mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		u.recheckLoop(ctx)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !u.state.Load().ready {
		if time.Now().After(deadline) {
			t.Fatal("domain did not recover on the fast retry interval")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rec := u.do(t, "GET", "/readyz"); rec.Code != 200 {
		t.Errorf("readyz after recovery: %d %s", rec.Code, rec.Body)
	}
	cancel()
	<-done
}

func TestNextIntervalShorterRecheck(t *testing.T) {
	u := newUnitServer(t)
	u.retry = time.Minute
	u.recheck = time.Second
	if got := u.nextInterval(); got != time.Second {
		t.Errorf("not ready with a shorter recheck: %v, want 1s", got)
	}
	u.recheck = 0
	if got := u.nextInterval(); got != time.Minute {
		t.Errorf("not ready without rechecks: %v, want 1m", got)
	}
	if err := u.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := u.nextInterval(); got != 0 {
		t.Errorf("ready without rechecks: %v, want 0", got)
	}
}
