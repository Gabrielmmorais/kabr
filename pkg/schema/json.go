package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Marshal returns the canonical JSON encoding of the catalog: a normalized
// deep copy (see [Catalog.Normalize]) indented with two spaces and terminated
// by a newline. The input catalog is not modified. Marshal does not validate;
// call [Catalog.Validate] first if needed.
//
// Two catalogs with the same content always marshal to identical bytes. Leave
// IntrospectedAt nil for output that is stable across runs.
func Marshal(c *Catalog) ([]byte, error) {
	if c == nil {
		return nil, errors.New("schema: cannot marshal nil catalog")
	}
	n := c.Clone()
	n.Normalize()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(n); err != nil {
		return nil, fmt.Errorf("schema: marshal catalog: %w", err)
	}
	return buf.Bytes(), nil
}

// Unmarshal decodes a JSON catalog, rejecting unknown fields, then
// normalizes and validates it. On a validation error the decoded catalog is
// still returned together with the error so callers can inspect it.
func Unmarshal(data []byte) (*Catalog, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c Catalog
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("schema: unmarshal catalog: %w", err)
	}
	if dec.More() {
		return nil, errors.New("schema: unmarshal catalog: trailing data after JSON document")
	}
	c.Normalize()
	if err := c.Validate(); err != nil {
		return &c, fmt.Errorf("schema: invalid catalog: %w", err)
	}
	return &c, nil
}
