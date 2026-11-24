package serve

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// newScanDests returns one scan destination per column, matching the
// projection of the column (see column.projection).
func newScanDests(cols []column) []any {
	dests := make([]any, len(cols))
	for i := range cols {
		c := &cols[i]
		switch {
		case c.treat != treatPlain, c.kind.textProjected(), c.kind == kindText, c.kind == kindArray:
			dests[i] = new(*string)
		case c.kind == kindInt:
			dests[i] = new(*int64)
		case c.kind == kindFloat:
			dests[i] = new(*float64)
		case c.kind == kindBool:
			dests[i] = new(*bool)
		case c.kind == kindTimestamp:
			dests[i] = new(pgtype.Timestamp)
		case c.kind == kindTimestampTZ:
			dests[i] = new(pgtype.Timestamptz)
		case c.kind == kindBytes:
			dests[i] = new([]byte)
		default:
			dests[i] = new(*string)
		}
	}
	return dests
}

var errUnexpectedDest = errors.New("unexpected scan destination")

// appendRow appends the JSON object of one scanned row, with the properties
// in column order.
func appendRow(buf []byte, cols []column, dests []any, hashKey []byte) ([]byte, error) {
	buf = append(buf, '{')
	for i := range cols {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = appendString(buf, cols[i].name)
		buf = append(buf, ':')
		var err error
		if buf, err = appendValue(buf, &cols[i], dests[i], hashKey); err != nil {
			return nil, fmt.Errorf("column %s: %w", cols[i].name, err)
		}
	}
	return append(buf, '}'), nil
}

func appendString(buf []byte, s string) []byte {
	b, _ := json.Marshal(s) // a string always marshals
	return append(buf, b...)
}

// appendValue appends the JSON encoding of one scanned value.
func appendValue(buf []byte, c *column, dest any, hashKey []byte) ([]byte, error) {
	switch d := dest.(type) {
	case **string:
		if *d == nil {
			return append(buf, "null"...), nil
		}
		s := **d
		switch {
		case c.treat == treatHash:
			return appendString(buf, hmacHex(hashKey, s)), nil
		case c.treat == treatMask:
			return appendString(buf, s), nil
		case c.kind == kindJSON, c.kind == kindArray:
			var out bytes.Buffer
			if err := json.Compact(&out, []byte(s)); err != nil {
				return nil, errors.New("invalid JSON from the database")
			}
			return append(buf, out.Bytes()...), nil
		default:
			return appendString(buf, s), nil
		}
	case **int64:
		if *d == nil {
			return append(buf, "null"...), nil
		}
		return strconv.AppendInt(buf, **d, 10), nil
	case **float64:
		if *d == nil {
			return append(buf, "null"...), nil
		}
		f := **d
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return appendString(buf, floatKey(f)), nil
		}
		b, err := json.Marshal(f)
		if err != nil {
			return nil, err
		}
		return append(buf, b...), nil
	case **bool:
		if *d == nil {
			return append(buf, "null"...), nil
		}
		return strconv.AppendBool(buf, **d), nil
	case *pgtype.Timestamp:
		if !d.Valid {
			return append(buf, "null"...), nil
		}
		return appendString(buf, timestampJSON(d.Time, d.InfinityModifier)), nil
	case *pgtype.Timestamptz:
		if !d.Valid {
			return append(buf, "null"...), nil
		}
		return appendString(buf, timestampJSON(d.Time, d.InfinityModifier)), nil
	case *[]byte:
		if *d == nil {
			return append(buf, "null"...), nil
		}
		return appendString(buf, base64.StdEncoding.EncodeToString(*d)), nil
	}
	return nil, errUnexpectedDest
}

// hmacHex is the hash action: lowercase hex HMAC-SHA256 of the text form.
func hmacHex(key []byte, s string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

// timestampJSON renders a timestamp as RFC 3339 in UTC, keeping infinities.
func timestampJSON(t time.Time, inf pgtype.InfinityModifier) string {
	switch inf {
	case pgtype.Infinity:
		return "infinity"
	case pgtype.NegativeInfinity:
		return "-infinity"
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// timestampKey renders a timestamp so that PostgreSQL parses it back to the
// same value: RFC 3339 with microseconds for years 1..9999, the ISO form
// with an explicit era otherwise. For timestamp without time zone the "Z"
// is ignored by the server, as the value was read in UTC.
func timestampKey(t time.Time, inf pgtype.InfinityModifier) string {
	switch inf {
	case pgtype.Infinity:
		return "infinity"
	case pgtype.NegativeInfinity:
		return "-infinity"
	}
	t = t.UTC()
	if y := t.Year(); y >= 1 && y <= 9999 {
		return t.Format(time.RFC3339Nano)
	}
	year, era := t.Year(), ""
	if year <= 0 { // proleptic Gregorian: year 0 is 1 BC
		year, era = 1-year, " BC"
	}
	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d.%06d+00%s",
		year, int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond()/1000, era)
}

// floatKey renders a float so that PostgreSQL parses it back exactly.
func floatKey(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

var errNullKey = errors.New("primary key value is NULL")

// keyString returns the cursor form of a scanned primary key value: a
// string the server parses back, for the column's compare expression, to
// the same value.
func keyString(dest any) (string, error) {
	switch d := dest.(type) {
	case **string:
		if *d == nil {
			return "", errNullKey
		}
		return **d, nil
	case **int64:
		if *d == nil {
			return "", errNullKey
		}
		return strconv.FormatInt(**d, 10), nil
	case **float64:
		if *d == nil {
			return "", errNullKey
		}
		return floatKey(**d), nil
	case **bool:
		if *d == nil {
			return "", errNullKey
		}
		return strconv.FormatBool(**d), nil
	case *pgtype.Timestamp:
		if !d.Valid {
			return "", errNullKey
		}
		return timestampKey(d.Time, d.InfinityModifier), nil
	case *pgtype.Timestamptz:
		if !d.Valid {
			return "", errNullKey
		}
		return timestampKey(d.Time, d.InfinityModifier), nil
	}
	return "", errUnexpectedDest
}
