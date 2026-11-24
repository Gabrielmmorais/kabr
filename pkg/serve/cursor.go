package serve

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	// maxValueBytes bounds one filter value, key value or cursor value.
	maxValueBytes = 4096
	// maxCursorBytes bounds the encoded cursor.
	maxCursorBytes = 16 << 10
)

var errInvalidCursor = errors.New("invalid cursor")

// cursorPayload is the JSON inside a cursor: the primary key values of the
// last returned row, in key order, as strings the server parses back to the
// same values (see keyString). Decoding uses pointers so that a null value
// is rejected instead of becoming "".
type cursorPayload struct {
	V []string `json:"v"`
}

type cursorInput struct {
	V []*string `json:"v"`
}

// cursorEncoding is unpadded base64url; Strict rejects non-canonical input.
var cursorEncoding = base64.RawURLEncoding.Strict()

// encodeCursor returns the opaque cursor for the key values.
func encodeCursor(values []string) string {
	data, _ := json.Marshal(cursorPayload{V: values}) // strings only: cannot fail
	return cursorEncoding.EncodeToString(data)
}

// decodeCursor parses a cursor of a table whose key has arity columns. Any
// defect yields errInvalidCursor: the values are untrusted and are only
// ever used as bind parameters.
func decodeCursor(s string, arity int) ([]string, error) {
	if len(s) > maxCursorBytes {
		return nil, errInvalidCursor
	}
	data, err := cursorEncoding.DecodeString(s)
	if err != nil {
		return nil, errInvalidCursor
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var in cursorInput
	if err := dec.Decode(&in); err != nil {
		return nil, errInvalidCursor
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errInvalidCursor
	}
	if len(in.V) != arity {
		return nil, errInvalidCursor
	}
	values := make([]string, len(in.V))
	for i, v := range in.V {
		if v == nil || checkValue(*v) != nil {
			return nil, errInvalidCursor
		}
		values[i] = *v
	}
	return values, nil
}

// checkValue rejects request values that can never be valid parameters.
func checkValue(v string) error {
	switch {
	case len(v) > maxValueBytes:
		return errors.New("value is too long")
	case !utf8.ValidString(v):
		return errors.New("value is not valid UTF-8")
	case strings.IndexByte(v, 0) >= 0:
		return errors.New("value contains a NUL byte")
	}
	return nil
}
