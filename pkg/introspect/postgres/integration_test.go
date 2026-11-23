//go:build integration

package postgres

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

var update = flag.Bool("update", false, "rewrite golden files")

const (
	fixtureImage = "postgres:17.12-alpine"
	fixtureDB    = "kabr_fixture"
)

var (
	fixtureOnce sync.Once
	fixtureErr  error
	fixtureCtr  *tcpostgres.PostgresContainer
	fixtureHost string
	fixturePort string

	// Random per run, set on the fixture roles once the container is up.
	rolePasswords = map[string]string{
		"kabr_reader": testutil.Secret(),
		"kabr_unsafe": testutil.Secret(),
	}
)

func TestMain(m *testing.M) {
	flag.Parse()
	code := m.Run()
	if fixtureCtr != nil {
		if err := testcontainers.TerminateContainer(fixtureCtr); err != nil {
			fmt.Fprintln(os.Stderr, "terminate fixture container:", err)
		}
	}
	os.Exit(code)
}

// fixtureDSN starts the shared container on first use and returns a DSN
// for the given role and password. It skips the test when Docker is
// unavailable.
func fixtureDSN(t *testing.T, user, password string) string {
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
			testcontainers.WithCmdArgs("-c", "autovacuum=off"),
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
		adminDSN, err := fixtureCtr.ConnectionString(ctx, "sslmode=disable")
		if fixtureErr = err; err != nil {
			return
		}
		fixtureErr = testutil.SetRolePasswords(ctx, adminDSN, rolePasswords)
	})
	if fixtureErr != nil {
		t.Fatalf("start fixture container: %v", fixtureErr)
	}
	return testutil.PostgresURL(user, password, fixtureHost, fixturePort, fixtureDB)
}

// roleDSN is fixtureDSN with the role's real (generated) password.
func roleDSN(t *testing.T, user string) string {
	t.Helper()
	return fixtureDSN(t, user, rolePasswords[user])
}

func openRole(t *testing.T, user string) *Introspector {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	in, err := Open(ctx, roleDSN(t, user), introspect.Config{})
	if err != nil {
		t.Fatalf("open as %s: %v", user, err)
	}
	t.Cleanup(func() { _ = in.Close() })
	return in
}

func fixtureOptions() introspect.Options {
	o := introspect.DefaultOptions()
	o.Schemas = []string{"public", "sales"}
	return o
}

// TestIntrospectGolden runs as kabr_reader, which has no SELECT privilege on
// any user table: success proves introspection is catalog-only.
func TestIntrospectGolden(t *testing.T) {
	in := openRole(t, "kabr_reader")
	ctx := context.Background()

	// Sanity: the role really cannot read user rows.
	err := in.readOnly(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT count(*) FROM public.customer")
		return err
	})
	if !isSQLState(err, "42501") {
		t.Fatalf("kabr_reader must not be able to SELECT user tables, got %v", err)
	}

	cat, err := in.Introspect(ctx, fixtureOptions())
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	if cat.IntrospectedAt != nil {
		t.Error("IntrospectedAt must be left nil")
	}
	got, err := schema.Marshal(cat)
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "catalog.golden.json")
	if *update {
		if err = os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("catalog differs from %s (run with -update after reviewing):\n%s", golden, got)
	}

	// Targeted assertions that document intent independently of the golden.
	if cat.Table("public", "measurement_2026_01") != nil {
		t.Error("partitions must be omitted by default")
	}
	for _, name := range []string{"customer_v", "customer_mv"} {
		if cat.Table("public", name) != nil {
			t.Errorf("%s (view/matview) must not be reported", name)
		}
	}
	if cat.Table("ignored", "hidden") != nil {
		t.Error("schema filter not applied")
	}
	cafe := cat.Table("public", "café")
	if cafe == nil || cafe.Column("código") == nil || cafe.Column("preço") == nil {
		t.Fatalf("non-ASCII identifiers not preserved: %+v", cafe)
	}
	note := cat.Table("public", "measurement_note")
	if note == nil || len(note.ForeignKeys) != 1 || note.ForeignKeys[0].RefTable != "measurement" {
		t.Errorf("FK to partitioned table must appear once, pointing at the parent: %+v", note)
	}
	// No pg_stats rows are visible without SELECT privilege.
	for _, tb := range cat.Tables {
		for _, c := range tb.Columns {
			if c.Stats.NDistinct != nil {
				t.Errorf("%s.%s: n_distinct visible to catalog-only role", tb.ID(), c.Name)
			}
		}
	}
}

