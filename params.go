package message

import (
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// DefaultParamLineBudget is the default maximum length, in bytes, of a
// header field line produced when formatting Content-Type and
// Content-Disposition parameters. Non-ASCII parameter values that don't fit
// are split into RFC 2231 continuation segments.
const DefaultParamLineBudget = 76

const upperhex = "0123456789ABCDEF"

// isTokenChar reports whether c is an RFC 2045 token character.
func isTokenChar(c byte) bool {
	if c <= ' ' || c >= 0x7f {
		return false
	}
	switch c {
	case '(', ')', '<', '>', '@', ',', ';', ':', '\\', '"', '/', '[', ']', '?', '=':
		return false
	}
	return true
}

func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isTokenChar(s[i]) {
			return false
		}
	}
	return true
}

// isAttrChar reports whether c is an RFC 2231 attr-char.
func isAttrChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '!', '#', '$', '&', '+', '-', '.', '^', '_', '`', '{', '|', '}', '~':
		return true
	}
	return false
}

// paramNeedsEncoding reports whether s contains bytes that cannot appear
// directly in a header parameter value.
func paramNeedsEncoding(s string) bool {
	for i := 0; i < len(s); i++ {
		if (s[i] < ' ' || s[i] > '~') && s[i] != '\t' {
			return true
		}
	}
	return false
}

// formatPlainParam formats a single parameter as a name=value pair, quoting
// and escaping the value when it is not a token.
func formatPlainParam(name, value string) string {
	if isToken(value) {
		return name + "=" + value
	}
	var sb strings.Builder
	sb.WriteString(name)
	sb.WriteString(`="`)
	for i := 0; i < len(value); i++ {
		if c := value[i]; c == '"' || c == '\\' {
			sb.WriteByte('\\')
		}
		sb.WriteByte(value[i])
	}
	sb.WriteByte('"')
	return sb.String()
}

