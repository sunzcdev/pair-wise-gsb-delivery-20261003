package message

import (
	"fmt"
	"mime"
	"sort"
	"strings"
)

// DefaultParameterLineLen is the default maximum length, in bytes, of a
// physical header line (excluding the trailing CRLF) produced for parameters
// that are split into RFC 2231 continuations.
const DefaultParameterLineLen = 76

// An Option configures how a Header field or a Writer formats messages.
type Option func(*options)

type options struct {
	paramLineLen int
}

func resolveOptions(opts []Option) options {
	o := options{paramLineLen: DefaultParameterLineLen}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// WithParameterLineLen sets the maximum length, in bytes, of a physical header
// line (excluding the trailing CRLF) produced for parameters that use RFC 2231
// continuations. Values below 1 fall back to the default length.
func WithParameterLineLen(n int) Option {
	return func(o *options) {
		if n >= 1 {
			o.paramLineLen = n
		}
	}
}

const upperhex = "0123456789ABCDEF"

// isTSpecial reports whether c is a tspecial character, as defined in RFC 2045.
func isTSpecial(c byte) bool {
	switch c {
	case '(', ')', '<', '>', '@', ',', ';', ':', '\\', '"', '/', '[', ']', '?', '=':
		return true
	}
	return false
}

func isTokenChar(c byte) bool {
	return c > ' ' && c < 0x7F && !isTSpecial(c)
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

// needsEncoding reports whether s cannot be represented as a token or quoted
// parameter value and therefore requires RFC 2231 extended encoding.
func needsEncoding(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] >= 0x7F {
			return true
		}
	}
	return false
}

// encode2231 percent-encodes s as an RFC 2231 extended parameter value.
// Encoding operates on UTF-8 bytes, so multi-byte runes are never split.
func encode2231(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch > ' ' && ch < 0x7F && ch != '*' && ch != '\'' && ch != '%' && !isTSpecial(ch) {
			b.WriteByte(ch)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upperhex[ch>>4])
		b.WriteByte(upperhex[ch&0x0F])
	}
	return b.String()
}

