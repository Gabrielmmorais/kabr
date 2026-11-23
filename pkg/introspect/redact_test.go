package introspect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Gabrielmmorais/kabr/internal/testutil"
)

// Every secret below is generated at run time; the punctuation that makes
// redaction hard ('@', '/', ':', '#', '?', quotes, spaces, bad escapes) is
// spliced in between random parts, so no credential is written in the source.

// pgURL builds a postgres:// URL by plain concatenation, without escaping.
func pgURL(user, password, rest string) string {
	return "postgres://" + user + ":" + password + "@" + rest
}

func TestRedactDSN(t *testing.T) {
	s := testutil.Secret
	plain, q1, q2 := s(), s(), s()
	special := s() + "@" + s() + "/w:" + s() + "#1"
	encodedHead := s() + "%40"
	encoded := encodedHead + s() + "%2F" + s()
	question := s() + "?" + s()
	badEscape := s() + "%zz" + s()
	kvPlain := s()
	kvA, kvB, kvC, kvD := s(), s(), s(), s()
	unterminated := s()
	redacted := pgURL("kabr", Redacted, "")

	tests := []struct {
		name   string
		dsn    string
		want   string
		secret string // must not appear in the output
	}{
		{
			name:   "url with password",
			dsn:    pgURL("kabr", plain, "pg.example.com:5432/app?sslmode=require"),
			want:   redacted + "pg.example.com:5432/app?sslmode=require",
			secret: plain,
		},
		{
			name: "url without password",
			dsn:  "postgresql://kabr@localhost/app",
			want: "postgresql://kabr@localhost/app",
		},
		{
			name: "url without userinfo",
			dsn:  "postgres://localhost:5432/app?sslmode=disable",
			want: "postgres://localhost:5432/app?sslmode=disable",
		},
		{
			name:   "url password with unescaped at slash colon and hash",
			dsn:    pgURL("kabr", special, "pg:5432/app"),
			want:   redacted + "pg:5432/app",
			secret: special,
		},
		{
			name:   "url password percent-encoded",
			dsn:    pgURL("kabr", encoded, "pg/app"),
			want:   redacted + "pg/app",
			secret: encodedHead,
		},
		{
			name:   "url password with question mark",
			dsn:    pgURL("kabr", question, "pg/app"),
			want:   redacted + "pg/app",
			secret: question,
		},
		{
			name:   "url password in query",
			dsn:    "postgres://pg/app?user=kabr&password=" + q1 + "&sslpassword=" + q2 + "&sslmode=verify-full",
			want:   "postgres://pg/app?user=kabr&password=" + Redacted + "&sslpassword=" + Redacted + "&sslmode=verify-full",
			secret: q1,
		},
		{
			name:   "invalid url escape does not leak",
			dsn:    pgURL("kabr", badEscape, "pg/app"),
			want:   redacted + "pg/app",
			secret: badEscape,
		},
		{
			name:   "keyword value",
			dsn:    "host=pg user=kabr password=" + kvPlain + " dbname=app",
			want:   "host=pg user=kabr password=" + Redacted + " dbname=app",
			secret: kvPlain,
		},
		{
			name:   "keyword value quoted with spaces and escapes",
			dsn:    "host=pg password = '" + kvA + " " + kvB + `\'` + kvC + "@" + kvD + "' dbname=app",
			want:   "host=pg password = " + Redacted + " dbname=app",
			secret: kvA + " " + kvB + `\'` + kvC + "@" + kvD,
		},
		{
			name:   "keyword value unterminated quote",
			dsn:    "host=pg password='" + unterminated + " " + s(),
			want:   "host=pg password=" + Redacted,
			secret: unterminated,
		},
		{
			name: "empty",
			dsn:  "",
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactDSN(tc.dsn)
			if got != tc.want {
				t.Errorf("RedactDSN(%q) = %q, want %q", tc.dsn, got, tc.want)
			}
			if tc.secret != "" && strings.Contains(got, tc.secret) {
				t.Errorf("RedactDSN(%q) leaked secret %q: %q", tc.dsn, tc.secret, got)
			}
		})
	}
}

