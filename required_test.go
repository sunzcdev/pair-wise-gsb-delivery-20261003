package toml_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/internal/assert"
)

// requiredKeys extracts the keys of the individual errors of a
// *toml.StrictMissingError, failing the test if err has another type.
func requiredKeys(t *testing.T, err error) []string {
	t.Helper()
	var sm *toml.StrictMissingError
	if !errors.As(err, &sm) {
		t.Fatalf("err should have been a *toml.StrictMissingError, but got %v (%T)", err, err)
	}
	keys := make([]string, 0, len(sm.Errors))
	for _, e := range sm.Errors {
		keys = append(keys, strings.Join(e.Key(), "."))
	}
	return keys
}

func TestRequiredMissingTopLevel(t *testing.T) {
	type doc struct {
		Name string `toml:"name,required"`
		Age  int    `toml:"age"`
	}
	var d doc
	err := toml.Unmarshal([]byte(`age = 42`), &d)
	assert.Equal(t, []string{"name"}, requiredKeys(t, err))
}

func TestRequiredZeroValuesAreProvided(t *testing.T) {
	type doc struct {
		B bool     `toml:"b,required"`
		N int      `toml:"n,required"`
		S string   `toml:"s,required"`
		A []string `toml:"a,required"`
	}
	d := doc{
		B: true,
		N: 42,
		S: "preset",
		A: []string{"preset"},
	}
	err := toml.Unmarshal([]byte("b = false\nn = 0\ns = \"\"\na = []"), &d)
	assert.NoError(t, err)
	assert.Equal(t, doc{B: false, N: 0, S: "", A: []string{}}, d)
}

func TestRequiredNestedStruct(t *testing.T) {
	type inner struct {
		Name string `toml:"name,required"`
	}
	type doc struct {
		Inner inner `toml:"inner"`
	}
	var d doc
	err := toml.Unmarshal([]byte("[inner]\n"), &d)
	assert.Equal(t, []string{"inner.name"}, requiredKeys(t, err))
}

func TestRequiredOptionalParentAbsent(t *testing.T) {
	type inner struct {
		Name string `toml:"name,required"`
	}
	type doc struct {
		Inner *inner `toml:"inner"`
	}
	var d doc
	// The optional parent does not appear in the document: its inner
	// required fields must not be reported.
	err := toml.Unmarshal([]byte(""), &d)
	assert.NoError(t, err)
	assert.Equal(t, (*inner)(nil), d.Inner)
}

func TestRequiredArrayTableElements(t *testing.T) {
	type item struct {
		Name string `toml:"name,required"`
		Qty  int    `toml:"qty,required"`
	}
	type doc struct {
		Items []item `toml:"items"`
	}
	var d doc
	err := toml.Unmarshal([]byte(`
[[items]]
name = "a"
qty = 1

[[items]]
qty = 2

[[items]]
name = "c"
`), &d)
	assert.Equal(t, []string{"items[1].name", "items[2].qty"}, requiredKeys(t, err))
}

func TestRequiredPreexistingValuesDoNotCount(t *testing.T) {
	type doc struct {
		Name string `toml:"name,required"`
	}
	// The target already holds a non-zero value: it is not evidence that
	// the document provided the field.
	d := doc{Name: "preset"}
	err := toml.Unmarshal([]byte(""), &d)
	assert.Equal(t, []string{"name"}, requiredKeys(t, err))
	assert.Equal(t, "preset", d.Name)
}

func TestRequiredAndUnknownFieldsTogether(t *testing.T) {
	type doc struct {
		Name string `toml:"name,required"`
	}
	var d doc
	r := strings.NewReader("unknown = 1\n")
	err := toml.NewDecoder(r).DisallowUnknownFields().Decode(&d)

	var sm *toml.StrictMissingError
	if !errors.As(err, &sm) {
		t.Fatalf("err should have been a *toml.StrictMissingError, but got %v (%T)", err, err)
	}
	if len(sm.Errors) != 2 {
		t.Fatalf("expected 2 errors, got %d: %v", len(sm.Errors), sm.Errors)
	}
	// Unknown-field errors come first (document order), then missing
	// required fields.
	assert.Equal(t, toml.Key{"unknown"}, sm.Errors[0].Key())
	assert.Equal(t, toml.Key{"name"}, sm.Errors[1].Key())
	assert.True(t, strings.Contains(sm.Errors[1].Error(), "missing required field"))
}

func TestRequiredInlineTable(t *testing.T) {
	type inner struct {
		Name string `toml:"name,required"`
	}
	type doc struct {
		Inner inner `toml:"inner,required"`
	}
	var d doc
	// The parent is provided as an empty inline table: the parent itself is
	// satisfied, but its inner required field is reported.
	err := toml.Unmarshal([]byte("inner = {}"), &d)
	assert.Equal(t, []string{"inner.name"}, requiredKeys(t, err))
}

