package toml

import (
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2/internal/assert"
	"github.com/pelletier/go-toml/v2/unstable"
)

func decodeWithMeta(t *testing.T, doc string, v interface{}) *unstable.Metadata {
	t.Helper()
	m := unstable.NewMetadata()
	err := NewDecoder(strings.NewReader(doc)).WithMetadata(m).Decode(v)
	assert.NoError(t, err)
	return m
}

func seg(names ...interface{}) []unstable.MetaSegment {
	var out []unstable.MetaSegment
	for _, n := range names {
		switch v := n.(type) {
		case string:
			out = append(out, unstable.MetaSegment{Name: v, Index: -1})
		case int:
			out = append(out, unstable.MetaSegment{Index: v})
		}
	}
	return out
}

func bytesAt(doc string, r unstable.Range) string {
	return doc[r.Offset : r.Offset+r.Length]
}

func findLoc(t *testing.T, m *unstable.Metadata, path []unstable.MetaSegment) unstable.Location {
	t.Helper()
	loc, ok := m.LookupPath(path)
	if !ok {
		t.Fatalf("no location for %s; got %d locations", unstable.Location{Field: path}.FieldPath(), m.Len())
	}
	return loc
}

// TestMetadataStructPaths checks that the full Go field path, the original
// TOML key path, and the value byte range are recorded for every assigned
// field of a struct target.
func TestMetadataStructPaths(t *testing.T) {
	doc := `title = "TOML Example"
point = { x = 1, y = 2 }
dot.ty = 9

[owner]
name = "Tom"
"a.b" = 7

[database]
ports = [8001, 8002]

[database.options]
enabled = true

[[products]]
name = "Hammer"

[[products]]
name = "Nail"
`
	type Config struct {
		Title string
		Owner struct {
			Name string
			AB   int `toml:"a.b"`
		}
		Database struct {
			Ports   []int
			Options struct {
				Enabled bool
			}
		}
		Products []struct {
			Name string
		}
		Point struct {
			X int
			Y int
		}
		Dot   struct{ Ty int }
		Ghost string
	}
	var cfg Config
	m := decodeWithMeta(t, doc, &cfg)

	loc := findLoc(t, m, seg("Title"))
	assert.Equal(t, []string{"title"}, []string(loc.Key))
	assert.Equal(t, `"TOML Example"`, bytesAt(doc, loc.Range))

	loc = findLoc(t, m, seg("Owner", "Name"))
	assert.Equal(t, []string{"owner", "name"}, []string(loc.Key))
	assert.Equal(t, `"Tom"`, bytesAt(doc, loc.Range))

	// A quoted key containing a dot is a single TOML part, not two levels.
	loc = findLoc(t, m, seg("Owner", "AB"))
	assert.Equal(t, []string{"owner", "a.b"}, []string(loc.Key))
	assert.Equal(t, "7", bytesAt(doc, loc.Range))

	// Ordinary array elements are distinguished by index.
	loc = findLoc(t, m, seg("Database", "Ports", 0))
	assert.Equal(t, []string{"database", "ports"}, []string(loc.Key))
	assert.Equal(t, "8001", bytesAt(doc, loc.Range))
	loc = findLoc(t, m, seg("Database", "Ports", 1))
	assert.Equal(t, "8002", bytesAt(doc, loc.Range))

	loc = findLoc(t, m, seg("Database", "Options", "Enabled"))
	assert.Equal(t, []string{"database", "options", "enabled"}, []string(loc.Key))
	assert.Equal(t, "true", bytesAt(doc, loc.Range))

	// Array table elements are separated by index and share the same key.
	loc = findLoc(t, m, seg("Products", 0, "Name"))
	assert.Equal(t, []string{"products", "name"}, []string(loc.Key))
	assert.Equal(t, `"Hammer"`, bytesAt(doc, loc.Range))
	loc = findLoc(t, m, seg("Products", 1, "Name"))
	assert.Equal(t, []string{"products", "name"}, []string(loc.Key))
	assert.Equal(t, `"Nail"`, bytesAt(doc, loc.Range))

	// Inline tables, dotted keys and ordinary tables share the same path
	// representation.
	loc = findLoc(t, m, seg("Point", "X"))
	assert.Equal(t, []string{"point", "x"}, []string(loc.Key))
	assert.Equal(t, "1", bytesAt(doc, loc.Range))
	loc = findLoc(t, m, seg("Point", "Y"))
	assert.Equal(t, "2", bytesAt(doc, loc.Range))
	loc = findLoc(t, m, seg("Dot", "Ty"))
	assert.Equal(t, []string{"dot", "ty"}, []string(loc.Key))
	assert.Equal(t, "9", bytesAt(doc, loc.Range))

	// Fields absent from the document have no location.
	_, ok := m.LookupField("Ghost")
	assert.False(t, ok)
	// The array table slice itself has no location; only its elements.
	_, ok = m.LookupPath(seg("Products"))
	assert.False(t, ok)

	// The array table header itself locates each element.
	loc = findLoc(t, m, seg("Products", 0))
	assert.Equal(t, "products", bytesAt(doc, loc.Range))
	loc = findLoc(t, m, seg("Products", 1))
	assert.Equal(t, "products", bytesAt(doc, loc.Range))
}

