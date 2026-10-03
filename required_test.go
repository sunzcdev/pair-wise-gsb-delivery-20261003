package toml_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/internal/assert"
)

func TestRequiredFieldMissing(t *testing.T) {
	type Config struct {
		Name string `toml:"name,required"`
		Port int    `toml:"port"`
	}

	var cfg Config
	err := toml.Unmarshal([]byte(`port = 8080`), &cfg)

	var mre *toml.MissingRequiredError
	if !errors.As(err, &mre) {
		t.Fatalf("expected a *toml.MissingRequiredError, got %v (%T)", err, err)
	}
	assert.Equal(t, 1, len(mre.Errors))
	assert.Equal(t, "toml: missing required field name", mre.Errors[0].Error())
	assert.Equal(t, toml.Key{"name"}, mre.Errors[0].Key())
}

func TestRequiredFieldPresent(t *testing.T) {
	type Config struct {
		Name string `toml:"name,required"`
	}

	var cfg Config
	err := toml.Unmarshal([]byte(`name = "foo"`), &cfg)
	assert.NoError(t, err)
	assert.Equal(t, "foo", cfg.Name)
}

// Zero values explicitly present in the document count as provided.
func TestRequiredFieldZeroValues(t *testing.T) {
	type Config struct {
		B     bool     `toml:"b,required"`
		I     int      `toml:"i,required"`
		S     string   `toml:"s,required"`
		Slice []string `toml:"slice,required"`
	}

	doc := `
b = false
i = 0
s = ""
slice = []
`
	var cfg Config
	err := toml.Unmarshal([]byte(doc), &cfg)
	assert.NoError(t, err)
}

// A non-zero value already present in the target does not count as the
// document providing the field.
func TestRequiredFieldPreexistingValue(t *testing.T) {
	type Config struct {
		Name string `toml:"name,required"`
	}

	cfg := Config{Name: "preset"}
	err := toml.Unmarshal([]byte(``), &cfg)

	var mre *toml.MissingRequiredError
	if !errors.As(err, &mre) {
		t.Fatalf("expected a *toml.MissingRequiredError, got %v (%T)", err, err)
	}
	assert.Equal(t, 1, len(mre.Errors))
	assert.Equal(t, toml.Key{"name"}, mre.Errors[0].Key())
	// The pre-existing value is left untouched.
	assert.Equal(t, "preset", cfg.Name)
}

func TestRequiredFieldNestedStruct(t *testing.T) {
	type Inner struct {
		Name string `toml:"name,required"`
	}
	type Config struct {
		Sub Inner `toml:"sub"`
	}

	t.Run("parent present, field missing", func(t *testing.T) {
		var cfg Config
		err := toml.Unmarshal([]byte("[sub]\n"), &cfg)
		var mre *toml.MissingRequiredError
		if !errors.As(err, &mre) {
			t.Fatalf("expected a *toml.MissingRequiredError, got %v (%T)", err, err)
		}
		assert.Equal(t, 1, len(mre.Errors))
		assert.Equal(t, "toml: missing required field sub.name", mre.Errors[0].Error())
		assert.Equal(t, toml.Key{"sub", "name"}, mre.Errors[0].Key())
	})

	t.Run("parent absent, inner fields not required", func(t *testing.T) {
		var cfg Config
		err := toml.Unmarshal([]byte(""), &cfg)
		assert.NoError(t, err)
	})

	t.Run("dotted key provides field", func(t *testing.T) {
		var cfg Config
		err := toml.Unmarshal([]byte(`sub.name = "x"`), &cfg)
		assert.NoError(t, err)
		assert.Equal(t, "x", cfg.Sub.Name)
	})

	t.Run("inline table provides field", func(t *testing.T) {
		var cfg Config
		err := toml.Unmarshal([]byte(`sub = {name = "x"}`), &cfg)
		assert.NoError(t, err)
		assert.Equal(t, "x", cfg.Sub.Name)
	})

	t.Run("empty inline table misses field", func(t *testing.T) {
		var cfg Config
		err := toml.Unmarshal([]byte(`sub = {}`), &cfg)
		var mre *toml.MissingRequiredError
		if !errors.As(err, &mre) {
			t.Fatalf("expected a *toml.MissingRequiredError, got %v (%T)", err, err)
		}
		assert.Equal(t, 1, len(mre.Errors))
		assert.Equal(t, toml.Key{"sub", "name"}, mre.Errors[0].Key())
	})
}

