package postgres

import (
	"cmp"
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/Gabrielmmorais/kabr/pkg/introspect"
)

// CheckPrivileges implements [introspect.Introspector]. It runs three
// batched queries (role + database, schemas, tables), never one per object.
//
// Privileges are evaluated for current_user with has_*_privilege, which
// includes PUBLIC grants and privileges inherited through role membership
// (for INHERIT memberships). Privileges reachable only via SET ROLE to a
// NOINHERIT membership are not reported.
func (p *Introspector) CheckPrivileges(ctx context.Context, opts introspect.Options) (introspect.PrivilegeReport, error) {
	schemas := opts.EffectiveSchemas()
	var rep introspect.PrivilegeReport
	err := p.readOnly(ctx, func(tx pgx.Tx) error {
		var (
			bypassRLS, createRole, createDB, replication, dbCreate bool
			db                                                     string
		)
		err := tx.QueryRow(ctx, qRole).Scan(&rep.Role, &rep.Superuser, &bypassRLS, &createRole,
			&createDB, &replication, &db, &dbCreate)
		if errors.Is(err, pgx.ErrNoRows) {
			return p.wrap("read role", errNoRole)
		}
		if err != nil {
			return p.wrap("read role", err)
		}
		add := func(obj, priv string) {
			rep.Findings = append(rep.Findings, introspect.Finding{Object: obj, Privilege: priv})
		}
		roleObj := "role " + rep.Role
		for _, a := range []struct {
			on   bool
			name string
		}{
			{rep.Superuser, "SUPERUSER"}, {bypassRLS, "BYPASSRLS"}, {createRole, "CREATEROLE"},
			{createDB, "CREATEDB"}, {replication, "REPLICATION"},
		} {
			if a.on {
				add(roleObj, a.name)
			}
		}
		if dbCreate {
			add("database "+db, "CREATE")
		}

		rows, err := tx.Query(ctx, qSchemaPrivileges, schemas)
		if err != nil {
			return p.wrap("check schema privileges", err)
		}
		for rows.Next() {
			var (
				name   string
				create bool
			)
			if err = rows.Scan(&name, &create); err != nil {
				rows.Close()
				return p.wrap("scan schema privileges", err)
			}
			if create {
				add("schema "+name, "CREATE")
			}
		}
		if err = rows.Err(); err != nil {
			return p.wrap("check schema privileges", err)
		}

		rows, err = tx.Query(ctx, qTablePrivileges, schemas)
		if err != nil {
			return p.wrap("check table privileges", err)
		}
		for rows.Next() {
			var (
				sch, tbl string
				privs    [6]bool
			)
			if err := rows.Scan(&sch, &tbl, &privs[0], &privs[1], &privs[2], &privs[3], &privs[4], &privs[5]); err != nil {
				rows.Close()
				return p.wrap("scan table privileges", err)
			}
			for i, name := range []string{"INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"} {
				if privs[i] {
					add("table "+sch+"."+tbl, name)
				}
			}
		}
		return p.wrap("check table privileges", rows.Err())
	})
	if err != nil {
		return introspect.PrivilegeReport{}, err
	}
	slices.SortFunc(rep.Findings, func(a, b introspect.Finding) int {
		return cmp.Or(cmp.Compare(a.Object, b.Object), cmp.Compare(a.Privilege, b.Privilege))
	})
	rep.ReadOnly = !rep.Superuser && len(rep.Findings) == 0
	return rep, nil
}
