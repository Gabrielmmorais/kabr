//go:build integration

package serve

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/Gabrielmmorais/kabr/internal/testutil"
	"github.com/Gabrielmmorais/kabr/pkg/introspect"
	"github.com/Gabrielmmorais/kabr/pkg/introspect/postgres"
	"github.com/Gabrielmmorais/kabr/pkg/plan"
)

const (
	fixtureImage = "postgres:17.12-alpine"
	fixtureDB    = "kabr_serve"
)

var (
	fixtureOnce sync.Once
	fixtureErr  error
	fixtureCtr  *tcpostgres.PostgresContainer
	fixtureHost string
	fixturePort string
	adminDSN    string

	// Random per run, set on the fixture roles once the container is up.
	rolePasswords = map[string]string{
		"kabr_api":     testutil.Secret(),
		"kabr_partial": testutil.Secret(),
	}
	testHashKey = []byte(testutil.Secret())
)

func TestMain(m *testing.M) {
	code := m.Run()
	if fixtureCtr != nil {
		if err := testcontainers.TerminateContainer(fixtureCtr); err != nil {
			fmt.Fprintln(os.Stderr, "terminate fixture container:", err)
		}
	}
	os.Exit(code)
}

// roleDSN starts the shared container on first use and returns the DSN of
// a fixture role. It skips the test when Docker is unavailable.
func roleDSN(t *testing.T, role string) string {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)
	fixtureOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		fixtureCtr, fixtureErr = tcpostgres.Run(ctx, fixtureImage,
			tcpostgres.WithDatabase(fixtureDB),
			tcpostgres.WithUsername("postgres"),
			tcpostgres.WithPassword(testutil.Secret()),
			tcpostgres.WithInitScripts(filepath.Join("testdata", "fixture.sql")),
			tcpostgres.BasicWaitStrategies(),
		)
		if fixtureErr != nil {
			return
		}
		if fixtureHost, fixtureErr = fixtureCtr.Host(ctx); fixtureErr != nil {
			return
		}
		p, err := fixtureCtr.MappedPort(ctx, "5432/tcp")
		if fixtureErr = err; err != nil {
			return
		}
		fixturePort = p.Port()
		if adminDSN, fixtureErr = fixtureCtr.ConnectionString(ctx, "sslmode=disable"); fixtureErr != nil {
			return
		}
		fixtureErr = testutil.SetRolePasswords(ctx, adminDSN, rolePasswords)
	})
	if fixtureErr != nil {
		t.Fatalf("start fixture container: %v", fixtureErr)
	}
	if role == "postgres" {
		return adminDSN
	}
	return testutil.PostgresURL(role, rolePasswords[role], fixtureHost, fixturePort, fixtureDB)
}

