package serve

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
)

func TestCursorRoundTrip(t *testing.T) {
	for _, vals := range [][]string{
		{"1"}, {"1", "2026-02-01T10:00:00.000001Z", "2"}, {"O'Brien"}, {"x') OR 1=1 --"}, {""}, {"Ünïcødé", "\"q\""},
	} {
		c := encodeCursor(vals)
		if strings.ContainsAny(c, "+/=") {
			t.Errorf("cursor %q is not unpadded base64url", c)
		}
		got, err := decodeCursor(c, len(vals))
		if err != nil || !reflect.DeepEqual(got, vals) {
			t.Errorf("round trip %v: got %v, %v", vals, got, err)
		}
	}
}

func TestDecodeCursorRejects(t *testing.T) {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	bad := map[string]string{
		"not base64":      "!!!",
		"padded":          base64.URLEncoding.EncodeToString([]byte(`{"v":["1"]}`)),
		"std alphabet":    strings.ReplaceAll(strings.ReplaceAll(enc(`{"v":["??>"]}`), "-", "+"), "_", "/"),
		"not JSON":        enc("not json"),
		"not an object":   enc(`["1"]`),
		"wrong arity":     enc(`{"v":["1","2"]}`),
		"no values":       enc(`{}`),
		"null values":     enc(`{"v":null}`),
		"number value":    enc(`{"v":[1]}`),
		"null value":      enc(`{"v":[null]}`),
		"unknown field":   enc(`{"v":["1"],"x":1}`),
		"trailing data":   enc(`{"v":["1"]} {}`),
		"NUL in value":    enc(`{"v":["a\u0000b"]}`),
		"value too long":  enc(`{"v":["` + strings.Repeat("a", maxValueBytes+1) + `"]}`),
		"cursor too long": strings.Repeat("A", maxCursorBytes+4),
		"empty":           "",
	}
	for name, c := range bad {
		if _, err := decodeCursor(c, 1); err != errInvalidCursor { //nolint:errorlint // sentinel returned as is
			t.Errorf("%s: err = %v, want errInvalidCursor", name, err)
		}
	}
}

func TestCheckValue(t *testing.T) {
	for v, ok := range map[string]bool{
		"plain": true, "": true, strings.Repeat("a", maxValueBytes): true,
		strings.Repeat("a", maxValueBytes+1): false, "a\x00b": false, "\xff": false,
	} {
		if got := checkValue(v) == nil; got != ok {
			t.Errorf("checkValue(%.20q) ok = %v, want %v", v, got, ok)
		}
	}
}
