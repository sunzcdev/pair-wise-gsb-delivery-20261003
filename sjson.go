// Package sjson provides setting json values.
package sjson

import (
	jsongo "encoding/json"
	"sort"
	"strconv"
	"unsafe"

	"github.com/tidwall/gjson"
)

type errorType struct {
	msg string
}

func (err *errorType) Error() string {
	return err.msg
}

// Options represents additional options for the Set and Delete functions.
type Options struct {
	// Optimistic is a hint that the value likely exists which
	// allows for the sjson to perform a fast-track search and replace.
	Optimistic bool
	// ReplaceInPlace is a hint to replace the input json rather than
	// allocate a new json byte slice. When this field is specified
	// the input json will not longer be valid and it should not be used
	// In the case when the destination slice doesn't have enough free
	// bytes to replace the data in place, a new bytes slice will be
	// created under the hood.
	// The Optimistic flag must be set to true and the input must be a
	// byte slice in order to use this field.
	ReplaceInPlace bool
}

type pathResult struct {
	part  string // current key part
	gpart string // gjson get part
	path  string // remaining path
	force bool   // force a string key
	more  bool   // there is more path to parse
}

func isSimpleChar(ch byte) bool {
	switch ch {
	case '|', '#', '@', '*', '?':
		return false
	default:
		return true
	}
}

func parsePath(path string) (res pathResult, simple bool) {
	var r pathResult
	if len(path) > 0 && path[0] == ':' {
		r.force = true
		path = path[1:]
	}
	for i := 0; i < len(path); i++ {
		if path[i] == '.' {
			r.part = path[:i]
			r.gpart = path[:i]
			r.path = path[i+1:]
			r.more = true
			return r, true
		}
		if !isSimpleChar(path[i]) {
			return r, false
		}
		if path[i] == '\\' {
			// go into escape mode. this is a slower path that
			// strips off the escape character from the part.
			epart := []byte(path[:i])
			gpart := []byte(path[:i+1])
			i++
			if i < len(path) {
				epart = append(epart, path[i])
				gpart = append(gpart, path[i])
				i++
				for ; i < len(path); i++ {
					if path[i] == '\\' {
						gpart = append(gpart, '\\')
						i++
						if i < len(path) {
							epart = append(epart, path[i])
							gpart = append(gpart, path[i])
						}
						continue
					} else if path[i] == '.' {
						r.part = string(epart)
						r.gpart = string(gpart)
						r.path = path[i+1:]
						r.more = true
						return r, true
					} else if !isSimpleChar(path[i]) {
						return r, false
					}
					epart = append(epart, path[i])
					gpart = append(gpart, path[i])
				}
			}
			// append the last part
			r.part = string(epart)
			r.gpart = string(gpart)
			return r, true
		}
	}
	r.part = path
	r.gpart = path
	return r, true
}

func mustMarshalString(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < ' ' || s[i] > 0x7f || s[i] == '"' || s[i] == '\\' {
			return true
		}
	}
	return false
}

// appendStringify makes a json string and appends to buf.
func appendStringify(buf []byte, s string) []byte {
	if mustMarshalString(s) {
		b, _ := jsongo.Marshal(s)
		return append(buf, b...)
	}
	buf = append(buf, '"')
	buf = append(buf, s...)
	buf = append(buf, '"')
	return buf
}

// appendBuild builds a json block from a json path.
func appendBuild(buf []byte, array bool, paths []pathResult, raw string,
	stringify bool) []byte {
	if !array {
		buf = appendStringify(buf, paths[0].part)
		buf = append(buf, ':')
	}
	if len(paths) > 1 {
		n, numeric := atoui(paths[1])
		if numeric || (!paths[1].force && paths[1].part == "-1") {
			buf = append(buf, '[')
			buf = appendRepeat(buf, "null,", n)
			buf = appendBuild(buf, true, paths[1:], raw, stringify)
			buf = append(buf, ']')
		} else {
			buf = append(buf, '{')
			buf = appendBuild(buf, false, paths[1:], raw, stringify)
			buf = append(buf, '}')
		}
	} else {
		if stringify {
			buf = appendStringify(buf, raw)
		} else {
			buf = append(buf, raw...)
		}
	}
	return buf
}

