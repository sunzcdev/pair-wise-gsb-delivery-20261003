package sjson

import (
	jsongo "encoding/json"
	"strconv"
	"unsafe"
)

// segmentKind identifies how a single path segment is interpreted.
type segmentKind int

const (
	segmentKey    segmentKind = iota // literal object key
	segmentIndex                     // non-negative array index
	segmentAppend                    // append after the last array element
)

// Segment is a single, literal part of a typed Path.
//
// A Segment is created with Key, Index, or Append. Unlike the string based
// path used by Set and Delete, a Segment is always interpreted literally:
// a Key such as "0", "a.b", "#", or "" is matched against object property
// names verbatim and is never treated as an array position, a separator,
// or a query.
type Segment struct {
	kind  segmentKind
	key   string
	index int
}

// Path is a sequence of literal Segment values.
//
// Build a Path by chaining the Key, Index, and Append constructors:
//
//	path := sjson.Key("users").Key("a.b").Index(2).Append()
//
// A nil or empty Path is not valid: at least one segment is required. A
// Path is an alias for a segment slice, so an existing []Segment can be
// assembled manually and passed directly to functions such as SetPaths.
type Path []Segment

// Key returns a Segment that addresses an object property by its exact,
// literal name. The key is never parsed, so "0" stays an object property,
// "a.b" does not split into two parts, "#" is not a query, and "" names
// the empty-string property.
func Key(name string) Segment {
	return Segment{kind: segmentKey, key: name}
}

// Index returns a Segment that addresses an array element at a
// non-negative position. It always means an array position, even for a
// value such as 0 whose textual form could otherwise be mistaken for an
// object key.
//
// Index panics if i is negative. Use Append to add after the last
// element.
func Index(i int) Segment {
	if i < 0 {
		panic("sjson: array index cannot be negative")
	}
	return Segment{kind: segmentIndex, index: i}
}

// Append returns a Segment that addresses the position immediately after
// the current last array element. Setting an Append segment grows the
// array by one; deleting it removes the current last element.
func Append() Segment {
	return Segment{kind: segmentAppend}
}

// Key appends a literal object key to the path and returns the extended
// path. It is a convenience for chaining segment constructors.
func (p Path) Key(name string) Path {
	return append(p, Key(name))
}

// Index appends a non-negative array position to the path and returns the
// extended path. It panics if i is negative.
func (p Path) Index(i int) Path {
	return append(p, Index(i))
}

// Append appends an array append position to the path and returns the
// extended path.
func (p Path) Append() Path {
	return append(p, Append())
}

// description renders a segment for human readable error messages.
func (seg Segment) description() string {
	switch seg.kind {
	default:
		b, _ := jsongo.Marshal(seg.key)
		return "object key " + string(b)
	case segmentIndex:
		return "array index " + strconv.Itoa(seg.index)
	case segmentAppend:
		return "array append position"
	}
}

// SetPaths sets a json value at the typed path.
//
// It behaves like Set, except that every path part is a literal Segment
// rather than dot syntax. See Path, Key, Index, and Append for the
// container creation, array padding, and conflict rules.
func SetPaths(json string, path Path, value interface{}) (string, error) {
	return SetPathsOptions(json, path, value, nil)
}

// SetPathsOptions is SetPaths with Options.
func SetPathsOptions(json string, path Path, value interface{}, opts *Options) (string, error) {
	if opts != nil && opts.ReplaceInPlace {
		// Replacing bytes in place cannot be driven from a string. Copy
		// the Options and clear the unsafe hint, matching SetOptions.
		nopts := *opts
		opts = &nopts
		opts.ReplaceInPlace = false
	}
	jsonh := *(*stringHeader)(unsafe.Pointer(&json))
	jsonbh := sliceHeader{data: jsonh.data, len: jsonh.len, cap: jsonh.len}
	jsonb := *(*[]byte)(unsafe.Pointer(&jsonbh))
	res, err := SetBytesPathsOptions(jsonb, path, value, opts)
	return string(res), err
}

// SetBytesPaths sets a json value at the typed path.
// If working with bytes, this method is preferred over SetPaths.
func SetBytesPaths(json []byte, path Path, value interface{}) ([]byte, error) {
	return SetBytesPathsOptions(json, path, value, nil)
}

// SetBytesPathsOptions is SetBytesPaths with Options.
//
// The Optimistic and ReplaceInPlace hints are accepted for API symmetry
// but do not apply to typed paths, which never use the string based fast
// track.
func SetBytesPathsOptions(json []byte, path Path, value interface{}, opts *Options) ([]byte, error) {
	raw, stringify, merr := marshalPathValue(value)
	if merr != nil {
		return nil, merr
	}
	_, del := value.(dtype)
	jstr := *(*string)(unsafe.Pointer(&json))
	res, err := setTyped(jstr, path, raw, stringify, del)
	if err == errNoChange {
		return json, nil
	}
	return res, err
}

// SetRawPaths sets a raw json value at the typed path.
// This mirrors SetRaw for literal Segment paths.
func SetRawPaths(json string, path Path, value string) (string, error) {
	return SetRawPathsOptions(json, path, value, nil)
}

// SetRawPathsOptions is SetRawPaths with Options.
func SetRawPathsOptions(json string, path Path, value string, opts *Options) (string, error) {
	res, err := setTyped(json, path, value, false, false)
	if err == errNoChange {
		return json, nil
	}
	return string(res), err
}

// SetRawBytesPaths sets a raw json value at the typed path.
// If working with bytes, this method is preferred over SetRawPaths.
func SetRawBytesPaths(json []byte, path Path, value []byte) ([]byte, error) {
	return SetRawBytesPathsOptions(json, path, value, nil)
}

// SetRawBytesPathsOptions is SetRawBytesPaths with Options.
func SetRawBytesPathsOptions(json []byte, path Path, value []byte, opts *Options) ([]byte, error) {
	jstr := *(*string)(unsafe.Pointer(&json))
	vstr := *(*string)(unsafe.Pointer(&value))
	res, err := setTyped(jstr, path, vstr, false, false)
	if err == errNoChange {
		return json, nil
	}
	return res, err
}

// DeletePaths deletes the value at the typed path.
//
// Deleting a missing object property or an out-of-range array element is a
// no change operation that leaves the document untouched. A path whose
// segment type conflicts with an existing container, such as a Key on an
// array or an Index on an object, returns an error.
func DeletePaths(json string, path Path) (string, error) {
	return DeletePathsOptions(json, path, nil)
}

// DeletePathsOptions is DeletePaths with Options.
func DeletePathsOptions(json string, path Path, options *Options) (string, error) {
	return SetPathsOptions(json, path, dtype{}, options)
}

// DeleteBytesPaths deletes the value at the typed path.
func DeleteBytesPaths(json []byte, path Path) ([]byte, error) {
	return DeleteBytesPathsOptions(json, path, nil)
}

// DeleteBytesPathsOptions is DeleteBytesPaths with Options.
func DeleteBytesPathsOptions(json []byte, path Path, options *Options) ([]byte, error) {
	return SetBytesPathsOptions(json, path, dtype{}, options)
}
