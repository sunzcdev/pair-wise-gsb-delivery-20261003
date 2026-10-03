package sjson

import (
	jsongo "encoding/json"
	"strconv"

	"github.com/tidwall/gjson"
)

// marshalPathValue converts a Set value into its raw json form, mirroring
// the type switch used by SetBytesOptions. The stringify return value
// reports whether raw must be encoded as a json string. A dtype value
// signals a delete.
func marshalPathValue(value interface{}) (raw string, stringify bool, err error) {
	switch v := value.(type) {
	default:
		b, merr := jsongo.Marshal(value)
		if merr != nil {
			return "", false, merr
		}
		return string(b), false, nil
	case dtype:
		return "", false, nil
	case string:
		return v, true, nil
	case []byte:
		return string(v), true, nil
	case bool:
		if v {
			return "true", false, nil
		}
		return "false", false, nil
	case int8:
		return strconv.FormatInt(int64(v), 10), false, nil
	case int16:
		return strconv.FormatInt(int64(v), 10), false, nil
	case int32:
		return strconv.FormatInt(int64(v), 10), false, nil
	case int64:
		return strconv.FormatInt(int64(v), 10), false, nil
	case uint8:
		return strconv.FormatUint(uint64(v), 10), false, nil
	case uint16:
		return strconv.FormatUint(uint64(v), 10), false, nil
	case uint32:
		return strconv.FormatUint(uint64(v), 10), false, nil
	case uint64:
		return strconv.FormatUint(uint64(v), 10), false, nil
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 64), false, nil
	case float64:
		return strconv.FormatFloat(float64(v), 'f', -1, 64), false, nil
	}
}

// setTyped applies a set or delete along a literal Segment path.
//
// Missing parent containers are created from the type of the next segment
// (a Key makes an object, an Index or Append makes an array). An existing
// container whose type conflicts with the next segment produces an error
// instead of replacing the old subtree.
func setTyped(jstr string, path Path, raw string, stringify, del bool) ([]byte, error) {
	if len(path) == 0 {
		return []byte(jstr), &errorType{"path cannot be empty"}
	}

	// Locate the document, keeping any leading and trailing whitespace so
	// it can be reproduced verbatim.
	start := 0
	for start < len(jstr) && jstr[start] <= ' ' {
		start++
	}
	trimmed := trim(jstr[start:])
	if len(trimmed) == 0 {
		if del {
			return nil, errNoChange
		}
		buf := make([]byte, 0, len(jstr)+len(raw)+16)
		buf = append(buf, jstr[:start]...)
		buf = emitChain(buf, path[0], path[1:], raw, stringify)
		buf = append(buf, jstr[start+len(trimmed):]...)
		return buf, nil
	}

	res := gjson.Parse(trimmed)
	if res.Type != gjson.JSON {
		// A scalar (or invalid) document is never a container, so the
		// segment cannot resolve into it and the value is never replaced.
		return nil, &errorType{
			"cannot apply " + path[0].description() +
				" to a non-container json value"}
	}

	buf := make([]byte, 0, len(jstr)+len(raw)+16)
	buf = append(buf, jstr[:start]...)
	var err error
	buf, err = applySegment(buf, res, path, raw, stringify, del)
	if err != nil {
		return nil, err
	}
	buf = append(buf, jstr[start+len(trimmed):]...)
	return buf, nil
}

// applySegment applies the first segment against an existing container.
func applySegment(buf []byte, container gjson.Result, path Path,
	raw string, stringify, del bool) ([]byte, error) {
	seg := path[0]

	switch {
	case seg.kind == segmentKey && !container.IsObject():
		return nil, &errorType{
			"cannot set object key " + quoteKey(seg.key) +
				" on a json array"}
	case (seg.kind == segmentIndex || seg.kind == segmentAppend) &&
		!container.IsArray():
		return nil, &errorType{
			"cannot apply " + seg.description() + " to a json object"}
	}

	// A set through an Append segment always inserts after the current
	// last element; it never replaces it. Deleting an Append segment is
	// handled below by targeting the last element.
	if seg.kind == segmentAppend && !del {
		return insertMissing(buf, container, path, raw, stringify)
	}

	child, found := lookupSegment(container, seg)
	if !found {
		if del {
			return nil, errNoChange
		}
		return insertMissing(buf, container, path, raw, stringify)
	}

	if len(path) > 1 {
		if child.Type != gjson.JSON {
			return nil, &errorType{
				"cannot descend through non-container value at " +
					seg.description()}
		}
		idx := child.Index
		buf = append(buf, container.Raw[:idx]...)
		// Reset the coordinate system before recursing so nested lookups
		// are relative to child.Raw, matching the string path machinery
		// which re-runs gjson.Get on the child document.
		child.Index = 0
		child.Indexes = nil
		var err error
		buf, err = applySegment(buf, child, path[1:], raw, stringify, del)
		if err != nil {
			return nil, err
		}
		buf = append(buf, container.Raw[idx+len(child.Raw):]...)
		return buf, nil
	}

	// Terminal segment.
	buf = append(buf, container.Raw[:child.Index]...)
	if del {
		var delNextComma bool
		buf, delNextComma = deleteTailItem(buf)
		exidx := 0
		if delNextComma {
			tail := container.Raw[child.Index+len(child.Raw):]
			for j := 0; j < len(tail); j++ {
				if tail[j] <= ' ' {
					continue
				}
				if tail[j] == ',' {
					exidx = j + 1
				}
				break
			}
		}
		buf = append(buf, container.Raw[child.Index+len(child.Raw)+exidx:]...)
		return buf, nil
	}
	buf = appendRawOrString(buf, raw, stringify)
	buf = append(buf, container.Raw[child.Index+len(child.Raw):]...)
	return buf, nil
}

