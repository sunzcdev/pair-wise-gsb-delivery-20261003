package validator

import (
	"encoding/json"
	"strings"
	"testing"
)

// This test suite is the correctness contract for the JSON Schema exporter:
//
//  1. A set of fields whose tags are all exactly expressible in JSON Schema
//     (SchemaSupported). The exported schema must be Complete(), and a shared
//     corpus of valid/invalid JSON documents must be accepted/rejected
//     identically by (a) the exported JSON Schema and (b) Go unmarshal +
//     validator.Struct.
//  2. A set of fields containing tags that cannot be expressed
//     (SchemaUnsupported). Every such tag must appear in Diagnostics, and the
//     corpus documents that exercise those tags demonstrate the precise places
//     where the two validators diverge.

// ---------------------------------------------------------------------------
// Fully supported model
// ---------------------------------------------------------------------------

type schemaAddress struct {
	Street string `json:"street" validate:"required,min=2,max=40"`
	Zip    string `json:"zip,omitempty" validate:"omitempty,len=5"`
}

type schemaSupported struct {
	Name      string         `json:"name" validate:"required,min=1,max=10"`
	Nick      string         `json:"nick,omitempty" validate:"omitempty,min=2,max=8"`
	Age       int            `json:"age" validate:"gte=0,lte=120"`
	Score     float64        `json:"score" validate:"gte=0,lte=100"`
	Level     int            `json:"level" validate:"gt=0,lt=5"`
	Kind      string         `json:"kind" validate:"oneof=red green blue"`
	Port      int            `json:"port" validate:"min=1,max=65535"`
	Active    bool           `json:"active"`
	Tags      []string       `json:"tags" validate:"min=1,max=3,dive,required,min=1"`
	Flags     []int          `json:"flags,omitempty" validate:"omitempty,dive,gte=0,lte=9"`
	Meta      map[string]int `json:"meta,omitempty" validate:"omitempty,min=1,dive,keys,len=2,endkeys,gte=0"`
	Matrix    [][]int        `json:"matrix,omitempty" validate:"omitempty,dive,dive,gte=0"`
	Address   schemaAddress  `json:"address"`
	OptAddr   *schemaAddress `json:"opt_addr,omitempty"`
	Countries []string       `json:"countries,omitempty" validate:"omitempty,unique"`
	Color     string         `json:"color,omitempty" validate:"omitempty,oneof=red green"`
}

var supportedValid = []string{
	// full valid document
	`{
		"name":"alice",
		"age":30,
		"score":12.5,
		"level":1,
		"kind":"red",
		"port":8080,
		"active":true,
		"tags":["go","rust"],
		"address":{"street":"Main","zip":"12345"},
		"color":"green"
	}`,
	// boundaries: name length 1 and 10, level 1 and 4, tags 1 and 3 entries
	`{"name":"a","age":0,"score":0,"level":1,"kind":"green","port":1,"tags":["x"],"address":{"street":"ab"}}`,
	`{"name":"0123456789","age":120,"score":100,"level":4,"kind":"blue","port":65535,
	  "tags":["a","b","c"],"flags":[0,9],"meta":{"ab":0},"matrix":[[0]],"address":{"street":"0123456789012345678901234567890123456789"}}`,
	// optional pointer omitted and present (null and value)
	`{"name":"bob","level":1,"kind":"red","port":1,"tags":["x"],"address":{"street":"Long Street"}}`,
	`{"name":"bob","level":1,"kind":"red","port":1,"tags":["x"],"address":{"street":"Long Street"},"opt_addr":null}`,
	`{"name":"bob","level":1,"kind":"red","port":1,"tags":["x"],"address":{"street":"Long Street"},"opt_addr":{"street":"Oak"}}`,
	// truly optional fields (nick/flags/meta/matrix/countries/color) omitted
	`{"name":"c","level":2,"kind":"red","port":2,"tags":["x"],"address":{"street":"Elm"},"countries":["us","de"]}`,
	// empty-but-present omitempty values are accepted because omitempty skips them
	`{"name":"d","level":2,"kind":"red","port":2,"tags":["x"],"address":{"street":"Elm"},"nick":"","flags":[]}`,
	// meta with one entry satisfies its own min=1 (present non-empty)
	`{"name":"e","level":2,"kind":"red","port":2,"tags":["x"],"address":{"street":"Elm"},"meta":{"ab":1}}`,
}