func TestRequiredFieldNestedPointer(t *testing.T) {
	type Inner struct {
		Name string `toml:"name,required"`
	}
	type Config struct {
		Sub *Inner `toml:"sub"`
	}

	t.Run("nil parent, no error", func(t *testing.T) {
		var cfg Config
		err := toml.Unmarshal([]byte(""), &cfg)
		assert.NoError(t, err)
	})

	t.Run("parent provided, field missing", func(t *testing.T) {
		var cfg Config
		err := toml.Unmarshal([]byte("[sub]\n"), &cfg)
		var mre *toml.MissingRequiredError
		if !errors.As(err, &mre) {
			t.Fatalf("expected a *toml.MissingRequiredError, got %v (%T)", err, err)
		}
		assert.Equal(t, 1, len(mre.Errors))
		assert.Equal(t, toml.Key{"sub", "name"}, mre.Errors[0].Key())
	})
}

// Each element of an array table is checked independently, and the error
// reports the full path including the element index.
func TestRequiredFieldArrayTable(t *testing.T) {
	type Server struct {
		Host string `toml:"host,required"`
		Port int    `toml:"port"`
	}
	type Config struct {
		Servers []Server `toml:"servers"`
	}

	doc := `
[[servers]]
host = "a"
port = 1

[[servers]]
port = 2
`
	var cfg Config
	err := toml.Unmarshal([]byte(doc), &cfg)
	var mre *toml.MissingRequiredError
	if !errors.As(err, &mre) {
		t.Fatalf("expected a *toml.MissingRequiredError, got %v (%T)", err, err)
	}
	assert.Equal(t, 1, len(mre.Errors))
	assert.Equal(t, "toml: missing required field servers[1].host", mre.Errors[0].Error())
	assert.Equal(t, toml.Key{"servers[1]", "host"}, mre.Errors[0].Key())
}

func TestRequiredFieldArrayOfInlineTables(t *testing.T) {
	type Server struct {
		Host string `toml:"host,required"`
	}
	type Config struct {
		Servers []Server `toml:"servers"`
	}

	doc := `servers = [{host = "a"}, {}]`
	var cfg Config
	err := toml.Unmarshal([]byte(doc), &cfg)
	var mre *toml.MissingRequiredError
	if !errors.As(err, &mre) {
		t.Fatalf("expected a *toml.MissingRequiredError, got %v (%T)", err, err)
	}
	assert.Equal(t, 1, len(mre.Errors))
	assert.Equal(t, toml.Key{"servers[1]", "host"}, mre.Errors[0].Key())
}

// Tables nested under an array-table element belong to that element.
func TestRequiredFieldNestedUnderArrayTable(t *testing.T) {
	type TLS struct {
		Enabled bool `toml:"enabled,required"`
	}
	type Server struct {
		Host string `toml:"host,required"`
		TLS  TLS    `toml:"tls"`
	}
	type Config struct {
		Servers []Server `toml:"servers"`
	}

	doc := `
[[servers]]
host = "a"
[servers.tls]
enabled = true

[[servers]]
host = "b"
[servers.tls]
`
	var cfg Config
	err := toml.Unmarshal([]byte(doc), &cfg)
	var mre *toml.MissingRequiredError
	if !errors.As(err, &mre) {
		t.Fatalf("expected a *toml.MissingRequiredError, got %v (%T)", err, err)
	}
	assert.Equal(t, 1, len(mre.Errors))
	assert.Equal(t, "toml: missing required field servers[1].tls.enabled", mre.Errors[0].Error())
	assert.Equal(t, toml.Key{"servers[1]", "tls", "enabled"}, mre.Errors[0].Key())
}

// Fixed-size arrays used as array tables track elements by index too.
func TestRequiredFieldFixedArrayTable(t *testing.T) {
	type Server struct {
		Host string `toml:"host,required"`
	}
	type Config struct {
		Servers [2]Server `toml:"servers"`
	}

	doc := `
[[servers]]
host = "a"

[[servers]]
`
	var cfg Config
	err := toml.Unmarshal([]byte(doc), &cfg)
	var mre *toml.MissingRequiredError
	if !errors.As(err, &mre) {
		t.Fatalf("expected a *toml.MissingRequiredError, got %v (%T)", err, err)
	}
	assert.Equal(t, 1, len(mre.Errors))
	assert.Equal(t, toml.Key{"servers[1]", "host"}, mre.Errors[0].Key())
}