// asciiFallback reduces s to a plain ASCII parameter value, as recommended by
// RFC 2231 section 4.3. Each non-ASCII rune is replaced by an underscore.
func asciiFallback(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x80 {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// consumeEncoded returns the longest RFC 2231 encoded prefix of enc that fits
// within maxLen bytes and the unconsumed rest. Cuts only happen on
// percent-encoding triplet boundaries, so escape sequences are never split.
func consumeEncoded(enc string, maxLen int) (piece, rest string) {
	if maxLen < 0 {
		maxLen = 0
	}
	end := 0
	for end < len(enc) {
		next := end + 1
		if enc[end] == '%' {
			next = end + 3
		}
		if next > maxLen {
			break
		}
		end = next
	}
	return enc[:end], enc[end:]
}

// quoteASCII wraps s in double quotes, escaping quotes and backslashes.
func quoteASCII(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
	return b.String()
}

// validMediaType validates and lowercases a media type or disposition value.
func validMediaType(t string) (string, bool) {
	major, sub, hasSlash := strings.Cut(t, "/")
	if !hasSlash {
		if !isToken(major) {
			return "", false
		}
		return strings.ToLower(major), true
	}
	if !isToken(major) || !isToken(sub) {
		return "", false
	}
	return strings.ToLower(major) + "/" + strings.ToLower(sub), true
}

// mediaFieldBuilder accumulates a media-type header field while folding
// physical lines within a byte budget. Continuation lines start with a
// folding space; a parameter separator is emitted as a trailing ";" on the
// previous line, so unfolding reconstructs "token; token".
type mediaFieldBuilder struct {
	lineLen int
	lines   []string
	cur     string
}

// breakLine closes the current line and opens a continuation line. A ";" is
// appended to the closed line only when it does not already end with one, so
// segment boundaries never produce duplicated separators.
func (b *mediaFieldBuilder) breakLine() {
	if !strings.HasSuffix(b.cur, ";") {
		b.cur += ";"
	}
	b.lines = append(b.lines, b.cur)
	b.cur = " "
}

// reserveToken ensures room on b.cur for leadOverhead fixed bytes plus at
// least dataLen payload bytes. When moreData is true one extra byte is kept
// free for the trailing ";". The "; " lead-in is applied on the current line
// when it fits; otherwise a continuation line is opened (no lead-in). It
// returns the payload room.
func (b *mediaFieldBuilder) reserveToken(leadOverhead, dataLen int, moreData bool) int {
	reserve := 0
	if moreData {
		reserve = 1
	}
	if b.lineLen-len(b.cur)-len("; ")-leadOverhead-reserve >= dataLen {
		b.cur += "; "
		return b.lineLen - len(b.cur) - leadOverhead - reserve
	}
	b.breakLine()
	return b.lineLen - len(b.cur) - leadOverhead - reserve
}

// emitPlain appends a whole, unsplittable parameter token (plain value or
// quoted fallback). The token is always kept intact on a single physical
// line: if it does not fit on the current line it is moved to a fresh
// continuation line. A token longer than an entire line (for example a
// multipart boundary under an unusually small budget) overflows rather than
// being split, since splitting an unquoted token is not representable.
func (b *mediaFieldBuilder) emitPlain(token string) {
	room := b.reserveToken(0, len(token), false)
	if len(token) <= room {
		b.cur += token
		return
	}
	// A fresh continuation line was already opened by reserveToken.
	b.cur += token
}

// emitSegment appends one RFC 2231 segment with the given prefix, packing
// percent-encoded atoms up to the line budget. When more encoded data remains
// it reserves the trailing ";" by shedding the final atom if necessary. It
// returns the unconsumed encoded data.
func (b *mediaFieldBuilder) emitSegment(prefix, enc string) string {
	// Always reserve a byte for the closing ";"; when this segment turns out
	// to be the last one that byte is left unused.
	room := b.reserveToken(len(prefix), 1, true)
	piece, rest := consumeEncoded(enc, room)
	if piece == "" {
		// Even one atom does not fit next to the prefix (absurdly small
		// budget); emit the first atom unconditionally to guarantee progress.
		piece, rest = consumeEncoded(enc, len(enc))
	}

	b.cur += prefix + piece
	if rest != "" {
		b.cur += ";"
	}
	return rest
}

func (b *mediaFieldBuilder) string() string {
	return strings.Join(append(b.lines, b.cur), "\r\n")
}

// formatMediaTypeField formats a media type (or disposition) value and its
// parameters as a folded header field value, using RFC 2231 continuations for
// parameters that require extended encoding. Every physical line (excluding
// the trailing CRLF) is at most lineLen bytes, except when the budget is too
// small to hold a single RFC 2231 segment prefix.
func formatMediaTypeField(mediaType string, params map[string]string, key string, lineLen int) (string, error) {
	t, ok := validMediaType(mediaType)
	if !ok {
		return "", fmt.Errorf("message: invalid media type %q", mediaType)
	}
	if lineLen < 1 {
		lineLen = DefaultParameterLineLen
	}

	b := &mediaFieldBuilder{lineLen: lineLen, cur: key + ": " + t}

	names := make([]string, 0, len(params))
	for name := range params {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		value := params[name]
		if strings.ContainsAny(name+value, "\r\n") {
			return "", fmt.Errorf("message: parameter %q contains line breaks", name)
		}
		name = strings.ToLower(name)
		if !isToken(name) {
			return "", fmt.Errorf("message: invalid parameter name %q", name)
		}

		switch {
		case !needsEncoding(value) && isToken(value):
			b.emitPlain(name + "=" + value)
		case !needsEncoding(value):
			b.emitPlain(name + "=" + quoteASCII(value))
		default:
			// RFC 2231 section 4.3 recommends a plain ASCII fallback.
			if fallback := asciiFallback(value); fallback != "" && fallback != value {
				b.emitPlain(truncateFallback(name, fallback, lineLen))
			}

			enc := encode2231(value)
			unsegmented := name + "*=utf-8''" + enc
			// Use the unsegmented form whenever it fits on one line.
			if len(" ")+len(unsegmented) <= lineLen {
				b.emitPlain(unsegmented)
				continue
			}

			rest := enc
			for index := 0; rest != ""; index++ {
				prefix := fmt.Sprintf("%s*%d*=", name, index)
				if index == 0 {
					prefix = name + "*0*=utf-8''"
				}
				rest = b.emitSegment(prefix, rest)
			}
		}
	}

	return b.string(), nil
}

// truncateFallback builds name="value" sized to fit a fresh continuation line
// within lineLen bytes, accounting for backslashes inserted before quotes and
// backslashes.
func truncateFallback(name, value string, lineLen int) string {
	budget := lineLen - len(" ") - len(name) - len("=") - len(`""`) - 1
	var out strings.Builder
	for i := 0; i < len(value) && budget > 0; i++ {
		c := value[i]
		cost := 1
		if c == '"' || c == '\\' {
			cost = 2
		}
		if cost > budget {
			break
		}
		if cost == 2 {
			out.WriteByte('\\')
		}
		out.WriteByte(c)
		budget -= cost
	}
	return name + "=\"" + out.String() + "\""
}

// setMediaTypeField formats and stores a media-type header field as a raw,
// pre-folded field. It falls back to permissive formatting on errors.
func (h *Header) setMediaTypeField(key, mediaType string, params map[string]string, opts []Option) {
	lineLen := resolveOptions(opts).paramLineLen
	formatted, err := formatMediaTypeField(mediaType, params, key, lineLen)
	if err != nil {
		formatted = mime.FormatMediaType(mediaType, params)
	}
	h.Del(key)
	h.SetRaw([]byte(formatted + "\r\n"))
}
