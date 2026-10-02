package envconfig

import (
	"fmt"
	"strings"
	"unicode"
)

func parseQuotedSlice(value string) ([]string, error) {
	runes := []rune(value)
	items := make([]string, 0)
	pos := 0

	for pos < len(runes) {
		item, next, err := readQuotedItem(runes, pos, ',', false, len(items)+1)
		if err != nil {
			return nil, err
		}
		items = append(items, item)

		if next >= len(runes) {
			break
		}
		if next+1 == len(runes) {
			items = append(items, "")
			break
		}
		pos = next + 1
	}

	return items, nil
}

func parseQuotedMap(value string) ([][2]string, error) {
	runes := []rune(value)
	pairs := make([][2]string, 0)
	pos := 0
	element := 1

	for pos < len(runes) {
		key, next, err := readQuotedItem(runes, pos, ':', true, element)
		if err != nil {
			return nil, err
		}
		if next >= len(runes) || runes[next] != ':' {
			return nil, fmt.Errorf("invalid map item at element %d", element)
		}

		val, next, err := readQuotedItem(runes, next+1, ',', false, element)
		if err != nil {
			return nil, err
		}
		pairs = append(pairs, [2]string{key, val})

		if next+1 == len(runes) {
			return nil, fmt.Errorf("invalid map item at element %d", element+1)
		}
		if next >= len(runes) {
			break
		}
		pos = next + 1
		element++
	}

	return pairs, nil
}

func readQuotedItem(runes []rune, pos int, separator rune, allowColonEscape bool, element int) (string, int, error) {
	for pos < len(runes) && unicode.IsSpace(runes[pos]) {
		pos++
	}

	var result strings.Builder
	inQuotes := false
	closed := false
	quoted := false
	escaped := false
	started := false
	trailing := -1

	for pos < len(runes) {
		r := runes[pos]

		if escaped {
			if r != '\\' && r != '"' && r != separator && r != ',' && !(allowColonEscape && r == ':') {
				return "", pos, fmt.Errorf("invalid escape sequence in element %d", element)
			}
			result.WriteRune(r)
			escaped = false
			started = true
			pos++
			continue
		}

		switch {
		case r == '\\':
			escaped = true
		case r == '"':
			if inQuotes {
				inQuotes = false
				closed = true
			} else if started {
				return "", pos, fmt.Errorf("unexpected quote in element %d", element)
			} else {
				inQuotes = true
				quoted = true
				started = true
			}
		case r == separator, r == ',':
			if inQuotes {
				result.WriteRune(r)
			} else if quoted {
				if trailing >= 0 {
					return result.String()[:trailing], pos, nil
				}
				return result.String(), pos, nil
			} else {
				if trailing >= 0 {
					return strings.TrimSpace(result.String()[:trailing]), pos, nil
				}
				return strings.TrimSpace(result.String()), pos, nil
			}
		case unicode.IsSpace(r):
			if inQuotes {
				result.WriteRune(r)
			} else if started && trailing < 0 {
				trailing = result.Len()
				result.WriteRune(r)
			} else if started {
				result.WriteRune(r)
			}
		default:
			if closed {
				return "", pos, fmt.Errorf("unexpected character after closing quote in element %d", element)
			}
			trailing = -1
			started = true
			result.WriteRune(r)
		}

		pos++
	}

	if inQuotes {
		return "", pos, fmt.Errorf("unterminated quote in element %d", element)
	}
	if escaped {
		return "", pos, fmt.Errorf("dangling escape in element %d", element)
	}
	if quoted {
		if trailing >= 0 {
			return result.String()[:trailing], pos, nil
		}
		return result.String(), pos, nil
	}
	if trailing >= 0 {
		return strings.TrimSpace(result.String()[:trailing]), pos, nil
	}
	return strings.TrimSpace(result.String()), pos, nil
}