// atoui does a rip conversion of string -> unigned int.
func atoui(r pathResult) (n int, ok bool) {
	if r.force {
		return 0, false
	}
	for i := 0; i < len(r.part); i++ {
		if r.part[i] < '0' || r.part[i] > '9' {
			return 0, false
		}
		n = n*10 + int(r.part[i]-'0')
	}
	return n, true
}

// appendRepeat repeats string "n" times and appends to buf.
func appendRepeat(buf []byte, s string, n int) []byte {
	for i := 0; i < n; i++ {
		buf = append(buf, s...)
	}
	return buf
}

// trim does a rip trim
func trim(s string) string {
	for len(s) > 0 {
		if s[0] <= ' ' {
			s = s[1:]
			continue
		}
		break
	}
	for len(s) > 0 {
		if s[len(s)-1] <= ' ' {
			s = s[:len(s)-1]
			continue
		}
		break
	}
	return s
}

// deleteTailItem deletes the previous key or comma.
func deleteTailItem(buf []byte) ([]byte, bool) {
loop:
	for i := len(buf) - 1; i >= 0; i-- {
		// look for either a ',',':','['
		switch buf[i] {
		case '[':
			return buf, true
		case ',':
			return buf[:i], false
		case ':':
			// delete tail string
			i--
			for ; i >= 0; i-- {
				if buf[i] == '"' {
					i--
					for ; i >= 0; i-- {
						if buf[i] == '"' {
							i--
							if i >= 0 && buf[i] == '\\' {
								i--
								continue
							}
							for ; i >= 0; i-- {
								// look for either a ',','{'
								switch buf[i] {
								case '{':
									return buf[:i+1], true
								case ',':
									return buf[:i], false
								}
							}
						}
					}
					break
				}
			}
			break loop
		}
	}
	return buf, false
}

var errNoChange = &errorType{"no change"}

func appendRawPaths(buf []byte, jstr string, paths []pathResult, raw string,
	stringify, del bool) ([]byte, error) {
	var err error
	var res gjson.Result
	var found bool
	if del {
		if paths[0].part == "-1" && !paths[0].force {
			res = gjson.Get(jstr, "#")
			if res.Int() > 0 {
				res = gjson.Get(jstr, strconv.FormatInt(int64(res.Int()-1), 10))
				found = true
			}
		}
	}
	if !found {
		res = gjson.Get(jstr, paths[0].gpart)
	}
	if res.Index > 0 {
		if len(paths) > 1 {
			buf = append(buf, jstr[:res.Index]...)
			buf, err = appendRawPaths(buf, res.Raw, paths[1:], raw,
				stringify, del)
			if err != nil {
				return nil, err
			}
			buf = append(buf, jstr[res.Index+len(res.Raw):]...)
			return buf, nil
		}
		buf = append(buf, jstr[:res.Index]...)
		var exidx int // additional forward stripping
		if del {
			var delNextComma bool
			buf, delNextComma = deleteTailItem(buf)
			if delNextComma {
				i, j := res.Index+len(res.Raw), 0
				for ; i < len(jstr); i, j = i+1, j+1 {
					if jstr[i] <= ' ' {
						continue
					}
					if jstr[i] == ',' {
						exidx = j + 1
					}
					break
				}
			}
		} else {
			if stringify {
				buf = appendStringify(buf, raw)
			} else {
				buf = append(buf, raw...)
			}
		}
		buf = append(buf, jstr[res.Index+len(res.Raw)+exidx:]...)
		return buf, nil
	}
	if del {
		return nil, errNoChange
	}
	n, numeric := atoui(paths[0])
	isempty := true
	for i := 0; i < len(jstr); i++ {
		if jstr[i] > ' ' {
			isempty = false
			break
		}
	}
	if isempty {
		if numeric {
			jstr = "[]"
		} else {
			jstr = "{}"
		}
	}
	jsres := gjson.Parse(jstr)
	if jsres.Type != gjson.JSON {
		if numeric {
			jstr = "[]"
		} else {
			jstr = "{}"
		}
		jsres = gjson.Parse(jstr)
	}
	var comma bool
	for i := 1; i < len(jsres.Raw); i++ {
		if jsres.Raw[i] <= ' ' {
			continue
		}
		if jsres.Raw[i] == '}' || jsres.Raw[i] == ']' {
			break
		}
		comma = true
		break
	}
	switch jsres.Raw[0] {
	default:
		return nil, &errorType{"json must be an object or array"}
	case '{':
		end := len(jsres.Raw) - 1
		for ; end > 0; end-- {
			if jsres.Raw[end] == '}' {
				break
			}
		}
		buf = append(buf, jsres.Raw[:end]...)
		if comma {
			buf = append(buf, ',')
		}
		buf = appendBuild(buf, false, paths, raw, stringify)
		buf = append(buf, '}')
		return buf, nil
	case '[':
		var appendit bool
		if !numeric {
			if paths[0].part == "-1" && !paths[0].force {
				appendit = true
			} else {
				return nil, &errorType{
					"cannot set array element for non-numeric key '" +
						paths[0].part + "'"}
			}
		}
		if appendit {
			njson := trim(jsres.Raw)
			if njson[len(njson)-1] == ']' {
				njson = njson[:len(njson)-1]
			}
			buf = append(buf, njson...)
			if comma {
				buf = append(buf, ',')
			}

			buf = appendBuild(buf, true, paths, raw, stringify)
			buf = append(buf, ']')
			return buf, nil
		}
		buf = append(buf, '[')
		ress := jsres.Array()
		for i := 0; i < len(ress); i++ {
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = append(buf, ress[i].Raw...)
		}
		if len(ress) == 0 {
			buf = appendRepeat(buf, "null,", n-len(ress))
		} else {
			buf = appendRepeat(buf, ",null", n-len(ress))
			if comma {
				buf = append(buf, ',')
			}
		}
		buf = appendBuild(buf, true, paths, raw, stringify)
		buf = append(buf, ']')
		return buf, nil
	}
}