func adminExec(t *testing.T, sql string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, roleDSN(t, "postgres"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func adminStrings(t *testing.T, sql string) []string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, roleDSN(t, "postgres"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	rows, err := conn.Query(ctx, sql)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// liveFingerprint introspects the schemas like the server does.
func liveFingerprint(t *testing.T, schemas ...string) string {
	t.Helper()
	ctx := context.Background()
	in, err := postgres.Open(ctx, roleDSN(t, "kabr_api"), introspect.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	cat, err := in.Introspect(ctx, introspect.Options{Schemas: schemas})
	if err != nil {
		t.Fatal(err)
	}
	return plan.Fingerprint(cat)
}

var allTables = []string{
	"public.audit_log", "public.customer", "public.film", "public.language",
	"public.rating_info", "public.rental_event", "public.staging_import",
}

var customerSensitive = []plan.SensitiveColumn{
	{Table: "public.customer", Column: "first_name", Class: plan.ClassPII, Action: plan.ActionDrop},
	{Table: "public.customer", Column: "email", Class: plan.ClassPII, Action: plan.ActionMask},
	{Table: "public.customer", Column: "password_hash", Class: plan.ClassCredential, Action: plan.ActionMask},
	{Table: "public.customer", Column: "card_number", Class: plan.ClassFinancial, Action: plan.ActionHash},
}

func publicConfig(t *testing.T, tables ...string) *Config {
	t.Helper()
	if len(tables) == 0 {
		tables = allTables
	}
	cfg := &Config{
		Domain:             "film",
		Plan:               "fixture",
		CatalogFingerprint: liveFingerprint(t, "public"),
		Schemas:            []string{"public"},
		RowLimit:           100,
		StatementTimeout:   Duration(5 * time.Second),
		Tables:             tables,
	}
	if slices.Contains(tables, "public.customer") {
		cfg.SensitiveColumns = customerSensitive
	}
	return cfg
}

type testServer struct {
	*Server
	url  string
	logs *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// startServer builds a server for cfg, runs one catalog check and serves
// its handler. wantReady fails the test when the check does not succeed.
func startServer(t *testing.T, cfg *Config, role string, hashKey []byte, wantReady bool) *testServer {
	t.Helper()
	logs := &syncBuffer{}
	s, err := New(Options{
		Config:  cfg,
		DSN:     roleDSN(t, role),
		HashKey: hashKey,
		Logger:  slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := s.refresh(ctx); wantReady && err != nil {
		t.Fatalf("refresh: %v\nlogs:\n%s", err, logs)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ts.Close()
		s.db.close()
	})
	return &testServer{Server: s, url: ts.URL, logs: logs}
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("invalid JSON body %q: %v", r.body, err)
	}
	return m
}

func do(t *testing.T, method, u string) response {
	t.Helper()
	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response{status: res.StatusCode, header: res.Header, body: body}
}

func get(t *testing.T, u string) response { t.Helper(); return do(t, http.MethodGet, u) }

// wantError asserts a JSON error answer and that it leaks nothing.
func wantError(t *testing.T, r response, status int, code string) map[string]any {
	t.Helper()
	if r.status != status {
		t.Fatalf("status = %d, want %d (body %s)", r.status, status, r.body)
	}
	m := r.json(t)
	if m["error"] != code {
		t.Fatalf("error = %v, want %s (body %s)", m["error"], code, r.body)
	}
	if _, ok := m["message"].(string); !ok || len(m) != 2 {
		t.Fatalf("error body must be exactly {error, message}: %s", r.body)
	}
	assertNoLeak(t, string(r.body))
	return m
}

func assertNoLeak(t *testing.T, s string) {
	t.Helper()
	low := strings.ToLower(s)
	for _, bad := range []string{"select ", " from ", "sqlstate", "pgx", "pgconn", "postgres://", "syntax", "invalid input"} {
		if strings.Contains(low, bad) {
			t.Errorf("answer leaks %q: %s", bad, s)
		}
	}
	for _, pw := range rolePasswords {
		if strings.Contains(s, pw) {
			t.Error("answer leaks a role password")
		}
	}
}

type page struct {
	Items      []map[string]any `json:"items"`
	NextCursor *string          `json:"nextCursor"`
}

func getPage(t *testing.T, u string) page {
	t.Helper()
	r := get(t, u)
	if r.status != http.StatusOK {
		t.Fatalf("GET %s: status %d: %s", u, r.status, r.body)
	}
	var p page
	dec := json.NewDecoder(bytes.NewReader(r.body))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		t.Fatalf("decode page: %v: %s", err, r.body)
	}
	return p
}

// walk pages through a table and returns the key of every item, in order.
func walk(t *testing.T, base string, limit int, key func(map[string]any) string) []string {
	t.Helper()
	var keys []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 1000 {
			t.Fatal("pagination does not terminate")
		}
		q := url.Values{}
		if limit > 0 {
			q.Set("limit", fmt.Sprint(limit))
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		sep := "?"
		if strings.Contains(base, "?") {
			sep = "&"
		}
		p := getPage(t, base+sep+q.Encode())
		if limit > 0 && len(p.Items) > limit {
			t.Fatalf("page has %d items, limit %d", len(p.Items), limit)
		}
		for _, it := range p.Items {
			keys = append(keys, key(it))
		}
		if p.NextCursor == nil {
			return keys
		}
		if len(p.Items) == 0 {
			t.Fatal("empty page with a next cursor")
		}
		cursor = *p.NextCursor
	}
}

func str(v any) string {
	if n, ok := v.(json.Number); ok {
		return n.String()
	}
	return fmt.Sprint(v)
}

func assertSameOrder(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("walked %d keys, want %d\n got: %v\nwant: %v", len(got), len(want), got, want)
	}
	seen := map[string]bool{}
	for _, k := range got {
		if seen[k] {
			t.Fatalf("key %s returned twice", k)
		}
		seen[k] = true
	}
}

func TestPaginationWalksEveryRowOnce(t *testing.T) {
	ts := startServer(t, publicConfig(t), "kabr_api", testHashKey, true)

	t.Run("integer key, default limit", func(t *testing.T) {
		p := getPage(t, ts.url+"/v1/public/film")
		if len(p.Items) != 100 || p.NextCursor == nil {
			t.Fatalf("first page: %d items, cursor %v", len(p.Items), p.NextCursor)
		}
		got := walk(t, ts.url+"/v1/public/film", 0, func(m map[string]any) string { return str(m["film_id"]) })
		want := adminStrings(t, "SELECT film_id::text AS k FROM public.film ORDER BY film_id")
		assertSameOrder(t, got, want)
	})
	t.Run("integer key, small pages", func(t *testing.T) {
		got := walk(t, ts.url+"/v1/public/film", 7, func(m map[string]any) string { return str(m["film_id"]) })
		assertSameOrder(t, got, adminStrings(t, "SELECT film_id::text AS k FROM public.film ORDER BY film_id"))
	})
	t.Run("text key", func(t *testing.T) {
		got := walk(t, ts.url+"/v1/public/language", 3, func(m map[string]any) string { return str(m["code"]) })
		assertSameOrder(t, got, adminStrings(t, "SELECT code FROM public.language ORDER BY code"))
	})
	t.Run("composite key with timestamp", func(t *testing.T) {
		got := walk(t, ts.url+"/v1/public/rental_event", 11, func(m map[string]any) string {
			at, err := time.Parse(time.RFC3339Nano, str(m["happened_at"]))
			if err != nil {
				t.Fatalf("happened_at %v: %v", m["happened_at"], err)
			}
			return str(m["store_id"]) + "|" + at.Format("2006-01-02 15:04:05.000000") + "|" + str(m["seq"])
		})
		want := adminStrings(t, `SELECT store_id || '|' || to_char(happened_at, 'YYYY-MM-DD HH24:MI:SS.US') || '|' || seq
			FROM public.rental_event ORDER BY store_id, happened_at, seq`)
		if len(want) != 150 {
			t.Fatalf("fixture has %d rental events", len(want))
		}
		assertSameOrder(t, got, want)
	})
	t.Run("timestamptz key with infinities and BC", func(t *testing.T) {
		got := walk(t, ts.url+"/v1/public/audit_log", 4, func(m map[string]any) string { return str(m["message"]) })
		want := adminStrings(t, "SELECT message FROM public.audit_log ORDER BY at")
		assertSameOrder(t, got, want)
		if got[0] != "start of time" || got[1] != "ides of march" || got[len(got)-1] != "end of time" {
			t.Errorf("unexpected order at the edges: %v", got)
		}
	})
	t.Run("enum key in text order", func(t *testing.T) {
		got := walk(t, ts.url+"/v1/public/rating_info", 2, func(m map[string]any) string { return str(m["rating"]) })
		assertSameOrder(t, got, adminStrings(t, "SELECT rating::text FROM public.rating_info ORDER BY rating::text"))
	})
	t.Run("filters with pagination", func(t *testing.T) {
		got := walk(t, ts.url+"/v1/public/film?rating=PG&is_active=true", 4, func(m map[string]any) string { return str(m["film_id"]) })
		want := adminStrings(t, "SELECT film_id::text AS k FROM public.film WHERE rating = 'PG' AND is_active ORDER BY film_id")
		if len(want) == 0 {
			t.Fatal("fixture has no matching films")
		}
		assertSameOrder(t, got, want)
	})
}

func TestLimitIsClamped(t *testing.T) {
	ts := startServer(t, publicConfig(t), "kabr_api", testHashKey, true)
	for q, want := range map[string]int{"limit=5000": 100, "limit=100": 100, "limit=0": 1, "limit=-3": 1, "limit=": 100, "limit=17": 17} {
		p := getPage(t, ts.url+"/v1/public/film?"+q)
		if len(p.Items) != want {
			t.Errorf("%s: %d items, want %d", q, len(p.Items), want)
		}
	}
	wantError(t, get(t, ts.url+"/v1/public/film?limit=abc"), http.StatusBadRequest, codeInvalidLimit)
	wantError(t, get(t, ts.url+"/v1/public/film?limit=1&limit=2"), http.StatusBadRequest, codeInvalidQuery)

	// A configured row limit below the default is enforced too.
	cfg := publicConfig(t, "public.film")
	cfg.RowLimit = 10
	small := startServer(t, cfg, "kabr_api", nil, true)
	if p := getPage(t, small.url+"/v1/public/film?limit=1000"); len(p.Items) != 10 || p.NextCursor == nil {
		t.Errorf("rowLimit 10: %d items", len(p.Items))
	}
}

func TestValueEncoding(t *testing.T) {
	ts := startServer(t, publicConfig(t), "kabr_api", testHashKey, true)
	r := get(t, ts.url+"/v1/public/film/1")
	if r.status != http.StatusOK {
		t.Fatalf("status %d: %s", r.status, r.body)
	}
	var row map[string]json.RawMessage
	if err := json.Unmarshal(r.body, &row); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"film_id":          `1`,
		"title":            `"Film 1"`,
		"rating":           `"PG-13"`,
		"release_year":     `"2006"`,
		"rental_rate":      `"4.99"`,
		"length_ratio":     `0.5`,
		"is_active":        `true`,
		"last_update":      `"2026-03-04T05:06:07.123456Z"`,
		"released_on":      `"2006-05-25"`,
		"poster":           `"AP8Q"`,
		"special_features": `["Trailers","Deleted Scenes"]`,
		"scores":           `[1,2]`,
		"fulltext":         `"'fox':2 'quick':1"`,
		"meta":             `{"a":[1,2.50,"x"],"b":null}`,
		"uid":              `"00000000-0000-0000-0000-000000000001"`,
	}
	if len(row) != len(want) {
		t.Errorf("row has %d columns, want %d: %s", len(row), len(want), r.body)
	}
	for k, v := range want {
		if got := string(row[k]); got != v {
			t.Errorf("%s = %s, want %s", k, got, v)
		}
	}
	// Column order follows the table.
	if !bytes.HasPrefix(r.body, []byte(`{"film_id":1,"title":"Film 1","rating":`)) {
		t.Errorf("columns out of order: %s", r.body)
	}

	nulls := get(t, ts.url+"/v1/public/film/2").json(t)
	for _, k := range []string{"rating", "release_year", "length_ratio", "released_on", "poster", "special_features", "scores", "fulltext", "meta", "uid"} {
		if v, ok := nulls[k]; !ok || v != nil {
			t.Errorf("film 2 %s = %v, want null", k, v)
		}
	}
	if nan := get(t, ts.url+"/v1/public/film/3").json(t); nan["length_ratio"] != "NaN" {
		t.Errorf("NaN encoded as %v", nan["length_ratio"])
	}
	if r := get(t, ts.url+"/v1/public/language/O'Brien"); r.status != http.StatusOK || r.json(t)["name"] != "quote" {
		t.Errorf("text key lookup: %d %s", r.status, r.body)
	}
	if r := get(t, ts.url+"/v1/public/audit_log/infinity"); r.status != http.StatusOK || r.json(t)["message"] != "end of time" {
		t.Errorf("timestamp key lookup: %d %s", r.status, r.body)
	}
	if r := get(t, ts.url+"/v1/public/rating_info/PG-13"); r.status != http.StatusOK || r.json(t)["description"] != "thirteen" {
		t.Errorf("enum key lookup: %d %s", r.status, r.body)
	}
	if r := do(t, http.MethodHead, ts.url+"/v1/public/film/1"); r.status != http.StatusOK || len(r.body) != 0 {
		t.Errorf("HEAD: %d %q", r.status, r.body)
	}
}

func TestSensitiveColumns(t *testing.T) {
	ts := startServer(t, publicConfig(t), "kabr_api", testHashKey, true)
	items := walk(t, ts.url+"/v1/public/customer", 6, func(m map[string]any) string {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		id := str(m["customer_id"])
		if got := strings.Join(keys, ","); got != "active,card_number,customer_id,email" {
			t.Errorf("customer %s exposes %s", id, got)
		}
		n, err := strconv.Atoi(id)
		if err != nil {
			t.Fatalf("customer_id %q", id)
		}
		if n%4 == 0 {
			if m["email"] != nil {
				t.Errorf("customer %s: NULL email must stay null, got %v", id, m["email"])
			}
		} else if m["email"] != "***" {
			t.Errorf("customer %s: email = %v, want ***", id, m["email"])
		}
		if want := hmacHex(testHashKey, fmt.Sprintf("4000-0000-0000-%04d", n)); m["card_number"] != want {
			t.Errorf("customer %s: card_number = %v, want %s", id, m["card_number"], want)
		}
		return id
	})
	if len(items) != 20 {
		t.Fatalf("walked %d customers", len(items))
	}

	r := get(t, ts.url+"/openapi.json")
	if r.status != http.StatusOK {
		t.Fatalf("openapi: %d", r.status)
	}
	for _, leaked := range []string{"password_hash", "first_name"} {
		if bytes.Contains(r.body, []byte(leaked)) {
			t.Errorf("OpenAPI document mentions dropped column %s", leaked)
		}
	}

	var msgs []string
	for _, col := range []string{"email", "password_hash", "first_name", "card_number", "nope"} {
		m := wantError(t, get(t, ts.url+"/v1/public/customer?"+col+"=x"), http.StatusBadRequest, codeInvalidFilter)
		msgs = append(msgs, strings.Replace(m["message"].(string), col, "COL", 1))
	}
	if len(slices.Compact(slices.Clone(msgs))) != 1 {
		t.Errorf("filter errors differ between unknown, dropped and sensitive columns: %v", msgs)
	}
	if p := getPage(t, ts.url+"/v1/public/customer?active=true"); len(p.Items) != 10 {
		t.Errorf("active=true: %d items", len(p.Items))
	}

	t.Run("hash without key is dropped", func(t *testing.T) {
		nokey := startServer(t, publicConfig(t), "kabr_api", nil, true)
		row := get(t, nokey.url+"/v1/public/customer/1").json(t)
		if _, ok := row["card_number"]; ok {
			t.Errorf("card_number exposed without a hash key: %v", row)
		}
		if !strings.Contains(nokey.logs.String(), "no hash key configured") {
			t.Error("missing warning about the hash key")
		}
	})
	t.Run("credential is dropped whatever the action", func(t *testing.T) {
		for _, action := range []plan.Action{plan.ActionMask, plan.ActionHash, plan.ActionDrop} {
			cfg := publicConfig(t, "public.customer")
			cfg.SensitiveColumns = []plan.SensitiveColumn{
				{Table: "public.customer", Column: "email", Class: plan.ClassCredential, Action: action},
			}
			s := startServer(t, cfg, "kabr_api", testHashKey, true)
			if row := get(t, s.url+"/v1/public/customer/1").json(t); row["email"] != nil || len(row) != 5 {
				t.Errorf("action %s: credential column exposed: %v", action, row)
			}
			if doc := get(t, s.url+"/openapi.json"); bytes.Contains(doc.body, []byte(`"email"`)) {
				t.Errorf("action %s: credential column in OpenAPI", action)
			}
		}
	})
}

func TestRequestErrors(t *testing.T) {
	ts := startServer(t, publicConfig(t), "kabr_api", testHashKey, true)
	cursor := func(payload string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(payload))
	}
	cases := []struct {
		name   string
		path   string
		status int
		code   string
	}{
		{"unknown table", "/v1/public/nope", 404, codeNotFound},
		{"table without primary key", "/v1/public/staging_import", 404, codeNotFound},
		{"table outside the domain", "/v1/public/slow_table", 404, codeNotFound},
		{"table outside the schemas", "/v1/drift/thing", 404, codeNotFound},
		{"system catalog", "/v1/pg_catalog/pg_authid", 404, codeNotFound},
		{"unknown endpoint", "/admin", 404, codeNotFound},
		{"cursor not base64", "/v1/public/film?cursor=!!!", 400, codeInvalidCursor},
		{"cursor not JSON", "/v1/public/film?cursor=" + cursor("not json"), 400, codeInvalidCursor},
		{"cursor wrong arity", "/v1/public/film?cursor=" + cursor(`{"v":["1","2"]}`), 400, codeInvalidCursor},
		{"cursor number value", "/v1/public/film?cursor=" + cursor(`{"v":[1]}`), 400, codeInvalidCursor},
		{"cursor extra field", "/v1/public/film?cursor=" + cursor(`{"v":["1"],"x":1}`), 400, codeInvalidCursor},
		{"cursor trailing data", "/v1/public/film?cursor=" + cursor(`{"v":["1"]}{}`), 400, codeInvalidCursor},
		{"cursor bad integer", "/v1/public/film?cursor=" + cursor(`{"v":["abc"]}`), 400, codeInvalidValue},
		{"cursor injection", "/v1/public/film?cursor=" + cursor(`{"v":["1) OR (1=1"]}`), 400, codeInvalidValue},
		{"cursor bad timestamp", "/v1/public/rental_event?cursor=" + cursor(`{"v":["1","yesterday-ish","1"]}`), 400, codeInvalidValue},
		{"cursor NUL", "/v1/public/language?cursor=" + cursor(`{"v":["a\u0000b"]}`), 400, codeInvalidCursor},
		{"filter bad integer", "/v1/public/film?film_id=abc", 400, codeInvalidValue},
		{"filter bad numeric", "/v1/public/film?rental_rate=abc", 400, codeInvalidValue},
		{"filter bad timestamp", "/v1/public/film?last_update=not-a-date", 400, codeInvalidValue},
		{"filter bad uuid", "/v1/public/film?uid=nope", 400, codeInvalidValue},
		{"filter bad boolean", "/v1/public/film?is_active=maybe", 400, codeInvalidValue},
		{"filter out of range", "/v1/public/rental_event?seq=99999", 400, codeInvalidValue},
		{"filter invalid UTF-8", "/v1/public/language?name=%ff", 400, codeInvalidValue},
		{"filter on array", "/v1/public/film?scores=1", 400, codeInvalidFilter},
		{"filter on json", "/v1/public/film?meta=1", 400, codeInvalidFilter},
		{"filter on bytea", "/v1/public/film?poster=1", 400, codeInvalidFilter},
		{"filter repeated", "/v1/public/film?title=a&title=b", 400, codeInvalidQuery},
		{"malformed query", "/v1/public/film?title=%zz", 400, codeInvalidQuery},
		{"get composite key", "/v1/public/rental_event/1", 404, codeNotFound},
		{"get with query", "/v1/public/film/1?x=1", 400, codeInvalidQuery},
		{"get bad key", "/v1/public/film/abc", 400, codeInvalidValue},
		{"get missing row", "/v1/public/film/99999", 404, codeNotFound},
		{"get bad timestamp key", "/v1/public/audit_log/nope", 400, codeInvalidValue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantError(t, get(t, ts.url+tc.path), tc.status, tc.code)
		})
	}

	t.Run("injection in a text filter is just a value", func(t *testing.T) {
		p := getPage(t, ts.url+"/v1/public/language?code="+url.QueryEscape("x') OR 1=1 --"))
		if len(p.Items) != 1 || p.Items[0]["name"] != "injection" {
			t.Errorf("got %v", p.Items)
		}
		p = getPage(t, ts.url+"/v1/public/film?title="+url.QueryEscape("x'); DROP TABLE public.film; --"))
		if len(p.Items) != 0 {
			t.Errorf("got %v", p.Items)
		}
	})
	t.Run("methods", func(t *testing.T) {
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			r := do(t, m, ts.url+"/v1/public/film")
			wantError(t, r, http.StatusMethodNotAllowed, codeMethodNotAllowed)
			if r.header.Get("Allow") != "GET, HEAD" {
				t.Errorf("%s: Allow = %q", m, r.header.Get("Allow"))
			}
		}
	})
	if n := adminStrings(t, "SELECT count(*)::text FROM public.film"); n[0] != "250" {
		t.Errorf("film has %s rows after the error cases", n[0])
	}
}

