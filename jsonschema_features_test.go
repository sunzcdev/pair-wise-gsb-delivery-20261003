package validator

import (
	"encoding/json"
	"strings"
	"testing"
)

func findDiag(diags []JSONSchemaDiagnostic, reason string) *JSONSchemaDiagnostic {
	for i := range diags {
		if diags[i].Reason == reason {
			return &diags[i]
		}
	}
	return nil
}

type featEmbedBase struct {
	Shared string `json:"shared" validate:"required,min=1"`
}

type featEmbedded struct {
	featEmbedBase
	Own string `json:"own" validate:"omitempty,max=5"`
}

type featNode struct {
	Value string     `json:"value" validate:"required,min=1"`
	Next  *featNode  `json:"next,omitempty"`
	Kids  []featNode `json:"kids,omitempty" validate:"omitempty,dive"`
}

type featSpecial struct {
	Bin   []byte  `json:"bin" validate:"required,min=1"`
	Num   int64   `json:"num,string" validate:"gte=1"`
	Ratio float64 `json:"ratio" validate:"gt=0"`
	Zero  string  `json:"zero" validate:"omitzero,min=3"`
	Color string  `json:"color" validate:"hexcolor|rgb"`
}

type featMapInt struct {
	M map[int]string `json:"m" validate:"dive,keys,min=1,endkeys,required"`
}

type featOr struct {
	V string `json:"v" validate:"eq=alice|eq=bob"`
}

type featEndKeys struct {
	M map[string]string `json:"m" validate:"dive,keys,len=3,endkeys,len=2"`
}

func TestJSONSchemaEndKeysDoesNotLeak(t *testing.T) {
	v := New()
	exp, err := v.JSONSchemaFor(featEndKeys{})
	if err != nil {
		t.Fatal(err)
	}
	if !exp.Complete() {
		t.Fatalf("keys/endkeys map should be fully translatable: %#v", exp.Diagnostics)
	}
	// key len 3, value len 2
	okDoc := map[string]interface{}{"m": map[string]interface{}{"abc": "xy"}}
	if errs := validateSchema(exp.Schema, okDoc); len(errs) != 0 {
		t.Fatalf("valid key/value doc rejected: %v", errs)
	}
	if errs := validateSchema(exp.Schema, map[string]interface{}{"m": map[string]interface{}{"ab": "xy"}}); len(errs) == 0 {
		t.Error("short key should fail propertyNames")
	}
	if errs := validateSchema(exp.Schema, map[string]interface{}{"m": map[string]interface{}{"abc": "x"}}); len(errs) == 0 {
		t.Error("short value should fail additionalProperties")
	}
}

func TestJSONSchemaEmbeddedPromotion(t *testing.T) {
	v := New()
	exp, err := v.JSONSchemaFor(featEmbedded{})
	if err != nil {
		t.Fatal(err)
	}
	if !exp.Complete() {
		t.Fatalf("expected complete, got %#v", exp.Diagnostics)
	}

	// Promoted field appears as a top-level property and is required.
	valid := map[string]interface{}{
		"shared": "x",
	}
	if errs := validateSchema(exp.Schema, valid); len(errs) != 0 {
		t.Fatalf("promoted valid doc rejected: %v", errs)
	}
	if errs := validateSchema(exp.Schema, map[string]interface{}{"own": "ok"}); len(errs) == 0 {
		t.Fatal("missing promoted required field should be rejected")
	}
}