// A required slice field is provided by any array-table or array
// expression, including an empty array.
func TestRequiredFieldRequiredSlice(t *testing.T) {
	type Server struct {
		Host string `toml:"host,required"`
	}
	type Config struct {
		Servers []Server `toml:"servers,required"`
	}

	t.Run("missing", func(t *testing.T) {
		var cfg Config
		err := toml.Unmarshal([]byte(""), &cfg)
		var mre *toml.MissingRequiredError
		if !errors.As(err, &mre) {
			t.Fatalf("expected a *toml.MissingRequiredError, got %v (%T)", err, err)
		}
		assert.Equal(t, 1, len(mre.Errors))
		assert.Equal(t, toml.Key{"servers"}, mre.Errors[0].Key())
	})

	t.Run("empty array", func(t *testing.T) {
		var cfg Config
		assert.NoError(t, toml.Unmarshal([]byte("servers = []"), &cfg))
	})

	t.Run("array table", func(t *testing.T) {
		var cfg Config
		assert.NoError(t, toml.Unmarshal([]byte("[[servers]]\nhost = \"a\"\n"), &cfg))
	})
}

// Dotted keys provide the intermediate instances they traverse.
func TestRequiredFieldDeepDottedKey(t *testing.T) {
	type C struct {
		V string `toml:"v,required"`
	}
	type B struct {
		C C `toml:"c"`
	}
	type A struct {
		B B `toml:"b"`
	}
	type Config struct {
		A A `toml:"a"`
	}

	t.Run("provided", func(t *testing.T) {
		var cfg Config
		assert.NoError(t, toml.Unmarshal([]byte(`a.b.c.v = "x"`), &cfg))
		assert.Equal(t, "x", cfg.A.B.C.V)
	})

	t.Run("missing leaf", func(t *testing.T) {
		var cfg Config
		err := toml.Unmarshal([]byte(`a.b.c = {}`), &cfg)
		var mre *toml.MissingRequiredError
		if !errors.As(err, &mre) {
			t.Fatalf("expected a *toml.MissingRequiredError, got %v (%T)", err, err)
		}
		assert.Equal(t, 1, len(mre.Errors))
		assert.Equal(t, toml.Key{"a", "b", "c", "v"}, mre.Errors[0].Key())
	})
}

func TestRequiredFieldInMap(t *testing.T) {
	type Inner struct {
		Name string `toml:"name,required"`
	}
	type Config struct {
		Items map[string]Inner `toml:"items"`
	}

	var cfg Config
	err := toml.Unmarshal([]byte("[items.first]\n"), &cfg)
	var mre *toml.MissingRequiredError
	if !errors.As(err, &mre) {
		t.Fatalf("expected a *toml.MissingRequiredError, got %v (%T)", err, err)
	}
	assert.Equal(t, 1, len(mre.Errors))
	assert.Equal(t, toml.Key{"items", "first", "name"}, mre.Errors[0].Key())
}

// Map values reached through dotted keys and inline tables are tracked too.
func TestRequiredFieldInMapDottedAndInline(t *testing.T) {
	type Inner struct {
		Name string `toml:"name,required"`
	}
	type Config struct {
		Items map[string]Inner `toml:"items"`
	}

	t.Run("dotted key provides", func(t *testing.T) {
		var cfg Config
		assert.NoError(t, toml.Unmarshal([]byte(`items.first.name = "x"`), &cfg))
		assert.Equal(t, "x", cfg.Items["first"].Name)
	})

	t.Run("inline table missing", func(t *testing.T) {
		var cfg Config
		err := toml.Unmarshal([]byte(`items = {first = {}}`), &cfg)
		var mre *toml.MissingRequiredError
		if !errors.As(err, &mre) {
			t.Fatalf("expected a *toml.MissingRequiredError, got %v (%T)", err, err)
		}
		assert.Equal(t, 1, len(mre.Errors))
		assert.Equal(t, toml.Key{"items", "first", "name"}, mre.Errors[0].Key())
	})
}

