package serve

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func ptr[T any](v T) *T { return &v }

func TestAppendValue(t *testing.T) {
	ts := time.Date(2026, 3, 4, 5, 6, 7, 123456000, time.FixedZone("x", 3600))
	str := func(s string) any { p := ptr(s); return &p }
	nilStr := func() any { var p *string; return &p }
	tests := []struct {
		name string
		col  column
		dest any
		want string
	}{
		{"text", column{kind: kindText}, str(`a "q"`), `"a \"q\""`},
		{"null text", column{kind: kindText}, nilStr(), `null`},
		{"numeric as string", column{kind: kindNumeric}, str("4.99"), `"4.99"`},
		{"other as text", column{kind: kindOther}, str("'fox':2"), `"'fox':2"`},
		{"json compacted", column{kind: kindJSON}, str("{\"a\": [1, 2.50]}"), `{"a":[1,2.50]}`},
		{"array", column{kind: kindArray}, str(`[1, null, 3]`), `[1,null,3]`},
		{"mask", column{kind: kindInt, treat: treatMask}, str("***"), `"***"`},
		{"mask null", column{kind: kindInt, treat: treatMask}, nilStr(), `null`},
		{"hash", column{kind: kindText, treat: treatHash}, str("4000"), `"` + hmacHex(unitHashKey, "4000") + `"`},
		{"hash null", column{kind: kindText, treat: treatHash}, nilStr(), `null`},
		{"int", column{kind: kindInt}, ptr(ptr(int64(-42))), `-42`},
		{"null int", column{kind: kindInt}, new(*int64), `null`},
		{"float", column{kind: kindFloat}, ptr(ptr(0.5)), `0.5`},
		{"float NaN", column{kind: kindFloat}, ptr(ptr(math.NaN())), `"NaN"`},
		{"float inf", column{kind: kindFloat}, ptr(ptr(math.Inf(-1))), `"-Infinity"`},
		{"null float", column{kind: kindFloat}, new(*float64), `null`},
		{"bool", column{kind: kindBool}, ptr(ptr(true)), `true`},
		{"null bool", column{kind: kindBool}, new(*bool), `null`},
		{"timestamptz", column{kind: kindTimestampTZ}, &pgtype.Timestamptz{Time: ts, Valid: true}, `"2026-03-04T04:06:07.123456Z"`},
		{"timestamptz infinity", column{kind: kindTimestampTZ}, &pgtype.Timestamptz{InfinityModifier: pgtype.Infinity, Valid: true}, `"infinity"`},
		{"timestamp -infinity", column{kind: kindTimestamp}, &pgtype.Timestamp{InfinityModifier: pgtype.NegativeInfinity, Valid: true}, `"-infinity"`},
		{"timestamp", column{kind: kindTimestamp}, &pgtype.Timestamp{Time: ts.UTC(), Valid: true}, `"2026-03-04T04:06:07.123456Z"`},
		{"null timestamp", column{kind: kindTimestamp}, &pgtype.Timestamp{}, `null`},
		{"bytes", column{kind: kindBytes}, ptr([]byte{0, 0xff, 0x10}), `"AP8Q"`},
		{"empty bytes", column{kind: kindBytes}, ptr([]byte{}), `""`},
		{"null bytes", column{kind: kindBytes}, new([]byte), `null`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := appendValue(nil, &tt.col, tt.dest, unitHashKey)
			if err != nil || string(got) != tt.want {
				t.Errorf("got %s, %v; want %s", got, err, tt.want)
			}
		})
	}
	if got, _ := appendValue(nil, &column{kind: kindText}, str("<script>"), nil); strings.ContainsAny(string(got), "<>") {
		t.Errorf("HTML characters not escaped: %s", got)
	}
	if _, err := appendValue(nil, &column{kind: kindJSON}, str("{bad"), nil); err == nil {
		t.Error("invalid JSON from the database accepted")
	}
	if _, err := appendValue(nil, &column{}, ptr(42), nil); err == nil {
		t.Error("unexpected destination accepted")
	}
}

func TestNewScanDestsMatchProjection(t *testing.T) {
	m, _ := mustResolve(t, catalogConfig(), unitHashKey)
	for _, tb := range m.tables {
		dests := newScanDests(tb.columns)
		for i := range tb.columns {
			c := &tb.columns[i]
			_, isString := dests[i].(**string)
			wantString := c.treat != treatPlain || c.kind.textProjected() || c.kind == kindText || c.kind == kindArray
			if isString != wantString {
				t.Errorf("%s.%s: string destination = %v, want %v", tb.id, c.name, isString, wantString)
			}
			if strings.Contains(c.projection(), "::pg_catalog.text") && !isString {
				t.Errorf("%s.%s: text projection scanned into %T", tb.id, c.name, dests[i])
			}
		}
	}
}

func TestKeyString(t *testing.T) {
	ts := time.Date(2026, 2, 1, 10, 0, 0, 1000, time.UTC)
	bc := time.Date(-43, 3, 15, 12, 0, 0, 1000, time.UTC) // 44 BC
	tests := []struct {
		dest any
		want string
	}{
		{ptr(ptr("O'Brien")), "O'Brien"},
		{ptr(ptr(int64(7))), "7"},
		{ptr(ptr(0.1)), "0.1"},
		{ptr(ptr(math.Inf(1))), "Infinity"},
		{ptr(ptr(math.NaN())), "NaN"},
		{ptr(ptr(false)), "false"},
		{&pgtype.Timestamp{Time: ts, Valid: true}, "2026-02-01T10:00:00.000001Z"},
		{&pgtype.Timestamptz{Time: ts, Valid: true}, "2026-02-01T10:00:00.000001Z"},
		{&pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true}, "-infinity"},
		{&pgtype.Timestamptz{Time: bc, Valid: true}, "0044-03-15 12:00:00.000001+00 BC"},
		{&pgtype.Timestamptz{Time: time.Date(12345, 1, 2, 3, 4, 5, 0, time.UTC), Valid: true}, "12345-01-02 03:04:05.000000+00"},
	}
	for _, tt := range tests {
		got, err := keyString(tt.dest)
		if err != nil || got != tt.want {
			t.Errorf("keyString(%T) = %q, %v; want %q", tt.dest, got, err, tt.want)
		}
	}
	for _, null := range []any{new(*string), new(*int64), new(*float64), new(*bool), &pgtype.Timestamp{}, &pgtype.Timestamptz{}} {
		if _, err := keyString(null); err == nil {
			t.Errorf("NULL key %T accepted", null)
		}
	}
	if _, err := keyString(new([]byte)); err == nil {
		t.Error("bytea key accepted")
	}
}
