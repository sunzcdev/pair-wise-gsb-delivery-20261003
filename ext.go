package digest

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// extCharset is the only charset allowed in extended parameter values.
const extCharset = "UTF-8"

// errInvalidExtValue is returned when an RFC 5987 ext-value cannot be decoded.
var errInvalidExtValue = errors.New("digest: invalid extended parameter value")

// isAttrChar reports whether b is an attr-char as defined by RFC 5987,
// Section 3.2.1. Such characters may appear literally in a value-chars
// sequence; everything else must be percent-encoded.
func isAttrChar(b byte) bool {
	switch b {
	case '!', '#', '$', '&', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z') || ('0' <= b && b <= '9')
}

// encodeExtValue encodes a UTF-8 string as an RFC 5987 Section 3.2 ext-value:
//
//	UTF-8''<percent-encoded bytes>
//
// The caller is responsible for ensuring s is valid UTF-8.
func encodeExtValue(s string) string {
	var b strings.Builder
	b.WriteString(extCharset)
	b.WriteString("''")
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isAttrChar(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
	}
	return b.String()
}

// decodeExtValue decodes an RFC 5987 ext-value of the form
//
//	charset "'" language "'" value-chars
//
// It only accepts the UTF-8 charset (matched case-insensitively), validates
// every percent-escape and requires the decoded bytes to be valid UTF-8.
func decodeExtValue(v string) (string, error) {
	first := strings.IndexByte(v, '\'')
	if first < 0 {
		return "", errInvalidExtValue
	}
	rest := v[first+1:]
	second := strings.IndexByte(rest, '\'')
	if second < 0 {
		return "", errInvalidExtValue
	}
	charset := v[:first]
	language := rest[:second]
	coded := rest[second+1:]
	// A third apostrophe is illegal: it is not an attr-char and therefore
	// must have been percent-encoded.
	if strings.IndexByte(language, '\'') >= 0 || strings.IndexByte(coded, '\'') >= 0 {
		return "", errInvalidExtValue
	}
	if !strings.EqualFold(charset, extCharset) {
		return "", fmt.Errorf("digest: unsupported charset in extended parameter: %q", charset)
	}
	out := make([]byte, 0, len(coded))
	for i := 0; i < len(coded); {
		c := coded[i]
		if c != '%' {
			out = append(out, c)
			i++
			continue
		}
		if i+2 >= len(coded) {
			return "", errInvalidExtValue
		}
		hi, ok1 := decodeHexNibble(coded[i+1])
		lo, ok2 := decodeHexNibble(coded[i+2])
		if !ok1 || !ok2 {
			return "", errInvalidExtValue
		}
		out = append(out, hi<<4|lo)
		i += 3
	}
	if !utf8.Valid(out) {
		return "", errInvalidExtValue
	}
	return string(out), nil
}

func decodeHexNibble(b byte) (byte, bool) {
	switch {
	case '0' <= b && b <= '9':
		return b - '0', true
	case 'a' <= b && b <= 'f':
		return b - 'a' + 10, true
	case 'A' <= b && b <= 'F':
		return b - 'A' + 10, true
	default:
		return 0, false
	}
}