func TestJSONSchemaRecursionUsesRef(t *testing.T) {
	v := New()
	exp, err := v.JSONSchemaFor(featNode{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(exp.Schema)
	if !strings.Contains(string(b), "#/$defs/") {
		t.Fatalf("expected recursive \u0024ref, got %s", b)
	}

	doc := map[string]interface{}{
		"value": "root",
		"next": map[string]interface{}{
			"value": "child",
			"next":  map[string]interface{}{"value": "gc"},
		},
		"kids": []interface{}{
			map[string]interface{}{"value": "k1"},
		},
	}
	if errs := validateSchema(exp.Schema, doc); len(errs) != 0 {
		t.Fatalf("recursive valid doc rejected: %v", errs)
	}
	bad := map[string]interface{}{"next": map[string]interface{}{}}
	if errs := validateSchema(exp.Schema, bad); len(errs) == 0 {
		t.Fatal("nested missing required value should be rejected")
	}
}

func TestJSONSchemaSpecialCaseDiagnostics(t *testing.T) {
	v := New()
	exp, err := v.JSONSchemaFor(featSpecial{})
	if err != nil {
		t.Fatal(err)
	}
	if exp.Complete() {
		t.Fatal("expected diagnostics for byte slice, string option, omitzero and float gt")
	}

	if findDiag(exp.Diagnostics, ReasonByteSlice) == nil {
		t.Errorf("missing byte slice diagnostic; got %#v", exp.Diagnostics)
	}
	if d := findDiag(exp.Diagnostics, ReasonStringOption); d == nil || d.GoNamespace != "Num" {
		t.Errorf("missing json ,string diagnostic on Num; got %#v", exp.Diagnostics)
	}
	if d := findDiag(exp.Diagnostics, ReasonOmitZero); d == nil || d.GoNamespace != "Zero" {
		t.Errorf("missing omitzero diagnostic; got %#v", exp.Diagnostics)
	}
	foundRatio := false
	for _, d := range exp.Diagnostics {
		if d.Reason == ReasonUnsupportedTag && d.GoNamespace == "Ratio" && d.Tag == "gt=0" {
			foundRatio = true
		}
	}
	if !foundRatio {
		t.Errorf("missing float gt diagnostic; got %#v", exp.Diagnostics)
	}
	// hexcolor and rgb are format-style tags; the 'or' of them must be reported,
	// not silently dropped.
	foundColor := false
	for _, d := range exp.Diagnostics {
		if d.GoNamespace == "Color" && strings.Contains(d.Tag, "hexcolor") {
			foundColor = true
		}
	}
	if !foundColor {
		t.Errorf("missing unsupported color or diagnostic; got %#v", exp.Diagnostics)
	}
}

func TestJSONSchemaNonStringMapKeysDiagnostic(t *testing.T) {
	v := New()
	exp, err := v.JSONSchemaFor(featMapInt{})
	if err != nil {
		t.Fatal(err)
	}
	if findDiag(exp.Diagnostics, ReasonMapKeys) == nil {
		t.Fatalf("expected non-string map key diagnostic, got %#v", exp.Diagnostics)
	}
}

func TestJSONSchemaOrExpression(t *testing.T) {
	v := New()
	exp, err := v.JSONSchemaFor(featOr{})
	if err != nil {
		t.Fatal(err)
	}
	if !exp.Complete() {
		t.Fatalf("eq=alice|eq=bob is expressible, got %#v", exp.Diagnostics)
	}
	for _, okVal := range []string{"alice", "bob"} {
		var g interface{}
		_ = json.Unmarshal([]byte(`{"v":"`+okVal+`"}`), &g)
		if errs := validateSchema(exp.Schema, g); len(errs) != 0 {
			t.Errorf("value %q should pass anyOf: %v", okVal, errs)
		}
	}
	var g interface{}
	_ = json.Unmarshal([]byte(`{"v":"carol"}`), &g)
	if errs := validateSchema(exp.Schema, g); len(errs) == 0 {
		t.Error("value carol should fail anyOf")
	}
}

func TestJSONSchemaPointerNullAndRequired(t *testing.T) {
	type ptrModel struct {
		Req *string `json:"req" validate:"required"`
		Opt *int    `json:"opt" validate:"gte=0"`
	}
	v := New()
	exp, err := v.JSONSchemaFor(ptrModel{})
	if err != nil {
		t.Fatal(err)
	}
	if !exp.Complete() {
		t.Fatalf("pointer model should be complete: %#v", exp.Diagnostics)
	}

	if errs := validateSchema(exp.Schema, map[string]interface{}{"req": "x"}); len(errs) != 0 {
		t.Errorf("valid doc rejected: %v", errs)
	}
	// required pointer: explicit null rejected
	if errs := validateSchema(exp.Schema, map[string]interface{}{"req": nil}); len(errs) == 0 {
		t.Error("required null pointer should be rejected")
	}
	// missing required pointer rejected
	if errs := validateSchema(exp.Schema, map[string]interface{}{}); len(errs) == 0 {
		t.Error("missing required pointer should be rejected")
	}
	// optional pointer accepts null
	if errs := validateSchema(exp.Schema, map[string]interface{}{"req": "x", "opt": nil}); len(errs) != 0 {
		t.Errorf("optional null pointer should be accepted: %v", errs)
	}
	// optional pointer, when present, still constrained
	if errs := validateSchema(exp.Schema, map[string]interface{}{"req": "x", "opt": -1.0}); len(errs) == 0 {
		t.Error("optional pointer value must still satisfy gte=0")
	}
}

func TestJSONSchemaOmitemptyScalars(t *testing.T) {
	type om struct {
		S string `json:"s,omitempty" validate:"omitempty,len=3"`
		N int    `json:"n,omitempty" validate:"omitempty,gte=5"`
	}
	v := New()
	exp, err := v.JSONSchemaFor(om{})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		doc  string
		want bool
	}{
		{`{}`, true},
		{`{"s":""}`, true},
		{`{"n":0}`, true},
		{`{"s":"abc"}`, true},
		{`{"n":5}`, true},
		{`{"s":"ab"}`, false},
		{`{"n":4}`, false},
	}
	for _, c := range cases {
		var g interface{}
		if err := json.Unmarshal([]byte(c.doc), &g); err != nil {
			t.Fatal(err)
		}
		got := len(validateSchema(exp.Schema, g)) == 0
		if got != c.want {
			t.Errorf("%s: got %v want %v", c.doc, got, c.want)
		}
	}
}

func TestJSONSchemaDiagnosticsAreSortedAndPointed(t *testing.T) {
	v := New()
	if !mustRegister2(t, v) {
		t.Fatal("register even")
	}
	exp, err := v.JSONSchemaFor(schemaUnsupported{})
	if err != nil {
		t.Fatal(err)
	}
	// Every error diagnostic must carry a JSON pointer and a Go namespace.
	for _, d := range exp.Diagnostics {
		if d.Severity != DiagnosticError {
			continue
		}
		if d.Pointer == "" {
			t.Errorf("diagnostic %s has empty pointer: %s", d.Tag, d.Message)
		}
		if d.GoNamespace == "" {
			t.Errorf("diagnostic %s has empty Go namespace", d.Tag)
		}
	}
}

func mustRegister2(t *testing.T, v *Validate) bool {
	t.Helper()
	return v.RegisterValidation("even", func(fl FieldLevel) bool {
		return fl.Field().Int()%2 == 0
	}) == nil
}