func TestTransactionsAreReadOnly(t *testing.T) {
	// Connect as the superuser: even a role allowed to write cannot write
	// through the runtime, because every transaction is READ ONLY.
	ts := startServer(t, publicConfig(t, "public.language"), "postgres", nil, true)
	ctx := context.Background()
	var readOnly, timeout string
	err := ts.db.readOnly(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "SHOW transaction_read_only").Scan(&readOnly); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "SHOW statement_timeout").Scan(&timeout)
	})
	if err != nil || readOnly != "on" || timeout != "5s" {
		t.Fatalf("transaction_read_only=%q statement_timeout=%q err=%v", readOnly, timeout, err)
	}
	for _, stmt := range []string{
		"INSERT INTO public.language VALUES ('zz', 'x')",
		"UPDATE public.language SET name = 'x'",
		"DELETE FROM public.language",
		"CREATE TEMP TABLE t (i int)",
		"SET TRANSACTION READ WRITE",
	} {
		err := ts.db.readOnly(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt)
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || (pgErr.Code != "25006" && pgErr.Code != "25001") {
			t.Errorf("%s: want read_only_sql_transaction (25006) or active_sql_transaction (25001), got %v", stmt, err)
		}
	}
	if n := adminStrings(t, "SELECT count(*)::text FROM public.language WHERE code = 'zz'"); n[0] != "0" {
		t.Error("a write went through")
	}
}