// Non-string map keys take the generic map-key path.
func TestRequiredFieldInIntMap(t *testing.T) {
	type Inner struct {
		Name string `toml:"name,required"`
	}
	type Config struct {
		Items map[int]Inner `toml:"items"`
	}

	var cfg Config
	assert.NoError(t, toml.Unmarshal([]byte(`items.1.name = "x"`), &cfg))
	assert.Equal(t, "x", cfg.Items[1].Name)
}

// A plain table header over a slice field targets its implicit element.
func TestRequiredFieldTableOverSlice(t *testing.T) {
	type Server struct {
		Host string `toml:"host,required"`
	}
	type Config struct {
		Servers []Server `toml:"servers"`
	}

	var cfg Config
	err := toml.Unmarshal([]byte("[servers]\n"), &cfg)
	var mre *toml.MissingRequiredError
	if !errors.As(err, &mre) {
		t.Fatalf("expected a *toml.MissingRequiredError, got %v (%T)", err, err)
	}
	assert.Equal(t, 1, len(mre.Errors))
	assert.Equal(t, toml.Key{"servers[0]", "host"}, mre.Errors[0].Key())
}

// Array tables assigned to an interface field do not disturb tracking.
func TestRequiredFieldInterfaceArrayTable(t *testing.T) {
	type Config struct {
		Name string      `toml:"name,required"`
		Any  interface{} `toml:"any"`
	}

	var cfg Config
	err := toml.Unmarshal([]byte("name = \"x\"\n[[any]]\nfoo = 1\n"), &cfg)
	assert.NoError(t, err)
}

// Fixed-size arrays of inline tables check each element.
func TestRequiredFieldFixedArrayInlineTables(t *testing.T) {
	type Server struct {
		Host string `toml:"host,required"`
	}
	type Config struct {
		Servers [2]Server `toml:"servers"`
	}

	var cfg Config
	err := toml.Unmarshal([]byte(`servers = [{host = "a"}, {}]`), &cfg)
	var mre *toml.MissingRequiredError
	if !errors.As(err, &mre) {
		t.Fatalf("expected a *toml.MissingRequiredError, got %v (%T)", err, err)
	}
	assert.Equal(t, 1, len(mre.Errors))
	assert.Equal(t, toml.Key{"servers[1]", "host"}, mre.Errors[0].Key())
}

// Dotted keys crossing slices and fixed-size arrays index the last/current
// element.
func TestRequiredFieldDottedKeyAcrossArrays(t *testing.T) {
	type Server struct {
		Host string `toml:"host,required"`
	}

	t.Run("slice", func(t *testing.T) {
		type Config struct {
			Servers []Server `toml:"servers"`
		}
		var cfg Config
		assert.NoError(t, toml.Unmarshal([]byte(`servers.host = "x"`), &cfg))
		assert.Equal(t, "x", cfg.Servers[0].Host)
	})

	t.Run("fixed array", func(t *testing.T) {
		type Config struct {
			Servers [2]Server `toml:"servers"`
		}
		var cfg Config
		assert.NoError(t, toml.Unmarshal([]byte(`servers.host = "x"`), &cfg))
		assert.Equal(t, "x", cfg.Servers[0].Host)
	})
}

// Array tables nested under a fixed-size array segment.
func TestRequiredFieldArrayTableUnderFixedArray(t *testing.T) {
	type C struct {
		Name string `toml:"name,required"`
	}
	type B struct {
		Cs []C `toml:"cs"`
	}
	type A struct {
		Bs [2]B `toml:"bs"`
	}
	type Config struct {
		A A `toml:"a"`
	}

	doc := `
[[a.bs.cs]]
name = "x"

[[a.bs.cs]]
`
	var cfg Config
	err := toml.Unmarshal([]byte(doc), &cfg)
	var mre *toml.MissingRequiredError
	if !errors.As(err, &mre) {
		t.Fatalf("expected a *toml.MissingRequiredError, got %v (%T)", err, err)
	}
	assert.Equal(t, 1, len(mre.Errors))
	assert.Equal(t, toml.Key{"a", "bs[0]", "cs[1]", "name"}, mre.Errors[0].Key())
}