// TestMetadataMapPaths checks the same document decoded into a generic map,
// and that common keys agree with the struct representation.
func TestMetadataMapPaths(t *testing.T) {
	doc := `title = "x"
ports = [1, 2]
point = { y = 3 }

[owner]
name = "Tom"

[[products]]
name = "Hammer"

[[products]]
name = "Nail"
`
	var out map[string]interface{}
	m := decodeWithMeta(t, doc, &out)

	loc, ok := m.LookupKey("title")
	assert.True(t, ok)
	assert.Equal(t, `"x"`, bytesAt(doc, loc.Range))
	assert.Equal(t, "title", loc.FieldPath())

	loc, ok = m.LookupKey("owner", "name")
	assert.True(t, ok)
	assert.Equal(t, `"Tom"`, bytesAt(doc, loc.Range))

	assert.Equal(t, "1", bytesAt(doc, findLoc(t, m, seg("ports", 0)).Range))
	assert.Equal(t, "2", bytesAt(doc, findLoc(t, m, seg("ports", 1)).Range))
	assert.Equal(t, "3", bytesAt(doc, findLoc(t, m, seg("point", "y")).Range))
	assert.Equal(t, `"Hammer"`, bytesAt(doc, findLoc(t, m, seg("products", 0, "name")).Range))
	assert.Equal(t, `"Nail"`, bytesAt(doc, findLoc(t, m, seg("products", 1, "name")).Range))
}

// TestMetadataStructMapCrossReference decodes the same document once into a
// struct and once into a map: the TOML key paths of their common keys must be
// identical and point at the same byte range.
func TestMetadataStructMapCrossReference(t *testing.T) {
	doc := `title = "v"

[server]
host = "localhost"

[[items]]
id = 1

[[items]]
id = 2
`
	type Item struct{ ID int }
	type Cfg struct {
		Title  string
		Server struct{ Host string }
		Items  []Item
	}
	var cfg Cfg
	ms := decodeWithMeta(t, doc, &cfg)
	var mp map[string]interface{}
	mm := decodeWithMeta(t, doc, &mp)

	keys := [][]string{
		{"title"},
		{"server", "host"},
		{"items", "id"},
	}
	for _, key := range keys {
		ls, ok := ms.LookupKey(key...)
		assert.True(t, ok, "struct key %v", key)
		lm, ok := mm.LookupKey(key...)
		assert.True(t, ok, "map key %v", key)
		assert.Equal(t, ls.Range, lm.Range)
		assert.Equal(t, ls.Key, unstable.KeyPath(key))
		assert.Equal(t, lm.Key, unstable.KeyPath(key))
	}

	// Array table element indexes agree as well.
	ls := findLoc(t, ms, seg("Items", 1, "ID"))
	lm := findLoc(t, mm, seg("items", 1, "id"))
	assert.Equal(t, ls.Range, lm.Range)
}

// TestMetadataEmbeddedAndPointer checks that embedded structs contribute
// every traversed Go field name, while pointers add no segment.
func TestMetadataEmbeddedAndPointer(t *testing.T) {
	type Inner struct{ B int }
	type Outer struct {
		Inner
		A *int
	}
	type Cfg struct{ O Outer }
	doc := "[o]\nb = 3\na = 5\n"
	var cfg Cfg
	m := decodeWithMeta(t, doc, &cfg)

	loc := findLoc(t, m, seg("O", "Inner", "B"))
	assert.Equal(t, []string{"o", "b"}, []string(loc.Key))
	assert.Equal(t, "3", bytesAt(doc, loc.Range))

	loc = findLoc(t, m, seg("O", "A"))
	assert.Equal(t, "5", bytesAt(doc, loc.Range))

	assert.Equal(t, "O.A", loc.FieldPath())
	assert.Equal(t, "O.Inner.B", findLoc(t, m, seg("O", "Inner", "B")).FieldPath())
}

