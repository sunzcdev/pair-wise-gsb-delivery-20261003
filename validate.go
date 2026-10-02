package fastjson

import (
	"fmt"
	"strconv"
	"strings"
)

// Validate validates JSON s.
func Validate(s string) error {
	return validate(s, false)
}

// ValidateStrict is like Validate, but it also rejects strings with invalid
// Unicode: invalid UTF-8, malformed \u escape sequences, unpaired surrogates
// and surrogate pairs in the wrong order.
func ValidateStrict(s string) error {
	return validate(s, true)
}

func validate(s string, strict bool) error {
	n := len(s)
	s = skipWS(s)

	vd := validator{
		strict:  strict,
		base:    n - len(s),
		fullLen: len(s),
	}
	tail, err := vd.validateValue(s)
	if err != nil {
		return fmt.Errorf("cannot parse JSON: %s; unparsed tail: %q", err, startEndString(tail))
	}
	tail = skipWS(tail)
	if len(tail) > 0 {
		return fmt.Errorf("unexpected tail: %q", startEndString(tail))
	}
	return nil
}

// ValidateBytes validates JSON b.
func ValidateBytes(b []byte) error {
	return Validate(b2s(b))
}

// ValidateBytesStrict is like ValidateBytes, but it also rejects strings
// with invalid Unicode. See ValidateStrict for details.
func ValidateBytesStrict(b []byte) error {
	return ValidateStrict(b2s(b))
}

type validator struct {
	// strict enables strict Unicode checks for strings.
	strict bool

	// base is the byte offset of the validated string in the original input
	// (the number of leading whitespace bytes skipped before validation).
	base int

	// fullLen is the length of the string being validated.
	// It is used for calculating byte offsets in the original input.
	fullLen int
}

// pos returns the byte offset of s[0] in the original input,
// where s is a suffix of the string being validated.
func (vd *validator) pos(s string) int {
	return vd.base + vd.fullLen - len(s)
}

func (vd *validator) validateValue(s string) (string, error) {
	if len(s) == 0 {
		return s, fmt.Errorf("cannot parse empty string")
	}

	if s[0] == '{' {
		tail, err := vd.validateObject(s[1:])
		if err != nil {
			return tail, fmt.Errorf("cannot parse object: %s", err)
		}
		return tail, nil
	}
	if s[0] == '[' {
		tail, err := vd.validateArray(s[1:])
		if err != nil {
			return tail, fmt.Errorf("cannot parse array: %s", err)
		}
		return tail, nil
	}
	if s[0] == '"' {
		sv, raw, tail, err := vd.validateString(s[1:])
		if err != nil {
			return tail, fmt.Errorf("cannot parse string: %s", err)
		}
		if vd.strict {
			if err := checkStringStrict(raw, vd.pos(s)+1); err != nil {
				return s, fmt.Errorf("cannot parse string: %s", err)
			}
		}
		// Scan the string for control chars.
		for i := range len(sv) {
			if sv[i] < 0x20 {
				return tail, fmt.Errorf("string cannot contain control char 0x%02X", sv[i])
			}
		}
		return tail, nil
	}
	if s[0] == 't' {
		if len(s) < len("true") || s[:len("true")] != "true" {
			return s, fmt.Errorf("unexpected value found: %q", s)
		}
		return s[len("true"):], nil
	}
	if s[0] == 'f' {
		if len(s) < len("false") || s[:len("false")] != "false" {
			return s, fmt.Errorf("unexpected value found: %q", s)
		}
		return s[len("false"):], nil
	}
	if s[0] == 'n' {
		if len(s) < len("null") || s[:len("null")] != "null" {
			return s, fmt.Errorf("unexpected value found: %q", s)
		}
		return s[len("null"):], nil
	}

	tail, err := validateNumber(s)
	if err != nil {
		return tail, fmt.Errorf("cannot parse number: %s", err)
	}
	return tail, nil
}

func (vd *validator) validateArray(s string) (string, error) {
	s = skipWS(s)
	if len(s) == 0 {
		return s, fmt.Errorf("missing ']'")
	}
	if s[0] == ']' {
		return s[1:], nil
	}

	for {
		var err error

		s = skipWS(s)
		s, err = vd.validateValue(s)
		if err != nil {
			return s, fmt.Errorf("cannot parse array value: %s", err)
		}

		s = skipWS(s)
		if len(s) == 0 {
			return s, fmt.Errorf("unexpected end of array")
		}
		if s[0] == ',' {
			s = s[1:]
			continue
		}
		if s[0] == ']' {
			s = s[1:]
			return s, nil
		}
		return s, fmt.Errorf("missing ',' after array value")
	}
}

