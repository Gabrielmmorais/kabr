package cli

import (
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/Gabrielmmorais/kabr/pkg/introspect"
)

// writePrivilegeWarnings prints one line per finding, each with the SQL
// that removes it, followed by general advice. It prints nothing for a
// read-only role.
func writePrivilegeWarnings(w io.Writer, rep introspect.PrivilegeReport) {
	if rep.ReadOnly {
		return
	}
	n := privilegeCount(rep)
	fmt.Fprintf(w, "kabr: warning: role %q holds %d privilege(s) beyond read-only catalog access:\n", rep.Role, n)
	if len(rep.Findings) == 0 && rep.Superuser {
		fmt.Fprintf(w, "  - role %s: SUPERUSER  (fix: ALTER ROLE %s NOSUPERUSER;)\n", rep.Role, quoteIdent(rep.Role))
	}
	var tableSchemas []string // schemas with table findings, in first-seen order
	for _, f := range rep.Findings {
		line := fmt.Sprintf("  - %s: %s", f.Object, f.Privilege)
		if fix := fixFor(rep.Role, f); fix != "" {
			line += "  (fix: " + fix + ")"
		}
		fmt.Fprintln(w, line)
		if kind, name, _ := strings.Cut(f.Object, " "); kind == "table" {
			if s, _, ok := strings.Cut(name, "."); ok && !slices.Contains(tableSchemas, s) {
				tableSchemas = append(tableSchemas, s)
			}
		}
	}
	if len(tableSchemas) > 0 {
		fmt.Fprintln(w, "kabr: hint: to revoke every table-level write privilege at once:")
		for _, s := range tableSchemas {
			fmt.Fprintf(w, "  REVOKE INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER ON ALL TABLES IN SCHEMA %s FROM %s;\n",
				quoteIdent(s), quoteIdent(rep.Role))
		}
	}
	fmt.Fprint(w, `kabr: hint: Kabr only reads the system catalog. A role with LOGIN, CONNECT on
  the database and USAGE on the schemas is enough. Grants may also come from
  PUBLIC or from roles this role is a member of; revoke them there if a REVOKE
  on the role itself does not clear the finding. Use --strict to refuse to run
  with an over-privileged role.
`)
}

// fixFor returns SQL that removes one finding, or "" when unknown.
func fixFor(role string, f introspect.Finding) string {
	kind, name, ok := strings.Cut(f.Object, " ")
	if !ok {
		return ""
	}
	r := quoteIdent(role)
	switch kind {
	case "role":
		return fmt.Sprintf("ALTER ROLE %s NO%s;", quoteIdent(name), f.Privilege)
	case "database":
		return fmt.Sprintf("REVOKE %s ON DATABASE %s FROM %s;", f.Privilege, quoteIdent(name), r)
	case "schema":
		return fmt.Sprintf("REVOKE %s ON SCHEMA %s FROM %s;", f.Privilege, quoteIdent(name), r)
	case "table":
		s, t, ok := strings.Cut(name, ".")
		if !ok {
			return ""
		}
		return fmt.Sprintf("REVOKE %s ON TABLE %s.%s FROM %s;", f.Privilege, quoteIdent(s), quoteIdent(t), r)
	}
	return ""
}

var simpleIdentRe = regexp.MustCompile(`^[a-z_][a-z0-9_$]*$`)

// quoteIdent double-quotes an SQL identifier unless it is a plain
// lower-case identifier.
func quoteIdent(s string) string {
	if simpleIdentRe.MatchString(s) {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
