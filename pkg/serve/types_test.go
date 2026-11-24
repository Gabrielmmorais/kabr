package serve

import "testing"

func TestKindOf(t *testing.T) {
	tests := []struct {
		dataType string
		kind     kind
		elem     kind
	}{
		{"smallint", kindInt, kindOther},
		{"integer", kindInt, kindOther},
		{"bigint", kindInt, kindOther},
		{"real", kindFloat, kindOther},
		{"double precision", kindFloat, kindOther},
		{"numeric", kindNumeric, kindOther},
		{"numeric(5,2)", kindNumeric, kindOther},
		{"boolean", kindBool, kindOther},
		{"text", kindText, kindOther},
		{"character varying(45)", kindText, kindOther},
		{"character(20)", kindText, kindOther},
		{"bpchar", kindText, kindOther},
		{"name", kindText, kindOther},
		{"uuid", kindUUID, kindOther},
		{"date", kindDate, kindOther},
		{"timestamp without time zone", kindTimestamp, kindOther},
		{"timestamp(3) without time zone", kindTimestamp, kindOther},
		{"timestamp with time zone", kindTimestampTZ, kindOther},
		{"timestamp(6) with time zone", kindTimestampTZ, kindOther},
		{"bytea", kindBytes, kindOther},
		{"json", kindJSON, kindOther},
		{"jsonb", kindJSON, kindOther},
		{"text[]", kindArray, kindText},
		{"character varying(20)[]", kindArray, kindText},
		{"integer[]", kindArray, kindInt},
		{"mpaa_rating[]", kindArray, kindOther},
		// Everything outside the whitelist is "other" (text form).
		{"mpaa_rating", kindOther, kindOther},
		{"public.mpaa_rating", kindOther, kindOther},
		{"year", kindOther, kindOther},
		{`"bıgınt"`, kindOther, kindOther},
		{`public."integer"`, kindOther, kindOther},
		{`"integer(5)"`, kindOther, kindOther},
		{`"char"`, kindOther, kindOther},
		{"tsvector", kindOther, kindOther},
		{"interval", kindOther, kindOther},
		{"time without time zone", kindOther, kindOther},
		{"money", kindOther, kindOther},
		{"bit varying(5)", kindOther, kindOther},
		{"", kindOther, kindOther},
	}
	for _, tt := range tests {
		k, elem := kindOf(tt.dataType)
		if k != tt.kind || elem != tt.elem {
			t.Errorf("kindOf(%q) = %s, %s; want %s, %s", tt.dataType, k, elem, tt.kind, tt.elem)
		}
	}
}

func TestKindCapabilities(t *testing.T) {
	for _, k := range []kind{kindBytes, kindJSON, kindArray} {
		if k.filterable() || k.keyable() || k.nativeCompare() {
			t.Errorf("%s must be neither filterable nor keyable", k)
		}
	}
	for _, k := range []kind{kindInt, kindFloat, kindNumeric, kindBool, kindText, kindUUID, kindDate, kindTimestamp, kindTimestampTZ} {
		if !k.nativeCompare() || !k.filterable() || !k.keyable() {
			t.Errorf("%s must compare natively", k)
		}
	}
	if kindOther.nativeCompare() || !kindOther.filterable() || !kindOther.keyable() {
		t.Error("other: compared as text, filterable and keyable")
	}
}