// A decode error inside a nested struct aborts decoding with that error, not
// a required-field report.
func TestRequiredFieldDecodeErrorInsideNested(t *testing.T) {
	type Inner struct {
		Port int `toml:"port"`
	}
	type Config struct {
		Name string `toml:"name,required"`
		Sub  Inner  `toml:"sub"`
	}

	var cfg Config
	err := toml.Unmarshal([]byte("name = \"x\"\nsub.port = \"bad\"\n"), &cfg)
	if err == nil {
		t.Fatal("expected a decode error")
	}
	var mre *toml.MissingRequiredError
	if errors.As(err, &mre) {
		t.Fatalf("did not expect a *toml.MissingRequiredError, got %v", err)
	}
}

// A decode error inside a map value aborts decoding with that error.
func TestRequiredFieldDecodeErrorInsideMap(t *testing.T) {
	type Inner struct {
		Port int `toml:"port"`
	}
	type Config struct {
		Name  string           `toml:"name,required"`
		Items map[string]Inner `toml:"items"`
	}

	var cfg Config
	err := toml.Unmarshal([]byte("name = \"x\"\nitems.first.port = \"bad\"\n"), &cfg)
	if err == nil {
		t.Fatal("expected a decode error")
	}
	var mre *toml.MissingRequiredError
	if errors.As(err, &mre) {
		t.Fatalf("did not expect a *toml.MissingRequiredError, got %v", err)
	}
}

// Types decoded through the unstable.Unmarshaler interface are opaque: their
// own required tags do not apply.
type requiredCustom struct {
	Name string `toml:"name,required"`
	raw  string
}

func (c *requiredCustom) UnmarshalTOML(data []byte) error {
	c.raw = string(data)
	return nil
}

func TestRequiredFieldUnmarshalerInterface(t *testing.T) {
	type Config struct {
		Sub requiredCustom `toml:"sub"`
	}

	var cfg Config
	err := toml.NewDecoder(strings.NewReader("[sub]\nanything = 1\n")).
		EnableUnmarshalerInterface().
		Decode(&cfg)
	assert.NoError(t, err)
}

// A slice as the root target cannot carry instance segments; decoding must
// not panic.
func TestRequiredFieldRootSlice(t *testing.T) {
	type Server struct {
		Host string `toml:"host,required"`
	}

	var s []Server
	assert.NoError(t, toml.Unmarshal([]byte(`host = "a"`), &s))
	assert.Equal(t, 1, len(s))
	assert.Equal(t, "a", s[0].Host)
}

// Multiple missing fields are all reported, in a stable order: instances in
// document order, fields in declaration order.
func TestRequiredFieldMultipleMissing(t *testing.T) {
	type Server struct {
		Host string `toml:"host,required"`
		Port int    `toml:"port,required"`
	}
	type Config struct {
		Name    string   `toml:"name,required"`
		Servers []Server `toml:"servers"`
	}

	doc := `
[[servers]]
host = "a"

[[servers]]
`
	var cfg Config
	err := toml.Unmarshal([]byte(doc), &cfg)
	var mre *toml.MissingRequiredError
	if !errors.As(err, &mre) {
		t.Fatalf("expected a *toml.MissingRequiredError, got %v (%T)", err, err)
	}
	assert.Equal(t, 4, len(mre.Errors))
	assert.Equal(t, toml.Key{"name"}, mre.Errors[0].Key())
	assert.Equal(t, toml.Key{"servers[0]", "port"}, mre.Errors[1].Key())
	assert.Equal(t, toml.Key{"servers[1]", "host"}, mre.Errors[2].Key())
	assert.Equal(t, toml.Key{"servers[1]", "port"}, mre.Errors[3].Key())
}

