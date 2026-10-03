package unstable

import "strings"

// MetaSegment is one element of the path describing where a decoded value
// lives. A segment is either a named field or map key, or the index of an
// array element.
//
// TOML array tables become index segments: the nth [[header]] of a document
// is segment Index(n), so the elements of an array table can be told apart.
// Ordinary (inline) arrays use index segments as well.
type MetaSegment struct {
	// Name is the name of a struct field (the Go field name, not the TOML
	// tag) or of a map key (the TOML key as decoded into the Go key). It is
	// empty for index segments.
	Name string
	// Index is the zero-based position of an array element. It is -1 for
	// named segments.
	Index int
}

// IsIndex reports whether the segment is an array index.
func (s MetaSegment) IsIndex() bool { return s.Index >= 0 }

// String returns the Go-style representation of the segment: the name for a
// named segment, or [n] for an index segment.
func (s MetaSegment) String() string {
	if s.Index >= 0 {
		return "[" + itoa(s.Index) + "]"
	}
	return s.Name
}

// KeyPath is a sequence of named key parts. Unlike a flat dotted string, a
// quoted key containing dots ("a.b") stays a single element, so dotted keys
// and bare keys have an unambiguous representation.
type KeyPath []string

// Join returns the parts joined with '.', without quoting. Prefer walking the
// slice directly when keys may contain dots.
func (p KeyPath) Join() string {
	return strings.Join(p, ".")
}

// Location records where the value assigned to one Go field (or map entry)
// came from in the TOML document.
//
// A single location corresponds to a single Go destination: a field that is
// populated from several places of the document (split tables, or a custom
// Unmarshaler fed multiple sections) produces one Location whose Extra ranges
// cover the additional source bytes.
type Location struct {
	// Field is the full path from the root of the decoded value to the
	// assigned Go destination, as struct field names (not TOML tags) and
	// array indexes. Embedded structs contribute the name of each traversed
	// field.
	//
	// For a map destination, named segments carry the map key as TOML
	// decoded it.
	Field []MetaSegment
	// Key is the original TOML key path of the value, one element per key
	// part. A quoted key containing dots is a single element.
	Key KeyPath
	// Range is the primary byte range of the source, as offsets into the
	// input document. For a key-value it spans the value; for a table header
	// the header key; for a custom Unmarshaler the first chunk of raw TOML
	// handed to UnmarshalTOML.
	Range Range
	// Extra holds the additional byte ranges that also fed the destination
	// (split tables, multiple sections delivered to one Unmarshaler). It is
	// nil when the value has a single contiguous source.
	Extra []Range
}

// Ranges returns all the source ranges of the location, starting with Range.
func (l Location) Ranges() []Range {
	if len(l.Extra) == 0 {
		return []Range{l.Range}
	}
	out := make([]Range, 0, 1+len(l.Extra))
	out = append(out, l.Range)
	out = append(out, l.Extra...)
	return out
}

// FieldPath returns Field formatted like a Go expression: dotted names with
// index segments rendered as [n].
func (l Location) FieldPath() string {
	var b strings.Builder
	for i, s := range l.Field {
		if s.Index >= 0 {
			b.WriteByte('[')
			b.WriteString(itoa(s.Index))
			b.WriteByte(']')
			continue
		}
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(s.Name)
	}
	return b.String()
}

// Metadata holds the locations collected while decoding a document. Enable it
// with Decoder.WithMetadata.
type Metadata struct {
	locations []Location
}

// NewMetadata returns an empty Metadata ready to pass to WithMetadata.
func NewMetadata() *Metadata {
	return &Metadata{}
}

// Reset removes all recorded locations, so the value can be reused.
func (m *Metadata) Reset() {
	if m != nil {
		m.locations = m.locations[:0]
	}
}

// Locations returns a copy of all the recorded locations.
func (m *Metadata) Locations() []Location {
	if m == nil {
		return nil
	}
	out := make([]Location, len(m.locations))
	copy(out, m.locations)
	return out
}

// LookupField returns the location of the Go destination described by names
// (struct field names, one per named segment), or false. Array indexes cannot
// be expressed this way; use LookupPath for destinations inside arrays.
func (m *Metadata) LookupField(names ...string) (Location, bool) {
	for _, l := range m.locations {
		if namedPathEqual(l.Field, names) {
			return l, true
		}
	}
	return Location{}, false
}

// LookupPath returns the location of the Go destination described by path, or
// false.
func (m *Metadata) LookupPath(path []MetaSegment) (Location, bool) {
	for _, l := range m.locations {
		if segmentsEqual(l.Field, path) {
			return l, true
		}
	}
	return Location{}, false
}

// LookupKey returns the location assigned from the TOML key path, or false.
// A dotted key must be passed as separate elements; a quoted key containing
// dots is a single element.
func (m *Metadata) LookupKey(key ...string) (Location, bool) {
	for _, l := range m.locations {
		if stringsEqual(l.Key, key) {
			return l, true
		}
	}
	return Location{}, false
}

// Add records a location. It is intended for use by the decoder; calling it
// directly allows augmenting a Metadata with locations from other sources.
func (m *Metadata) Add(l Location) {
	if m == nil {
		return
	}
	m.locations = append(m.locations, l)
}

// Len returns the number of recorded locations.
func (m *Metadata) Len() int {
	if m == nil {
		return 0
	}
	return len(m.locations)
}

func segmentsEqual(a, b []MetaSegment) bool {
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

func namedPathEqual(segments []MetaSegment, names []string) bool {
	j := 0
	for _, s := range segments {
		if s.Index >= 0 {
			return false
		}
		if j >= len(names) || s.Name != names[j] {
			return false
		}
		j++
	}
	return j == len(names)
}

func stringsEqual(a, b []string) bool {
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

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
