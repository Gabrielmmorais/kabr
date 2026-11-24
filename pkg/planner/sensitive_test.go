package planner

import (
	"slices"
	"strings"
	"testing"

	"github.com/Gabrielmmorais/kabr/pkg/plan"
	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

func TestClassifySensitivePagila(t *testing.T) {
	got := map[string]plan.SensitiveColumn{}
	for _, sc := range ClassifySensitive(loadPagila(t)) {
		if sc.Source != plan.SourceHeuristic || !strings.Contains(sc.Rationale, "⇒") {
			t.Errorf("%s.%s: source %q rationale %q", sc.Table, sc.Column, sc.Source, sc.Rationale)
		}
		got[sc.Table+"."+sc.Column] = sc
	}
	want := map[string]struct {
		class plan.SensitiveClass
		act   plan.Action
	}{
		"public.staff.password":      {plan.ClassCredential, plan.ActionDrop},
		"public.staff.username":      {plan.ClassCredential, plan.ActionHash},
		"public.customer.email":      {plan.ClassPII, plan.ActionMask},
		"public.staff.email":         {plan.ClassPII, plan.ActionMask},
		"public.address.phone":       {plan.ClassPII, plan.ActionMask},
		"public.address.address":     {plan.ClassPII, plan.ActionMask},
		"public.customer.first_name": {plan.ClassPII, plan.ActionMask},
		"public.customer.last_name":  {plan.ClassPII, plan.ActionMask},
		"public.staff.first_name":    {plan.ClassPII, plan.ActionMask},
		"public.staff.last_name":     {plan.ClassPII, plan.ActionMask},
		"public.payment.amount":      {plan.ClassFinancial, plan.ActionMask},
		// Accepted extras (documented): a second address line, the postal
		// code (GDPR personal data) and the actors' names (name tokens are
		// PII by default).
		"public.address.address2":    {plan.ClassPII, plan.ActionMask},
		"public.address.postal_code": {plan.ClassPII, plan.ActionMask},
		"public.actor.first_name":    {plan.ClassPII, plan.ActionMask},
		"public.actor.last_name":     {plan.ClassPII, plan.ActionMask},
	}
	for k, w := range want {
		sc, ok := got[k]
		if !ok {
			t.Errorf("%s not flagged", k)
			continue
		}
		if sc.Class != w.class || sc.Action != w.act {
			t.Errorf("%s = %s/%s, want %s/%s", k, sc.Class, sc.Action, w.class, w.act)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected flag %s", k)
		}
	}
}

func TestClassifySensitiveColumns(t *testing.T) {
	tests := []struct {
		col, typ string
		class    plan.SensitiveClass // "" = not flagged
		act      plan.Action
	}{
		// Negative cases.
		{"address_id", "integer", "", ""},
		{"rental_rate", "numeric(4,2)", "", ""},
		{"replacement_cost", "numeric(5,2)", "", ""},
		{"name", "text", "", ""},
		{"password_reset_required", "boolean", "", ""},
		{"password_changed_at", "timestamp with time zone", "", ""},
		{"last_login", "timestamp", "", ""},
		{"login_count", "integer", "", ""},
		{"user_id", "bigint", "", ""},
		{"amount", "text", "", ""},
		{"fulltext", "tsvector", "", ""},
		{"email_verified", "bool", "", ""},
		// Credential tokens on non-textual types.
		{"token_count", "integer", "", ""},
		{"secret", "integer", "", ""},
		{"hash", "bigint", "", ""},
		{"salt", "timestamp", "", ""},
		// Metadata tokens (first or last).
		{"hash_algorithm", "text", "", ""},
		{"password_length", "text", "", ""},
		{"num_tokens", "text", "", ""},
		{"token_type", "text", "", ""},
		{"password_version", "varchar(10)", "", ""},
		{"email_count", "text", "", ""},
		{"phone_kind", "text", "", ""},
		{"zip_size", "text", "", ""},
		// Flags, whatever their type.
		{"email_verified", "integer", "", ""},
		{"phone_confirmed", "smallint", "", ""},
		{"is_email_public", "integer", "", ""},
		{"has_password", "text", "", ""},
		{"isEmailVerified", "integer", "", ""},
		{"mfa_enabled", "char(1)", "", ""},
		// Positive cases.
		{"password_hash", "text", plan.ClassCredential, plan.ActionDrop},
		{"api_key", "varchar(64)", plan.ClassCredential, plan.ActionDrop},
		{"apiKey", "text", plan.ClassCredential, plan.ActionDrop},
		{"private_key", "bytea", plan.ClassCredential, plan.ActionDrop},
		{"refresh_token", "text", plan.ClassCredential, plan.ActionDrop},
		{"user_name", "text", plan.ClassCredential, plan.ActionHash},
		{"login", "character varying(40)", plan.ClassCredential, plan.ActionHash},
		{"date_of_birth", "date", plan.ClassPII, plan.ActionMask},
		{"dob", "date", plan.ClassPII, plan.ActionMask},
		{"tax_id", "text", plan.ClassPII, plan.ActionMask},
		{"national_id", "text", plan.ClassPII, plan.ActionMask},
		{"ip_address", "inet", plan.ClassPII, plan.ActionMask},
		{"e_mail", "text", plan.ClassPII, plan.ActionMask},
		{"mobile_phone", "text", plan.ClassPII, plan.ActionMask},
		{"FirstName", "text", plan.ClassPII, plan.ActionMask},
		{"street", "text", plan.ClassPII, plan.ActionMask},
		{"ssn", "char(11)", plan.ClassPII, plan.ActionMask},
		{"salary", "money", plan.ClassFinancial, plan.ActionMask},
		{"account_balance", "double precision", plan.ClassFinancial, plan.ActionMask},
		{"credit_card", "text", plan.ClassFinancial, plan.ActionMask},
		{"card_number", "text", plan.ClassFinancial, plan.ActionMask},
		{"iban", "text", plan.ClassFinancial, plan.ActionMask},
		{"account_number", "text", plan.ClassFinancial, plan.ActionMask},
		{"user_password_hash", "text", plan.ClassCredential, plan.ActionDrop},
		{"password", "bytea", plan.ClassCredential, plan.ActionDrop},
		// camelCase.
		{"passwordHash", "text", plan.ClassCredential, plan.ActionDrop},
		{"APIKey", "text", plan.ClassCredential, plan.ActionDrop},
		{"userName", "text", plan.ClassCredential, plan.ActionHash},
		{"emailAddress", "text", plan.ClassPII, plan.ActionMask},
		{"userEmail", "text", plan.ClassPII, plan.ActionMask},
		{"phoneNumber", "text", plan.ClassPII, plan.ActionMask},
		{"lastName", "text", plan.ClassPII, plan.ActionMask},
		{"firstname", "text", plan.ClassPII, plan.ActionMask},
		{"LASTNAME", "text", plan.ClassPII, plan.ActionMask},
		{"creditCardNumber", "text", plan.ClassFinancial, plan.ActionMask},
		{"Address", "text", plan.ClassPII, plan.ActionMask},
		// Card verification codes and postal codes.
		{"cvv", "char(3)", plan.ClassFinancial, plan.ActionDrop},
		{"card_cvc", "smallint", plan.ClassFinancial, plan.ActionDrop},
		{"cvv2", "text", plan.ClassFinancial, plan.ActionDrop},
		{"postal_code", "text", plan.ClassPII, plan.ActionMask},
		{"zip_code", "varchar(10)", plan.ClassPII, plan.ActionMask},
		{"zip", "integer", plan.ClassPII, plan.ActionMask},
		{"postalCode", "text", plan.ClassPII, plan.ActionMask},
	}
	for _, tt := range tests {
		t.Run(tt.col, func(t *testing.T) {
			c := &schema.Catalog{Tables: []schema.Table{{Schema: "s", Name: "t", Columns: []schema.Column{{Name: tt.col, DataType: tt.typ}}}}}
			got := ClassifySensitive(c)
			if tt.class == "" {
				if len(got) != 0 {
					t.Fatalf("flagged %+v, want nothing", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("got %+v, want one %s", got, tt.class)
			}
			if got[0].Class != tt.class || got[0].Action != tt.act || got[0].Table != "s.t" || got[0].Column != tt.col {
				t.Errorf("got %+v, want %s/%s", got[0], tt.class, tt.act)
			}
		})
	}
}

func TestClassifySensitiveNil(t *testing.T) {
	if got := ClassifySensitive(nil); got != nil {
		t.Errorf("got %v", got)
	}
}

func TestTokenize(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"first_name", []string{"first", "name"}},
		{"firstname", []string{"firstname"}},
		{"FirstName", []string{"first", "name"}},
		{"creditCardNumber", []string{"credit", "card", "number"}},
		{"APIKey", []string{"api", "key"}},
		{"userID", []string{"user", "id"}},
		{"address2", []string{"address2"}},
		{"cvv2Code", []string{"cvv2", "code"}},
		{"__e-mail__", []string{"e", "mail"}},
		{"LASTNAME", []string{"lastname"}},
		{"", nil},
	}
	for _, tt := range tests {
		if got := tokenize(tt.in); !slices.Equal(got, tt.want) {
			t.Errorf("tokenize(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