// escapeSegmentKey escapes a literal object key so that it can be used as
// a single-component gjson lookup path. Every character that is special
// to the gjson path syntax is prefixed with a backslash, which makes the
// resulting path match the key literally.
func escapeSegmentKey(key string) string {
	for i := 0; i < len(key); i++ {
		switch key[i] {
		case '.', '|', '*', '?', '\\', '@', '!', '#', '[', '{':
			esc := make([]byte, 0, len(key)+8)
			esc = append(esc, key[:i]...)
			for ; i < len(key); i++ {
				switch key[i] {
				case '.', '|', '*', '?', '\\', '@', '!', '#', '[', '{':
					esc = append(esc, '\\')
				}
				esc = append(esc, key[i])
			}
			return string(esc)
		}
	}
	return key
}

// appendSegmentPaths is the typed-segment counterpart of appendRawPaths.
// The paths are pre-encoded from Segment values: a Key segment sets the
// force flag so that atoui and the "-1" checks never treat it as an array
// position, an Index segment stores its canonical decimal form, and an
// Append segment stores "-1". This encoding allows appendBuild to create
// missing containers of the correct kind.
//
// Unlike appendRawPaths, an existing node that conflicts with the
// container kind required by the current segment produces an error
// instead of being coerced or replaced.
func appendSegmentPaths(buf []byte, jstr string, paths []pathResult,
	raw string, stringify, del bool) ([]byte, error) {
	var err error
	var res gjson.Result
	var found bool
	iskey := paths[0].force
	isappend := !iskey && paths[0].part == "-1"
	var jtype byte
	for i := 0; i < len(jstr); i++ {
		if jstr[i] > ' ' {
			jtype = jstr[i]
			break
		}
	}
	switch jtype {
	case 0:
		// no value yet, a container is created below
	case '{':
		if !iskey {
			return nil, &errorType{
				"cannot address an array element on a json object"}
		}
	case '[':
		if iskey {
			return nil, &errorType{
				"cannot address object key '" + paths[0].part +
					"' on a json array"}
		}
	default:
		return nil, &errorType{
			"cannot address a path segment on a non-container json value"}
	}
	if del && isappend {
		res = gjson.Get(jstr, "#")
		if res.Int() > 0 {
			res = gjson.Get(jstr, strconv.FormatInt(int64(res.Int()-1), 10))
			found = true
		}
	}
	if !found {
		res = gjson.Get(jstr, paths[0].gpart)
	}
	if res.Index > 0 {
		if len(paths) > 1 {
			buf = append(buf, jstr[:res.Index]...)
			buf, err = appendSegmentPaths(buf, res.Raw, paths[1:], raw,
				stringify, del)
			if err != nil {
				return nil, err
			}
			buf = append(buf, jstr[res.Index+len(res.Raw):]...)
			return buf, nil
		}
		buf = append(buf, jstr[:res.Index]...)
		var exidx int // additional forward stripping
		if del {
			var delNextComma bool
			buf, delNextComma = deleteTailItem(buf)
			if delNextComma {
				i, j := res.Index+len(res.Raw), 0
				for ; i < len(jstr); i, j = i+1, j+1 {
					if jstr[i] <= ' ' {
						continue
					}
					if jstr[i] == ',' {
						exidx = j + 1
					}
					break
				}
			}
		} else {
			if stringify {
				buf = appendStringify(buf, raw)
			} else {
				buf = append(buf, raw...)
			}
		}
		buf = append(buf, jstr[res.Index+len(res.Raw)+exidx:]...)
		return buf, nil
	}
	if del {
		return nil, errNoChange
	}
	if jtype == 0 {
		if iskey {
			jstr = "{}"
		} else {
			jstr = "[]"
		}
	}
	jsres := gjson.Parse(jstr)
	var comma bool
	for i := 1; i < len(jsres.Raw); i++ {
		if jsres.Raw[i] <= ' ' {
			continue
		}
		if jsres.Raw[i] == '}' || jsres.Raw[i] == ']' {
			break
		}
		comma = true
		break
	}
	if jsres.Raw[0] == '{' {
		end := len(jsres.Raw) - 1
		for ; end > 0; end-- {
			if jsres.Raw[end] == '}' {
				break
			}
		}
		buf = append(buf, jsres.Raw[:end]...)
		if comma {
			buf = append(buf, ',')
		}
		buf = appendBuild(buf, false, paths, raw, stringify)
		buf = append(buf, '}')
		return buf, nil
	}
	if isappend {
		njson := trim(jsres.Raw)
		if njson[len(njson)-1] == ']' {
			njson = njson[:len(njson)-1]
		}
		buf = append(buf, njson...)
		if comma {
			buf = append(buf, ',')
		}
		buf = appendBuild(buf, true, paths, raw, stringify)
		buf = append(buf, ']')
		return buf, nil
	}
	n, _ := atoui(paths[0])
	buf = append(buf, '[')
	ress := jsres.Array()
	for i := 0; i < len(ress); i++ {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, ress[i].Raw...)
	}
	if len(ress) == 0 {
		buf = appendRepeat(buf, "null,", n-len(ress))
	} else {
		buf = appendRepeat(buf, ",null", n-len(ress))
		if comma {
			buf = append(buf, ',')
		}
	}
	buf = appendBuild(buf, true, paths, raw, stringify)
	buf = append(buf, ']')
	return buf, nil
}