var supportedInvalid = []string{
	// missing required name
	`{"level":1,"kind":"red","port":1,"tags":["x"],"address":{"street":"Elm"}}`,
	// name too long
	`{"name":"01234567890","level":1,"kind":"red","port":1,"tags":["x"],"address":{"street":"Elm"}}`,
	// name empty string (present) fails required/min=1
	`{"name":"","level":1,"kind":"red","port":1,"tags":["x"],"address":{"street":"Elm"}}`,
	// age out of range
	`{"name":"a","level":1,"age":-1,"kind":"red","port":1,"tags":["x"],"address":{"street":"Elm"}}`,
	`{"name":"a","level":1,"age":121,"kind":"red","port":1,"tags":["x"],"address":{"street":"Elm"}}`,
	// score out of range
	`{"name":"a","level":1,"port":1,"score":100.01,"kind":"red","tags":["x"],"address":{"street":"Elm"}}`,
	// level strict bounds: 0 and 5 both invalid (gt=0,lt=5)
	`{"name":"a","level":0,"kind":"red","port":1,"tags":["x"],"address":{"street":"Elm"}}`,
	`{"name":"a","level":5,"kind":"red","port":1,"tags":["x"],"address":{"street":"Elm"}}`,
	// enum violation
	`{"name":"a","level":1,"kind":"purple","port":1,"tags":["x"],"address":{"street":"Elm"}}`,
	// port bounds
	`{"name":"a","level":1,"kind":"red","port":0,"tags":["x"],"address":{"street":"Elm"}}`,
	`{"name":"a","level":1,"kind":"red","port":65536,"tags":["x"],"address":{"street":"Elm"}}`,
	// tags: empty array violates min=1
	`{"name":"a","level":1,"kind":"red","port":1,"tags":[],"address":{"street":"Elm"}}`,
	// tags: too many
	`{"name":"a","level":1,"kind":"red","port":1,"tags":["a","b","c","d"],"address":{"street":"Elm"}}`,
	// tags: empty string element violates dive,required + min=1
	`{"name":"a","level":1,"kind":"red","port":1,"tags":[""],"address":{"street":"Elm"}}`,
	// flags element out of range
	`{"name":"a","level":1,"kind":"red","port":1,"tags":["x"],"flags":[10],"address":{"street":"Elm"}}`,
	// meta: value out of range
	`{"name":"a","level":1,"kind":"red","port":1,"tags":["x"],"meta":{"ab":-1},"address":{"street":"Elm"}}`,
	// meta: key length must be 2
	`{"name":"a","level":1,"kind":"red","port":1,"tags":["x"],"meta":{"abc":0},"address":{"street":"Elm"}}`,
	// meta: omitempty,min=1 -> present empty map fails minProperties
	`{"name":"a","level":1,"kind":"red","port":1,"tags":["x"],"meta":{},"address":{"street":"Elm"}}`,
	// matrix inner element out of range
	`{"name":"a","level":1,"kind":"red","port":1,"tags":["x"],"matrix":[[-1]],"address":{"street":"Elm"}}`,
	// nested required street missing
	`{"name":"a","level":1,"kind":"red","port":1,"tags":["x"],"address":{}}`,
	// nested street too short
	`{"name":"a","level":1,"kind":"red","port":1,"tags":["x"],"address":{"street":"x"}}`,
	// zip present but wrong length
	`{"name":"a","level":1,"kind":"red","port":1,"tags":["x"],"address":{"street":"Elm","zip":"12"}}`,
	// countries not unique
	`{"name":"a","level":1,"kind":"red","port":1,"tags":["x"],"address":{"street":"Elm"},"countries":["us","us"]}`,
	// color enum violation on present value
	`{"name":"a","level":1,"kind":"red","port":1,"tags":["x"],"address":{"street":"Elm"},"color":"blue"}`,
	// wrong JSON type for a field
	`{"name":123,"level":1,"kind":"red","port":1,"tags":["x"],"address":{"street":"Elm"}}`,
}

