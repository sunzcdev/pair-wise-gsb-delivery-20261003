package sjson

import (
	jsongo "encoding/json"
	"unsafe"
)

// renameMember records the positions of a single object member.
type renameMember struct {
	// keyStart and keyEnd bound the quoted key token, including quotes.
	keyStart int
	keyEnd   int
	// key is the decoded member name.
	key string
	// valStart and valEnd bound the value token.
	valStart int
	valEnd   int
	// comma is the index of the comma that follows the value, or -1.
	comma int
	// prevComma is the index of the comma that precedes the key, or -1.
	prevComma int
}

// scanRenameMembers scans a json document that must be an object and
// returns the top level member positions.
func scanRenameMembers(json []byte) ([]renameMember, error) {
	i := 0
	for i < len(json) && json[i] <= ' ' {
		i++
	}
	if i >= len(json) || json[i] != '{' {
		return nil, &errorType{"json must be an object"}
	}
	i++
	var members []renameMember
	prevComma := -1
	for {
		for i < len(json) && json[i] <= ' ' {
			i++
		}
		if i >= len(json) {
			return nil, &errorType{"unexpected end of json"}
		}
		if json[i] == '}' {
			return members, nil
		}
		if json[i] != '"' {
			return nil, &errorType{"expected object key"}
		}
		keyStart := i
		key, ni, err := scanJSONString(json, i)
		if err != nil {
			return nil, err
		}
		keyEnd := ni
		i = ni
		for i < len(json) && json[i] <= ' ' {
			i++
		}
		if i >= len(json) || json[i] != ':' {
			return nil, &errorType{"expected ':' after object key"}
		}
		i++
		for i < len(json) && json[i] <= ' ' {
			i++
		}
		if i >= len(json) {
			return nil, &errorType{"unexpected end of json"}
		}
		valStart := i
		i, err = scanJSONValue(json, i)
		if err != nil {
			return nil, err
		}
		valEnd := i
		for i < len(json) && json[i] <= ' ' {
			i++
		}
		comma := -1
		if i < len(json) && json[i] == ',' {
			comma = i
			i++
		} else if i >= len(json) || json[i] != '}' {
			return nil, &errorType{"expected ',' or '}' after object value"}
		}
		members = append(members, renameMember{
			keyStart:  keyStart,
			keyEnd:    keyEnd,
			key:       key,
			valStart:  valStart,
			valEnd:    valEnd,
			comma:     comma,
			prevComma: prevComma,
		})
		prevComma = comma
	}
}

// scanJSONString starts at a quote and returns the decoded string and the
// index immediately after the closing quote.
func scanJSONString(json []byte, i int) (string, int, error) {
	start := i
	i++
	for i < len(json) {
		switch json[i] {
		case '\\':
			i += 2
			if i > len(json) {
				return "", 0, &errorType{"unexpected end of json"}
			}
			continue
		case '"':
			var key string
			if err := jsongo.Unmarshal(json[start:i+1], &key); err != nil {
				return "", 0, &errorType{"invalid json string: " + err.Error()}
			}
			return key, i + 1, nil
		}
		i++
	}
	return "", 0, &errorType{"unexpected end of json"}
}

// scanJSONValue returns the index immediately after the value that begins at i.
func scanJSONValue(json []byte, i int) (int, error) {
	switch json[i] {
	case '"':
		_, ni, err := scanJSONString(json, i)
		return ni, err
	case '{', '[':
		open, close := json[i], byte('}')
		if open == '[' {
			close = ']'
		}
		depth := 0
		for i < len(json) {
			switch json[i] {
			case '"':
				_, ni, err := scanJSONString(json, i)
				if err != nil {
					return 0, err
				}
				i = ni
				continue
			case open:
				depth++
			case close:
				depth--
				if depth == 0 {
					return i + 1, nil
				}
			}
			i++
		}
		return 0, &errorType{"unexpected end of json"}
	default:
		for i < len(json) {
			if json[i] <= ' ' || json[i] == ',' || json[i] == '}' ||
				json[i] == ']' {
				return i, nil
			}
			i++
		}
		return i, nil
	}
}

// renameEdit is a single replacement applied to the original json bytes.
type renameEdit struct {
	start int
	end   int
	rep   []byte
}

