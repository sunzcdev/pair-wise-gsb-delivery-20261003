package fastjson

import (
	"fmt"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// checkStringStrict verifies that the raw (still escaped) JSON string s
// contains only valid Unicode. off is the byte offset of s[0] in the
// original input and is used for error positions.
//
// It rejects invalid UTF-8, malformed \u escape sequences, unpaired
// surrogates and surrogate pairs in the wrong order.
func checkStringStrict(s string, off int) error {
	for i := 0; i < len(s); {
		c := s[i]
		if c == '\\' {
			start := i
			i++
			// s cannot end with a backslash, since the caller already
			// found an unescaped closing quote.
			esc := s[i]
			i++
			if esc != 'u' {
				// Non-unicode escape sequence, e.g. \\, \" or \n.
				// Skipping both chars, so an escaped backslash
				// cannot be mistaken for the start of a \u escape.
				continue
			}
			if i+4 > len(s) {
				return fmt.Errorf("too short \\u escape sequence at position %d", off+start)
			}
			xs := s[i : i+4]
			x, err := strconv.ParseUint(xs, 16, 16)
			if err != nil {
				return fmt.Errorf("invalid escape sequence \\u%s at position %d", xs, off+start)
			}
			i += 4
			r := rune(x)
			if !utf16.IsSurrogate(r) {
				continue
			}
			if r >= 0xDC00 {
				return fmt.Errorf("unpaired low surrogate \\u%s at position %d", xs, off+start)
			}
			// A high surrogate must be immediately followed
			// by a low surrogate escape sequence.
			if i+6 > len(s) || s[i] != '\\' || s[i+1] != 'u' {
				return fmt.Errorf("unpaired high surrogate \\u%s at position %d", xs, off+start)
			}
			xs2 := s[i+2 : i+6]
			x2, err := strconv.ParseUint(xs2, 16, 16)
			if err != nil {
				return fmt.Errorf("invalid escape sequence \\u%s at position %d", xs2, off+i)
			}
			if r2 := rune(x2); r2 < 0xDC00 || r2 > 0xDFFF {
				return fmt.Errorf("unpaired high surrogate \\u%s at position %d", xs, off+start)
			}
			i += 6
			continue
		}
		if c < utf8.RuneSelf {
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			return fmt.Errorf("invalid UTF-8 byte 0x%02X at position %d", c, off+i)
		}
		i += size
	}
	return nil
}
