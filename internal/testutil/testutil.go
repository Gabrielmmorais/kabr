// Package testutil holds helpers shared by Kabr's tests. Production code
// must not import it.
//
// Credentials used by tests are generated at run time, never written in the
// source: the repository is public and secret scanners rightly flag any
// committed password, even a fake one.
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"

	"github.com/jackc/pgx/v5"
)

// Secret returns a fresh random secret of 32 lowercase hex characters.
// Hex keeps it safe inside URLs, keyword/value DSNs and SQL literals, and
// two calls never return the same value in practice.
func Secret() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // never returns an error (crypto/rand, Go 1.24+)
	return hex.EncodeToString(b[:])
}

// PostgresURL returns a postgres:// URL for the given role, built with
// net/url so that the user and password are escaped correctly.
func PostgresURL(user, password, host, port, database string) string {
	return (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(user, password),
		Host:     net.JoinHostPort(host, port),
		Path:     "/" + database,
		RawQuery: "sslmode=disable",
	}).String()
}

// SetRolePasswords connects with adminDSN (a superuser) and sets the
// password of each role. ALTER ROLE does not take bind parameters, so the
// statement is built server-side with format('%I', '%L'), which quotes the
// identifier and the literal safely.
func SetRolePasswords(ctx context.Context, adminDSN string, passwords map[string]string) error {
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return fmt.Errorf("connect as admin: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	for role, pw := range passwords {
		var stmt string
		if err := conn.QueryRow(ctx, "SELECT format('ALTER ROLE %I PASSWORD %L', $1::text, $2::text)", role, pw).Scan(&stmt); err != nil {
			return fmt.Errorf("build ALTER ROLE for %s: %w", role, err)
		}
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("set password for %s: %w", role, err)
		}
	}
	return nil
}