func (vd *validator) validateObject(s string) (string, error) {
	s = skipWS(s)
	if len(s) == 0 {
		return s, fmt.Errorf("missing '}'")
	}
	if s[0] == '}' {
		return s[1:], nil
	}

	for {
		var err error

		// Parse key.
		s = skipWS(s)
		if len(s) == 0 || s[0] != '"' {
			return s, fmt.Errorf(`cannot find opening '"" for object key`)
		}

		var key, keyRaw string
		keyStr := s
		key, keyRaw, s, err = vd.validateKey(s[1:])
		if err != nil {
			return s, fmt.Errorf("cannot parse object key: %s", err)
		}
		if vd.strict {
			if err := checkStringStrict(keyRaw, vd.pos(keyStr)+1); err != nil {
				return keyStr, fmt.Errorf("cannot parse object key: %s", err)
			}
		}
		// Scan the key for control chars.
		for i := range len(key) {
			if key[i] < 0x20 {
				return s, fmt.Errorf("object key cannot contain control char 0x%02X", key[i])
			}
		}
		s = skipWS(s)
		if len(s) == 0 || s[0] != ':' {
			return s, fmt.Errorf("missing ':' after object key")
		}
		s = s[1:]

		// Parse value
		s = skipWS(s)
		s, err = vd.validateValue(s)
		if err != nil {
			return s, fmt.Errorf("cannot parse object value: %s", err)
		}
		s = skipWS(s)
		if len(s) == 0 {
			return s, fmt.Errorf("unexpected end of object")
		}
		if s[0] == ',' {
			s = s[1:]
			continue
		}
		if s[0] == '}' {
			return s[1:], nil
		}
		return s, fmt.Errorf("missing ',' after object value")
	}
}

// validateKey is similar to validateString, but is optimized
// for typical object keys, which are quite small and have no escape sequences.
//
// It returns the key for the control chars scan, the full raw key
// for strict Unicode checks and the unparsed tail.
func (vd *validator) validateKey(s string) (key, raw, tail string, err error) {
	for i := range len(s) {
		if s[i] == '"' {
			// Fast path - the key doesn't contain escape sequences.
			return s[:i], s[:i], s[i+1:], nil
		}
		if s[i] == '\\' {
			// Slow path - the key contains escape sequences.
			return vd.validateString(s)
		}
	}
	return "", "", s, fmt.Errorf(`missing closing '"'`)
}

// validateString returns the string for the control chars scan, the full
// raw string for strict Unicode checks and the unparsed tail.
func (vd *validator) validateString(s string) (sv, raw, tail string, err error) {
	// Try fast path - a string without escape sequences.
	if n := strings.IndexByte(s, '"'); n >= 0 && strings.IndexByte(s[:n], '\\') < 0 {
		return s[:n], s[:n], s[n+1:], nil
	}

	// Slow path - escape sequences are present.
	rs, tail, err := parseRawString(s)
	if err != nil {
		return "", rs, tail, err
	}
	raw = rs
	for {
		n := strings.IndexByte(rs, '\\')
		if n < 0 {
			return rs, raw, tail, nil
		}
		n++
		if n >= len(rs) {
			return rs, raw, tail, fmt.Errorf("BUG: parseRawString returned invalid string with trailing backslash: %q", rs)
		}
		ch := rs[n]
		rs = rs[n+1:]
		switch ch {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			// Valid escape sequences - see http://json.org/
			break
		case 'u':
			if len(rs) < 4 {
				return rs, raw, tail, fmt.Errorf(`too short escape sequence: \u%s`, rs)
			}
			xs := rs[:4]
			_, err := strconv.ParseUint(xs, 16, 16)
			if err != nil {
				return rs, raw, tail, fmt.Errorf(`invalid escape sequence \u%s: %s`, xs, err)
			}
			rs = rs[4:]
		default:
			return rs, raw, tail, fmt.Errorf(`unknown escape sequence \%c`, ch)
		}
	}
}

func validateNumber(s string) (string, error) {
	if len(s) == 0 {
		return s, fmt.Errorf("zero-length number")
	}
	if s[0] == '-' {
		s = s[1:]
		if len(s) == 0 {
			return s, fmt.Errorf("missing number after minus")
		}
	}
	i := 0
	for i < len(s) {
		if s[i] < '0' || s[i] > '9' {
			break
		}
		i++
	}
	if i <= 0 {
		return s, fmt.Errorf("expecting 0..9 digit, got %c", s[0])
	}
	if s[0] == '0' && i != 1 {
		return s, fmt.Errorf("unexpected number starting from 0")
	}
	if i >= len(s) {
		return "", nil
	}
	if s[i] == '.' {
		// Validate fractional part
		s = s[i+1:]
		if len(s) == 0 {
			return s, fmt.Errorf("missing fractional part")
		}
		i = 0
		for i < len(s) {
			if s[i] < '0' || s[i] > '9' {
				break
			}
			i++
		}
		if i == 0 {
			return s, fmt.Errorf("expecting 0..9 digit in fractional part, got %c", s[0])
		}
		if i >= len(s) {
			return "", nil
		}
	}
	if s[i] == 'e' || s[i] == 'E' {
		// Validate exponent part
		s = s[i+1:]
		if len(s) == 0 {
			return s, fmt.Errorf("missing exponent part")
		}
		if s[0] == '-' || s[0] == '+' {
			s = s[1:]
			if len(s) == 0 {
				return s, fmt.Errorf("missing exponent part")
			}
		}
		i = 0
		for i < len(s) {
			if s[i] < '0' || s[i] > '9' {
				break
			}
			i++
		}
		if i == 0 {
			return s, fmt.Errorf("expecting 0..9 digit in exponent part, got %c", s[0])
		}
		if i >= len(s) {
			return "", nil
		}
	}
	return s[i:], nil
}