func rename(jsonb []byte, oldKey, newKey string,
	inplace, overwrite bool) ([]byte, error) {
	// Renaming to the same name is a no-op regardless of key duplicates.
	if oldKey == newKey {
		return jsonb, nil
	}
	if inplace && cap(jsonb) < len(jsonb) {
		inplace = false
	}
	members, err := scanRenameMembers(jsonb)
	if err != nil {
		return jsonb, err
	}
	var oldIdxs []int
	newCount := 0
	newIdx := -1
	for i := range members {
		if members[i].key == oldKey {
			oldIdxs = append(oldIdxs, i)
		}
		if members[i].key == newKey {
			newCount++
			newIdx = i
		}
	}
	if len(oldIdxs) == 0 {
		return jsonb, &errorType{
			"cannot rename nonexistent member '" + oldKey + "'"}
	}
	if len(oldIdxs) > 1 {
		return jsonb, &errorType{
			"rename source key is ambiguous: multiple members named '" +
				oldKey + "'"}
	}
	if newCount > 1 {
		return jsonb, &errorType{
			"rename target key is ambiguous: multiple members named '" +
				newKey + "'"}
	}
	oldIdx := oldIdxs[0]
	if newCount == 1 && !overwrite {
		return jsonb, &errorType{
			"cannot rename member to existing key '" + newKey + "'"}
	}
	newToken, err := jsongo.Marshal(newKey)
	if err != nil {
		return jsonb, err
	}
	var edits []renameEdit
	if newCount == 1 {
		// Remove the target member. Removing the first member strips the
		// following comma; otherwise the comma that precedes it is removed.
		target := members[newIdx]
		if target.prevComma == -1 {
			end := target.comma
			if end == -1 {
				end = target.valEnd
			} else {
				end++
			}
			edits = append(edits, renameEdit{start: target.keyStart, end: end})
		} else {
			edits = append(edits, renameEdit{
				start: target.prevComma, end: target.valEnd})
		}
	}
	old := members[oldIdx]
	edits = append(edits, renameEdit{
		start: old.keyStart, end: old.keyEnd, rep: newToken})
	sortRenameEdits(edits)
	if inplace && renameEditsFit(jsonb, edits) {
		return applyRenameEditsInPlace(jsonb, edits), nil
	}
	return applyRenameEditsCopy(jsonb, edits), nil
}

// sortRenameEdits orders edits by their start position.
func sortRenameEdits(edits []renameEdit) {
	for i := 1; i < len(edits); i++ {
		for j := i; j > 0 && edits[j].start < edits[j-1].start; j-- {
			edits[j], edits[j-1] = edits[j-1], edits[j]
		}
	}
}

// renameEditsFit reports whether the edits fit without growing the slice.
func renameEditsFit(json []byte, edits []renameEdit) bool {
	delta := 0
	for _, edit := range edits {
		delta += len(edit.rep) - (edit.end - edit.start)
	}
	return len(json)+delta <= cap(json)
}

// applyRenameEditsInPlace rewrites the backing array. Edits are applied from
// right to left so overlapping copy ranges stay valid.
func applyRenameEditsInPlace(json []byte, edits []renameEdit) []byte {
	out := json[:cap(json)]
	n := len(json)
	for i := len(edits) - 1; i >= 0; i-- {
		edit := edits[i]
		oldn := edit.end - edit.start
		newn := len(edit.rep)
		shift := newn - oldn
		if shift > 0 {
			copy(out[edit.end+shift:n+shift], out[edit.end:n])
		} else if shift < 0 {
			copy(out[edit.start+newn:n+shift], out[edit.end:n])
		}
		copy(out[edit.start:edit.start+newn], edit.rep)
		n += shift
		out = out[:n]
	}
	return out
}

// applyRenameEditsCopy builds a new byte slice.
func applyRenameEditsCopy(json []byte, edits []renameEdit) []byte {
	size := len(json)
	for _, edit := range edits {
		size += len(edit.rep) - (edit.end - edit.start)
	}
	buf := make([]byte, 0, size)
	pos := 0
	for _, edit := range edits {
		buf = append(buf, json[pos:edit.start]...)
		buf = append(buf, edit.rep...)
		pos = edit.end
	}
	buf = append(buf, json[pos:]...)
	return buf
}

// Rename renames an object member key.
// The value and its position in the object are left untouched.
// Old and new names are compared as decoded strings; dot syntax and other
// query expressions are not interpreted, so empty keys and keys containing
// dots or backslashes can be renamed directly.
// By default an error is returned when the target key already exists.
// Pass an Options value with Overwrite set to true to replace the existing
// member instead.
func Rename(json, oldName, newName string) (string, error) {
	return RenameOptions(json, oldName, newName, nil)
}

// RenameBytes renames an object member key.
// If working with bytes, this method preferred over
// Rename(string(data), oldName, newName)
func RenameBytes(json []byte, oldName, newName string) ([]byte, error) {
	return RenameBytesOptions(json, oldName, newName, nil)
}

// RenameOptions renames an object member key with options.
func RenameOptions(json, oldName, newName string,
	opts *Options) (string, error) {
	if opts != nil && opts.ReplaceInPlace {
		// it's not safe to replace bytes in-place for strings
		// copy the Options and set options.ReplaceInPlace to false.
		nopts := *opts
		opts = &nopts
		opts.ReplaceInPlace = false
	}
	jsonh := *(*stringHeader)(unsafe.Pointer(&json))
	jsonbh := sliceHeader{data: jsonh.data, len: jsonh.len, cap: jsonh.len}
	jsonb := *(*[]byte)(unsafe.Pointer(&jsonbh))
	res, err := RenameBytesOptions(jsonb, oldName, newName, opts)
	return string(res), err
}

// RenameBytesOptions renames an object member key with options.
// If working with bytes, this method preferred over
// RenameOptions(string(data), oldName, newName, opts)
func RenameBytesOptions(json []byte, oldName, newName string,
	opts *Options) ([]byte, error) {
	var inplace, overwrite bool
	if opts != nil {
		inplace = opts.ReplaceInPlace
		overwrite = opts.Overwrite
	}
	return rename(json, oldName, newName, inplace, overwrite)
}