// setSegments sets or deletes a raw json value for a typed segment path.
func setSegments(jstr string, path []Segment, raw string,
	stringify, del bool) ([]byte, error) {
	if len(path) == 0 {
		return []byte(jstr), &errorType{"path cannot be empty"}
	}
	paths := make([]pathResult, len(path))
	for i, seg := range path {
		switch seg.kind {
		case KeySegment:
			paths[i] = pathResult{
				part:  seg.key,
				gpart: escapeSegmentKey(seg.key),
				force: true,
			}
		case IndexSegment:
			if seg.idx < 0 {
				return []byte(jstr),
					&errorType{"array index cannot be negative"}
			}
			part := strconv.Itoa(seg.idx)
			paths[i] = pathResult{part: part, gpart: part}
		case AppendSegment:
			paths[i] = pathResult{part: "-1", gpart: "-1"}
		default:
			return []byte(jstr), &errorType{"invalid path segment"}
		}
	}
	njson, err := appendSegmentPaths(nil, jstr, paths, raw, stringify, del)
	if err != nil {
		return []byte(jstr), err
	}
	return njson, nil
}

func isOptimisticPath(path string) bool {
	for i := 0; i < len(path); i++ {
		if path[i] < '.' || path[i] > 'z' {
			return false
		}
		if path[i] > '9' && path[i] < 'A' {
			return false
		}
		if path[i] > 'z' {
			return false
		}
	}
	return true
}

