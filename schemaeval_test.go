package validator

import (
	"encoding/json"
	"fmt"
)

// validateSchema evaluates a JSON Schema (draft 2020-12) document against a
// generic, encoding/json-decoded value. It implements precisely the subset of
// the dialect that the exporter emits:
//
//	type, const, enum, minimum, maximum, minLength, maxLength,
//	minItems, maxItems, uniqueItems, items, properties, required,
//	additionalProperties, propertyNames, anyOf, allOf, not, $ref/$defs.
//
// It exists so the exported schema can be checked without pulling in an
// external JSON Schema library (the module intentionally has none).
func validateSchema(doc map[string]interface{}, value interface{}) []string {
	ev := &schemaEval{doc: doc}
	return ev.eval(doc, value, "")
}

type schemaEval struct {
	doc map[string]interface{}
}

func (ev *schemaEval) eval(node map[string]interface{}, value interface{}, path string) []string {
	var errs []string

	if ref, ok := node["$ref"].(string); ok {
		target, ok := ev.resolve(ref)
		if !ok {
			return []string{path + ": unresolved $ref " + ref}
		}
		return nonNil(ev.eval(target, value, path))
	}

	if !checkType(node, value) {
		errs = append(errs, fmt.Sprintf("%s: type mismatch, got %s", path, jsonTypeName(value)))
		// Type mismatches make further assertions meaningless.
		return nonNil(errs)
	}

	if c, ok := node["const"]; ok && !deepEqualJSON(c, value) {
		errs = append(errs, fmt.Sprintf("%s: const mismatch", path))
	}

	if en, ok := node["enum"].([]interface{}); ok {
		matched := false
		for _, candidate := range en {
			if deepEqualJSON(candidate, value) {
				matched = true
				break
			}
		}
		if !matched {
			errs = append(errs, fmt.Sprintf("%s: value not in enum", path))
		}
	}

	if not, ok := node["not"].(map[string]interface{}); ok {
		if len(ev.eval(not, value, path+"/not")) == 0 {
			errs = append(errs, fmt.Sprintf("%s: value matched schema under not", path))
		}
	}

	if anyOf, ok := node["anyOf"].([]interface{}); ok {
		ok := false
		for _, branch := range anyOf {
			if bm, isMap := branch.(map[string]interface{}); isMap {
				if len(ev.eval(bm, value, path+"/anyOf")) == 0 {
					ok = true
					break
				}
			}
		}
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: no anyOf branch matched", path))
		}
	}

	if allOf, ok := node["allOf"].([]interface{}); ok {
		for _, branch := range allOf {
			if bm, isMap := branch.(map[string]interface{}); isMap {
				errs = append(errs, ev.eval(bm, value, path+"/allOf")...)
			}
		}
	}

	switch x := value.(type) {
	case float64:
		if min, ok := num(node["minimum"]); ok && x < min {
			errs = append(errs, fmt.Sprintf("%s: %v < minimum %v", path, x, min))
		}
		if max, ok := num(node["maximum"]); ok && x > max {
			errs = append(errs, fmt.Sprintf("%s: %v > maximum %v", path, x, max))
		}
	case string:
		if min, ok := intKeyword(node["minLength"]); ok && int64(len([]rune(x))) < min {
			errs = append(errs, fmt.Sprintf("%s: string shorter than minLength %d", path, min))
		}
		if max, ok := intKeyword(node["maxLength"]); ok && int64(len([]rune(x))) > max {
			errs = append(errs, fmt.Sprintf("%s: string longer than maxLength %d", path, max))
		}
	case []interface{}:
		if min, ok := intKeyword(node["minItems"]); ok && int64(len(x)) < min {
			errs = append(errs, fmt.Sprintf("%s: fewer than minItems %d", path, min))
		}
		if max, ok := intKeyword(node["maxItems"]); ok && int64(len(x)) > max {
			errs = append(errs, fmt.Sprintf("%s: more than maxItems %d", path, max))
		}
		if b, ok := node["uniqueItems"].(bool); ok && b && !uniqueJSON(x) {
			errs = append(errs, fmt.Sprintf("%s: items not unique", path))
		}
		if items, ok := node["items"].(map[string]interface{}); ok {
			for i, item := range x {
				errs = append(errs, ev.eval(items, item, fmt.Sprintf("%s/%d", path, i))...)
			}
		}
	case map[string]interface{}:
		if min, ok := intKeyword(node["minProperties"]); ok && int64(len(x)) < min {
			errs = append(errs, fmt.Sprintf("%s: fewer than minProperties %d", path, min))
		}
		if max, ok := intKeyword(node["maxProperties"]); ok && int64(len(x)) > max {
			errs = append(errs, fmt.Sprintf("%s: more than maxProperties %d", path, max))
		}
		props, _ := node["properties"].(map[string]interface{})
		if req, ok := node["required"].([]interface{}); ok {
			for _, r := range req {
				name, _ := r.(string)
				if _, present := x[name]; !present {
					errs = append(errs, fmt.Sprintf("%s/%s: missing required property", path, name))
				}
			}
		}
		for k, v := range x {
			if pschema, ok := props[k].(map[string]interface{}); ok {
				errs = append(errs, ev.eval(pschema, v, path+"/"+escapeTest(k))...)
			} else if add, hasAdd := node["additionalProperties"].(map[string]interface{}); hasAdd {
				errs = append(errs, ev.eval(add, v, path+"/"+escapeTest(k))...)
			}
		}
		if pn, ok := node["propertyNames"].(map[string]interface{}); ok {
			for k := range x {
				errs = append(errs, ev.eval(pn, k, path+"/propertyNames")...)
			}
		}
	}

	return nonNil(errs)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (ev *schemaEval) resolve(ref string) (map[string]interface{}, bool) {
	const prefix = "#/$defs/"
	if len(ref) <= len(prefix) || ref[:len(prefix)] != prefix {
		return nil, false
	}
	name := ref[len(prefix):]
	defs, ok := ev.doc["$defs"].(map[string]interface{})
	if !ok {
		return nil, false
	}
	target, ok := defs[name].(map[string]interface{})
	return target, ok
}

