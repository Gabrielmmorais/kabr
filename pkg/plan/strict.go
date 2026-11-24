package plan

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// checkSingleDocument rejects input that holds more than one YAML document.
// A leading "---" (after blank lines, comments or directives) is allowed; a
// "---" separator after content, or any content after a "..." end marker,
// is not. Document markers are only recognized at column 0, where YAML
// defines them (they also terminate block scalars there).
func checkSingleDocument(data []byte) error {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), len(data)+1)
	seenContent, seenMarker, ended := false, false, false
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimRight(sc.Text(), "\r")
		switch {
		case isMarker(text, "---"):
			if seenContent || seenMarker || ended {
				return fmt.Errorf("line %d: multiple YAML documents are not allowed", line)
			}
			seenMarker = true
			// "--- value" starts the single document inline.
			if rest := strings.TrimSpace(text[3:]); rest != "" && !strings.HasPrefix(rest, "#") {
				seenContent = true
			}
		case isMarker(text, "..."):
			ended = true
		default:
			trimmed := strings.TrimSpace(text)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") || (!seenContent && strings.HasPrefix(text, "%")) {
				continue
			}
			if ended {
				return fmt.Errorf("line %d: content after end of YAML document (\"...\") is not allowed", line)
			}
			seenContent = true
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("scan input: %w", err)
	}
	return nil
}

func isMarker(line, marker string) bool {
	if !strings.HasPrefix(line, marker) {
		return false
	}
	rest := line[len(marker):]
	return rest == "" || rest[0] == ' ' || rest[0] == '\t'
}

var timeType = reflect.TypeFor[time.Time]()

// checkExactKeys re-reads the document generically and verifies that every
// object key matches a JSON field name of the corresponding Go struct
// exactly. encoding/json matches keys case-insensitively, so without this
// check "KIND: DomainPlan" or a shadow "Kind:" next to "kind:" would pass.
func checkExactKeys(data []byte) error {
	j, err := yaml.YAMLToJSON(data)
	if err != nil {
		return fmt.Errorf("convert YAML to JSON: %w", err)
	}
	var doc any
	if err := json.Unmarshal(j, &doc); err != nil {
		return fmt.Errorf("decode generic document: %w", err)
	}
	var errs []error
	walkKeys("", doc, reflect.TypeFor[DomainPlan](), &errs)
	return errors.Join(errs...)
}

func walkKeys(path string, v any, t reflect.Type, errs *[]error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == timeType {
		return
	}
	switch t.Kind() {
	case reflect.Struct:
		obj, ok := v.(map[string]any)
		if !ok {
			return // type errors are reported by the typed decoder
		}
		fields := map[string]reflect.Type{}
		for f := range t.Fields() {
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name != "" && name != "-" {
				fields[name] = f.Type
			}
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			child := k
			if path != "" {
				child = path + "." + k
			}
			ft, ok := fields[k]
			if !ok {
				*errs = append(*errs, fmt.Errorf("%s: unknown field %q (keys are case-sensitive)", child, k))
				continue
			}
			walkKeys(child, obj[k], ft, errs)
		}
	case reflect.Slice:
		arr, ok := v.([]any)
		if !ok {
			return
		}
		for i, e := range arr {
			walkKeys(fmt.Sprintf("%s[%d]", path, i), e, t.Elem(), errs)
		}
	default:
		// Maps (annotations) have free-form keys; scalars have no keys.
	}
}
