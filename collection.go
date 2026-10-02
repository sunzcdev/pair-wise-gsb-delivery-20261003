// Copyright (c) 2013 Kelsey Hightower. All rights reserved.
// Use of this source code is governed by the MIT License that can be found in
// the LICENSE file.

package envconfig

import (
	"fmt"
	"reflect"
	"strings"
)

// Sentinel errors reported while tokenising a collection value. They are
// wrapped with the offending element position before reaching the caller.
var (
	errUnterminatedQuote = fmt.Errorf("unterminated quote in collection element")
	errDanglingEscape    = fmt.Errorf("dangling escape at end of collection element")
	errEmptyMapItem      = fmt.Errorf("invalid empty map item")
	errInvalidMapItem    = fmt.Errorf("invalid map item: missing ':'")
)

// collectionFieldError annotates a collection element or pair with its
// 1-based position so problems can be traced back to the offending part of
// the environment value.
type collectionFieldError struct {
	kind  string // "element" or "map item"
	index int
	err   error
}

func (e *collectionFieldError) Error() string {
	return fmt.Sprintf("%s %d: %s", e.kind, e.index, e.err)
}

func (e *collectionFieldError) Unwrap() error { return e.err }

// splitCollection splits a collection value on unquoted, unescaped
// occurrences of sep while keeping surrounding whitespace and quoting intact.
// The returned fragments are post-processed by parseCollectionPart.
//
// Quoting rules:
//
//   - A double quote starts a quoted span; separators, the map pair separator
//     and whitespace inside a quoted span are treated literally.
//   - A backslash escapes the following character.
//   - Two consecutive double quotes inside a quoted span denote one literal
//     double quote.
func splitCollection(value, sep, kind string) ([]string, error) {
	var (
		parts   []string
		current strings.Builder
		quoted  bool
		escaped bool
		element = 1
	)

	for i := 0; i < len(value); i++ {
		ch := value[i]

		if escaped {
			current.WriteByte(ch)
			escaped = false
			continue
		}

		if ch == '\\' {
			if i == len(value)-1 {
				return nil, &collectionFieldError{kind: kind, index: element, err: errDanglingEscape}
			}
			current.WriteByte(ch)
			escaped = true
			continue
		}

		if ch == '"' {
			if quoted && i+1 < len(value) && value[i+1] == '"' {
				current.WriteByte('\\')
				current.WriteByte('"')
				i++
				continue
			}
			quoted = !quoted
			current.WriteByte(ch)
			continue
		}

		if !quoted && strings.HasPrefix(value[i:], sep) {
			parts = append(parts, current.String())
			current.Reset()
			element++
			i += len(sep) - 1
			continue
		}

		current.WriteByte(ch)
	}

	if escaped {
		return nil, &collectionFieldError{kind: kind, index: element, err: errDanglingEscape}
	}
	if quoted {
		return nil, &collectionFieldError{kind: kind, index: element, err: errUnterminatedQuote}
	}

	parts = append(parts, current.String())
	return parts, nil
}

// parseCollectionPart turns a single, possibly quoted fragment produced by
// splitCollection into its final string value.
//
// Leading and trailing whitespace is stripped for unquoted fragments.
// Whitespace inside quotes is significant: the only way to represent a single
// empty string element is an explicit pair of quotes ("").
func parseCollectionPart(raw string) string {
	if !isQuotedPart(raw) {
		return unescapeCollectionPart(strings.TrimSpace(raw))
	}

	i := 0
	for i < len(raw) && isSpace(raw[i]) {
		i++
	}

	var (
		out     strings.Builder
		quoted  bool
		escaped bool
	)
	for ; i < len(raw); i++ {
		ch := raw[i]

		if escaped {
			out.WriteByte(ch)
			escaped = false
			continue
		}

		switch {
		case ch == '\\':
			if i == len(raw)-1 {
				out.WriteByte(ch)
			} else {
				escaped = true
			}
		case ch == '"':
			quoted = !quoted
		case isSpace(ch) && !quoted:
			// Whitespace immediately before the opening quote or after the
			// closing quote is formatting, not content.
			continue
		default:
			out.WriteByte(ch)
		}
	}
	if escaped {
		out.WriteByte('\\')
	}
	return out.String()
}

// splitCollectionPair splits a map item on its first unquoted, unescaped
// colon. Colons that belong to the (possibly quoted) key or value are left
// untouched, so a value such as url:http://example:8080 is not split again.
func splitCollectionPair(raw string) (string, string, bool) {
	var (
		quoted  bool
		escaped bool
	)
	for i := 0; i < len(raw); i++ {
		ch := raw[i]

		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == '"' {
			quoted = !quoted
			continue
		}
		if ch == ':' && !quoted {
			return raw[:i], raw[i+1:], true
		}
	}
	return "", "", false
}

func isQuotedPart(raw string) bool {
	for i := 0; i < len(raw); i++ {
		if !isSpace(raw[i]) {
			return raw[i] == '"'
		}
	}
	return false
}

func unescapeCollectionPart(raw string) string {
	if !strings.ContainsRune(raw, '\\') {
		return raw
	}
	var out strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\\' && i+1 < len(raw) {
			i++
		}
		out.WriteByte(raw[i])
	}
	return out.String()
}

func isSpace(ch byte) bool {
	switch ch {
	case ' ', '\t', '\n', '\v', '\f', '\r':
		return true
	}
	return false
}

// parseCollectionElements applies the optional quoted collection syntax to a
// slice value, returning the final elements before any type conversion takes
// place.
//
// An empty (or whitespace-only) value denotes an empty collection; a single
// empty string element must be written explicitly as "".
func parseCollectionElements(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	raw, err := splitCollection(value, ",", "element")
	if err != nil {
		return nil, err
	}
	elements := make([]string, len(raw))
	for i, part := range raw {
		elements[i] = parseCollectionPart(part)
	}
	return elements, nil
}

// processCollectionMap populates mp using the optional quoted collection
// syntax. Each item is split on its first unquoted, unescaped colon; colons
// that are part of the key or the value are preserved. Duplicate keys resolve
// to the last assigned value, mirroring native map assignment semantics.
func processCollectionMap(value string, typ reflect.Type, mp reflect.Value) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	items, err := splitCollection(value, ",", "map item")
	if err != nil {
		return err
	}
	for i, item := range items {
		if strings.TrimSpace(item) == "" {
			return &collectionFieldError{kind: "map item", index: i + 1, err: errEmptyMapItem}
		}
		rawKey, rawValue, ok := splitCollectionPair(item)
		if !ok {
			return &collectionFieldError{kind: "map item", index: i + 1, err: errInvalidMapItem}
		}

		key := reflect.New(typ.Key()).Elem()
		if err := processField(parseCollectionPart(rawKey), key, ""); err != nil {
			return &collectionFieldError{kind: "map item", index: i + 1, err: err}
		}
		val := reflect.New(typ.Elem()).Elem()
		if err := processField(parseCollectionPart(rawValue), val, ""); err != nil {
			return &collectionFieldError{kind: "map item", index: i + 1, err: err}
		}
		mp.SetMapIndex(key, val)
	}
	return nil
}