// lookupSegment finds the literal child addressed by seg inside an
// existing object or array. The returned index is relative to
// container.Raw.
func lookupSegment(container gjson.Result, seg Segment) (gjson.Result, bool) {
	var child gjson.Result
	found := false
	switch seg.kind {
	case segmentKey:
		container.ForEach(func(key, value gjson.Result) bool {
			if key.Str == seg.key {
				child = value
				found = true
				return false
			}
			return true
		})
	case segmentIndex:
		i := 0
		container.ForEach(func(key, value gjson.Result) bool {
			if i == seg.index {
				child = value
				found = true
				return false
			}
			i++
			return true
		})
	case segmentAppend:
		elems := container.Array()
		if len(elems) > 0 {
			child = elems[len(elems)-1]
			found = true
		}
	}
	return child, found
}

// insertMissing adds the remainder of a path that does not exist in an
// existing container.
func insertMissing(buf []byte, container gjson.Result, path Path,
	raw string, stringify bool) ([]byte, error) {
	seg := path[0]

	// Whether the container already holds at least one member.
	comma := false
	for i := 1; i < len(container.Raw); i++ {
		if container.Raw[i] <= ' ' {
			continue
		}
		if container.Raw[i] == '}' || container.Raw[i] == ']' {
			break
		}
		comma = true
		break
	}

	if container.IsObject() {
		end := len(container.Raw) - 1
		for ; end > 0; end-- {
			if container.Raw[end] == '}' {
				break
			}
		}
		buf = append(buf, container.Raw[:end]...)
		if comma {
			buf = append(buf, ',')
		}
		buf = appendStringify(buf, seg.key)
		buf = append(buf, ':')
		buf = emitValueOrChain(buf, path[1:], raw, stringify)
		buf = append(buf, '}')
		return buf, nil
	}

	// Array container.
	if seg.kind == segmentAppend {
		njson := trim(container.Raw)
		if njson[len(njson)-1] == ']' {
			njson = njson[:len(njson)-1]
		}
		buf = append(buf, njson...)
		if comma {
			buf = append(buf, ',')
		}
		buf = emitValueOrChain(buf, path[1:], raw, stringify)
		buf = append(buf, ']')
		return buf, nil
	}

	// A missing index is at or past the current length. Pad the gap with
	// null so the new element lands exactly at the requested position.
	elems := container.Array()
	pad := seg.index - len(elems)
	if pad < 0 {
		return nil, &errorType{"malformed json array"}
	}
	buf = append(buf, '[')
	for i := 0; i < len(elems); i++ {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, elems[i].Raw...)
	}
	if len(elems) == 0 {
		buf = appendRepeat(buf, "null,", pad)
	} else {
		buf = appendRepeat(buf, ",null", pad)
		if comma {
			buf = append(buf, ',')
		}
	}
	buf = emitValueOrChain(buf, path[1:], raw, stringify)
	buf = append(buf, ']')
	return buf, nil
}

// emitValueOrChain writes the raw terminal value or, when more segments
// remain, the container chain described by them.
func emitValueOrChain(buf []byte, rest Path, raw string,
	stringify bool) []byte {
	if len(rest) == 0 {
		return appendRawOrString(buf, raw, stringify)
	}
	return emitChain(buf, rest[0], rest[1:], raw, stringify)
}

// appendRawOrString appends the terminal raw json value, quoting it as a
// json string when stringify is set.
func appendRawOrString(buf []byte, raw string, stringify bool) []byte {
	if stringify {
		return appendStringify(buf, raw)
	}
	return append(buf, raw...)
}

// emitChain builds a missing subtree. It creates the container addressed
// by seg and places the child chain (or terminal raw value) at the exact
// position named by seg. The container kind always follows the segment
// type: a Key yields an object, an Index or Append yields an array.
func emitChain(buf []byte, seg Segment, rest Path, raw string,
	stringify bool) []byte {
	child := emitValueOrChain(nil, rest, raw, stringify)
	switch seg.kind {
	case segmentKey:
		buf = append(buf, '{')
		buf = appendStringify(buf, seg.key)
		buf = append(buf, ':')
		buf = append(buf, child...)
		buf = append(buf, '}')
	case segmentIndex:
		buf = append(buf, '[')
		buf = appendRepeat(buf, "null,", seg.index)
		buf = append(buf, child...)
		buf = append(buf, ']')
	default: // segmentAppend
		buf = append(buf, '[')
		buf = append(buf, child...)
		buf = append(buf, ']')
	}
	return buf
}

// quoteKey renders an object key as a json quoted string for messages.
func quoteKey(key string) string {
	b, _ := jsongo.Marshal(key)
	return string(b)
}