// Set sets a json value for the specified path.
// A path is in dot syntax, such as "name.last" or "age".
// This function expects that the json is well-formed, and does not validate.
// Invalid json will not panic, but it may return back unexpected results.
// An error is returned if the path is not valid.
//
// A path is a series of keys separated by a dot.
//
//  {
//    "name": {"first": "Tom", "last": "Anderson"},
//    "age":37,
//    "children": ["Sara","Alex","Jack"],
//    "friends": [
//      {"first": "James", "last": "Murphy"},
//      {"first": "Roger", "last": "Craig"}
//    ]
//  }
//  "name.last"          >> "Anderson"
//  "age"                >> 37
//  "children.1"         >> "Alex"
//
func Set(json, path string, value interface{}) (string, error) {
	return SetOptions(json, path, value, nil)
}

// SetBytes sets a json value for the specified path.
// If working with bytes, this method preferred over
// Set(string(data), path, value)
func SetBytes(json []byte, path string, value interface{}) ([]byte, error) {
	return SetBytesOptions(json, path, value, nil)
}

// SetRaw sets a raw json value for the specified path.
// This function works the same as Set except that the value is set as a
// raw block of json. This allows for setting premarshalled json objects.
func SetRaw(json, path, value string) (string, error) {
	return SetRawOptions(json, path, value, nil)
}

// SetRawOptions sets a raw json value for the specified path with options.
// This furnction works the same as SetOptions except that the value is set
// as a raw block of json. This allows for setting premarshalled json objects.
func SetRawOptions(json, path, value string, opts *Options) (string, error) {
	var optimistic bool
	if opts != nil {
		optimistic = opts.Optimistic
	}
	res, err := set(json, path, value, false, false, optimistic, false)
	if err == errNoChange {
		return json, nil
	}
	return string(res), err
}

// SetRawBytes sets a raw json value for the specified path.
// If working with bytes, this method preferred over
// SetRaw(string(data), path, value)
func SetRawBytes(json []byte, path string, value []byte) ([]byte, error) {
	return SetRawBytesOptions(json, path, value, nil)
}

type dtype struct{}

// Delete deletes a value from json for the specified path.
func Delete(json, path string) (string, error) {
	return Set(json, path, dtype{})
}

// DeleteOptions deletes a value from json for the specified path, with the given options.
func DeleteOptions(json, path string, options *Options) (string, error) {
	return SetOptions(json, path, dtype{}, options)
}

// DeleteBytes deletes a value from json for the specified path.
func DeleteBytes(json []byte, path string) ([]byte, error) {
	return SetBytes(json, path, dtype{})
}

// DeleteBytesOptions deletes a value from json for the specified path, with the given options.
func DeleteBytesOptions(json []byte, path string, options *Options) ([]byte, error) {
	return SetBytesOptions(json, path, dtype{}, options)
}

// SegmentKind is the kind of a typed path Segment.
type SegmentKind int

const (
	// KeySegment is a segment that addresses a literal object key.
	KeySegment SegmentKind = iota
	// IndexSegment is a segment that addresses a non-negative array index.
	IndexSegment
	// AppendSegment is a segment that appends a new element to an array.
	AppendSegment
)

// Segment is a single typed path segment used by the SetPath, SetRawPath
// and DeletePath family of functions. Create segments with the Key, Index
// and Append constructors.
//
// Unlike the dot-syntax string paths used by Set and Delete, a Segment is
// always interpreted literally and is never parsed as a path expression.
type Segment struct {
	kind SegmentKind
	key  string
	idx  int
}