// TestMetadataUnmarshalerRange checks the scope taken over by a custom
// Unmarshaler: one location, with several ranges for a split table.
func TestMetadataUnmarshalerRange(t *testing.T) {
	type Cfg struct {
		Plugin unstable.RawMessage
		Inline unstable.RawMessage
		Items  []unstable.RawMessage
	}
	doc := `inline = {a = 1}

[plugin]
x = 1

[plugin.sub]
y = 2

[[items]]
z = 3
`
	var cfg Cfg
	m := unstable.NewMetadata()
	err := NewDecoder(strings.NewReader(doc)).
		EnableUnmarshalerInterface().
		WithMetadata(m).
		Decode(&cfg)
	assert.NoError(t, err)

	loc := findLoc(t, m, seg("Plugin"))
	assert.Equal(t, []string{"plugin"}, []string(loc.Key))
	ranges := loc.Ranges()
	assert.True(t, len(ranges) >= 2, "want header + sub-table ranges, got %v", ranges)
	// No nested key-values are recorded individually: the Unmarshaler owns
	// the whole subtree.
	_, ok := m.LookupPath(seg("Plugin", "x"))
	assert.False(t, ok)

	loc = findLoc(t, m, seg("Inline"))
	assert.Equal(t, "{a = 1}", bytesAt(doc, loc.Range))

	loc = findLoc(t, m, seg("Items", 0))
	assert.Equal(t, "items", bytesAt(doc, loc.Range))
	assert.Equal(t, []string{"items"}, []string(loc.Key))
	// The element's own key-value is part of the scope handed to the
	// Unmarshaler, so it contributes a second range.
	assert.Equal(t, 2, len(loc.Ranges()))
	assert.Equal(t, "z = 3", bytesAt(doc, loc.Ranges()[1]))
}

// TestMetadataInlineArrayDetails checks byte ranges of nested inline
// structures, including arrays inside arrays.
func TestMetadataInlineArrayDetails(t *testing.T) {
	type Cfg struct {
		Empty []int
		Nest  []interface {
		}
	}
	doc := `empty = []
nest = [5, {b = 2}, [1]]
`
	var cfg Cfg
	m := decodeWithMeta(t, doc, &cfg)

	assert.Equal(t, "5", bytesAt(doc, findLoc(t, m, seg("Nest", 0)).Range))
	assert.Equal(t, "{b = 2}", bytesAt(doc, findLoc(t, m, seg("Nest", 1)).Range))
	assert.Equal(t, "[1]", bytesAt(doc, findLoc(t, m, seg("Nest", 2)).Range))

	// Empty array: no element locations.
	_, ok := m.LookupPath(seg("Empty", 0))
	assert.False(t, ok)
}

// TestMetadataRootUnmarshaler checks the whole-document capture of a root
// Unmarshaler: one empty-path location spanning the entire document.
func TestMetadataRootUnmarshaler(t *testing.T) {
	doc := "a = 1\n"
	var root unstable.RawMessage
	m := unstable.NewMetadata()
	err := NewDecoder(strings.NewReader(doc)).
		EnableUnmarshalerInterface().
		WithMetadata(m).
		Decode(&root)
	assert.NoError(t, err)
	assert.Equal(t, 1, m.Len())
	locs := m.Locations()
	assert.Equal(t, 0, len(locs[0].Field))
	assert.Equal(t, 0, len(locs[0].Key))
	assert.Equal(t, uint32(0), locs[0].Range.Offset)
	assert.Equal(t, uint32(len(doc)), locs[0].Range.Length)
	assert.Equal(t, 1, len(locs[0].Ranges()))
}

// TestMetadataNilAndReuse checks that nil metadata is a no-op and that the
// decoder pool does not leak metadata between decodes.
func TestMetadataNilAndReuse(t *testing.T) {
	var v map[string]interface{}
	err := NewDecoder(strings.NewReader("a = 1\n")).WithMetadata(nil).Decode(&v)
	assert.NoError(t, err)

	m := unstable.NewMetadata()
	err = NewDecoder(strings.NewReader("b = 2\n")).WithMetadata(m).Decode(&v)
	assert.NoError(t, err)
	assert.Equal(t, 1, m.Len())
	_, ok := m.LookupKey("a")
	assert.False(t, ok)
	_, ok = m.LookupKey("b")
	assert.True(t, ok)

	// Reset clears the locations.
	m.Reset()
	assert.Equal(t, 0, m.Len())
}

// TestMetadataUnknownFields checks that unknown keys produce no locations,
// even without strict mode.
func TestMetadataUnknownFields(t *testing.T) {
	type Cfg struct{ A int }
	var cfg Cfg
	m := decodeWithMeta(t, "a = 1\nb = 2\n[c]\nd = 3\n", &cfg)
	assert.Equal(t, 1, m.Len())
	_, ok := m.LookupKey("b")
	assert.False(t, ok)
	_, ok = m.LookupKey("c", "d")
	assert.False(t, ok)
}

