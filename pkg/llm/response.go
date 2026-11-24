package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Gabrielmmorais/kabr/pkg/plan"
)

// ResponseSchema returns the JSON Schema of the model's answer. It is a new
// map on every call. It only uses keywords supported by both Anthropic
// structured outputs and Ollama's "format": every object is closed
// (additionalProperties false) with all properties required, and class and
// action are enums. Domain name syntax is checked later by plan.Apply.
func ResponseSchema() map[string]any {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	object := func(props map[string]any, order ...string) map[string]any {
		return map[string]any{
			"type":                 "object",
			"properties":           props,
			"required":             order,
			"additionalProperties": false,
		}
	}
	array := func(items map[string]any) map[string]any {
		return map[string]any{"type": "array", "items": items}
	}
	return object(map[string]any{
		"renames": array(object(map[string]any{
			"from":        str("Baseline domain name."),
			"to":          str("New DNS-1123 label (lowercase letters, digits, '-')."),
			"description": str("One-sentence business description of the domain."),
			"rationale":   str("Why, based on the metadata."),
		}, "from", "to", "description", "rationale")),
		"moves": array(object(map[string]any{
			"table":     str("Table ID 'schema.table' from the input."),
			"to":        str("Target domain name after renames."),
			"rationale": str("Why, based on the metadata."),
		}, "table", "to", "rationale")),
		"sensitiveColumns": array(object(map[string]any{
			"table":  str("Table ID 'schema.table' from the input."),
			"column": str("Column name from the input."),
			"class": map[string]any{
				"type": "string",
				"enum": []any{string(plan.ClassPII), string(plan.ClassCredential), string(plan.ClassFinancial), string(plan.ClassOther)},
			},
			"action": map[string]any{
				"type": "string",
				"enum": []any{string(plan.ActionMask), string(plan.ActionDrop), string(plan.ActionHash)},
			},
			"rationale": str("Why, based on the metadata."),
		}, "table", "column", "class", "action", "rationale")),
	}, "renames", "moves", "sensitiveColumns")
}

// wire types mirror ResponseSchema; they deliberately have no "source".
type wireResponse struct {
	Renames          []wireRename    `json:"renames"`
	Moves            []wireMove      `json:"moves"`
	SensitiveColumns []wireSensitive `json:"sensitiveColumns"`
}

type wireRename struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Description string `json:"description"`
	Rationale   string `json:"rationale"`
}

type wireMove struct {
	Table     string `json:"table"`
	To        string `json:"to"`
	Rationale string `json:"rationale"`
}

type wireSensitive struct {
	Table     string `json:"table"`
	Column    string `json:"column"`
	Class     string `json:"class"`
	Action    string `json:"action"`
	Rationale string `json:"rationale"`
}

// ParseResponse decodes a model answer strictly: exactly one JSON object
// (not null), no unknown fields, keys matched case-sensitively, class and action within their enums. Strings are
// trimmed and every sensitive column gets Source = plan.SourceLLM. Table,
// column and domain names are NOT checked against the catalog: plan.Apply
// does that and skips invalid items with a warning.
func ParseResponse(text []byte) (plan.Refinement, error) {
	text = bytes.TrimSpace(text)
	dec := json.NewDecoder(bytes.NewReader(text))
	dec.DisallowUnknownFields()
	var w wireResponse
	if err := dec.Decode(&w); err != nil {
		return plan.Refinement{}, fmt.Errorf("llm: decode response: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return plan.Refinement{}, errors.New("llm: decode response: unexpected data after the JSON object")
	}
	if err := checkExactKeys(text); err != nil {
		return plan.Refinement{}, fmt.Errorf("llm: decode response: %w", err)
	}

	var r plan.Refinement
	for _, rn := range w.Renames {
		r.Renames = append(r.Renames, plan.DomainRename{
			From:        strings.TrimSpace(rn.From),
			To:          strings.TrimSpace(rn.To),
			Description: strings.TrimSpace(rn.Description),
			Rationale:   strings.TrimSpace(rn.Rationale),
		})
	}
	for _, mv := range w.Moves {
		r.Moves = append(r.Moves, plan.TableMove{
			Table:     strings.TrimSpace(mv.Table),
			To:        strings.TrimSpace(mv.To),
			Rationale: strings.TrimSpace(mv.Rationale),
		})
	}
	for i, sc := range w.SensitiveColumns {
		class := plan.SensitiveClass(strings.TrimSpace(sc.Class))
		switch class {
		case plan.ClassPII, plan.ClassCredential, plan.ClassFinancial, plan.ClassOther:
		default:
			return plan.Refinement{}, fmt.Errorf("llm: sensitiveColumns[%d]: invalid class %q (want pii, credential, financial or other)", i, sc.Class)
		}
		action := plan.Action(strings.TrimSpace(sc.Action))
		switch action {
		case plan.ActionMask, plan.ActionDrop, plan.ActionHash:
		default:
			return plan.Refinement{}, fmt.Errorf("llm: sensitiveColumns[%d]: invalid action %q (want mask, drop or hash)", i, sc.Action)
		}
		r.SensitiveColumns = append(r.SensitiveColumns, plan.SensitiveColumn{
			Table:     strings.TrimSpace(sc.Table),
			Column:    strings.TrimSpace(sc.Column),
			Class:     class,
			Action:    action,
			Source:    plan.SourceLLM,
			Rationale: strings.TrimSpace(sc.Rationale),
		})
	}
	return r, nil
}

// Exact key sets of the answer. encoding/json matches keys
// case-insensitively, so "Renames" would otherwise be accepted.
var (
	topKeys       = map[string]bool{"renames": true, "moves": true, "sensitiveColumns": true}
	renameKeys    = map[string]bool{"from": true, "to": true, "description": true, "rationale": true}
	moveKeys      = map[string]bool{"table": true, "to": true, "rationale": true}
	sensitiveKeys = map[string]bool{"table": true, "column": true, "class": true, "action": true, "rationale": true}
	itemKeys      = map[string]map[string]bool{"renames": renameKeys, "moves": moveKeys, "sensitiveColumns": sensitiveKeys}
)

// checkExactKeys re-reads the answer generically and verifies that it is an
// object whose keys (and its items' keys) match the schema exactly.
func checkExactKeys(text []byte) error {
	var doc any
	if err := json.Unmarshal(text, &doc); err != nil {
		return err
	}
	top, ok := doc.(map[string]any)
	if !ok {
		return errors.New("answer must be a JSON object")
	}
	for k, v := range top {
		if !topKeys[k] {
			return fmt.Errorf("unknown field %q (keys are case-sensitive)", k)
		}
		items, _ := v.([]any)
		for i, item := range items {
			obj, ok := item.(map[string]any)
			if !ok {
				continue // type errors are reported by the typed decoder
			}
			for ik := range obj {
				if !itemKeys[k][ik] {
					return fmt.Errorf("%s[%d]: unknown field %q (keys are case-sensitive)", k, i, ik)
				}
			}
		}
	}
	return nil
}
