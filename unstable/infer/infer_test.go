package infer

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func mustSchema(t *testing.T, samples ...string) map[string]interface{} {
	t.Helper()
	readers := make([]io.Reader, len(samples))
	for i, s := range samples {
		readers[i] = strings.NewReader(s)
	}
	raw, err := Schema(readers...)
	if err != nil {
		t.Fatalf("Schema() error = %v", err)
	}
	var schema map[string]interface{}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("unmarshalling inferred schema: %v\n%s", err, string(raw))
	}
	return schema
}

func mustReencoded(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("re-encoding: %v", err)
	}
	return string(b)
}

func at(t *testing.T, schema map[string]interface{}, path ...string) map[string]interface{} {
	t.Helper()
	cur := schema
	for _, key := range path {
		props, ok := cur["properties"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected properties at %v, got %v", path, cur)
		}
		child, ok := props[key].(map[string]interface{})
		if !ok {
			t.Fatalf("property %q missing at %v", key, path)
		}
		cur = child
	}
	return cur
}

func requiredList(n map[string]interface{}) []string {
	raw, ok := n["required"].([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		out = append(out, v.(string))
	}
	return out
}

func TestSchemaRequiresSample(t *testing.T) {
	if _, err := Schema(); err == nil {
		t.Fatal("Schema() with no samples expected error, got nil")
	}
}

func TestSchemaReportsParseErrors(t *testing.T) {
	good := strings.NewReader("name = \"x\"\n")
	bad := strings.NewReader("name = \n")
	_, err := Schema(good, bad)
	if err == nil {
		t.Fatal("expected parse error for invalid sample, got nil")
	}
	var parseErr *json.SyntaxError
	if errors.As(err, &parseErr) {
		t.Fatal("TOML parse error should not be reported as a JSON syntax error")
	}
}

func TestSchemaHasDraftURIAndObjectRoot(t *testing.T) {
	schema := mustSchema(t, "name = \"x\"\n")
	if got := schema["$schema"]; got != "https://json-schema.org/draft/2020-12/schema" {
		t.Fatalf("$schema = %v", got)
	}
	if got := schema["type"]; got != "object" {
		t.Fatalf("root type = %v, want object", got)
	}
}

func TestRequiredIsIntersectionOfSeenKeys(t *testing.T) {
	schema := mustSchema(t,
		"a = 1\nb = 2\n[nested]\nx = 1\ny = 2\n",
		"a = 3\nc = 4\n[nested]\ny = 3\nz = 4\n",
	)

	if got := requiredList(schema); len(got) != 2 || got[0] != "a" || got[1] != "nested" {
		t.Fatalf("root required = %v, want [a nested]", got)
	}
	nested := at(t, schema, "nested")
	if got := requiredList(nested); len(got) != 1 || got[0] != "y" {
		t.Fatalf("nested required = %v, want [y]", got)
	}

	// Properties that only appear in some samples are still described.
	props := schema["properties"].(map[string]interface{})
	for _, key := range []string{"a", "b", "c"} {
		if _, ok := props[key]; !ok {
			t.Errorf("property %q missing", key)
		}
	}
}

func TestEmptyArrayGetsNoItems(t *testing.T) {
	schema := mustSchema(t,
		"values = []\n",
		"other = 1\n",
	)
	values := at(t, schema, "values")
	if got := values["type"]; got != "array" {
		t.Fatalf("values type = %v", got)
	}
	if _, present := values["items"]; present {
		t.Fatal("empty array must not produce an items schema")
	}

	// A later non-empty array is allowed to provide evidence retroactively.
	schema2 := mustSchema(t,
		"values = []\n",
		"values = [\"a\"]\n",
	)
	values2 := at(t, schema2, "values")
	items, ok := values2["items"].(map[string]interface{})
	if !ok {
		t.Fatalf("items missing after non-empty sample: %v", values2)
	}
	if got := items["type"]; got != "string" {
		t.Fatalf("items type = %v, want string", got)
	}
}

func TestArrayMembersMergeAcrossSamples(t *testing.T) {
	schema := mustSchema(t,
		"[[rows]]\nname = \"a\"\nport = 1\n",
		"[[rows]]\nname = \"b\"\n[[rows]]\nport = 2\n",
	)
	items := at(t, schema, "rows")
	if got := items["type"]; got != "array" {
		t.Fatalf("rows type = %v", got)
	}
	member := items["items"].(map[string]interface{})
	if got := member["type"]; got != "object" {
		t.Fatalf("member type = %v", got)
	}
	if got := requiredList(member); got != nil {
		t.Fatalf("array-table members must not have required keys, got %v", got)
	}
	memberProps := member["properties"].(map[string]interface{})
	if _, ok := memberProps["name"]; !ok {
		t.Error("member property name missing")
	}
	if _, ok := memberProps["port"]; !ok {
		t.Error("member property port missing")
	}
}

func TestHeterogeneousValuesBecomeAnyOf(t *testing.T) {
	schema := mustSchema(t,
		"value = 1\nmix = [1]\n",
		"value = \"x\"\nmix = [\"y\", true]\n",
	)

	value := at(t, schema, "value")
	anyOf, ok := value["anyOf"].([]interface{})
	if !ok || len(anyOf) != 2 {
		t.Fatalf("value anyOf = %v", value)
	}

	mix := at(t, schema, "mix")
	items := mix["items"].(map[string]interface{})
	itemAnyOf, ok := items["anyOf"].([]interface{})
	if !ok || len(itemAnyOf) != 3 {
		t.Fatalf("mix items anyOf = %v", items)
	}
}

func TestTwoHeterogeneousArraysMergeTheirUnions(t *testing.T) {
	// Each sample independently builds an anyOf for its array items; merging
	// the samples must fold the two unions together without duplicating the
	// shared string variant.
	schema := mustSchema(t,
		"mix = [1, \"x\"]\n",
		"mix = [\"y\", true]\n",
	)
	items := at(t, schema, "mix")["items"].(map[string]interface{})
	anyOf, ok := items["anyOf"].([]interface{})
	if !ok {
		t.Fatalf("expected anyOf items, got %v", items)
	}
	if len(anyOf) != 3 {
		t.Fatalf("expected 3 item variants, got %d: %v", len(anyOf), anyOf)
	}
}

func TestCompositeKindConflictsBecomeAnyOf(t *testing.T) {
	schema := mustSchema(t,
		"value.k = 1\nlist = [1]\n",
		"value = \"x\"\nlist = \"y\"\n",
	)

	value := at(t, schema, "value")
	valueAnyOf, ok := value["anyOf"].([]interface{})
	if !ok || len(valueAnyOf) != 2 {
		t.Fatalf("object/string conflict expected 2 anyOf variants, got %v", value)
	}

	list := at(t, schema, "list")
	listAnyOf, ok := list["anyOf"].([]interface{})
	if !ok || len(listAnyOf) != 2 {
		t.Fatalf("array/string conflict expected 2 anyOf variants, got %v", list)
	}
}

func TestAnyOfVariantsAreDeduplicatedAndMerged(t *testing.T) {
	// Across samples "v" takes object, string, and integer shapes. Object
	// evidence arrives in two samples (including via the union/union fold) and
	// must be merged into one variant rather than duplicated.
	schema := mustSchema(t,
		"v.k = 1\n",
		"v = \"x\"\n",
		"v = 2\n",
		"v.k2 = 3\n",
	)

	v := at(t, schema, "v")
	anyOf, ok := v["anyOf"].([]interface{})
	if !ok {
		t.Fatalf("expected anyOf, got %v", v)
	}
	if len(anyOf) != 3 {
		t.Fatalf("expected 3 merged variants (object/string/integer), got %d: %v",
			len(anyOf), anyOf)
	}

	var objectVariant map[string]interface{}
	for _, variant := range anyOf {
		m := variant.(map[string]interface{})
		if m["type"] == "object" {
			objectVariant = m
		}
	}
	if objectVariant == nil {
		t.Fatal("object variant missing")
	}
	props := objectVariant["properties"].(map[string]interface{})
	if _, ok := props["k"]; !ok {
		t.Error("object evidence from the first sample lost")
	}
	if _, ok := props["k2"]; !ok {
		t.Error("object evidence from the last sample lost")
	}
}

func TestScalarTypes(t *testing.T) {
	schema := mustSchema(t, `
s = "x"
i = 3
f = 1.5
b = true
`)
	cases := map[string]string{
		"s": "string",
		"i": "integer",
		"f": "number",
		"b": "boolean",
	}
	for key, want := range cases {
		node := at(t, schema, key)
		if got := node["type"]; got != want {
			t.Errorf("%s type = %v, want %s", key, got, want)
		}
	}

	// Integers and floats are distinct scalar kinds, not merged into number.
	both := mustSchema(t, "n = 1\n", "n = 1.5\n")
	node := at(t, both, "n")
	if _, ok := node["anyOf"]; !ok {
		t.Fatalf("integer+float expected anyOf, got %v", node)
	}
}

func TestTemporalKindsAreAnnotatedStrings(t *testing.T) {
	schema := mustSchema(t, `
offset = 1985-04-12T23:20:50.52-07:00
localdt = 1985-04-12T23:20:50.52
date = 1985-04-12
clock = 23:20:50.52
`)
	cases := map[string]string{
		"offset":  "offset-date-time",
		"localdt": "local-date-time",
		"date":    "local-date",
		"clock":   "local-time",
	}
	for key, want := range cases {
		node := at(t, schema, key)
		if got := node["type"]; got != "string" {
			t.Errorf("%s type = %v, want string", key, got)
		}
		if got := node["x-toml-type"]; got != want {
			t.Errorf("%s x-toml-type = %v, want %s", key, got, want)
		}
	}
}

func TestQuotedDottedKeyIsOneProperty(t *testing.T) {
	schema := mustSchema(t, "\"a.b.c\" = 1\nd = 2\n")
	props := schema["properties"].(map[string]interface{})
	if _, ok := props["a.b.c"]; !ok {
		t.Fatalf("quoted dotted key missing; props = %v", props)
	}
	// A quoted key must not be split into nested tables.
	if _, ok := props["a"]; ok {
		t.Fatal("quoted key was incorrectly split into nested tables")
	}
}

func TestOrderDoesNotChangeContract(t *testing.T) {
	samples := []string{
		"name = \"a\"\nport = 80\nmix = [1, \"x\"]\n[nested]\nx = 1\n",
		"port = 81\nmix = [true]\nflag = true\n[nested]\ny = 2\n",
		"name = \"b\"\n[[rows]]\nid = 1\n[[rows]]\nlabel = \"r\"\n",
	}

	forward := mustSchema(t, samples...)

	reversed := make([]string, len(samples))
	for i, s := range samples {
		reversed[len(samples)-1-i] = s
	}
	backward := mustSchema(t, reversed...)

	if mustReencoded(t, forward) != mustReencoded(t, backward) {
		t.Fatalf("contracts differ with sample order:\n%s\n!=\n%s",
			mustReencoded(t, forward), mustReencoded(t, backward))
	}

	// Within an array, member order must not change the contract either.
	a := mustSchema(t, "[[rows]]\nx = 1\n[[rows]]\ny = 2\n")
	b := mustSchema(t, "[[rows]]\ny = 2\n[[rows]]\nx = 1\n")
	if mustReencoded(t, a) != mustReencoded(t, b) {
		t.Fatal("array member order changed the contract")
	}
}

func TestOutputContainsNoForbiddenKeywords(t *testing.T) {
	raw, err := Schema(
		strings.NewReader("name = \"x\"\nn = 3\n"),
		strings.NewReader("name = \"y\"\nn = 4\ndata = []\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{
		"minimum", "maximum", "minLength", "maxLength",
		"pattern", "default", "enum", "format",
		"minItems", "maxItems", "additionalProperties",
	}
	text := string(raw)
	for _, word := range forbidden {
		if bytes.Contains(raw, []byte("\""+word+"\"")) {
			t.Errorf("output must not assert %q\n%s", word, text)
		}
	}
}

func TestEmptyDocumentIsObjectSample(t *testing.T) {
	// An empty TOML document is an empty table; it still counts as an object
	// sample, so keys seen only in the other document are not required.
	schema := mustSchema(t, "", "name = \"x\"\n")
	if got := requiredList(schema); got != nil {
		t.Fatalf("expected no required keys, got %v", got)
	}
}

func TestNodeFromValueRejectsUnexpectedType(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for an unsupported decoded type")
		}
	}()
	// Values decoded by toml.Decoder are never structs of this type.
	_ = nodeFromValue(struct{ x int }{})
}
