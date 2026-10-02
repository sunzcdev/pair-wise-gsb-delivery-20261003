package fastjson

import (
	"fmt"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// ParseStrict parses s containing JSON like Parse does, but additionally
// rejects strings and object keys containing invalid UTF-8, lone UTF-16
// surrogates or mis-ordered surrogate pairs.
//
// The returned value is valid until the next call to Parse*.
func (p *Parser) ParseStrict(s string) (*Value, error) {
	if err := validateUnicodeStrict(s); err != nil {
		return nil, fmt.Errorf("cannot parse JSON: %s", err)
	}
	return p.Parse(s)
}

// ParseBytesStrict parses b containing JSON like ParseBytes does,
// but additionally rejects strings and object keys containing invalid UTF-8,
// lone UTF-16 surrogates or mis-ordered surrogate pairs.
//
// The returned Value is valid until the next call to Parse*.
func (p *Parser) ParseBytesStrict(b []byte) (*Value, error) {
	return p.ParseStrict(b2s(b))
}

// ParseStrict parses json string s like Parse does, but additionally
// rejects strings and object keys containing invalid UTF-8, lone UTF-16
// surrogates or mis-ordered surrogate pairs.
//
// The function is slower than the Parser.ParseStrict for re-used Parser.
func ParseStrict(s string) (*Value, error) {
	var p Parser
	return p.ParseStrict(s)
}

// ParseBytesStrict parses b containing json like ParseBytes does,
// but additionally rejects strings and object keys containing invalid UTF-8,
// lone UTF-16 surrogates or mis-ordered surrogate pairs.
//
// The function is slower than the Parser.ParseBytesStrict for re-used Parser.
func ParseBytesStrict(b []byte) (*Value, error) {
	var p Parser
	return p.ParseBytesStrict(b)
}

// ValidateStrict validates JSON s like Validate does, but additionally
// rejects strings and object keys containing invalid UTF-8, lone UTF-16
// surrogates or mis-ordered surrogate pairs.
func ValidateStrict(s string) error {
	if err := validateUnicodeStrict(s); err != nil {
		return fmt.Errorf("cannot parse JSON: %s", err)
	}
	return Validate(s)
}

// ValidateBytesStrict validates JSON b like ValidateBytes does,
// but additionally rejects strings and object keys containing invalid UTF-8,
// lone UTF-16 surrogates or mis-ordered surrogate pairs.
func ValidateBytesStrict(b []byte) error {
	return ValidateStrict(b2s(b))
}

// validateUnicodeStrict scans s for JSON string literals and verifies that
// every string, including object keys, contains only valid UTF-8 bytes and
// well-formed \u escape sequences.
//
// It performs a single pass over the raw input, so all the reported offsets
// are byte offsets into s, not offsets into the decoded strings.
//
// Strings, which cannot be located because of a malformed JSON structure,
// are skipped - the parser will report the structural error afterwards.
func validateUnicodeStrict(s string) error {
	i := 0
	for i < len(s) {
		if s[i] != '"' {
			i++
			continue
		}
		// Locate the closing quote for the string starting at s[i].
		j := i + 1
		for j < len(s) {
			if s[j] == '\\' {
				// Skip the escaped char.
				j += 2
				continue
			}
			if s[j] == '"' {
				break
			}
			j++
		}
		if j >= len(s) {
			// Unterminated string. Validate the available part and let
			// the parser report the missing closing quote afterwards.
			return validateStringUnicode(s[i+1:], i+1)
		}
		if err := validateStringUnicode(s[i+1:j], i+1); err != nil {
			return err
		}
		i = j + 1
	}
	return nil
}

// validateStringUnicode verifies that the raw JSON string contents rs
// (without the surrounding quotes and with escape sequences kept intact)
// contains only valid UTF-8 bytes and well-formed escape sequences.
//
// base is the byte offset of rs within the original input. It is used
// for reporting error offsets in original input bytes.
func validateStringUnicode(rs string, base int) error {
	for i := 0; i < len(rs); {
		c := rs[i]
		if c == '\\' {
			if i+1 >= len(rs) {
				// Trailing backslash. The parser will report it.
				return nil
			}
			off := base + i
			switch rs[i+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				// Valid escape sequences - see http://json.org/
				i += 2
			case 'u':
				n, err := validateUnicodeEscape(rs[i:], off)
				if err != nil {
					return err
				}
				i += n
			default:
				return fmt.Errorf("unknown escape sequence \\%c at offset %d", rs[i+1], off)
			}
			continue
		}
		if c < 0x20 {
			return fmt.Errorf("string cannot contain control char 0x%02X at offset %d", c, base+i)
		}
		if c < utf8.RuneSelf {
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(rs[i:])
		if r == utf8.RuneError && size <= 1 {
			return fmt.Errorf("invalid UTF-8 byte 0x%02X at offset %d", c, base+i)
		}
		i += size
	}
	return nil
}

// validateUnicodeEscape validates the \uXXXX escape sequence at the start
// of s, which must begin with `\u`. It returns the number of bytes consumed
// from s: 6 for a BMP char and 12 for a surrogate pair.
//
// off is the byte offset of s[0] within the original input.
func validateUnicodeEscape(s string, off int) (int, error) {
	if len(s) < 6 {
		return 0, fmt.Errorf(`too short escape sequence: \u%s at offset %d`, s[2:], off)
	}
	xs := s[2:6]
	x, err := strconv.ParseUint(xs, 16, 16)
	if err != nil {
		return 0, fmt.Errorf(`invalid escape sequence \u%s at offset %d`, xs, off)
	}
	r := rune(x)
	if !utf16.IsSurrogate(r) {
		return 6, nil
	}
	if r >= 0xDC00 {
		// A low surrogate without a preceding high surrogate.
		return 0, fmt.Errorf(`unexpected low surrogate \u%s at offset %d`, xs, off)
	}
	// A high surrogate must be followed by a low surrogate escape sequence.
	if len(s) < 12 || s[6] != '\\' || s[7] != 'u' {
		return 0, fmt.Errorf(`lone surrogate \u%s at offset %d`, xs, off)
	}
	xs1 := s[8:12]
	x1, err := strconv.ParseUint(xs1, 16, 16)
	if err != nil {
		return 0, fmt.Errorf(`invalid escape sequence \u%s at offset %d`, xs1, off+6)
	}
	if r1 := rune(x1); r1 < 0xDC00 || r1 > 0xDFFF {
		return 0, fmt.Errorf(`lone surrogate \u%s at offset %d`, xs, off)
	}
	return 12, nil
}