// Key returns a Segment that addresses the literal object key name.
// The name is used as-is and no character has a special meaning, so keys
// such as "0", "a.b", "#", "-1" and "" are all reachable without escaping.
func Key(name string) Segment {
	return Segment{kind: KeySegment, key: name}
}

// Index returns a Segment that addresses the array element at idx.
// The idx must be non-negative; using a negative index results in an
// error when the segment is used.
func Index(idx int) Segment {
	return Segment{kind: IndexSegment, idx: idx}
}

// Append returns a Segment that appends a new element to the end of an
// array. When used with DeletePath it addresses the last array element.
func Append() Segment {
	return Segment{kind: AppendSegment}
}

// SetPath sets a json value for the specified typed path.
// A typed path is a series of Segment values created with the Key, Index
// and Append constructors, for example:
//
//	[]Segment{Key("friends"), Index(1), Key("last")}
//
// Every segment is interpreted literally. A Key segment addresses the
// exact object key it holds, an Index segment addresses a non-negative
// array element, and an Append segment appends a new element to an array.
//
// Missing parent nodes are created as needed; the kind of each new
// container is decided by the next segment, an object for a Key segment
// and an array for an Index or Append segment. If an existing node
// conflicts with the container kind required by a segment, an error is
// returned and the json is left unmodified; an existing value is never
// silently replaced by a fresh container. This also applies to
// non-container values such as strings, numbers, booleans and null.
//
// When an Index segment addresses an element past the end of an array,
// the array is extended with null elements up to and including the
// addressed index.
//
// This function expects that the json is well-formed, and does not
// validate. Invalid json will not panic, but it may return back
// unexpected results.
func SetPath(json string, path []Segment, value interface{}) (string, error) {
	jsonh := *(*stringHeader)(unsafe.Pointer(&json))
	jsonbh := sliceHeader{data: jsonh.data, len: jsonh.len, cap: jsonh.len}
	jsonb := *(*[]byte)(unsafe.Pointer(&jsonbh))
	res, err := SetPathBytes(jsonb, path, value)
	return string(res), err
}

// SetPathBytes sets a json value for the specified typed path.
// If working with bytes, this method preferred over
// SetPath(string(data), path, value)
func SetPathBytes(json []byte, path []Segment, value interface{}) ([]byte, error) {
	return setBytesValue(json, value,
		func(jstr, raw string, stringify, del bool) ([]byte, error) {
			return setSegments(jstr, path, raw, stringify, del)
		})
}

// SetRawPath sets a raw json value for the specified typed path.
// This function works the same as SetPath except that the value is set as
// a raw block of json. This allows for setting premarshalled json objects.
func SetRawPath(json string, path []Segment, value string) (string, error) {
	res, err := setSegments(json, path, value, false, false)
	return string(res), err
}

// SetRawPathBytes sets a raw json value for the specified typed path.
// If working with bytes, this method preferred over
// SetRawPath(string(data), path, value)
func SetRawPathBytes(json []byte, path []Segment, value []byte) ([]byte, error) {
	jstr := *(*string)(unsafe.Pointer(&json))
	vstr := *(*string)(unsafe.Pointer(&value))
	return setSegments(jstr, path, vstr, false, false)
}

// DeletePath deletes a value from json for the specified typed path.
// The path follows the same literal rules as SetPath.
//
// An Append segment deletes the last element of an array.
// Deleting a path that does not exist is not an error: a missing object
// key, an array index that is out of range, or an Append segment on an
// empty array all return the input json unchanged with a nil error.
// A container type conflict, on the other hand, returns an error, just
// like SetPath.
func DeletePath(json string, path []Segment) (string, error) {
	return SetPath(json, path, dtype{})
}

// DeletePathBytes deletes a value from json for the specified typed path.
// If working with bytes, this method preferred over
// DeletePath(string(data), path)
func DeletePathBytes(json []byte, path []Segment) ([]byte, error) {
	return SetPathBytes(json, path, dtype{})
}

type stringHeader struct {
	data unsafe.Pointer
	len  int
}

type sliceHeader struct {
	data unsafe.Pointer
	len  int
	cap  int
}