func checkType(node map[string]interface{}, value interface{}) bool {
	t, hasType := node["type"]
	if !hasType {
		return true
	}
	actual := jsonTypeName(value)
	switch tv := t.(type) {
	case string:
		if tv == "number" && actual == "integer" {
			return true
		}
		return tv == actual
	case []interface{}:
		for _, candidate := range tv {
			if cs, ok := candidate.(string); ok {
				if cs == actual || (cs == "number" && actual == "integer") {
					return true
				}
			}
		}
	}
	return false
}

func jsonTypeName(value interface{}) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64:
		if isIntegral(value.(float64)) {
			return "integer"
		}
		return "number"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	default:
		return "unknown"
	}
}

func isIntegral(f float64) bool {
	return f == float64(int64(f))
}

func num(v interface{}) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

// intKeyword accepts numeric keywords whether they arrive as int64 (in-memory
// schemaNode maps) or float64 (JSON-round-tripped documents).
func intKeyword(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	}
	return 0, false
}

func uniqueJSON(xs []interface{}) bool {
	for i := 0; i < len(xs); i++ {
		for j := i + 1; j < len(xs); j++ {
			if deepEqualJSON(xs[i], xs[j]) {
				return false
			}
		}
	}
	return true
}

func deepEqualJSON(a, b interface{}) bool {
	// Schema-side constants can be native ints/bools/strings while the
	// document side is always JSON-decoded (numbers as float64), so compare
	// numerically rather than relying on reflect.DeepEqual.
	switch av := a.(type) {
	case nil:
		return b == nil
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case int:
		return numericEqual(float64(av), b)
	case int64:
		return numericEqual(float64(av), b)
	case float64:
		return numericEqual(av, b)
	case []interface{}:
		bv, ok := b.([]interface{})
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !deepEqualJSON(av[i], bv[i]) {
				return false
			}
		}
		return true
	case map[string]interface{}:
		bv, ok := b.(map[string]interface{})
		if !ok || len(av) != len(bv) {
			return false
		}
		for k := range av {
			if !deepEqualJSON(av[k], bv[k]) {
				return false
			}
		}
		return true
	}
	return false
}

func numericEqual(a float64, b interface{}) bool {
	switch bv := b.(type) {
	case int:
		return a == float64(bv)
	case int64:
		return a == float64(bv)
	case float64:
		return a == bv
	}
	return false
}

func escapeTest(token string) string {
	out := make([]byte, 0, len(token))
	for i := 0; i < len(token); i++ {
		switch token[i] {
		case '~':
			out = append(out, '~', '0')
		case '/':
			out = append(out, '~', '1')
		default:
			out = append(out, token[i])
		}
	}
	return string(out)
}

// ensure encoding/json import is retained.
var _ = json.Marshal