func TestRequiredInlineArrayOfTables(t *testing.T) {
	type item struct {
		Name string `toml:"name,required"`
	}
	type doc struct {
		Items []item `toml:"items"`
	}
	var d doc
	err := toml.Unmarshal([]byte(`items = [{name = "a"}, {}]`), &d)
	assert.Equal(t, []string{"items[1].name"}, requiredKeys(t, err))
}

func TestRequiredDottedKeys(t *testing.T) {
	type inner struct {
		Name string `toml:"name,required"`
	}
	type doc struct {
		Inner inner `toml:"inner"`
	}
	var d doc
	err := toml.Unmarshal([]byte(`inner.name = "x"`), &d)
	assert.NoError(t, err)
	assert.Equal(t, "x", d.Inner.Name)
}

func TestRequiredMapOfStructs(t *testing.T) {
	type server struct {
		Host string `toml:"host,required"`
		Port int    `toml:"port"`
	}
	type doc struct {
		Servers map[string]server `toml:"servers"`
	}
	var d doc
	err := toml.Unmarshal([]byte("[servers.alpha]\nport = 1\n[servers.beta]\nhost = \"b\"\n"), &d)
	assert.Equal(t, []string{"servers.alpha.host"}, requiredKeys(t, err))
}

func TestRequiredRootMap(t *testing.T) {
	type inner struct {
		Name string `toml:"name,required"`
	}
	m := map[string]inner{}
	err := toml.Unmarshal([]byte("[a]\n"), &m)
	assert.Equal(t, []string{"a.name"}, requiredKeys(t, err))
}

func TestRequiredRootSlice(t *testing.T) {
	type inner struct {
		Name string `toml:"name,required"`
	}
	var s []inner
	err := toml.Unmarshal([]byte("other = 1\n"), &s)
	assert.Equal(t, []string{"[0].name"}, requiredKeys(t, err))
}

func TestRequiredNoneMissing(t *testing.T) {
	type doc struct {
		Name string `toml:"name,required"`
	}
	var d doc
	err := toml.Unmarshal([]byte(`name = "x"`), &d)
	assert.NoError(t, err)
	assert.Equal(t, "x", d.Name)
}

func TestRequiredTagWithoutName(t *testing.T) {
	type doc struct {
		Name string `toml:",required"`
	}
	var d doc
	err := toml.Unmarshal([]byte(""), &d)
	assert.Equal(t, []string{"Name"}, requiredKeys(t, err))

	err = toml.Unmarshal([]byte(`Name = "x"`), &d)
	assert.NoError(t, err)
}

func TestRequiredCaseInsensitiveMatch(t *testing.T) {
	type doc struct {
		Name string `toml:"name,required"`
	}
	var d doc
	// The document key matches the field with a different casing: it still
	// counts as provided.
	err := toml.Unmarshal([]byte(`Name = "x"`), &d)
	assert.NoError(t, err)
	assert.Equal(t, "x", d.Name)
}

func TestRequiredEmbeddedStruct(t *testing.T) {
	type base struct {
		Name string `toml:"name,required"`
	}
	type doc struct {
		base
		Age int `toml:"age"`
	}
	var d doc
	err := toml.Unmarshal([]byte(`age = 1`), &d)
	assert.Equal(t, []string{"name"}, requiredKeys(t, err))

	err = toml.Unmarshal([]byte(`name = "x"`), &d)
	assert.NoError(t, err)
	assert.Equal(t, "x", d.Name)
}

func TestRequiredPointerParentProvided(t *testing.T) {
	type inner struct {
		Name string `toml:"name,required"`
	}
	type doc struct {
		Inner *inner `toml:"inner"`
	}
	var d doc
	// The optional parent appears but omits its required field.
	err := toml.Unmarshal([]byte("[inner]\n"), &d)
	assert.Equal(t, []string{"inner.name"}, requiredKeys(t, err))
}

// requiredTextValue decodes itself from a single string, so its inner
// required field is never expected to appear in the document.
type requiredTextValue struct {
	Code string `toml:"code,required"`
}

func (v *requiredTextValue) UnmarshalText(text []byte) error {
	v.Code = string(text)
	return nil
}

func TestRequiredTextUnmarshaler(t *testing.T) {
	type doc struct {
		V requiredTextValue `toml:"v,required"`
	}
	var d doc
	err := toml.Unmarshal([]byte(`v = "red"`), &d)
	assert.NoError(t, err)
	assert.Equal(t, "red", d.V.Code)
}

func TestRequiredCombinedWithOtherOptions(t *testing.T) {
	type doc struct {
		Name string `toml:"name,required,omitempty"`
	}
	var d doc
	err := toml.Unmarshal([]byte(""), &d)
	assert.Equal(t, []string{"name"}, requiredKeys(t, err))

	// The encoder ignores the decode-only option.
	out, err := toml.Marshal(doc{Name: "x"})
	assert.NoError(t, err)
	assert.Equal(t, "name = 'x'\n", string(out))
}
