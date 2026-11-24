package serve

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestProjections(t *testing.T) {
	m, _ := mustResolve(t, catalogConfig(), unitHashKey)
	film := m.table("public", "film")
	want := `"film_id", "title", "rating"::pg_catalog.text AS "rating", "rental_rate"::pg_catalog.text AS "rental_rate", ` +
		`"ratio", "active", "last_update", "released_on"::pg_catalog.text AS "released_on", "poster", ` +
		`pg_catalog.array_to_json("features")::pg_catalog.text AS "features", "meta"::pg_catalog.text AS "meta", ` +
		`"uid"::pg_catalog.text AS "uid", "limit"`
	if got := film.selectList(); got != want {
		t.Errorf("film select list:\n got %s\nwant %s", got, want)
	}
	cust := m.table("public", "customer")
	want = `"customer_id", CASE WHEN "email" IS NULL THEN NULL ELSE '***' END AS "email", "card"::pg_catalog.text AS "card"`
	if got := cust.selectList(); got != want {
		t.Errorf("customer select list:\n got %s\nwant %s", got, want)
	}
	weird := m.table("public", `we"ird`)
	if got := buildProbe(weird); got != `SELECT "i""d", "café" FROM "public"."we""ird" LIMIT 0` {
		t.Errorf("probe = %s", got)
	}
}

func TestBuildList(t *testing.T) {
	m, _ := mustResolve(t, catalogConfig(), unitHashKey)
	cust := m.table("public", "customer")
	tests := []struct {
		name     string
		table    *table
		q        listQuery
		wantSQL  string
		wantArgs []any
	}{
		{
			name:     "first page",
			table:    cust,
			q:        listQuery{limit: 10},
			wantSQL:  `SELECT ` + cust.selectList() + ` FROM "public"."customer" ORDER BY "customer_id" ASC LIMIT $1`,
			wantArgs: []any{11},
		},
		{
			name:     "filter and cursor",
			table:    cust,
			q:        listQuery{limit: 5, filters: []filterArg{{col: 0, value: "42"}}, after: []string{"41"}},
			wantSQL:  `SELECT ` + cust.selectList() + ` FROM "public"."customer" WHERE "customer_id" = $1 AND "customer_id" > $2 ORDER BY "customer_id" ASC LIMIT $3`,
			wantArgs: []any{"42", "41", 6},
		},
		{
			name:  "composite key",
			table: m.table("public", "rental_event"),
			q: listQuery{limit: 100, filters: []filterArg{{col: 3, value: "x"}},
				after: []string{"1", "2026-02-01T10:00:00.000001Z", "2"}},
			wantSQL: `SELECT "store_id", "happened_at", "seq", "note" FROM "public"."rental_event" WHERE "note" = $1 ` +
				`AND ("store_id", "happened_at", "seq") > ($2, $3, $4) ORDER BY "store_id" ASC, "happened_at" ASC, "seq" ASC LIMIT $5`,
			wantArgs: []any{"x", "1", "2026-02-01T10:00:00.000001Z", "2", 101},
		},
		{
			name:  "text-compared key and filter",
			table: m.table("public", "rating_info"),
			q:     listQuery{limit: 2, filters: []filterArg{{col: 0, value: "PG"}}, after: []string{"G"}},
			wantSQL: `SELECT "rating"::pg_catalog.text AS "rating", "description" FROM "public"."rating_info" ` +
				`WHERE "rating"::pg_catalog.text = $1 AND "rating"::pg_catalog.text > $2 ORDER BY "rating"::pg_catalog.text ASC LIMIT $3`,
			wantArgs: []any{"PG", "G", 3},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sql, args := buildList(tt.table, tt.q)
			if sql != tt.wantSQL {
				t.Errorf("sql:\n got %s\nwant %s", sql, tt.wantSQL)
			}
			if !reflect.DeepEqual(args, tt.wantArgs) {
				t.Errorf("args = %#v, want %#v", args, tt.wantArgs)
			}
		})
	}
}

func TestBuildGet(t *testing.T) {
	m, _ := mustResolve(t, catalogConfig(), unitHashKey)
	sql, args := buildGet(m.table("public", "rating_info"), "PG-13")
	want := `SELECT "rating"::pg_catalog.text AS "rating", "description" FROM "public"."rating_info" WHERE "rating"::pg_catalog.text = $1 LIMIT 1`
	if sql != want || !reflect.DeepEqual(args, []any{"PG-13"}) {
		t.Errorf("get:\n got %s %v\nwant %s", sql, args, want)
	}
}

// TestRequestValuesNeverReachSQL drives the full request parser with
// hostile input: the statement text is the same whatever the values are
// (it only depends on which allowlisted columns are filtered and whether a
// cursor is present), and every value ends up in the arguments.
func TestRequestValuesNeverReachSQL(t *testing.T) {
	m, _ := mustResolve(t, catalogConfig(), unitHashKey)
	film := m.table("public", "film")
	build := func(v string) (string, []any) {
		t.Helper()
		q, _, aerr := parseListQuery(film, "title="+urlQuery(v)+"&rating="+urlQuery(v)+
			"&cursor="+encodeCursor([]string{v}), 50)
		if aerr != nil {
			t.Fatalf("%q rejected: %v", v, aerr)
		}
		return buildList(film, q)
	}
	baseline, _ := build("benign")
	hostile := []string{
		`x'); DROP TABLE public.film; --`, `" OR 1=1 --`, `$1`, `\'`, `film_id`, `*/ SELECT 1 /*`,
		"1; DELETE FROM public.film", `E'\x41'`, `"title"`, `pg_catalog.pg_sleep(10)`, "line\nbreak",
	}
	for _, v := range hostile {
		sql, args := build(v)
		if sql != baseline {
			t.Errorf("value %q changed the SQL text:\n%s\nvs\n%s", v, sql, baseline)
		}
		if n := strings.Count(sql, "$"); n != len(args) {
			t.Errorf("%d placeholders for %d args", n, len(args))
		}
		for i, a := range args[:3] {
			if a != v {
				t.Errorf("arg %d = %v, want the request value", i, a)
			}
		}
	}
}

func urlQuery(s string) string { return url.QueryEscape(s) }
