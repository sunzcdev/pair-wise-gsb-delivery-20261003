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

// appendDeleteRange removes the value occupying jstr[start:end]. Any comma
// or object key that belongs to the value is stripped as well, while leading
// and trailing whitespace that belongs to the surrounding document is left
// untouched. buf is expected to already contain jstr[:start].
//
// The returned exidx value is the number of bytes following end (a leading
// comma and any preceding whitespace) that must also be skipped.
func appendDeleteRange(buf []byte, jstr string, start, end int) ([]byte, int) {
	var delNextComma bool
	buf, delNextComma = deleteTailItem(buf)
	if !delNextComma {
		return buf, 0
	}
	var exidx int
	i, j := end, 0
	for ; i < len(jstr); i, j = i+1, j+1 {
		if jstr[i] <= ' ' {
			continue
		}
		if jstr[i] == ',' {
			exidx = j + 1
		}
		break
	}
	return buf, exidx
}

// deleteHit describes a single value matched by one of the delete paths.
type deleteHit struct {
	start int  // index of the raw value in the original json
	end   int  // end index, exclusive
	root  bool // the entire document is being deleted
}

// gjsonPath joins simple path parts into a path suitable for gjson.Get.
func gjsonPath(parts []pathResult) string {
	var b []byte
	for i, part := range parts {
		if i > 0 {
			b = append(b, '.')
		}
		b = append(b, part.gpart...)
	}
	return string(b)
}

// skipLeadingWhitespace returns the index of the first non-whitespace byte.
func skipLeadingWhitespace(s string) int {
	i := 0
	for i < len(s) && s[i] <= ' ' {
		i++
	}
	return i
}

// rootHit reports whether res represents the entire json document. gjson uses
// a zero index for both "unknown" and root values, and points at the first
// non-whitespace byte otherwise, so the raw value is compared against the
// document body.
func rootHit(jstr string, res gjson.Result) bool {
	if !res.Exists() || len(res.Indexes) != 0 || len(res.Raw) == 0 {
		return false
	}
	start := skipLeadingWhitespace(jstr)
	return res.Index == start && start+len(res.Raw) <= len(jstr) &&
		jstr[start:start+len(res.Raw)] == res.Raw
}

// appendResultHits appends every value represented by res. Query results
// (res.Indexes) are expanded into their individual values.
func appendResultHits(hits []deleteHit, res gjson.Result) []deleteHit {
	if len(res.Indexes) > 0 {
		matched := make([]gjson.Result, 0, len(res.Indexes))
		res.ForEach(func(_, vres gjson.Result) bool {
			matched = append(matched, vres)
			return true
		})
		if len(matched) != len(res.Indexes) {
			return hits
		}
		for _, vres := range matched {
			if vres.Index > 0 {
				hits = append(hits, deleteHit{
					vres.Index, vres.Index + len(vres.Raw), false,
				})
			}
		}
		return hits
	}
	if res.Index > 0 {
		hits = append(hits, deleteHit{
			res.Index, res.Index + len(res.Raw), false,
		})
	}
	return hits
}

// collectDeleteHit evaluates path against the original jstr and appends any
// matched values to hits. Every path is evaluated against the same original
// json, so array indices always refer to the positions before any deletion.
func collectDeleteHit(hits []deleteHit, jstr, path string) ([]deleteHit, error) {
	if path == "" {
		return hits, &errorType{"path cannot be empty"}
	}

	var minusOne bool
	var parts []pathResult
	r, simple := parsePath(path)
	if simple {
		parts = append(parts, r)
		for r.more {
			r, simple = parsePath(r.path)
			if !simple {
				break
			}
			parts = append(parts, r)
		}
		if simple {
			last := parts[len(parts)-1]
			if !last.force && last.part == "-1" && !last.more {
				minusOne = true
				parts = parts[:len(parts)-1]
			}
		}
	}

	var res gjson.Result
	if !simple {
		// complex path, such as a gjson query
		res = gjson.Get(jstr, path)
		if !res.Exists() {
			return hits, nil
		}
		if rootHit(jstr, res) {
			return append(hits, deleteHit{root: true}), nil
		}
		return appendResultHits(hits, res), nil
	}

	parent := gjson.Parse(jstr)
	if len(parts) > 0 {
		parent = gjson.Get(jstr, gjsonPath(parts))
	}
	if !parent.Exists() {
		return hits, nil
	}
	if minusOne {
		if !parent.IsArray() {
			return hits, nil
		}
		items := parent.Array()
		if len(items) == 0 || items[len(items)-1].Index == 0 {
			return hits, nil
		}
		return appendResultHits(hits, items[len(items)-1]), nil
	}
	if len(parts) == 0 {
		return append(hits, deleteHit{root: true}), nil
	}
	if rootHit(jstr, parent) {
		return append(hits, deleteHit{root: true}), nil
	}
	return appendResultHits(hits, parent), nil
}

