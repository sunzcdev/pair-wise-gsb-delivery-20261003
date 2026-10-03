package toml

import (
	"sort"
	"strconv"
	"strings"
)

// PathSegment identifies one step in the path to a decoded value.
type PathSegment struct {
	// Key is the name of the struct field or map key this segment refers
	// to.
	Key string
	// Index is the index of the element within an array of tables, or -1
	// when the segment does not select an array element.
	Index int
}

// Path is the full path of a decoded value, from the root of the target.
// Each segment is a struct field name or a map key; elements of arrays of
// tables are distinguished by their index.
type Path []PathSegment

// String returns a human-readable representation of the path, with key
// segments joined by dots and array-of-tables indexes in brackets (e.g.
// "servers[1].addr"). Keys containing dots are quoted.
func (p Path) String() string {
	var b strings.Builder
	for i, s := range p {
		if i > 0 {
			b.WriteByte('.')
		}
		if strings.ContainsAny(s.Key, ". ") {
			b.WriteString(strconv.Quote(s.Key))
		} else {
			b.WriteString(s.Key)
		}
		if s.Index >= 0 {
			b.WriteByte('[')
			b.WriteString(strconv.Itoa(s.Index))
			b.WriteByte(']')
		}
	}
	return b.String()
}

// ByteRange describes a range of bytes in the source document.
type ByteRange struct {
	// Start is the offset of the first byte of the range.
	Start int
	// End is the offset one past the last byte of the range.
	End int
}

// Origin describes where a value decoded from a TOML document came from.
type Origin struct {
	// Path is the full path of the assigned Go field, from the root of the
	// decoded value. Array-of-tables elements are distinguished by their
	// index.
	Path Path
	// Key is the original TOML key path of the value, as written in the
	// document. Quoted keys, including keys containing dots, are preserved
	// as single segments.
	Key Key
	// Range is the byte range of the value in the source document.
	Range ByteRange
	// Unmarshaled reports whether the value was decoded by a custom
	// Unmarshaler (UnmarshalTOML) implementation. In that case, Range
	// covers the whole region of the document the Unmarshaler received.
	Unmarshaled bool
}

// MetaData records where the values decoded from a TOML document came
// from. It is populated by Decoder when enabled with RecordOrigins, and by
// UnmarshalWithMeta.
//
// Only values actually assigned during decoding are recorded: fields that
// do not appear in the document produce no origin. Tables, dotted keys and
// inline tables share the same path representation, so the origin of a
// value does not depend on how its table was expressed. Decoding the same
// document into a struct and into a map yields matching Keys and Ranges
// for the common keys.
type MetaData struct {
	origins []Origin
}

// Origins returns the recorded origins, in document order.
func (m *MetaData) Origins() []Origin {
	return m.origins
}

// Lookup returns the origin of the value at the given TOML key path, or
// false if no value was assigned at that path. For array tables, Lookup
// returns the origin of the first element; use LookupAll to retrieve all
// of them.
func (m *MetaData) Lookup(key ...string) (Origin, bool) {
	for _, o := range m.origins {
		if keyEqual(o.Key, key) {
			return o, true
		}
	}
	return Origin{}, false
}

// LookupAll returns the origins of all the values assigned at the given
// TOML key path. It is useful for array tables, where several elements
// share the same key path.
func (m *MetaData) LookupAll(key ...string) []Origin {
	var out []Origin
	for _, o := range m.origins {
		if keyEqual(o.Key, key) {
			out = append(out, o)
		}
	}
	return out
}

// LookupField returns the origin of the value assigned to the Go field at
// the given path (struct field names or map keys), or false if no value
// was assigned at that path. Array-of-tables indexes are ignored: for
// array tables, LookupField returns the origin of the first element; use
// LookupFieldAll to retrieve all of them.
func (m *MetaData) LookupField(field ...string) (Origin, bool) {
	for _, o := range m.origins {
		if pathEqual(o.Path, field) {
			return o, true
		}
	}
	return Origin{}, false
}

// LookupFieldAll returns the origins of all the values assigned to the Go
// field at the given path (struct field names or map keys), ignoring
// array-of-tables indexes.
func (m *MetaData) LookupFieldAll(field ...string) []Origin {
	var out []Origin
	for _, o := range m.origins {
		if pathEqual(o.Path, field) {
			out = append(out, o)
		}
	}
	return out
}

func pathEqual(a Path, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Key != b[i] {
			return false
		}
	}
	return true
}

func keyEqual(a Key, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sort orders the recorded origins by their start offset in the document.
func (m *MetaData) sort() {
	less := func(i, j int) bool { return m.origins[i].Range.Start < m.origins[j].Range.Start }
	sort.SliceStable(m.origins, less)
}

// record appends an origin to the metadata. The path and key are copied to
// a private buffer, so the caller may reuse its own buffers.
func (m *MetaData) record(path []PathSegment, key Key, rng ByteRange, unmarshaled bool) {
	p := make([]PathSegment, len(path))
	copy(p, path)
	k := make(Key, len(key))
	copy(k, key)
	m.origins = append(m.origins, Origin{
		Path:        p,
		Key:         k,
		Range:       rng,
		Unmarshaled: unmarshaled,
	})
}