func set(jstr, path, raw string,
	stringify, del, optimistic, inplace bool) ([]byte, error) {
	if path == "" {
		return []byte(jstr), &errorType{"path cannot be empty"}
	}
	if !del && optimistic && isOptimisticPath(path) {
		res := gjson.Get(jstr, path)
		if res.Exists() && res.Index > 0 {
			sz := len(jstr) - len(res.Raw) + len(raw)
			if stringify {
				sz += 2
			}
			if inplace && sz <= len(jstr) {
				if !stringify || !mustMarshalString(raw) {
					jsonh := *(*stringHeader)(unsafe.Pointer(&jstr))
					jsonbh := sliceHeader{
						data: jsonh.data, len: jsonh.len, cap: jsonh.len}
					jbytes := *(*[]byte)(unsafe.Pointer(&jsonbh))
					if stringify {
						jbytes[res.Index] = '"'
						copy(jbytes[res.Index+1:], []byte(raw))
						jbytes[res.Index+1+len(raw)] = '"'
						copy(jbytes[res.Index+1+len(raw)+1:],
							jbytes[res.Index+len(res.Raw):])
					} else {
						copy(jbytes[res.Index:], []byte(raw))
						copy(jbytes[res.Index+len(raw):],
							jbytes[res.Index+len(res.Raw):])
					}
					return jbytes[:sz], nil
				}
				return []byte(jstr), nil
			}
			buf := make([]byte, 0, sz)
			buf = append(buf, jstr[:res.Index]...)
			if stringify {
				buf = appendStringify(buf, raw)
			} else {
				buf = append(buf, raw...)
			}
			buf = append(buf, jstr[res.Index+len(res.Raw):]...)
			return buf, nil
		}
	}
	var paths []pathResult
	r, simple := parsePath(path)
	if simple {
		paths = append(paths, r)
		for r.more {
			r, simple = parsePath(r.path)
			if !simple {
				break
			}
			paths = append(paths, r)
		}
	}
	if !simple {
		if del {
			return []byte(jstr),
				&errorType{"cannot delete value from a complex path"}
		}
		return setComplexPath(jstr, path, raw, stringify)
	}
	njson, err := appendRawPaths(nil, jstr, paths, raw, stringify, del)
	if err != nil {
		return []byte(jstr), err
	}
	return njson, nil
}

func setComplexPath(jstr, path, raw string, stringify bool) ([]byte, error) {
	res := gjson.Get(jstr, path)
	if !res.Exists() || !(res.Index != 0 || len(res.Indexes) != 0) {
		return []byte(jstr), errNoChange
	}
	if res.Index != 0 {
		njson := []byte(jstr[:res.Index])
		if stringify {
			njson = appendStringify(njson, raw)
		} else {
			njson = append(njson, raw...)
		}
		njson = append(njson, jstr[res.Index+len(res.Raw):]...)
		jstr = string(njson)
	}
	if len(res.Indexes) > 0 {
		type val struct {
			index int
			res   gjson.Result
		}
		vals := make([]val, 0, len(res.Indexes))
		res.ForEach(func(_, vres gjson.Result) bool {
			vals = append(vals, val{res: vres})
			return true
		})
		if len(res.Indexes) != len(vals) {
			return []byte(jstr), errNoChange
		}
		for i := 0; i < len(res.Indexes); i++ {
			vals[i].index = res.Indexes[i]
		}
		sort.SliceStable(vals, func(i, j int) bool {
			return vals[i].index > vals[j].index
		})
		for _, val := range vals {
			vres := val.res
			index := val.index
			njson := []byte(jstr[:index])
			if stringify {
				njson = appendStringify(njson, raw)
			} else {
				njson = append(njson, raw...)
			}
			njson = append(njson, jstr[index+len(vres.Raw):]...)
			jstr = string(njson)
		}
	}
	return []byte(jstr), nil
}

