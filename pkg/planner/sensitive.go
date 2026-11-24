package planner

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/Gabrielmmorais/kabr/pkg/plan"
	"github.com/Gabrielmmorais/kabr/pkg/schema"
)

// typeClass is the coarse family of a column data type.
type typeClass int

const (
	typeOther typeClass = iota
	typeBool
	typeTemporal
	typeNumeric
)

// sensitiveRule flags a column when its name matches.
type sensitiveRule struct {
	// tokens are single name tokens matched anywhere in the name.
	tokens []string
	// phrases are consecutive token sequences, e.g. {"first", "name"}.
	phrases [][]string
	// exact are whole column names (lowercased).
	exact []string
	class plan.SensitiveClass
	act   plan.Action
	// allowID lets the rule match names ending in an identifier token (tax_id).
	allowID bool
	// types restricts the data type families; nil means any family except
	// boolean and temporal.
	types []typeClass
}

// sensitiveRules is evaluated in order; the first matching rule wins.
//
// Rationale per group:
//   - credential/drop: secrets that must never leave the database, not even
//     hashed (a password hash is still crackable material). Only textual or
//     binary columns qualify: an integer "token_count" holds no secret.
//   - credential/hash: login identifiers; hashing keeps them joinable without
//     revealing them.
//   - pii/mask: direct identifiers and contact data of people. Person names
//     are flagged in every table (including Pagila's actor, whose rows are
//     public figures) because name tokens are PII by default; a human or the
//     LLM refinement can downgrade them.
//   - pii/mask also covers postal codes (postal_code, zip_code, zip): they
//     are personal data under GDPR when tied to a person's address. On
//     Pagila this flags address.postal_code, which the gold standard omits
//     (an accepted precision cost).
//   - financial/drop: card verification codes (cvv, cvc) must never be
//     stored or exposed (PCI DSS), so they are dropped, not masked.
//   - financial/mask: monetary values and payment instruments. Generic price
//     words ("rate", "cost", "price") are deliberately not flagged: they
//     usually describe products, not people.
//
// Guards applied before any rule:
//   - boolean columns are never flagged (a flag such as
//     password_reset_required carries no secret), and neither are flag-named
//     columns of any type: first token is/has/can/should, or last token
//     verified/confirmed/enabled (email_verified integer);
//   - metadata about a value is not the value: a first or last token among
//     count, num, len, length, size, algorithm, algo, type, kind, version
//     (token_count, hash_algorithm, password_length, num_tokens);
//   - temporal columns match only birth dates (password_changed_at,
//     last_login are metadata, not secrets);
//   - columns whose last token is an identifier (address_id, user_id) are
//     skipped unless the rule says otherwise: a surrogate key is not PII by
//     itself.
var sensitiveRules = []sensitiveRule{
	{
		tokens:  []string{"password", "passwd", "pwd", "secret", "token", "apikey", "salt", "hash"},
		phrases: [][]string{{"api", "key"}, {"private", "key"}},
		exact:   []string{"privatekey"},
		class:   plan.ClassCredential, act: plan.ActionDrop,
		types: []typeClass{typeOther},
	},
	{
		tokens:  []string{"username", "login"},
		phrases: [][]string{{"user", "name"}},
		class:   plan.ClassCredential, act: plan.ActionHash,
		types: []typeClass{typeOther},
	},
	{
		tokens: []string{"birth", "birthdate", "birthday", "dob"},
		class:  plan.ClassPII, act: plan.ActionMask,
		types: []typeClass{typeTemporal, typeOther, typeNumeric},
	},
	{
		tokens:  []string{"taxid", "nationalid"},
		phrases: [][]string{{"tax", "id"}, {"national", "id"}},
		class:   plan.ClassPII, act: plan.ActionMask,
		allowID: true,
	},
	{
		tokens: []string{
			"email", "phone", "mobile", "fax", "firstname", "lastname", "fullname",
			"ssn", "passport", "ipaddress",
		},
		phrases: [][]string{
			{"e", "mail"}, {"first", "name"}, {"last", "name"}, {"full", "name"}, {"ip", "address"},
		},
		exact: []string{"address", "address1", "address2", "street"},
		class: plan.ClassPII, act: plan.ActionMask,
	},
	{
		tokens:  []string{"zip", "zipcode", "postcode", "postalcode"},
		phrases: [][]string{{"postal", "code"}, {"zip", "code"}, {"post", "code"}},
		class:   plan.ClassPII, act: plan.ActionMask,
	},
	{
		tokens: []string{"cvv", "cvc", "cvv2", "cvc2"},
		class:  plan.ClassFinancial, act: plan.ActionDrop,
	},
	{
		tokens: []string{"amount", "salary", "balance"},
		class:  plan.ClassFinancial, act: plan.ActionMask,
		types: []typeClass{typeNumeric},
	},
	{
		tokens:  []string{"iban"},
		phrases: [][]string{{"card", "number"}, {"credit", "card"}, {"cc", "number"}, {"account", "number"}},
		exact:   []string{"cardnumber", "creditcard", "ccnumber", "accountnumber"},
		class:   plan.ClassFinancial, act: plan.ActionMask,
	},
}

// idTokens end a name that holds an identifier, not a value.
var idTokens = map[string]bool{"id": true, "ids": true, "uuid": true, "pk": true, "fk": true}

// metaTokens, first or last, mark metadata about a value (token_count).
var metaTokens = map[string]bool{
	"count": true, "num": true, "len": true, "length": true, "size": true,
	"algorithm": true, "algo": true, "type": true, "kind": true, "version": true,
}

// flagPrefixes (first token) and flagSuffixes (last token) mark flags.
var (
	flagPrefixes = map[string]bool{"is": true, "has": true, "can": true, "should": true}
	flagSuffixes = map[string]bool{"verified": true, "confirmed": true, "enabled": true}
)