// mergeDeleteHits drops duplicate hits and hits that are already covered by an
// ancestor hit, then sorts the remaining hits by start position.
func mergeDeleteHits(hits []deleteHit) []deleteHit {
	if len(hits) < 2 {
		return hits
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].start != hits[j].start {
			return hits[i].start < hits[j].start
		}
		return hits[i].end < hits[j].end
	})
	merged := hits[:0]
	for _, hit := range hits {
		if n := len(merged); n > 0 {
			prev := merged[n-1]
			if hit.start == prev.start && hit.end == prev.end {
				continue // duplicate hit
			}
			if hit.start >= prev.start && hit.end <= prev.end {
				continue // already covered by a parent hit
			}
		}
		merged = append(merged, hit)
	}
	return merged
}

// deleteMany evaluates every path against the original json and then removes
// all matched values in a single pass. The result never depends on the order
// of the supplied paths.
func deleteMany(jstr string, paths []string) ([]byte, error) {
	var hits []deleteHit
	for _, path := range paths {
		var err error
		hits, err = collectDeleteHit(hits, jstr, path)
		if err != nil {
			return []byte(jstr), err
		}
	}
	for _, hit := range hits {
		if hit.root {
			return []byte{}, nil
		}
	}
	hits = mergeDeleteHits(hits)
	if len(hits) == 0 {
		return []byte(jstr), nil
	}

	// Apply the hits right-to-left, exactly like a sequence of single Deletes
	// performed in descending order. Earlier (lower-offset) hits are untouched
	// when later ones are removed, so the original indices stay valid and all
	// comma/key/whitespace handling is resolved against the document as it
	// exists at that moment.
	buf := []byte(jstr)
	for i := len(hits) - 1; i >= 0; i-- {
		hit := hits[i]
		head := append([]byte(nil), buf[:hit.start]...)
		head, exidx := appendDeleteRange(head, string(buf), hit.start, hit.end)
		buf = append(head, buf[hit.end+exidx:]...)
	}
	return buf, nil
}

// DeleteMany deletes all of the values matched by paths from json.
//
// Every path is first evaluated against the original, unmodified json, and
// only afterwards are the matches removed. This means that array indices
// always refer to their original position: deleting "items.0" and "items.2"
// removes the first and third items as they were before any deletion, and
// the result is the same regardless of the order in which the paths are
// given.
//
// Paths may be ordinary dot paths, "-1" for the last array element, or gjson
// queries that match multiple values, such as "friends.#(age>30)#.name".
// Duplicate matches are removed only once, a match nested inside an already
// deleted parent is not handled separately, and deleting the root document
// (for example with the path "@this") returns an empty string.
//
// Whitespace and formatting of the untouched parts of the document are
// preserved.
func DeleteMany(json string, paths ...string) (string, error) {
	res, err := deleteMany(json, paths)
	return string(res), err
}

// DeleteManyBytes deletes all of the values matched by paths from json.
// If working with bytes, this method preferred over
// DeleteMany(string(data), paths...).
func DeleteManyBytes(json []byte, paths ...string) ([]byte, error) {
	jstr := *(*string)(unsafe.Pointer(&json))
	res, err := deleteMany(jstr, paths)
	if err != nil {
		return json, err
	}
	return res, nil
}

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
			buf, exidx = appendDeleteRange(buf, jstr, res.Index,
				res.Index+len(res.Raw))
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
		res, err = set(jstr, path, raw, false, false, optimistic, inplace)
	case dtype:
		res, err = set(jstr, path, "", false, true, optimistic, inplace)
	case string:
		res, err = set(jstr, path, v, true, false, optimistic, inplace)
	case []byte:
		raw := *(*string)(unsafe.Pointer(&v))
		res, err = set(jstr, path, raw, true, false, optimistic, inplace)
	case bool:
		if v {
			res, err = set(jstr, path, "true", false, false, optimistic, inplace)
		} else {
			res, err = set(jstr, path, "false", false, false, optimistic, inplace)
		}
	case int8:
		res, err = set(jstr, path, strconv.FormatInt(int64(v), 10),
			false, false, optimistic, inplace)
	case int16:
		res, err = set(jstr, path, strconv.FormatInt(int64(v), 10),
			false, false, optimistic, inplace)
	case int32:
		res, err = set(jstr, path, strconv.FormatInt(int64(v), 10),
			false, false, optimistic, inplace)
	case int64:
		res, err = set(jstr, path, strconv.FormatInt(int64(v), 10),
			false, false, optimistic, inplace)
	case uint8:
		res, err = set(jstr, path, strconv.FormatUint(uint64(v), 10),
			false, false, optimistic, inplace)
	case uint16:
		res, err = set(jstr, path, strconv.FormatUint(uint64(v), 10),
			false, false, optimistic, inplace)
	case uint32:
		res, err = set(jstr, path, strconv.FormatUint(uint64(v), 10),
			false, false, optimistic, inplace)
	case uint64:
		res, err = set(jstr, path, strconv.FormatUint(uint64(v), 10),
			false, false, optimistic, inplace)
	case float32:
		res, err = set(jstr, path, strconv.FormatFloat(float64(v), 'f', -1, 64),
			false, false, optimistic, inplace)
	case float64:
		res, err = set(jstr, path, strconv.FormatFloat(float64(v), 'f', -1, 64),
			false, false, optimistic, inplace)
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