// SetOptions sets a json value for the specified path with options.
// A path is in dot syntax, such as "name.last" or "age".
// This function expects that the json is well-formed, and does not validate.
// Invalid json will not panic, but it may return back unexpected results.
// An error is returned if the path is not valid.
func SetOptions(json, path string, value interface{},
	opts *Options) (string, error) {
	if opts != nil {
		if opts.ReplaceInPlace {
			// it's not safe to replace bytes in-place for strings
			// copy the Options and set options.ReplaceInPlace to false.
			nopts := *opts
			opts = &nopts
			opts.ReplaceInPlace = false
		}
	}
	jsonh := *(*stringHeader)(unsafe.Pointer(&json))
	jsonbh := sliceHeader{data: jsonh.data, len: jsonh.len, cap: jsonh.len}
	jsonb := *(*[]byte)(unsafe.Pointer(&jsonbh))
	res, err := SetBytesOptions(jsonb, path, value, opts)
	return string(res), err
}

// SetBytesOptions sets a json value for the specified path with options.
// If working with bytes, this method preferred over
// SetOptions(string(data), path, value)
func SetBytesOptions(json []byte, path string, value interface{},
	opts *Options) ([]byte, error) {
	var optimistic, inplace bool
	if opts != nil {
		optimistic = opts.Optimistic
		inplace = opts.ReplaceInPlace
	}
	return setBytesValue(json, value,
		func(jstr, raw string, stringify, del bool) ([]byte, error) {
			return set(jstr, path, raw, stringify, del, optimistic, inplace)
		})
}

// setBytesValue marshals value and calls setfn to perform the actual
// json modification. It is shared by the string path and typed segment
// path entry points.
func setBytesValue(json []byte, value interface{},
	setfn func(jstr, raw string, stringify, del bool) ([]byte, error),
) ([]byte, error) {
	jstr := *(*string)(unsafe.Pointer(&json))
	var res []byte
	var err error
	switch v := value.(type) {
	default:
		b, merr := jsongo.Marshal(value)
		if merr != nil {
			return nil, merr
		}
		raw := *(*string)(unsafe.Pointer(&b))
		res, err = setfn(jstr, raw, false, false)
	case dtype:
		res, err = setfn(jstr, "", false, true)
	case string:
		res, err = setfn(jstr, v, true, false)
	case []byte:
		raw := *(*string)(unsafe.Pointer(&v))
		res, err = setfn(jstr, raw, true, false)
	case bool:
		if v {
			res, err = setfn(jstr, "true", false, false)
		} else {
			res, err = setfn(jstr, "false", false, false)
		}
	case int8:
		res, err = setfn(jstr, strconv.FormatInt(int64(v), 10), false, false)
	case int16:
		res, err = setfn(jstr, strconv.FormatInt(int64(v), 10), false, false)
	case int32:
		res, err = setfn(jstr, strconv.FormatInt(int64(v), 10), false, false)
	case int64:
		res, err = setfn(jstr, strconv.FormatInt(int64(v), 10), false, false)
	case uint8:
		res, err = setfn(jstr, strconv.FormatUint(uint64(v), 10), false, false)
	case uint16:
		res, err = setfn(jstr, strconv.FormatUint(uint64(v), 10), false, false)
	case uint32:
		res, err = setfn(jstr, strconv.FormatUint(uint64(v), 10), false, false)
	case uint64:
		res, err = setfn(jstr, strconv.FormatUint(uint64(v), 10), false, false)
	case float32:
		res, err = setfn(jstr, strconv.FormatFloat(float64(v), 'f', -1, 64),
			false, false)
	case float64:
		res, err = setfn(jstr, strconv.FormatFloat(float64(v), 'f', -1, 64),
			false, false)
	}
	if err == errNoChange {
		return json, nil
	}
	return res, err
}

// SetRawBytesOptions sets a raw json value for the specified path with options.
// If working with bytes, this method preferred over
// SetRawOptions(string(data), path, value, opts)
func SetRawBytesOptions(json []byte, path string, value []byte,
	opts *Options) ([]byte, error) {
	jstr := *(*string)(unsafe.Pointer(&json))
	vstr := *(*string)(unsafe.Pointer(&value))
	var optimistic, inplace bool
	if opts != nil {
		optimistic = opts.Optimistic
		inplace = opts.ReplaceInPlace
	}
	res, err := set(jstr, path, vstr, false, false, optimistic, inplace)
	if err == errNoChange {
		return json, nil
	}
	return res, err
}