// ClassifySensitive flags the columns of c that look sensitive, using only
// the column name and data type (never table data). Names are split on "_"
// (and other non-alphanumeric characters) and on camelCase boundaries, then
// lowercased into tokens (passwordHash ⇒ password, hash); the
// rules, evaluated in order with the first match winning, are documented on
// each returned column's Rationale (for example `name token "password" ⇒
// credential; action drop`). Every result has Source [plan.SourceHeuristic].
// The result is sorted by (table, column); a nil catalog yields nil.
func ClassifySensitive(c *schema.Catalog) []plan.SensitiveColumn {
	if c == nil {
		return nil
	}
	var out []plan.SensitiveColumn
	for i := range c.Tables {
		t := &c.Tables[i]
		for _, col := range t.Columns {
			if sc, ok := classifyColumn(t.ID(), col); ok {
				out = append(out, sc)
			}
		}
	}
	slices.SortFunc(out, func(a, b plan.SensitiveColumn) int {
		return cmp.Or(cmp.Compare(a.Table, b.Table), cmp.Compare(a.Column, b.Column))
	})
	return slices.CompactFunc(out, func(a, b plan.SensitiveColumn) bool {
		return a.Table == b.Table && a.Column == b.Column
	})
}

func classifyColumn(tableID string, col schema.Column) (plan.SensitiveColumn, bool) {
	name := strings.ToLower(col.Name)
	toks := tokenize(col.Name)
	if len(toks) == 0 {
		return plan.SensitiveColumn{}, false
	}
	tc := classifyType(col.DataType)
	first, last := toks[0], toks[len(toks)-1]
	if tc == typeBool || flagPrefixes[first] || flagSuffixes[last] ||
		(len(toks) > 1 && (metaTokens[first] || metaTokens[last])) {
		return plan.SensitiveColumn{}, false
	}
	endsWithID := idTokens[last]
	for _, r := range sensitiveRules {
		if !r.acceptsType(tc) || (endsWithID && !r.allowID) {
			continue
		}
		why, ok := r.match(name, toks)
		if !ok {
			continue
		}
		return plan.SensitiveColumn{
			Table:     tableID,
			Column:    col.Name,
			Class:     r.class,
			Action:    r.act,
			Source:    plan.SourceHeuristic,
			Rationale: fmt.Sprintf("%s ⇒ %s; action %s", why, r.class, r.act),
		}, true
	}
	return plan.SensitiveColumn{}, false
}

func (r sensitiveRule) acceptsType(tc typeClass) bool {
	if r.types == nil {
		return tc != typeBool && tc != typeTemporal
	}
	return slices.Contains(r.types, tc)
}

// match reports whether the rule matches and describes why.
func (r sensitiveRule) match(name string, toks []string) (string, bool) {
	if slices.Contains(r.exact, name) {
		return fmt.Sprintf("column named %q", name), true
	}
	for _, tok := range r.tokens {
		if slices.Contains(toks, tok) {
			return fmt.Sprintf("name token %q", tok), true
		}
	}
	for _, ph := range r.phrases {
		if containsPhrase(toks, ph) {
			return fmt.Sprintf("name tokens %q", strings.Join(ph, "_")), true
		}
	}
	return "", false
}

func containsPhrase(toks, ph []string) bool {
	for i := 0; i+len(ph) <= len(toks); i++ {
		if slices.Equal(toks[i:i+len(ph)], ph) {
			return true
		}
	}
	return false
}

// tokenize splits an identifier on every non-alphanumeric ASCII character
// and on camelCase boundaries (a lower-case letter or digit followed by an
// upper-case one, and the last capital of an acronym followed by a
// lower-case letter: "APIKey" ⇒ api, key), and lowercases the tokens. Digits
// stay attached to the preceding letters ("address2"); a name written in one
// case ("firstname") stays one token.
func tokenize(name string) []string {
	var toks []string
	var cur []byte
	flush := func() {
		if len(cur) > 0 {
			toks = append(toks, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	isUpper := func(b byte) bool { return b >= 'A' && b <= 'Z' }
	isLower := func(b byte) bool { return b >= 'a' && b <= 'z' }
	isDigit := func(b byte) bool { return b >= '0' && b <= '9' }
	for i := 0; i < len(name); i++ {
		b := name[i]
		if !isUpper(b) && !isLower(b) && !isDigit(b) {
			flush()
			continue
		}
		if isUpper(b) && len(cur) > 0 {
			prev := cur[len(cur)-1]
			nextLower := i+1 < len(name) && isLower(name[i+1])
			if isLower(prev) || isDigit(prev) || (isUpper(prev) && nextLower) {
				flush()
			}
		}
		cur = append(cur, b)
	}
	flush()
	return toks
}

// classifyType maps a database type name to a coarse family.
func classifyType(dataType string) typeClass {
	t := strings.ToLower(strings.TrimSpace(dataType))
	if i := strings.IndexAny(t, "(["); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	switch {
	case t == "boolean" || t == "bool":
		return typeBool
	case strings.HasPrefix(t, "timestamp") || strings.HasPrefix(t, "time") ||
		t == "date" || t == "datetime" || t == "interval":
		return typeTemporal
	case t == "numeric" || t == "decimal" || t == "money" || t == "real" || t == "float" ||
		t == "number" || strings.HasPrefix(t, "double") || strings.HasPrefix(t, "float") ||
		strings.HasSuffix(t, "int") || strings.HasPrefix(t, "int") ||
		strings.HasPrefix(t, "smallint") || strings.HasPrefix(t, "bigint"):
		return typeNumeric
	default:
		return typeOther
	}
}