// TestMetadataQuotedDottedKeyInMap checks a quoted key containing dots used
// as a map key: it stays a single TOML path part while its Go path is the map
// entry name.
func TestMetadataQuotedDottedKeyInMap(t *testing.T) {
	type Cfg struct {
		M map[string]int
	}
	var cfg Cfg
	doc := "m.\"a.b.c\" = 42\nm.plain = 7\n"
	m := decodeWithMeta(t, doc, &cfg)

	loc := findLoc(t, m, seg("M", "a.b.c"))
	assert.Equal(t, []string{"m", "a.b.c"}, []string(loc.Key))
	assert.Equal(t, "42", bytesAt(doc, loc.Range))

	loc = findLoc(t, m, seg("M", "plain"))
	assert.Equal(t, []string{"m", "plain"}, []string(loc.Key))
	assert.Equal(t, "7", bytesAt(doc, loc.Range))
}

// TestMetadataContainerRanges checks that brackets inside strings do not
// confuse container span detection, and that multi-line arrays are covered.
func TestMetadataContainerRanges(t *testing.T) {
	type Cfg struct {
		T []map[string]interface{}
		M map[string]interface{}
	}
	doc := "t = [{k = \"a]b\"}, {k = 'c{d'}]\n[m]\nn = [\n  1,\n  2,\n]\n"
	var cfg Cfg
	m := decodeWithMeta(t, doc, &cfg)

	assert.Equal(t, `{k = "a]b"}`, bytesAt(doc, findLoc(t, m, seg("T", 0)).Range))
	assert.Equal(t, `{k = 'c{d'}`, bytesAt(doc, findLoc(t, m, seg("T", 1)).Range))
	assert.Equal(t, `"a]b"`, bytesAt(doc, findLoc(t, m, seg("T", 0, "k")).Range))
	assert.Equal(t, `'c{d'`, bytesAt(doc, findLoc(t, m, seg("T", 1, "k")).Range))
	assert.Equal(t, "1", bytesAt(doc, findLoc(t, m, seg("M", "n", 0)).Range))
	assert.Equal(t, "2", bytesAt(doc, findLoc(t, m, seg("M", "n", 1)).Range))
}

// TestMetadataNonStringMapKey checks integer-keyed maps: the Go path uses the
// rendered key, while the TOML path keeps the original key spelling.
func TestMetadataNonStringMapKey(t *testing.T) {
	type Cfg struct {
		M map[int]string
	}
	var cfg Cfg
	doc := "[m]\n7 = \"seven\"\n"
	m := decodeWithMeta(t, doc, &cfg)

	loc := findLoc(t, m, seg("M", "7"))
	assert.Equal(t, []string{"m", "7"}, []string(loc.Key))
	assert.Equal(t, `"seven"`, bytesAt(doc, loc.Range))
}

// TestMetadataReuseAcrossDocuments checks that a Metadata value can be reset
// and reused, with the decoder pool not leaking earlier locations.
func TestMetadataReuseAcrossDocuments(t *testing.T) {
	m := unstable.NewMetadata()
	var v1 map[string]interface{}
	decodeWithMetaInto(t, m, "a = 1\n", &v1)
	assert.Equal(t, 1, m.Len())

	m.Reset()
	var v2 map[string]interface{}
	decodeWithMetaInto(t, m, "b = 2\nc = 3\n", &v2)
	assert.Equal(t, 2, m.Len())
	_, ok := m.LookupKey("a")
	assert.False(t, ok)
	_, ok = m.LookupKey("b")
	assert.True(t, ok)
}

func decodeWithMetaInto(t *testing.T, m *unstable.Metadata, doc string, v interface{}) {
	t.Helper()
	assert.NoError(t, NewDecoder(strings.NewReader(doc)).WithMetadata(m).Decode(v))
}

// TestMetadataReusedDecoder exercises a single Decoder across documents,
// which exercises the internal recorder reset path.
func TestMetadataReusedDecoder(t *testing.T) {
	m := unstable.NewMetadata()
	dec := NewDecoder(strings.NewReader("a = 1\n")).WithMetadata(m)
	var v1 map[string]interface{}
	assert.NoError(t, dec.Decode(&v1))
	assert.Equal(t, 1, m.Len())

	m.Reset()
	dec2 := NewDecoder(strings.NewReader("b = 2\n")).WithMetadata(m)
	var v2 map[string]interface{}
	assert.NoError(t, dec2.Decode(&v2))
	assert.Equal(t, 1, m.Len())
	_, ok := m.LookupKey("b")
	assert.True(t, ok)
}