func TestPanicInTransactionReleasesTheConnection(t *testing.T) {
	cfg := publicConfig(t, "public.language")
	s, err := New(Options{Config: cfg, DSN: roleDSN(t, "kabr_api"), MaxConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.db.close()
	ctx := context.Background()
	for range 3 {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("panic swallowed")
				}
			}()
			_ = s.db.readOnly(ctx, func(pgx.Tx) error { panic("boom") })
		}()
	}
	// With one connection, a leaked slot would block here forever.
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var one int
	if err := s.db.readOnly(qctx, func(tx pgx.Tx) error {
		return tx.QueryRow(qctx, "SELECT 1").Scan(&one)
	}); err != nil || one != 1 {
		t.Fatalf("pool unusable after panics: %v", err)
	}
}

func TestStatementTimeout(t *testing.T) {
	cfg := publicConfig(t, "public.slow_table")
	cfg.StatementTimeout = Duration(MinStatementTimeout)
	ts := startServer(t, cfg, "kabr_api", nil, true)

	start := time.Now()
	wantError(t, get(t, ts.url+"/v1/public/slow_table"), http.StatusGatewayTimeout, codeTimeout)
	wantError(t, get(t, ts.url+"/v1/public/slow_table/1"), http.StatusGatewayTimeout, codeTimeout)
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("two requests took %s; the policy sleeps 3s per row, so the timeout did not cut them", d)
	}
	// The connection is reusable after a timeout.
	if r := get(t, ts.url+"/readyz"); r.status != http.StatusOK {
		t.Errorf("readyz after timeouts: %d %s", r.status, r.body)
	}
	err := ts.db.readOnly(context.Background(), func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), "SELECT pg_catalog.pg_sleep(2)")
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Errorf("pg_sleep(2) with a 100ms timeout: %v", err)
	}
}

