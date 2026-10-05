package validator

import (
	"encoding/json"
	"strings"
	"testing"

	. "github.com/go-playground/assert/v2"
)

// schemaDemo covers the exported subset (json names, pointers, omitempty,
// string lengths, numeric ranges, oneof, nested structs, slices, maps) as well
// as tags that intentionally remain server-side (email, gtfield) so the
// diagnostics path is exercised end to end.
type schemaAddress struct {
	City   string `json:"city" validate:"required,min=2,max=40"`
	Street string `json:"street,omitempty" validate:"omitempty,min=3"`
}

type schemaDemo struct {
	Name      string         `json:"name" validate:"required,min=1,max=10"`
	Nick      string         `json:"nick,omitempty" validate:"omitempty,max=8"`
	Age       int            `json:"age" validate:"gte=0,lte=130"`
	Score     float64        `json:"score" validate:"gt=0,lt=100"`
	Role      string         `json:"role" validate:"oneof=admin member guest"`
	Email     string         `json:"email" validate:"required,email"`
	Manager   *string        `json:"manager" validate:"omitempty,min=2"`
	Tags      []string       `json:"tags" validate:"min=1,dive,min=1,max=6"`
	Metadata  map[string]int `json:"metadata,omitempty" validate:"omitempty,dive,keys,min=1,endkeys,gte=0"`
	Address   schemaAddress  `json:"address" validate:"required"`
	Confirmed bool           `json:"confirmed" validate:"eq=true"`
}

func TestExportJSONSchemaBasicShape(t *testing.T) {
	v := New()
	exp, err := v.ExportJSONSchema(schemaDemo{})
	Equal(t, err, nil)

	root := exp.Schema
	Equal(t, root.Schema, jsonSchemaDialect)

	raw, err := json.MarshalIndent(root, "", "  ")
	Equal(t, err, nil)

	doc := string(raw)
	// JSON names, not Go field names, are emitted.
	Equal(t, strings.Contains(doc, `"name"`), true)
	Equal(t, strings.Contains(doc, `"city"`), true)
	Equal(t, strings.Contains(doc, `"Nick"`), false)
	// numeric range keywords
	Equal(t, strings.Contains(doc, `"minimum"`), true)
	Equal(t, strings.Contains(doc, `"maximum"`), true)
	Equal(t, strings.Contains(doc, `"exclusiveMinimum"`), true)
	Equal(t, strings.Contains(doc, `"exclusiveMaximum"`), true)
	// string length keywords
	Equal(t, strings.Contains(doc, `"minLength"`), true)
	Equal(t, strings.Contains(doc, `"maxLength"`), true)
	// oneof -> enum
	Equal(t, strings.Contains(doc, `"enum"`), true)

	// Diagnostics must mention the intentionally untranslatable tags.
	tags := diagnosticTags(exp.Diagnostics)
	Equal(t, sliceContains(tags, "email"), true)
	Equal(t, sliceContains(tags, "keys"), false) // keys tag is translated to propertyNames
}

func diagnosticTags(diags []SchemaDiagnostic) []string {
	out := make([]string, 0, len(diags))
	for _, d := range diags {
		out = append(out, d.Tag)
	}
	return out
}

func sliceContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// ---- Minimal draft 2020-12 evaluator (just the keywords this exporter uses) ----

type schemaDoc struct {
	root *Schema
	defs map[string]*Schema
}

func loadDoc(t *testing.T, s *Schema) *schemaDoc {
	t.Helper()
	return &schemaDoc{root: s, defs: s.Defs}
}

func (d *schemaDoc) resolve(s *Schema) *Schema {
	if s == nil || s.Ref == "" {
		return s
	}
	const prefix = "#/$defs/"
	if strings.HasPrefix(s.Ref, prefix) {
		if target, ok := d.defs[strings.TrimPrefix(s.Ref, prefix)]; ok {
			return target
		}
	}
	return s
}

