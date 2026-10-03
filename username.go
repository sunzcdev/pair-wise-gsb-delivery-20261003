package digest

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// isASCII returns true if the string only contains ASCII characters
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// isAttrChar returns true if the byte can be used unencoded
// in an RFC 5987 ext-value
func isAttrChar(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	switch c {
	case '!', '#', '$', '&', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return false
}

// encodeUsernameStar formats the username as an RFC 5987 ext-value
// for use in the username* parameter (UTF-8 charset, percent-encoded)
func encodeUsernameStar(username string) string {
	var b strings.Builder
	b.Grow(len(username) + 7)
	b.WriteString("UTF-8''")
	for i := 0; i < len(username); i++ {
		c := username[i]
		if isAttrChar(c) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// decodeUsernameStar parses an RFC 5987 ext-value username* parameter.
// Only the UTF-8 charset is supported.
func decodeUsernameStar(value string) (string, error) {
	charset, encoded, ok := strings.Cut(value, "'")
	if !ok {
		return "", fmt.Errorf("digest: invalid username* value: %q", value)
	}
	// skip the (optional) language tag
	if _, rest, ok := strings.Cut(encoded, "'"); ok {
		encoded = rest
	} else {
		return "", fmt.Errorf("digest: invalid username* value: %q", value)
	}
	if !strings.EqualFold(charset, "UTF-8") {
		return "", fmt.Errorf("digest: unsupported username* charset: %q", charset)
	}
	var b strings.Builder
	b.Grow(len(encoded))
	for i := 0; i < len(encoded); i++ {
		c := encoded[i]
		if c != '%' {
			b.WriteByte(c)
			continue
		}
		if i+2 >= len(encoded) {
			return "", fmt.Errorf("digest: invalid percent encoding in username*: %q", value)
		}
		n, err := strconv.ParseUint(encoded[i+1:i+3], 16, 8)
		if err != nil {
			return "", fmt.Errorf("digest: invalid percent encoding in username*: %q", value)
		}
		b.WriteByte(byte(n))
		i += 2
	}
	username := b.String()
	if !utf8.ValidString(username) {
		return "", fmt.Errorf("digest: username* is not valid UTF-8: %q", value)
	}
	return username, nil
}
