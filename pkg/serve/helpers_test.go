package serve

import (
	"strings"
	"time"

	"github.com/Gabrielmmorais/kabr/pkg/plan"
	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

var testFingerprint = "sha256:" + strings.Repeat("ab", 32)

func validConfig() *Config {
	return &Config{
		Domain:             "film",
		Plan:               "pagila",
		CatalogFingerprint: testFingerprint,
		Schemas:            []string{"public"},
		RowLimit:           100,
		StatementTimeout:   Duration(5 * time.Second),
		Tables:             []string{"public.film", "public.customer"},
		SensitiveColumns: []plan.SensitiveColumn{
			{Table: "public.customer", Column: "email", Class: plan.ClassPII, Action: plan.ActionMask},
		},
	}
}

func col(name, typ string, nullable bool) schema.Column {
	return schema.Column{Name: name, DataType: typ, Nullable: nullable}
}

func tbl(sch, name string, pk []string, cols ...schema.Column) schema.Table {
	for i := range cols {
		cols[i].Ordinal = i + 1
	}
	t := schema.Table{Schema: sch, Name: name, Columns: cols}
	if pk != nil {
		t.PrimaryKey = &schema.PrimaryKey{Name: name + "_pkey", Columns: pk}
	}
	return t
}

// testCatalog covers every kind, keys of several shapes and hostile names.
func testCatalog() *schema.Catalog {
	c := schema.New(schema.DialectPostgres, "app")
	c.Schemas = []string{"public"}
	c.Tables = []schema.Table{
		tbl("public", "film", []string{"film_id"},
			col("film_id", "integer", false),
			col("title", "text", false),
			col("rating", "mpaa_rating", true),
			col("rental_rate", "numeric(4,2)", false),
			col("ratio", "double precision", true),
			col("active", "boolean", false),
			col("last_update", "timestamp with time zone", false),
			col("released_on", "date", true),
			col("poster", "bytea", true),
			col("features", "text[]", true),
			col("meta", "jsonb", true),
			col("uid", "uuid", true),
			col("limit", "integer", true),
		),
		tbl("public", "customer", []string{"customer_id"},
			col("customer_id", "bigint", false),
			col("email", "text", true),
			col("password", "text", true),
			col("card", "text", true),
			col("first_name", "text", false),
		),
		tbl("public", "rental_event", []string{"store_id", "happened_at", "seq"},
			col("store_id", "integer", false),
			col("happened_at", "timestamp without time zone", false),
			col("seq", "smallint", false),
			col("note", "text", true),
		),
		tbl("public", "rating_info", []string{"rating"},
			col("rating", "mpaa_rating", false),
			col("description", "text", false),
		),
		tbl("public", "staging", nil, col("line", "text", true)),
		tbl("public", "doc", []string{"body"}, col("body", "jsonb", false)),
		tbl("public", `we"ird`, []string{`i"d`}, col(`i"d`, "integer", false), col("café", "text", true)),
	}
	return c
}

func catalogConfig() *Config {
	c := validConfig()
	c.Tables = []string{"public.film", "public.customer", "public.rental_event", "public.rating_info",
		"public.staging", "public.doc", `public.we"ird`}
	c.SensitiveColumns = []plan.SensitiveColumn{
		{Table: "public.customer", Column: "email", Class: plan.ClassPII, Action: plan.ActionMask},
		{Table: "public.customer", Column: "password", Class: plan.ClassCredential, Action: plan.ActionMask},
		{Table: "public.customer", Column: "card", Class: plan.ClassFinancial, Action: plan.ActionHash},
		{Table: "public.customer", Column: "first_name", Class: plan.ClassPII, Action: plan.ActionDrop},
	}
	return c
}

var unitHashKey = []byte("0123456789abcdef0123456789abcdef")