// ---------------------------------------------------------------------------
// Partially supported model (demonstrates diagnostics + real gaps)
// ---------------------------------------------------------------------------

type schemaUnsupported struct {
	// email has a regex validator with no JSON Schema keyword.
	Email string `json:"email" validate:"required,email"`
	// gtefield is cross-field: Start must be <= End.
	Start int `json:"start"`
	End   int `json:"end" validate:"gtefield=Start"`
	// required_if is conditional on another field.
	Reason string `json:"reason,omitempty" validate:"required_if=Kind other"`
	Kind   string `json:"kind" validate:"oneof=normal other"`
	// datetime uses a Go time layout.
	When string `json:"when,omitempty" validate:"omitempty,datetime=2006-01-02"`
	// contains is a substring rule.
	Token string `json:"token,omitempty" validate:"omitempty,contains=abc"`
	// custom registered validator.
	Even int `json:"even" validate:"even"`
}

// unsupportedCase is one corpus document plus, for each of the two
// validators, whether it is expected to pass. They deliberately differ for
// documents that exercise untranslated tags.
type unsupportedCase struct {
	name     string
	json     string
	goOK     bool
	schemaOK bool
	note     string
}

var unsupportedCases = []unsupportedCase{
	{
		name:     "all good",
		json:     `{"email":"a@b.com","start":1,"end":2,"kind":"normal","when":"2020-01-01","token":"xxabc","even":4}`,
		goOK:     true,
		schemaOK: true,
	},
	{
		name:     "bad email passes schema but fails Go",
		json:     `{"email":"not-an-email","kind":"normal","even":2}`,
		goOK:     false,
		schemaOK: true,
		note:     "email tag untranslated",
	},
	{
		name:     "end before start passes schema but fails Go",
		json:     `{"email":"a@b.com","kind":"normal","start":5,"end":1,"even":2}`,
		goOK:     false,
		schemaOK: true,
		note:     "gtefield cross-field untranslated",
	},
	{
		name:     "missing required_if reason: Go rejects, schema accepts (gap)",
		json:     `{"email":"a@b.com","kind":"other","even":2}`,
		goOK:     false,
		schemaOK: true,
		note:     "required_if conditional presence untranslated",
	},
	{
		name:     "required_if reason present",
		json:     `{"email":"a@b.com","kind":"other","reason":"why","even":2}`,
		goOK:     true,
		schemaOK: true,
	},
	{
		name:     "bad datetime passes schema, fails Go",
		json:     `{"email":"a@b.com","kind":"normal","when":"01/02/2020","even":2}`,
		goOK:     false,
		schemaOK: true,
		note:     "datetime layout untranslated",
	},
	{
		name:     "missing substring passes schema, fails Go",
		json:     `{"email":"a@b.com","kind":"normal","token":"xyz","even":2}`,
		goOK:     false,
		schemaOK: true,
		note:     "contains untranslated",
	},
	{
		name:     "odd number passes schema, fails Go (custom tag)",
		json:     `{"email":"a@b.com","kind":"normal","even":3}`,
		goOK:     false,
		schemaOK: true,
		note:     "custom validator opaque",
	},
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestJSONSchemaSupportedIsComplete(t *testing.T) {
	v := New()
	exp, err := v.JSONSchemaFor(schemaSupported{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exp.Complete() {
		for _, d := range exp.Diagnostics {
			t.Errorf("unexpected diagnostic [%s] %s @%s tag=%s: %s", d.Severity, d.Reason, d.Pointer, d.Tag, d.Message)
		}
	}
}

func TestJSONSchemaSupportedCorpusAgrees(t *testing.T) {
	v := New()
	exp, err := v.JSONSchemaFor(schemaSupported{})
	if err != nil {
		t.Fatal(err)
	}

	check := func(t *testing.T, label, raw string, wantGoOK, wantSchemaOK bool) {
		t.Helper()

		var generic interface{}
		if err := json.Unmarshal([]byte(raw), &generic); err != nil {
			t.Fatalf("%s: invalid test JSON: %v", label, err)
		}
		schemaErrs := validateSchema(exp.Schema, generic)
		schemaOK := len(schemaErrs) == 0

		var s schemaSupported
		// Decode into the concrete struct exactly like a service would before
		// running struct validation.
		unmarshalErr := json.Unmarshal([]byte(raw), &s)
		structErr := error(nil)
		if unmarshalErr == nil {
			structErr = v.Struct(s)
		}
		goOK := unmarshalErr == nil && structErr == nil

		if schemaOK != wantSchemaOK {
			t.Errorf("%s: schema result = %v (errors %v), want %v", label, schemaOK, schemaErrs, wantSchemaOK)
		}
		if goOK != wantGoOK {
			t.Errorf("%s: go result = %v (unmarshal %v, struct %v), want %v", label, goOK, unmarshalErr, structErr, wantGoOK)
		}
		if schemaOK != goOK {
			t.Errorf("%s: MISMATCH schema=%v go=%v", label, schemaOK, goOK)
		}
	}

	for i, raw := range supportedValid {
		check(t, label("valid", i, raw), raw, true, true)
	}
	for i, raw := range supportedInvalid {
		check(t, label("invalid", i, raw), raw, false, false)
	}
}

func TestJSONSchemaUnsupportedDiagnostics(t *testing.T) {
	v := New()
	mustRegister(t, v)

	exp, err := v.JSONSchemaFor(schemaUnsupported{})
	if err != nil {
		t.Fatal(err)
	}
	if exp.Complete() {
		t.Fatal("expected diagnostics for unsupported model, got Complete()=true")
	}

	wantTags := map[string]string{
		"email":       ReasonUnsupportedTag,
		"gtefield":    ReasonUnsupportedTag,
		"required_if": ReasonUnsupportedTag,
		"datetime":    ReasonUnsupportedTag,
		"contains":    ReasonUnsupportedTag,
		"even":        ReasonCustomValidator,
	}

	seen := map[string]bool{}
	for _, d := range exp.Diagnostics {
		if d.Severity != DiagnosticError {
			continue
		}
		base := d.Tag
		if i := strings.IndexByte(base, '='); i >= 0 {
			base = base[:i]
		}
		if reason, ok := wantTags[base]; ok {
			if d.Reason != reason {
				t.Errorf("tag %q reason = %q, want %q", base, d.Reason, reason)
			}
			if !strings.HasPrefix(d.Pointer, "/") || d.Pointer == "" {
				t.Errorf("tag %q missing JSON pointer, got %q", base, d.Pointer)
			}
			seen[base] = true
		}
	}
	for tag := range wantTags {
		if !seen[tag] {
			t.Errorf("missing diagnostic for tag %q", tag)
		}
	}
}

func TestJSONSchemaUnsupportedCorpusDemonstratesGaps(t *testing.T) {
	v := New()
	mustRegister(t, v)

	exp, err := v.JSONSchemaFor(schemaUnsupported{})
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range unsupportedCases {
		t.Run(c.name, func(t *testing.T) {
			var generic interface{}
			if err := json.Unmarshal([]byte(c.json), &generic); err != nil {
				t.Fatalf("bad test JSON: %v", err)
			}
			schemaOK := len(validateSchema(exp.Schema, generic)) == 0

			var s schemaUnsupported
			umErr := json.Unmarshal([]byte(c.json), &s)
			structErr := error(nil)
			if umErr == nil {
				structErr = v.Struct(s)
			}
			goOK := umErr == nil && structErr == nil

			if schemaOK != c.schemaOK {
				t.Errorf("schema result=%v want %v", schemaOK, c.schemaOK)
			}
			if goOK != c.goOK {
				t.Errorf("go result=%v want %v (struct=%v)", goOK, c.goOK, structErr)
			}
		})
	}
}

func mustRegister(t *testing.T, v *Validate) {
	t.Helper()
	if err := v.RegisterValidation("even", func(fl FieldLevel) bool {
		return fl.Field().Int()%2 == 0
	}); err != nil {
		t.Fatal(err)
	}
}

func label(kind string, i int, raw string) string {
	short := raw
	if len(short) > 50 {
		short = short[:50] + "..."
	}
	return kind + "#" + itoa(i) + " " + strings.ReplaceAll(short, "\n", " ")
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