func TestRedactText(t *testing.T) {
	head, tail, kvPw, literal := testutil.Secret(), testutil.Secret(), testutil.Secret(), testutil.Secret()
	// The URL password itself contains an unescaped '@'.
	msg := "failed to connect to " + pgURL("kabr", head+"@"+tail, "pg/app") +
		": password=" + kvPw + ` rejected for "kabr", literal ` + literal
	got := RedactText(msg, literal, "")
	for _, leak := range []string{head, tail + "@pg", kvPw, literal} {
		if strings.Contains(got, leak) {
			t.Errorf("RedactText leaked %q: %q", leak, got)
		}
	}
	if !strings.Contains(got, pgURL("kabr", Redacted, "pg/app")) {
		t.Errorf("RedactText lost the redacted URL: %q", got)
	}
}

func TestSanitizeError(t *testing.T) {
	if SanitizeError("x", nil) != nil {
		t.Fatal("SanitizeError(nil) must be nil")
	}
	pw := testutil.Secret()
	cause := fmt.Errorf("dial %s: %w", pgURL("u", pw, "h/db"), context.DeadlineExceeded)
	err := SanitizeError("introspect: connect", cause, pw)
	if strings.Contains(err.Error(), pw) {
		t.Errorf("leaked secret: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "introspect: connect: ") {
		t.Errorf("missing prefix: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is lost the cause chain")
	}
}

func TestDialectForDSN(t *testing.T) {
	tests := []struct {
		dsn     string
		want    string
		wantErr bool
	}{
		{"postgres://u@h/db", "postgres", false},
		{"PostgreSQL://u@h/db", "postgres", false},
		{"host=h dbname=db", "postgres", false},
		{"mysql://u@h/db", "mysql", false},
		{"sqlserver://u@h?database=db", "sqlserver", false},
		{"oracle://u@h/db", "", true},
		{"", "", true},
		{"garbage", "", true},
	}
	for _, tc := range tests {
		got, err := DialectForDSN(tc.dsn)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("DialectForDSN(%q) = %q, %v; want %q, err=%v", tc.dsn, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestOpenUnsupportedDialect(t *testing.T) {
	pw := testutil.Secret()
	_, err := Open(context.Background(), "mysql://root:"+pw+"@pg.example/db", Config{})
	if !errors.Is(err, ErrDialectNotSupported) {
		t.Fatalf("want ErrDialectNotSupported, got %v", err)
	}
	if strings.Contains(err.Error(), pw) {
		t.Errorf("leaked secret: %v", err)
	}
	_, err = Open(context.Background(), "sqlserver://sa:"+pw+"@pg.example", Config{})
	if !errors.Is(err, ErrDialectNotSupported) {
		t.Fatalf("want ErrDialectNotSupported, got %v", err)
	}
}

func TestOpenPostgresNotRegistered(t *testing.T) {
	// This package's tests do not import the postgres driver.
	pw := testutil.Secret()
	_, err := Open(context.Background(), pgURL("u", pw, "pg.example/db"), Config{})
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("want not-registered error, got %v", err)
	}
	if strings.Contains(err.Error(), pw) {
		t.Errorf("leaked secret: %v", err)
	}
}

func TestDefaults(t *testing.T) {
	o := DefaultOptions()
	if !o.IncludeStats || len(o.Schemas) != 1 || o.Schemas[0] != "public" || o.IncludePartitions {
		t.Errorf("DefaultOptions() = %+v", o)
	}
	if got := (Options{}).EffectiveSchemas(); len(got) != 1 || got[0] != "public" {
		t.Errorf("EffectiveSchemas() = %v", got)
	}
	c := Config{}.WithDefaults()
	if c.StatementTimeout != DefaultStatementTimeout || c.ConnectTimeout != DefaultConnectTimeout {
		t.Errorf("WithDefaults() = %+v", c)
	}
}