// encode2231Units percent-encodes s per RFC 2231 and returns the encoded
// form as a list of units. Each unit holds a single character: either one
// attr-char byte or the %XX triplets of one (possibly multi-byte) character.
// Units are atomic: a continuation segment boundary never falls inside a
// unit, so %XX triplets and multi-byte characters are never split.
func encode2231Units(s string) []string {
	var units []string
	for i := 0; i < len(s); {
		if b := s[i]; b < utf8.RuneSelf && isAttrChar(b) {
			units = append(units, s[i:i+1])
			i++
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		var sb strings.Builder
		for j := i; j < i+size; j++ {
			sb.WriteByte('%')
			sb.WriteByte(upperhex[s[j]>>4])
			sb.WriteByte(upperhex[s[j]&0x0F])
		}
		units = append(units, sb.String())
		i += size
	}
	return units
}

// format2231Pieces formats value as RFC 2231 extended parameter pieces: a
// single name*= piece carrying the charset declaration if it fits in
// pieceBudget bytes, or name*N*= continuation segments otherwise. Segment
// sizes are counted in encoded bytes. The first continuation segment
// carries the charset declaration.
func format2231Pieces(name, value string, pieceBudget int) []string {
	units := encode2231Units(value)
	if single := name + "*=utf-8''" + strings.Join(units, ""); len(single) <= pieceBudget {
		return []string{single}
	}

	var pieces []string
	for n, i := 0, 0; i < len(units); n++ {
		prefix := name + "*" + strconv.Itoa(n) + "*="
		if n == 0 {
			prefix += "utf-8''"
		}
		segBudget := pieceBudget - len(prefix)
		if segBudget < 1 {
			segBudget = 1 // guarantee progress on degenerate budgets
		}
		var seg strings.Builder
		for i < len(units) && (seg.Len() == 0 || seg.Len()+len(units[i]) <= segBudget) {
			seg.WriteString(units[i])
			i++
		}
		pieces = append(pieces, prefix+seg.String())
	}
	return pieces
}

// fallbackWordBudget returns the maximum encoded-word length for the plain
// fallback parameter, so that a word glued to its name=" prefix still fits
// on a folded line.
func fallbackWordBudget(name string, budget int) int {
	const minWordLen = len("=?UTF-8?Q??=") + 1
	maxWord := budget - len(name) - len(`="`) - 1 // leading WSP on folded lines
	if maxWord > 75 {
		maxWord = 75 // RFC 2047 maximum encoded-word length
	}
	if maxWord < minWordLen {
		maxWord = minWordLen
	}
	return maxWord
}

// encodeParamFallback encodes s as a sequence of RFC 2047 encoded-words
// joined by spaces, each at most maxWord bytes long. Double quotes and
// backslashes are escaped so that the result is safe to embed in a
// quoted-string, and multi-byte characters are never split across words.
func encodeParamFallback(s string, maxWord int) string {
	const (
		prefix = "=?UTF-8?Q?"
		suffix = "?="
	)
	contentLimit := maxWord - len(prefix) - len(suffix)
	if contentLimit < 1 {
		contentLimit = 1
	}

	var words []string
	var cur strings.Builder
	curLen := 0
	flush := func() {
		if curLen > 0 {
			words = append(words, prefix+cur.String()+suffix)
			cur.Reset()
			curLen = 0
		}
	}
	for i := 0; i < len(s); {
		var enc string
		size := 1
		switch b := s[i]; {
		case b == ' ':
			enc = "_"
		case b == '"' || b == '\\':
			// Keep the encoded-word safe to embed in a quoted-string
			enc = "=" + string(upperhex[b>>4]) + string(upperhex[b&0x0F])
		case b >= '!' && b <= '~' && b != '=' && b != '?' && b != '_':
			enc = s[i : i+1]
		default:
			_, size = utf8.DecodeRuneInString(s[i:])
			var sb strings.Builder
			for j := i; j < i+size; j++ {
				sb.WriteByte('=')
				sb.WriteByte(upperhex[s[j]>>4])
				sb.WriteByte(upperhex[s[j]&0x0F])
			}
			enc = sb.String()
		}
		if curLen > 0 && curLen+len(enc) > contentLimit {
			flush()
		}
		cur.WriteString(enc)
		curLen += len(enc)
		i += size
	}
	flush()
	return strings.Join(words, " ")
}

// formatParams formats a Content-Type or Content-Disposition header field
// value with its parameters. Parameter values that need encoding are
// emitted both as a plain RFC 2047 encoded parameter (a fallback for
// receivers without RFC 2231 support) and as RFC 2231 extended parameter
// pieces, split into continuation segments when they exceed the line
// budget. If budget is negative, RFC 2231 formatting is disabled.
func formatParams(f string, params map[string]string, budget int) string {
	names := make([]string, 0, len(params))
	for name := range params {
		names = append(names, name)
	}
	sort.Strings(names)

	var sb strings.Builder
	sb.WriteString(strings.ToLower(f))
	for _, name := range names {
		value := params[name]
		name = strings.ToLower(name)
		sb.WriteString("; ")
		if budget >= 0 && paramNeedsEncoding(value) {
			// Plain fallback first: receivers that understand RFC 2231
			// prefer the extended form and still get the complete value
			sb.WriteString(formatPlainParam(name, encodeParamFallback(value, fallbackWordBudget(name, budget))))
			for _, piece := range format2231Pieces(name, value, budget-2) {
				sb.WriteString("; ")
				sb.WriteString(piece)
			}
			continue
		}
		sb.WriteString(formatPlainParam(name, encodeHeader(value)))
	}
	return sb.String()
}

// foldParamsField builds a raw header field, folding value at spaces so
// that every line is at most budget bytes long (including the key prefix on
// the first line). Tokens longer than the budget are placed on their own
// line.
func foldParamsField(key, value string, budget int) []byte {
	var sb strings.Builder
	sb.WriteString(key)
	sb.WriteByte(':')
	lineLen := len(key) + 1
	for i, token := range strings.Split(value, " ") {
		if i > 0 && lineLen+1+len(token) > budget {
			sb.WriteString("\r\n")
			lineLen = 0
		}
		sb.WriteByte(' ')
		sb.WriteString(token)
		lineLen += 1 + len(token)
	}
	sb.WriteString("\r\n")
	return []byte(sb.String())
}
