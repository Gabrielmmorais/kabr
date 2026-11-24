package serve

import (
	"regexp"
	"strings"
)

// kind is how the runtime selects, encodes, filters and paginates a column.
// It is derived from the catalog data type through the fixed whitelist in
// [kindOf]; the type name itself never reaches the SQL text.
type kind int

const (
	// kindOther is any type outside the whitelist (enums, domains,
	// tsvector, interval, inet, ...): selected and compared as its text form.
	kindOther kind = iota
	kindInt
	kindFloat
	kindNumeric
	kindBool
	kindText
	kindUUID
	kindDate
	kindTimestamp   // timestamp without time zone
	kindTimestampTZ // timestamp with time zone
	kindBytes
	kindJSON
	kindArray
)

var kindNames = map[kind]string{
	kindOther: "other", kindInt: "int", kindFloat: "float", kindNumeric: "numeric", kindBool: "bool",
	kindText: "text", kindUUID: "uuid", kindDate: "date", kindTimestamp: "timestamp",
	kindTimestampTZ: "timestamptz", kindBytes: "bytes", kindJSON: "json", kindArray: "array",
}

func (k kind) String() string { return kindNames[k] }

// builtinKinds maps the base name printed by PostgreSQL's format_type (type
// modifiers removed) of the built-in types the runtime handles natively.
// Anything else, including schema-qualified or quoted names, is kindOther.
var builtinKinds = map[string]kind{
	"smallint":                    kindInt,
	"integer":                     kindInt,
	"bigint":                      kindInt,
	"real":                        kindFloat,
	"double precision":            kindFloat,
	"numeric":                     kindNumeric,
	"boolean":                     kindBool,
	"text":                        kindText,
	"character varying":           kindText,
	"character":                   kindText,
	"bpchar":                      kindText,
	"name":                        kindText,
	"uuid":                        kindUUID,
	"date":                        kindDate,
	"timestamp without time zone": kindTimestamp,
	"timestamp with time zone":    kindTimestampTZ,
	"bytea":                       kindBytes,
	"json":                        kindJSON,
	"jsonb":                       kindJSON,
}

// typmodRE matches a type modifier such as "(45)" or "(10,2)".
var typmodRE = regexp.MustCompile(`\([0-9, ]*\)`)

// kindOf classifies a catalog data type. For arrays it returns kindArray and
// the kind of the element type.
func kindOf(dataType string) (k, elem kind) {
	base := strings.TrimSpace(dataType)
	if b, ok := strings.CutSuffix(base, "[]"); ok {
		e, _ := kindOf(b)
		return kindArray, e
	}
	base = strings.Join(strings.Fields(typmodRE.ReplaceAllString(base, "")), " ")
	if k, ok := builtinKinds[base]; ok {
		return k, kindOther
	}
	return kindOther, kindOther
}

// textProjected reports whether the column is selected as its text form
// (::pg_catalog.text) and scanned as a string.
func (k kind) textProjected() bool {
	switch k {
	case kindNumeric, kindUUID, kindDate, kindJSON, kindOther:
		return true
	}
	return false
}

// nativeCompare reports whether keys and filters compare the column itself
// with the pg_catalog operators of its built-in type. Other filterable and
// keyable kinds compare their text form.
func (k kind) nativeCompare() bool {
	switch k {
	case kindInt, kindFloat, kindNumeric, kindBool, kindText, kindUUID, kindDate,
		kindTimestamp, kindTimestampTZ:
		return true
	}
	return false
}

// filterable reports whether an equality filter is allowed on the kind.
// bytea, json and arrays are not: their JSON form is not what a client
// could type in a query string.
func (k kind) filterable() bool {
	return k.nativeCompare() || k == kindOther
}

// keyable reports whether a primary key column of this kind supports keyset
// pagination: its value can travel in a cursor as a string that the server
// parses back to the same value.
func (k kind) keyable() bool { return k.filterable() }