func TestIntrospectPartitionsAndStats(t *testing.T) {
	in := openRole(t, "kabr_unsafe")
	opts := fixtureOptions()
	opts.IncludePartitions = true
	cat, err := in.Introspect(context.Background(), opts)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	for _, p := range []string{"measurement_2026_01", "measurement_2026_02"} {
		if cat.Table("public", p) == nil {
			t.Errorf("partition %s missing with IncludePartitions", p)
		}
	}
	// kabr_unsafe may SELECT, so pg_stats exposes the analyzed columns.
	c := cat.Table("public", "customer").Column("name")
	if c.Stats.NDistinct == nil || *c.Stats.NDistinct == 0 {
		t.Errorf("expected n_distinct for customer.name, got %v", c.Stats.NDistinct)
	}
	if rows := cat.Table("public", "customer").Stats.EstimatedRows; rows == nil || *rows != 4 {
		t.Errorf("customer estimated rows = %v, want 4", rows)
	}

	opts.IncludeStats = false
	cat, err = in.Introspect(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, tb := range cat.Tables {
		if tb.Stats.EstimatedRows != nil {
			t.Errorf("%s: stats present with IncludeStats=false", tb.ID())
		}
	}
}

func TestCheckPrivileges(t *testing.T) {
	ctx := context.Background()
	reader := openRole(t, "kabr_reader")
	rep, err := reader.CheckPrivileges(ctx, fixtureOptions())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Role != "kabr_reader" || rep.Superuser || !rep.ReadOnly || len(rep.Findings) != 0 {
		t.Errorf("reader report = %+v, want clean", rep)
	}

	unsafe := openRole(t, "kabr_unsafe")
	rep, err = unsafe.CheckPrivileges(ctx, fixtureOptions())
	if err != nil {
		t.Fatal(err)
	}
	if rep.ReadOnly || rep.Superuser {
		t.Errorf("unsafe report must not be read-only: %+v", rep)
	}
	want := []introspect.Finding{
		{Object: "role kabr_unsafe", Privilege: "CREATEDB"},
		{Object: "schema public", Privilege: "CREATE"},
		{Object: "table public.customer", Privilege: "DELETE"},
		{Object: "table public.customer", Privilege: "INSERT"},
		{Object: "table public.customer", Privilege: "TRUNCATE"},
		{Object: "table public.customer", Privilege: "UPDATE"},
		{Object: "table public.measurement_2026_01", Privilege: "INSERT"},
	}
	for _, f := range want {
		if !slices.Contains(rep.Findings, f) {
			t.Errorf("missing finding %+v in %+v", f, rep.Findings)
		}
	}
	for _, f := range rep.Findings {
		if strings.HasPrefix(f.Object, "table sales.") || f.Object == "schema sales" {
			t.Errorf("unexpected finding on read-only schema: %+v", f)
		}
	}
	if !slices.IsSortedFunc(rep.Findings, func(a, b introspect.Finding) int {
		return strings.Compare(a.Object+"\x00"+a.Privilege, b.Object+"\x00"+b.Privilege)
	}) {
		t.Error("findings not sorted")
	}
}

// TestSessionIsReadOnly proves writes fail even for a role that holds
// INSERT and CREATE: both inside the introspector's transaction and on the
// bare session (default_transaction_read_only=on).
func TestSessionIsReadOnly(t *testing.T) {
	in := openRole(t, "kabr_unsafe")
	ctx := context.Background()
	err := in.readOnly(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO public.orphan_log (message) VALUES ('x')")
		return err
	})
	if !isSQLState(err, "25006") {
		t.Errorf("write inside read-only tx: want SQLSTATE 25006, got %v", err)
	}
	_, err = in.conn.Exec(ctx, "CREATE TABLE public.kabr_should_not_exist (id int)")
	if !isSQLState(err, "25006") {
		t.Errorf("DDL on session: want SQLSTATE 25006, got %v", err)
	}
	var app, ro, timeout string
	if err := in.conn.QueryRow(ctx,
		"SELECT current_setting('application_name'), current_setting('default_transaction_read_only'), current_setting('statement_timeout')").
		Scan(&app, &ro, &timeout); err != nil {
		t.Fatal(err)
	}
	if app != "kabr" || ro != "on" || timeout != "15s" {
		t.Errorf("session params = %q %q %q", app, ro, timeout)
	}
}

func TestConnectErrorDoesNotLeakPassword(t *testing.T) {
	wrong := testutil.Secret()
	dsn := fixtureDSN(t, "kabr_reader", wrong)
	_, err := Open(context.Background(), dsn, introspect.Config{})
	if err == nil {
		t.Fatal("expected authentication failure")
	}
	if strings.Contains(err.Error(), wrong) {
		t.Errorf("error leaks password: %v", err)
	}
	if !isSQLState(err, "28P01") {
		t.Errorf("errors.As to *pgconn.PgError should still work, got %v", err)
	}
}

func TestOpenViaRegistry(t *testing.T) {
	in, err := introspect.Open(context.Background(), roleDSN(t, "kabr_reader"), introspect.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if _, err := in.Introspect(context.Background(), fixtureOptions()); err != nil {
		t.Fatal(err)
	}
}

// TestDemoPagila runs against the docker compose demo when KABR_DEMO_DSN is
// set; `make test-demo` sets it from deploy/compose/.env.demo.
func TestDemoPagila(t *testing.T) {
	dsn := os.Getenv("KABR_DEMO_DSN")
	if dsn == "" {
		t.Skip("KABR_DEMO_DSN not set")
	}
	in, err := introspect.Open(context.Background(), dsn, introspect.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	cat, err := in.Introspect(context.Background(), introspect.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Tables) != 15 || cat.Table("public", "payment") == nil {
		t.Errorf("pagila: got %d tables, want 15 including payment", len(cat.Tables))
	}
	rep, err := in.CheckPrivileges(context.Background(), introspect.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("demo role %s read-only=%v findings=%d", rep.Role, rep.ReadOnly, len(rep.Findings))
}

func isSQLState(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}