func TestFingerprintMismatch(t *testing.T) {
	cfg := publicConfig(t)
	cfg.CatalogFingerprint = "sha256:" + strings.Repeat("0", 64)
	ts := startServer(t, cfg, "kabr_api", nil, false)

	m := wantError(t, get(t, ts.url+"/readyz"), http.StatusServiceUnavailable, codeNotReady)
	live := liveFingerprint(t, "public")
	if m["message"] != notReadyCatalogDrift {
		t.Errorf("readyz message = %q, want %q", m["message"], notReadyCatalogDrift)
	}
	if body := fmt.Sprint(m); strings.Contains(body, live) || strings.Contains(body, cfg.CatalogFingerprint) {
		t.Errorf("readyz discloses a fingerprint: %s", body)
	}
	if want := "catalog drift: expected " + cfg.CatalogFingerprint + ", got " + live; !strings.Contains(ts.logs.String(), want) {
		t.Errorf("detailed reason %q not logged:\n%s", want, ts.logs)
	}
	for _, p := range []string{"/v1/public/film", "/v1/public/film/1", "/v1/public/nope", "/openapi.json"} {
		wantError(t, get(t, ts.url+p), http.StatusServiceUnavailable, codeNotReady)
	}
	if r := get(t, ts.url+"/healthz"); r.status != http.StatusOK {
		t.Errorf("healthz: %d", r.status)
	}
}