func (d *schemaDoc) validate(s *Schema, v any) bool {
	s = d.resolve(s)

	if s.AnyOf != nil {
		ok := false
		for _, sub := range s.AnyOf {
			if d.validate(sub, v) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}

	if !matchTypes(s.Type, v) {
		return false
	}

	if s.Enum != nil {
		found := false
		for _, candidate := range s.Enum {
			if jsonEqual(candidate, v) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	if s.Not != nil && d.validate(s.Not, v) {
		return false
	}

	switch x := v.(type) {
	case string:
		if s.MinLength != nil && len([]rune(x)) < *s.MinLength {
			return false
		}
		if s.MaxLength != nil && len([]rune(x)) > *s.MaxLength {
			return false
		}

	case float64:
		// JSON numbers arrive as float64; encoded ints come from json.Number
		// conversion in parity tests below.
		if s.Minimum != nil && !checkNumber(x, *s.Minimum, false, false) {
			return false
		}
		if s.Maximum != nil && !checkNumber(x, *s.Maximum, true, false) {
			return false
		}
		if s.ExclusiveMinimum != nil && !checkNumber(x, *s.ExclusiveMinimum, false, true) {
			return false
		}
		if s.ExclusiveMaximum != nil && !checkNumber(x, *s.ExclusiveMaximum, true, true) {
			return false
		}

	case []any:
		if s.MinItems != nil && len(x) < *s.MinItems {
			return false
		}
		if s.MaxItems != nil && len(x) > *s.MaxItems {
			return false
		}
		if s.UniqueItems && !uniqueScalars(x) {
			return false
		}
		if s.Items != nil {
			for _, el := range x {
				if !d.validate(s.Items, el) {
					return false
				}
			}
		}

	case map[string]any:
		if s.MinProperties != nil && len(x) < *s.MinProperties {
			return false
		}
		if s.MaxProperties != nil && len(x) > *s.MaxProperties {
			return false
		}

		if s.PropertyNames != nil {
			for k := range x {
				if !d.validate(s.PropertyNames, k) {
					return false
				}
			}
		}

		if len(s.Required) > 0 {
			for _, name := range s.Required {
				if _, ok := x[name]; !ok {
					return false
				}
			}
		}

		if s.Items == nil && len(s.Properties) > 0 {
			for k, val := range x {
				if prop, ok := s.Properties[k]; ok {
					if !d.validate(prop, val) {
						return false
					}
				}
			}
		}

		if ap, ok := s.AdditionalProperties.(*Schema); ok && ap != nil {
			for _, val := range x {
				if !d.validate(ap, val) {
					return false
				}
			}
		}
	}

	return true
}

func matchTypes(types []string, v any) bool {
	if len(types) == 0 {
		return true
	}
	for _, want := range types {
		switch want {
		case "string":
			if _, ok := v.(string); ok {
				return true
			}
		case "integer":
			if f, ok := v.(float64); ok && f == float64(int64(f)) {
				return true
			}
		case "number":
			if _, ok := v.(float64); ok {
				return true
			}
		case "boolean":
			if _, ok := v.(bool); ok {
				return true
			}
		case "array":
			if _, ok := v.([]any); ok {
				return true
			}
		case "object":
			if _, ok := v.(map[string]any); ok {
				return true
			}
		case "null":
			if v == nil {
				return true
			}
		}
	}
	return false
}

func checkNumber(x float64, bound json.Number, upper, exclusive bool) bool {
	b, err := bound.Float64()
	if err != nil {
		return false
	}
	if upper {
		if exclusive {
			return x < b
		}
		return x <= b
	}
	if exclusive {
		return x > b
	}
	return x >= b
}

func jsonEqual(a, b any) bool {
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

func uniqueScalars(items []any) bool {
	seen := map[string]struct{}{}
	for _, it := range items {
		switch it.(type) {
		case string, float64, bool, nil:
		default:
			return true // evaluator limit; only scalars are compared
		}
		b, _ := json.Marshal(it)
		key := string(b)
		if _, ok := seen[key]; ok {
			return false
		}
		seen[key] = struct{}{}
	}
	return true
}

// These cases document where the two validators intentionally diverge. They
// are kept separate from TestJSONSchemaParity so the "agree" set and the
// "known gaps" set are both explicit.
type gapCase struct {
	name     string
	json     string
	schemaOK bool
	goOK     bool
}

func TestJSONSchemaKnownGaps(t *testing.T) {
	v := New()
	exp, err := v.ExportJSONSchema(schemaDemo{})
	Equal(t, err, nil)
	doc := loadDoc(t, exp.Schema)

	cases := []gapCase{
		{
			// omitempty: Go skips min=3 when street is "" but present; the
			// schema still applies minLength and rejects the empty string.
			name: "omitempty min with present zero value",
			json: `{
				"name": "bob",
				"age": 1,
				"score": 1,
				"role": "member",
				"email": "b@example.com",
				"tags": ["x"],
				"address": {"city": "Rome", "street": ""},
				"confirmed": true
			}`,
			schemaOK: false,
			goOK:     true,
		},
		{
			// email is not exported (format-only, diagnostics); a syntactically
			// invalid email passes the schema but fails Go.
			name: "email format enforced only by Go",
			json: `{
				"name": "bob",
				"age": 1,
				"score": 1,
				"role": "member",
				"email": "not-an-email",
				"tags": ["x"],
				"address": {"city": "Rome"},
				"confirmed": true
			}`,
			schemaOK: true,
			goOK:     false,
		},
	}

	for _, gc := range cases {
		t.Run(gc.name, func(t *testing.T) {
			var raw any
			Equal(t, json.Unmarshal([]byte(gc.json), &raw), nil)
			schemaOK := doc.validate(doc.root, raw)

			var demo schemaDemo
			Equal(t, json.Unmarshal([]byte(gc.json), &demo), nil)
			goOK := v.Struct(demo) == nil

			if schemaOK != gc.schemaOK || goOK != gc.goOK {
				t.Fatalf("gap drift: schema=%v (want %v) go=%v (want %v)",
					schemaOK, gc.schemaOK, goOK, gc.goOK)
			}
		})
	}

	// Every gap category must have a matching diagnostic so the export never
	// silently claims completeness.
	tags := diagnosticTags(exp.Diagnostics)
	Equal(t, sliceContains(tags, "email"), true)

	var hasOmitApprox bool
	for _, d := range exp.Diagnostics {
		if d.Tag == omitempty && d.Kind == DiagnosticApproximate {
			hasOmitApprox = true
		}
	}
	Equal(t, hasOmitApprox, true)
}

func TestExportJSONSchemaRejectsNonStruct(t *testing.T) {
	v := New()
	_, err := v.ExportJSONSchema(42)
	NotEqual(t, err, nil)

	var bad *schemaDemo
	_, err = v.ExportJSONSchema(bad)
	NotEqual(t, err, nil)
}

func TestExportJSONSchemaDiagnosticsForUnknownTag(t *testing.T) {
	type custom struct {
		Value string `json:"value" validate:"required,made_up_tag=123"`
	}
	v := New()
	exp, err := v.ExportJSONSchema(custom{})
	Equal(t, err, nil)

	var found bool
	for _, d := range exp.Diagnostics {
		if d.Tag == "made_up_tag" {
			found = true
			Equal(t, d.Kind, DiagnosticUnsupported)
		}
	}
	Equal(t, found, true)

	// Structural export still happens despite the unknown tag.
	raw, err := json.Marshal(exp.Schema)
	Equal(t, err, nil)
	Equal(t, strings.Contains(string(raw), `"value"`), true)
}

type schemaPointerDemo struct {
	Optional *string `json:"optional,omitempty" validate:"omitempty,min=2"`
	Required *int    `json:"required" validate:"required"`
	Level    int     `json:"level" validate:"oneof=1 2 3"`
	Code     string  `json:"code" validate:"ne=admin"`
}

func TestExportJSONSchemaPointerAndEnum(t *testing.T) {
	v := New()
	exp, err := v.ExportJSONSchema(schemaPointerDemo{})
	Equal(t, err, nil)
	doc := loadDoc(t, exp.Schema)

	type check struct {
		name string
		json string
		ok   bool
	}
	checks := []check{
		{name: "all valid with null optional", ok: true, json: `{"optional": null, "required": 5, "level": 2, "code": "x"}`},
		{name: "required pointer null rejected", ok: false, json: `{"required": null, "level": 2, "code": "x"}`},
		{name: "required pointer missing rejected", ok: false, json: `{"level": 2, "code": "x"}`},
		{name: "int enum out of range rejected", ok: false, json: `{"required": 1, "level": 9, "code": "x"}`},
		{name: "ne admin enforced", ok: false, json: `{"required": 1, "level": 1, "code": "admin"}`},
		{name: "optional short value rejected", ok: false, json: `{"optional": "a", "required": 1, "level": 1, "code": "x"}`},
		{name: "optional omitted accepted", ok: true, json: `{"required": 1, "level": 1, "code": "x"}`},
	}

	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			var raw any
			Equal(t, json.Unmarshal([]byte(c.json), &raw), nil)
			schemaOK := doc.validate(doc.root, raw)

			var demo schemaPointerDemo
			Equal(t, json.Unmarshal([]byte(c.json), &demo), nil)
			goOK := v.Struct(demo) == nil

			if schemaOK != c.ok || goOK != c.ok {
				t.Fatalf("schema=%v go=%v want %v", schemaOK, goOK, c.ok)
			}
		})
	}
}

type schemaItem struct {
	Kind string `json:"kind" validate:"required,min=1"`
}

type schemaSliceDemo struct {
	Items []schemaItem `json:"items" validate:"dive"`
}

func TestExportJSONSchemaSliceOfStructs(t *testing.T) {
	v := New()
	exp, err := v.ExportJSONSchema(schemaSliceDemo{})
	Equal(t, err, nil)
	doc := loadDoc(t, exp.Schema)

	valid := `{"items":[{"kind":"a"}]}`
	invalid := `{"items":[{"kind":""}]}`

	var rawValid any
	Equal(t, json.Unmarshal([]byte(valid), &rawValid), nil)
	Equal(t, doc.validate(doc.root, rawValid), true)

	var rawInvalid any
	Equal(t, json.Unmarshal([]byte(invalid), &rawInvalid), nil)
	Equal(t, doc.validate(doc.root, rawInvalid), false)

	var good schemaSliceDemo
	Equal(t, json.Unmarshal([]byte(valid), &good), nil)
	Equal(t, v.Struct(good), nil)

	var bad schemaSliceDemo
	Equal(t, json.Unmarshal([]byte(invalid), &bad), nil)
	NotEqual(t, v.Struct(bad), nil)
}

type schemaNode struct {
	Value    string       `json:"value" validate:"required,max=5"`
	Children []schemaNode `json:"children" validate:"dive"`
}

func TestExportJSONSchemaRecursive(t *testing.T) {
	plain := New()
	exp, err := plain.ExportJSONSchema(schemaNode{})
	Equal(t, err, nil)

	// Recursive type must produce a $ref back to its own $defs entry.
	raw, err := json.Marshal(exp.Schema)
	Equal(t, err, nil)
	doc := string(raw)
	Equal(t, strings.Contains(doc, `"$ref":"#/$defs/v10_schemaNode"`), true)

	evaluator := loadDoc(t, exp.Schema)

	valid := `{"value":"a","children":[{"value":"bb"}]}`
	invalid := `{"value":"toolong","children":[]}`

	var rawValid any
	Equal(t, json.Unmarshal([]byte(valid), &rawValid), nil)
	Equal(t, evaluator.validate(evaluator.root, rawValid), true)

	var rawInvalid any
	Equal(t, json.Unmarshal([]byte(invalid), &rawInvalid), nil)
	Equal(t, evaluator.validate(evaluator.root, rawInvalid), false)

	var good schemaNode
	Equal(t, json.Unmarshal([]byte(valid), &good), nil)
	Equal(t, plain.Struct(good), nil)

	var bad schemaNode
	Equal(t, json.Unmarshal([]byte(invalid), &bad), nil)
	NotEqual(t, plain.Struct(bad), nil)
}

type schemaEmbeddedBase struct {
	ID string `json:"id" validate:"required,max=3"`
}

type schemaEmbedded struct {
	schemaEmbeddedBase
	Extra string `json:"extra" validate:"min=2"`
	Skip  string `json:"-" validate:"required"`
}

func TestExportJSONSchemaEmbeddedAndIgnored(t *testing.T) {
	v := New()
	exp, err := v.ExportJSONSchema(schemaEmbedded{})
	Equal(t, err, nil)
	doc := loadDoc(t, exp.Schema)

	raw, err := json.Marshal(exp.Schema)
	Equal(t, err, nil)
	text := string(raw)
	Equal(t, strings.Contains(text, `"id"`), true)
	Equal(t, strings.Contains(text, `"extra"`), true)
	Equal(t, strings.Contains(text, `"Skip"`), false)

	valid := `{"id":"ab","extra":"xx"}`
	invalidID := `{"id":"toolong","extra":"xx"}`

	var rv any
	Equal(t, json.Unmarshal([]byte(valid), &rv), nil)
	Equal(t, doc.validate(doc.root, rv), true)

	var ri any
	Equal(t, json.Unmarshal([]byte(invalidID), &ri), nil)
	Equal(t, doc.validate(doc.root, ri), false)

	var good schemaEmbedded
	Equal(t, json.Unmarshal([]byte(valid), &good), nil)
	// json:"-" only affects encoding/json; the Go validator still runs the
	// field's `validate` tag. Set Skip explicitly so the Go comparison focuses
	// on the embedded-field constraint shared by both paths.
	good.Skip = "present"
	Equal(t, v.Struct(good), nil)

	var bad schemaEmbedded
	Equal(t, json.Unmarshal([]byte(invalidID), &bad), nil)
	bad.Skip = "present"
	NotEqual(t, v.Struct(bad), nil)
}

type schemaBareOmit struct {
	OnlyOmit string `json:"only_omit,omitempty" validate:"omitempty"`
	WithTag  string `json:"with_tag,omitempty" validate:"omitempty,min=2"`
}

func TestExportJSONSchemaBareOmitEmptyNoDiagnostic(t *testing.T) {
	v := New()
	exp, err := v.ExportJSONSchema(schemaBareOmit{})
	Equal(t, err, nil)

	var omitDiags int
	for _, d := range exp.Diagnostics {
		if d.Tag == omitempty {
			omitDiags++
			Equal(t, d.JSONPointer, "/$defs/v10_schemaBareOmit/properties/with_tag")
		}
	}
	Equal(t, omitDiags, 1)
}

func TestExportJSONSchemaAnonymousRecursive(t *testing.T) {
	type anon struct {
		V    string `json:"v" validate:"required,max=2"`
		Kids []struct {
			V    string        `json:"v" validate:"required,max=2"`
			Kids []interface{} `json:"kids"`
		} `json:"kids"`
	}

	v := New()
	exp, err := v.ExportJSONSchema(anon{})
	Equal(t, err, nil)

	doc := loadDoc(t, exp.Schema)
	valid := `{"v":"a","kids":[{"v":"b","kids":[]}]}`
	invalid := `{"v":"toolong","kids":[]}`

	var rv any
	Equal(t, json.Unmarshal([]byte(valid), &rv), nil)
	Equal(t, doc.validate(doc.root, rv), true)

	var ri any
	Equal(t, json.Unmarshal([]byte(invalid), &ri), nil)
	Equal(t, doc.validate(doc.root, ri), false)

	raw, _ := json.Marshal(exp.Schema)
	Equal(t, strings.Contains(string(raw), `"$defs"`), true)
}

func TestExportJSONSchemaDiagnosticsSerializable(t *testing.T) {
	v := New()
	exp, err := v.ExportJSONSchema(schemaDemo{})
	Equal(t, err, nil)
	NotEqual(t, len(exp.Diagnostics), 0)

	raw, err := json.MarshalIndent(exp.Diagnostics, "", "  ")
	Equal(t, err, nil)

	text := string(raw)
	Equal(t, strings.Contains(text, `"jsonPointer"`), true)
	Equal(t, strings.Contains(text, `"kind"`), true)
	Equal(t, strings.Contains(text, `"reason"`), true)

	var back []SchemaDiagnostic
	Equal(t, json.Unmarshal(raw, &back), nil)
	Equal(t, len(back), len(exp.Diagnostics))

	// Kinds must be the exported constants so callers can switch on them.
	kinds := map[DiagnosticKind]bool{}
	for _, d := range back {
		kinds[d.Kind] = true
	}
	Equal(t, kinds[DiagnosticUnsupported], true)
	Equal(t, kinds[DiagnosticApproximate], true)
}

// parityCase is one JSON document classified valid/invalid. The same bytes are
// run through both the exported JSON Schema and the Go struct validator.
type parityCase struct {
	name  string
	json  string
	valid bool
}

func TestJSONSchemaParity(t *testing.T) {
	v := New()
	exp, err := v.ExportJSONSchema(schemaDemo{})
	Equal(t, err, nil)
	doc := loadDoc(t, exp.Schema)

	cases := []parityCase{
		{
			name:  "fully valid",
			valid: true,
			json: `{
				"name": "alice",
				"age": 30,
				"score": 12.5,
				"role": "admin",
				"email": "a@example.com",
				"tags": ["ops"],
				"metadata": {"env": 1},
				"address": {"city": "Berlin", "street": "Main"},
				"confirmed": true
			}`,
		},
		{
			name:  "omitempty zero nick accepted by both",
			valid: true,
			json: `{
				"name": "bob",
				"nick": "",
				"age": 0,
				"score": 1,
				"role": "member",
				"email": "b@example.com",
				"tags": ["x"],
				"address": {"city": "Rome"},
				"confirmed": true
			}`,
		},
		{
			name:  "missing required name",
			valid: false,
			json: `{
				"age": 30,
				"score": 1,
				"role": "guest",
				"email": "c@example.com",
				"tags": ["x"],
				"address": {"city": "Lima"},
				"confirmed": true
			}`,
		},
		{
			name:  "name too long",
			valid: false,
			json: `{
				"name": "0123456789X",
				"age": 1,
				"score": 1,
				"role": "guest",
				"email": "d@example.com",
				"tags": ["x"],
				"address": {"city": "Lima"},
				"confirmed": true
			}`,
		},
		{
			name:  "age out of range",
			valid: false,
			json: `{
				"name": "eve",
				"age": 200,
				"score": 1,
				"role": "guest",
				"email": "e@example.com",
				"tags": ["x"],
				"address": {"city": "Lima"},
				"confirmed": true
			}`,
		},
		{
			name:  "score at excluded lower bound",
			valid: false,
			json: `{
				"name": "fay",
				"age": 1,
				"score": 0,
				"role": "guest",
				"email": "f@example.com",
				"tags": ["x"],
				"address": {"city": "Lima"},
				"confirmed": true
			}`,
		},
		{
			name:  "role not in enum",
			valid: false,
			json: `{
				"name": "gus",
				"age": 1,
				"score": 1,
				"role": "root",
				"email": "g@example.com",
				"tags": ["x"],
				"address": {"city": "Lima"},
				"confirmed": true
			}`,
		},
		{
			name:  "tag element too long",
			valid: false,
			json: `{
				"name": "hal",
				"age": 1,
				"score": 1,
				"role": "guest",
				"email": "h@example.com",
				"tags": ["toolong"],
				"address": {"city": "Lima"},
				"confirmed": true
			}`,
		},
		{
			name:  "map value below zero",
			valid: false,
			json: `{
				"name": "ivy",
				"age": 1,
				"score": 1,
				"role": "guest",
				"email": "i@example.com",
				"tags": ["x"],
				"metadata": {"k": -1},
				"address": {"city": "Lima"},
				"confirmed": true
			}`,
		},
		{
			name:  "nested city missing",
			valid: false,
			json: `{
				"name": "jon",
				"age": 1,
				"score": 1,
				"role": "guest",
				"email": "j@example.com",
				"tags": ["x"],
				"address": {},
				"confirmed": true
			}`,
		},
		{
			name:  "confirmed false violates eq=true",
			valid: false,
			json: `{
				"name": "kim",
				"age": 1,
				"score": 1,
				"role": "guest",
				"email": "k@example.com",
				"tags": ["x"],
				"address": {"city": "Lima"},
				"confirmed": false
			}`,
		},
		{
			name:  "manager null tolerated by omitempty",
			valid: true,
			json: `{
				"name": "lou",
				"age": 1,
				"score": 1,
				"role": "guest",
				"email": "l@example.com",
				"manager": null,
				"tags": ["x"],
				"address": {"city": "Lima"},
				"confirmed": true
			}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// JSON Schema path: decode into interface{} so types match what a
			// browser would send.
			var raw any
			Equal(t, json.Unmarshal([]byte(tc.json), &raw), nil)
			schemaOK := doc.validate(doc.root, raw)

			// Go path: decode into the typed struct, then run Struct.
			var demo schemaDemo
			Equal(t, json.Unmarshal([]byte(tc.json), &demo), nil)
			err := v.Struct(demo)
			goOK := err == nil

			if schemaOK != goOK {
				t.Fatalf("parity mismatch: schema=%v go=%v (want valid=%v)", schemaOK, goOK, tc.valid)
			}
			if schemaOK != tc.valid {
				t.Fatalf("unexpected verdict: got valid=%v want %v", schemaOK, tc.valid)
			}
		})
	}
}
