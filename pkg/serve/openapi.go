package serve

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// OpenAPIVersion is the version of the generated document.
const OpenAPIVersion = "3.0.3"

// obj is a JSON object of the OpenAPI document; encoding/json sorts its
// keys, so the document is deterministic.
type obj = map[string]any

// componentInvalidRE matches characters OpenAPI 3.0 forbids in component
// keys.
var componentInvalidRE = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// tablePath is the URL path of the list endpoint of t.
func tablePath(t *table) string {
	return "/v1/" + url.PathEscape(t.schema) + "/" + url.PathEscape(t.name)
}

// componentNames maps table IDs to unique, OpenAPI-valid component keys.
func componentNames(tables []*table) map[string]string {
	names := make(map[string]string, len(tables))
	used := map[string]bool{"Error": true, "ListMeta": true}
	for _, t := range tables {
		base := componentInvalidRE.ReplaceAllString(t.id, "_")
		name := base
		for i := 2; used[name]; i++ {
			name = base + "_" + strconv.Itoa(i)
		}
		used[name] = true
		names[t.id] = name
	}
	return names
}

// buildOpenAPI renders the OpenAPI 3.0.3 document of the served model: one
// list path per table, one get-by-key path per table with a single-column
// primary key, and one component schema per table with only the exposed
// columns (masked and hashed columns as strings, dropped columns absent).
func buildOpenAPI(m *model) ([]byte, error) {
	names := componentNames(m.tables)
	paths := obj{}
	schemas := obj{"Error": obj{
		"type":     "object",
		"required": []string{"error", "message"},
		"properties": obj{
			"error":   obj{"type": "string", "description": "Machine-readable error code."},
			"message": obj{"type": "string", "description": "Human-readable explanation."},
		},
	}}
	for _, t := range m.tables {
		name := names[t.id]
		ref := "#/components/schemas/" + name
		schemas[name] = tableSchema(t)
		paths[tablePath(t)] = obj{"get": listOperation(t, name, ref, m.rowLimit)}
		if len(t.pk) == 1 {
			paths[tablePath(t)+"/{pk}"] = obj{"get": getOperation(t, name, ref)}
		}
	}
	errResp := func(desc string) obj {
		return obj{"description": desc, "content": obj{"application/json": obj{
			"schema": obj{"$ref": "#/components/schemas/Error"}}}}
	}
	doc := obj{
		"openapi": OpenAPIVersion,
		"info": obj{
			"title":   "Kabr domain " + m.cfg.Domain,
			"version": m.cfg.Plan + "@" + m.cfg.CatalogFingerprint,
			"description": "Read-only REST API of the domain " + m.cfg.Domain + " of the approved plan " +
				m.cfg.Plan + ". Lists are paginated with an opaque cursor over the primary key and return at most " +
				strconv.Itoa(m.rowLimit) + " rows. Sensitive columns are masked, hashed or omitted.",
		},
		"paths": paths,
		"components": obj{
			"schemas": schemas,
			"responses": obj{
				"BadRequest": errResp("Invalid filter, cursor, limit or value."),
				"NotFound":   errResp("Unknown table or row."),
				"NotReady":   errResp("Not ready: the catalog changed since the plan was approved, or the database is unavailable."),
				"Timeout":    errResp("The query exceeded the statement timeout."),
				"Internal":   errResp("Internal error."),
			},
		},
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func ref(name string) obj { return obj{"$ref": "#/components/responses/" + name} }

func listOperation(t *table, name, schemaRef string, rowLimit int) obj {
	params := []obj{
		{"name": paramLimit, "in": "query", "required": false,
			"description": "Rows per page (clamped to 1.." + strconv.Itoa(rowLimit) + ").",
			"schema":      obj{"type": "integer", "minimum": 1, "maximum": rowLimit, "default": rowLimit}},
		{"name": paramCursor, "in": "query", "required": false,
			"description": "Opaque cursor: the nextCursor of the previous page.",
			"schema":      obj{"type": "string"}},
	}
	for i := range t.columns {
		c := &t.columns[i]
		if !c.filterable() {
			continue
		}
		params = append(params, obj{
			"name": c.name, "in": "query", "required": false,
			"description": "Equality filter on " + c.name + " (filters are ANDed).",
			"schema":      filterSchema(c),
		})
	}
	return obj{
		"operationId": "list_" + name,
		"summary":     "List rows of " + t.id,
		"description": "Rows ordered by the primary key (" + strings.Join(t.pkNames(), ", ") + ").",
		"parameters":  params,
		"responses": obj{
			"200": obj{"description": "A page of rows.", "content": obj{"application/json": obj{"schema": obj{
				"type":     "object",
				"required": []string{"items", "nextCursor"},
				"properties": obj{
					"items":      obj{"type": "array", "items": obj{"$ref": schemaRef}},
					"nextCursor": obj{"type": "string", "nullable": true, "description": "Cursor of the next page, null on the last page."},
				},
			}}}},
			"400": ref("BadRequest"),
			"500": ref("Internal"),
			"503": ref("NotReady"),
			"504": ref("Timeout"),
		},
	}
}

func getOperation(t *table, name, schemaRef string) obj {
	pk := &t.columns[t.pk[0]]
	return obj{
		"operationId": "get_" + name,
		"summary":     "Get one row of " + t.id + " by " + pk.name,
		"parameters": []obj{{
			"name": "pk", "in": "path", "required": true,
			"description": "Value of the primary key column " + pk.name + ".",
			"schema":      filterSchema(pk),
		}},
		"responses": obj{
			"200": obj{"description": "The row.", "content": obj{"application/json": obj{
				"schema": obj{"$ref": schemaRef}}}},
			"400": ref("BadRequest"),
			"404": ref("NotFound"),
			"500": ref("Internal"),
			"503": ref("NotReady"),
			"504": ref("Timeout"),
		},
	}
}

func (t *table) pkNames() []string {
	out := make([]string, len(t.pk))
	for i, ci := range t.pk {
		out[i] = t.columns[ci].name
	}
	return out
}

func tableSchema(t *table) obj {
	props := obj{}
	required := make([]string, 0, len(t.columns))
	for i := range t.columns {
		c := &t.columns[i]
		props[c.name] = columnSchema(c)
		required = append(required, c.name)
	}
	return obj{
		"type":       "object",
		"required":   required,
		"properties": props,
		"description": "Row of " + t.id + ". Every exposed column is always present; " +
			"columns dropped by the plan are absent.",
	}
}

// columnSchema is the JSON schema of the value of a column in a row.
func columnSchema(c *column) obj {
	desc := "PostgreSQL type " + c.dataType + "."
	var s obj
	switch c.treat {
	case treatMask:
		s = obj{"type": "string"}
		desc = "Masked: \"***\" when the value is set, null otherwise."
	case treatHash:
		s = obj{"type": "string", "pattern": "^[0-9a-f]{64}$"}
		desc = "HMAC-SHA256 (hex) of the text form of the value."
	default:
		s = valueSchema(c.kind, c.elem, c.dataType)
		switch c.kind {
		case kindNumeric:
			desc += " Exact decimal as a string."
		case kindFloat:
			desc += " NaN and infinities are strings."
		case kindTimestamp, kindTimestampTZ:
			desc += " RFC 3339 in UTC, or \"infinity\" / \"-infinity\"."
		case kindBytes:
			desc += " Base64."
		case kindArray:
			desc += " Elements as rendered by PostgreSQL array_to_json."
		case kindOther:
			desc += " PostgreSQL text form."
		}
	}
	s["description"] = desc
	if c.nullable {
		s["nullable"] = true
	}
	return s
}

func valueSchema(k, elem kind, dataType string) obj {
	switch k {
	case kindInt:
		if strings.HasPrefix(strings.TrimSpace(dataType), "bigint") {
			return obj{"type": "integer", "format": "int64"}
		}
		return obj{"type": "integer", "format": "int32"}
	case kindFloat:
		if strings.HasPrefix(strings.TrimSpace(dataType), "real") {
			return obj{"type": "number", "format": "float"}
		}
		return obj{"type": "number", "format": "double"}
	case kindNumeric:
		return obj{"type": "string", "format": "decimal"}
	case kindBool:
		return obj{"type": "boolean"}
	case kindUUID:
		return obj{"type": "string", "format": "uuid"}
	case kindDate:
		return obj{"type": "string", "format": "date"}
	case kindTimestamp, kindTimestampTZ:
		return obj{"type": "string", "format": "date-time"}
	case kindBytes:
		return obj{"type": "string", "format": "byte"}
	case kindJSON:
		return obj{}
	case kindArray:
		return obj{"type": "array", "items": arrayItemSchema(elem)}
	}
	return obj{"type": "string"}
}

// arrayItemSchema follows array_to_json: numbers for numeric types, JSON
// for json, strings for the rest.
func arrayItemSchema(elem kind) obj {
	switch elem {
	case kindInt:
		return obj{"type": "integer"}
	case kindFloat, kindNumeric:
		return obj{"type": "number"}
	case kindBool:
		return obj{"type": "boolean"}
	case kindJSON:
		return obj{}
	}
	return obj{"type": "string"}
}

// filterSchema is the schema of a query (or path) value for the column.
func filterSchema(c *column) obj {
	switch c.kind {
	case kindInt:
		return valueSchema(c.kind, c.elem, c.dataType)
	case kindFloat:
		return obj{"type": "number"}
	case kindBool:
		return obj{"type": "boolean"}
	case kindUUID, kindDate, kindTimestamp, kindTimestampTZ:
		return valueSchema(c.kind, c.elem, c.dataType)
	}
	return obj{"type": "string"}
}