func TestMissingTableAndPrivileges(t *testing.T) {
	ts := startServer(t, publicConfig(t, "public.film", "public.ghost"), "kabr_api", nil, false)
	m := wantError(t, get(t, ts.url+"/readyz"), http.StatusServiceUnavailable, codeNotReady)
	if m["message"] != notReadyConfig {
		t.Errorf("readyz message: %v", m["message"])
	}
	if !strings.Contains(ts.logs.String(), "table public.ghost not found") {
		t.Errorf("detailed reason not logged:\n%s", ts.logs)
	}
	wantError(t, get(t, ts.url+"/v1/public/film"), http.StatusServiceUnavailable, codeNotReady)

	partial := startServer(t, publicConfig(t, "public.film", "public.language"), "kabr_partial", nil, false)
	m = wantError(t, get(t, partial.url+"/readyz"), http.StatusServiceUnavailable, codeNotReady)
	if m["message"] != notReadyTable {
		t.Errorf("readyz message: %v", m["message"])
	}
	if !strings.Contains(partial.logs.String(), "cannot read table public.language: permission denied") {
		t.Errorf("detailed reason not logged:\n%s", partial.logs)
	}
}

func TestDriftAfterStartupFlipsReadiness(t *testing.T) {
	cfg := &Config{
		Domain: "drift", Plan: "fixture", Schemas: []string{"drift"}, Tables: []string{"drift.thing"},
		CatalogFingerprint: liveFingerprint(t, "drift"),
	}
	logs := &syncBuffer{}
	s, err := New(Options{Config: cfg, DSN: roleDSN(t, "kabr_api"), RecheckInterval: 100 * time.Millisecond,
		Logger: slog.New(slog.NewJSONHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()

	waitStatus := func(path string, status int) response {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			r := get(t, base+path)
			if r.status == status {
				return r
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: status %d, want %d: %s\nlogs:\n%s", path, r.status, status, r.body, logs)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitStatus("/readyz", http.StatusOK)
	if p := getPage(t, base+"/v1/drift/thing"); len(p.Items) != 2 {
		t.Fatalf("items: %v", p.Items)
	}

	adminExec(t, "ALTER TABLE drift.thing ADD COLUMN extra integer")
	r := waitStatus("/readyz", http.StatusServiceUnavailable)
	if m := r.json(t); m["message"] != notReadyCatalogDrift {
		t.Errorf("readyz after drift: %s", r.body)
	}
	wantError(t, get(t, base+"/v1/drift/thing"), http.StatusServiceUnavailable, codeNotReady)

	adminExec(t, "ALTER TABLE drift.thing DROP COLUMN extra")
	waitStatus("/readyz", http.StatusOK)
	if p := getPage(t, base+"/v1/drift/thing"); len(p.Items) != 2 {
		t.Fatalf("items after revert: %v", p.Items)
	}
}

func TestOpenAPIDocument(t *testing.T) {
	ts := startServer(t, publicConfig(t), "kabr_api", testHashKey, true)
	r := get(t, ts.url+"/openapi.json")
	if r.status != http.StatusOK {
		t.Fatalf("status %d", r.status)
	}
	var doc struct {
		OpenAPI string                                `json:"openapi"`
		Paths   map[string]map[string]json.RawMessage `json:"paths"`
		Comps   struct {
			Schemas map[string]struct {
				Required   []string                  `json:"required"`
				Properties map[string]map[string]any `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(r.body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.OpenAPI != "3.0.3" {
		t.Errorf("openapi = %q", doc.OpenAPI)
	}
	var paths []string
	for p := range doc.Paths {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	want := []string{
		"/v1/public/audit_log", "/v1/public/audit_log/{pk}", "/v1/public/customer", "/v1/public/customer/{pk}",
		"/v1/public/film", "/v1/public/film/{pk}", "/v1/public/language", "/v1/public/language/{pk}",
		"/v1/public/rating_info", "/v1/public/rating_info/{pk}", "/v1/public/rental_event",
	}
	if !slices.Equal(paths, want) {
		t.Errorf("paths = %v\nwant %v", paths, want)
	}
	film := doc.Comps.Schemas["public.film"].Properties
	checks := map[string]map[string]any{
		"film_id":          {"type": "integer", "format": "int32"},
		"rental_rate":      {"type": "string", "format": "decimal"},
		"last_update":      {"type": "string", "format": "date-time"},
		"poster":           {"type": "string", "format": "byte", "nullable": true},
		"rating":           {"type": "string", "nullable": true},
		"special_features": {"type": "array"},
		"uid":              {"type": "string", "format": "uuid"},
	}
	for col, fields := range checks {
		for k, v := range fields {
			if film[col][k] != v {
				t.Errorf("film.%s.%s = %v, want %v", col, k, film[col][k], v)
			}
		}
	}
	if _, ok := doc.Comps.Schemas["public.staging_import"]; ok {
		t.Error("table without primary key in OpenAPI")
	}
	cust := doc.Comps.Schemas["public.customer"]
	if got := strings.Join(cust.Required, ","); got != "customer_id,email,card_number,active" {
		t.Errorf("customer required = %s", got)
	}
	var listCustomer struct {
		Parameters []struct {
			Name string `json:"name"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(doc.Paths["/v1/public/customer"]["get"], &listCustomer); err != nil {
		t.Fatal(err)
	}
	var params []string
	for _, p := range listCustomer.Parameters {
		params = append(params, p.Name)
	}
	if got := strings.Join(params, ","); got != "limit,cursor,customer_id,active" {
		t.Errorf("customer list parameters = %s (sensitive columns must not be filters)", got)
	}
}

func TestLogsHoldNoValues(t *testing.T) {
	ts := startServer(t, publicConfig(t), "kabr_api", testHashKey, true)
	secret := "Secret-" + testutil.Secret()
	getPage(t, ts.url+"/v1/public/language?name="+secret)
	get(t, ts.url+"/v1/public/language/"+secret)
	get(t, ts.url+"/v1/public/film?rental_rate="+secret)
	get(t, ts.url+"/v1/public/film?cursor="+base64.RawURLEncoding.EncodeToString([]byte(`{"v":["`+secret+`"]}`)))
	logs := ts.logs.String()
	if strings.Contains(logs, secret) {
		t.Errorf("a request value reached the log:\n%s", logs)
	}
	for _, pw := range rolePasswords {
		if strings.Contains(logs, pw) {
			t.Error("a role password reached the log")
		}
	}
	for _, want := range []string{`"filters":["name"]`, `"path":"/v1/public/language/{pk}"`, `"table":"public.language"`, `"rows":0`} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %s:\n%s", want, logs)
		}
	}
}