// Required and unknown-field errors are returned together, in a stable
// order: unknown fields first, then missing required fields.
func TestRequiredFieldWithStrictMode(t *testing.T) {
	type Config struct {
		Name string `toml:"name,required"`
	}

	doc := `unknown = 1`
	var cfg Config
	err := toml.NewDecoder(strings.NewReader(doc)).DisallowUnknownFields().Decode(&cfg)
	if err == nil {
		t.Fatal("expected an error")
	}

	var sme *toml.StrictMissingError
	if !errors.As(err, &sme) {
		t.Fatalf("expected a *toml.StrictMissingError, got %v (%T)", err, err)
	}
	assert.Equal(t, 1, len(sme.Errors))

	var mre *toml.MissingRequiredError
	if !errors.As(err, &mre) {
		t.Fatalf("expected a *toml.MissingRequiredError, got %v (%T)", err, err)
	}
	assert.Equal(t, 1, len(mre.Errors))
	assert.Equal(t, toml.Key{"name"}, mre.Errors[0].Key())

	// The joined message mentions the unknown field first.
	idxUnknown := strings.Index(err.Error(), "strict mode")
	idxRequired := strings.Index(err.Error(), "required")
	if idxUnknown < 0 || idxRequired < 0 || idxUnknown > idxRequired {
		t.Fatalf("unexpected combined error message: %q", err.Error())
	}
}

// Without strict mode, unknown fields are still ignored while required
// fields are enforced.
func TestRequiredFieldWithoutStrictMode(t *testing.T) {
	type Config struct {
		Name string `toml:"name,required"`
	}

	var cfg Config
	err := toml.Unmarshal([]byte("unknown = 1"), &cfg)
	var mre *toml.MissingRequiredError
	if !errors.As(err, &mre) {
		t.Fatalf("expected a *toml.MissingRequiredError, got %v (%T)", err, err)
	}
	var sme *toml.StrictMissingError
	if errors.As(err, &sme) {
		t.Fatalf("did not expect a *toml.StrictMissingError, got %v", err)
	}
}

func TestRequiredFieldTagVariants(t *testing.T) {
	type Config struct {
		A string `toml:",required"`
		B string `toml:"renamed,required"`
	}

	var cfg Config
	err := toml.Unmarshal([]byte(""), &cfg)
	var mre *toml.MissingRequiredError
	if !errors.As(err, &mre) {
		t.Fatalf("expected a *toml.MissingRequiredError, got %v (%T)", err, err)
	}
	assert.Equal(t, 2, len(mre.Errors))
	assert.Equal(t, toml.Key{"A"}, mre.Errors[0].Key())
	assert.Equal(t, toml.Key{"renamed"}, mre.Errors[1].Key())
}

// Case-insensitive key matching still counts as providing the field.
func TestRequiredFieldCaseInsensitive(t *testing.T) {
	type Config struct {
		Name string `toml:"name,required"`
	}

	var cfg Config
	err := toml.Unmarshal([]byte(`NAME = "x"`), &cfg)
	assert.NoError(t, err)
	assert.Equal(t, "x", cfg.Name)
}

func TestRequiredFieldEmbeddedStruct(t *testing.T) {
	type Base struct {
		Name string `toml:"name,required"`
	}
	type Config struct {
		Base
	}

	var cfg Config
	err := toml.Unmarshal([]byte(""), &cfg)
	var mre *toml.MissingRequiredError
	if !errors.As(err, &mre) {
		t.Fatalf("expected a *toml.MissingRequiredError, got %v (%T)", err, err)
	}
	assert.Equal(t, 1, len(mre.Errors))
	assert.Equal(t, toml.Key{"name"}, mre.Errors[0].Key())
}

// Targets without required fields are unaffected.
func TestRequiredFieldNone(t *testing.T) {
	type Config struct {
		Name string `toml:"name"`
	}

	var cfg Config
	assert.NoError(t, toml.Unmarshal([]byte(""), &cfg))

	var m map[string]interface{}
	assert.NoError(t, toml.Unmarshal([]byte(""), &m))

	var i interface{}
	assert.NoError(t, toml.Unmarshal([]byte(""), &i))
}

func TestMissingRequiredErrorString(t *testing.T) {
	type Config struct {
		Name string `toml:"name,required"`
	}

	var cfg Config
	err := toml.Unmarshal([]byte("port = 1\n"), &cfg)
	var mre *toml.MissingRequiredError
	if !errors.As(err, &mre) {
		t.Fatalf("expected a *toml.MissingRequiredError, got %v (%T)", err, err)
	}
	assert.Equal(t, "toml: document is missing required fields", mre.Error())
	if !strings.Contains(mre.String(), "missing required field name") {
		t.Fatalf("unexpected String() output: %q", mre.String())
	}
	// Unwrap exposes the individual DecodeErrors.
	assert.Equal(t, 1, len(mre.Unwrap()))
}
