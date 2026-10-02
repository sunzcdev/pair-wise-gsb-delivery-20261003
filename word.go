package message

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

var errInvalidWord = errors.New("message: invalid RFC 2047 encoded-word")

// A WordError describes a single RFC 2047 encoded-word in a header field
// that could not be decoded.
type WordError struct {
	// Offset is the byte offset of the encoded-word in the original header
	// field value.
	Offset int
	// Length is the length in bytes of the encoded-word in the original
	// header field value.
	Length int
	// Err is the error that prevented the encoded-word from being decoded.
	Err error
}

func (e WordError) Error() string {
	return fmt.Sprintf("message: cannot decode encoded-word at byte offset %d: %v", e.Offset, e.Err)
}

func (e WordError) Unwrap() error { return e.Err }

// DecodeHeaderRecovering decodes an internationalized header field containing
// RFC 2047 encoded-words.
//
// Unlike decodeHeader, it recovers from undecodable encoded-words: they are
// kept verbatim in the returned string and reported in the returned
// diagnostics. WordError positions are byte offsets in s, the original
// header field value, not in the decoded string. Valid encoded-words and
// plain text are always preserved. Whitespace between two adjacent
// successfully decoded encoded-words is dropped, as per RFC 2047.
func DecodeHeaderRecovering(s string) (string, []WordError) {
	// If there is no encoded-word, return before creating a buffer.
	i := strings.Index(s, "=?")
	if i == -1 {
		return s, nil
	}

	var buf strings.Builder
	var errs []WordError

	buf.WriteString(s[:i])
	s = s[i:]
	base := i // byte offset of s[0] in the original header field value

	betweenWords := false
	for {
		start := strings.Index(s, "=?")
		if start == -1 {
			break
		}
		cur := start + len("=?")

		i := strings.Index(s[cur:], "?")
		if i == -1 {
			break
		}
		charset := s[cur : cur+i]
		cur += i + len("?")

		if len(s) < cur+len("Q??=") {
			break
		}
		encoding := s[cur]
		cur++

		if s[cur] != '?' {
			break
		}
		cur++

		j := strings.Index(s[cur:], "?=")
		if j == -1 {
			break
		}
		text := s[cur : cur+j]
		end := cur + j + len("?=")

		content, err := decodeWord(charset, encoding, text)
		if err != nil {
			errs = append(errs, WordError{
				Offset: base + start,
				Length: end - start,
				Err:    err,
			})
			betweenWords = false
			buf.WriteString(s[:end])
			base += end
			s = s[end:]
			continue
		}

		// Write characters before the encoded-word. Whitespace separating
		// two encoded-words must be deleted.
		if start > 0 && (!betweenWords || hasNonWhitespace(s[:start])) {
			buf.WriteString(s[:start])
		}
		buf.WriteString(content)

		base += end
		s = s[end:]
		betweenWords = true
	}

	buf.WriteString(s)

	return buf.String(), errs
}

// decodeWord decodes the content of a single encoded-word and converts it
// from charset to UTF-8.
func decodeWord(charset string, encoding byte, text string) (string, error) {
	var raw []byte
	var err error
	switch encoding {
	case 'B', 'b':
		raw, err = base64.StdEncoding.DecodeString(text)
	case 'Q', 'q':
		raw, err = qDecodeWord(text)
	default:
		err = errInvalidWord
	}
	if err != nil {
		return "", err
	}

	return convertWordCharset(charset, raw)
}

// convertWordCharset converts decoded encoded-word content from charset to
// UTF-8. It handles the same charsets as mime.WordDecoder and falls back to
// charsetReader for the rest.
func convertWordCharset(charset string, content []byte) (string, error) {
	switch strings.ToLower(charset) {
	case "utf-8":
		return string(content), nil
	case "iso-8859-1":
		runes := make([]rune, len(content))
		for i, c := range content {
			runes[i] = rune(c)
		}
		return string(runes), nil
	case "us-ascii":
		var buf strings.Builder
		buf.Grow(len(content))
		for _, c := range content {
			if c >= utf8.RuneSelf {
				buf.WriteRune(unicode.ReplacementChar)
			} else {
				buf.WriteByte(c)
			}
		}
		return buf.String(), nil
	default:
		r, err := charsetReader(charset, bytes.NewReader(content))
		if err != nil {
			return "", err
		}
		var buf strings.Builder
		if _, err := io.Copy(&buf, r); err != nil {
			return "", err
		}
		return buf.String(), nil
	}
}

// qDecodeWord decodes a Q-encoded string, as defined in RFC 2047.
func qDecodeWord(s string) ([]byte, error) {
	dec := make([]byte, len(s))
	n := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '_':
			dec[n] = ' '
		case c == '=':
			if i+2 >= len(s) {
				return nil, errInvalidWord
			}
			b, err := readHexByte(s[i+1], s[i+2])
			if err != nil {
				return nil, err
			}
			dec[n] = b
			i += 2
		case (c <= '~' && c >= ' ') || c == '\n' || c == '\r' || c == '\t':
			dec[n] = c
		default:
			return nil, errInvalidWord
		}
		n++
	}

	return dec[:n], nil
}

// readHexByte returns the byte from its quoted-printable representation.
func readHexByte(a, b byte) (byte, error) {
	hb, err := fromHexByte(a)
	if err != nil {
		return 0, err
	}
	lb, err := fromHexByte(b)
	if err != nil {
		return 0, err
	}
	return hb<<4 | lb, nil
}

func fromHexByte(b byte) (byte, error) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', nil
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, nil
	// Accept badly encoded bytes.
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, nil
	}
	return 0, fmt.Errorf("message: invalid hex byte %#02x", b)
}

// hasNonWhitespace reports whether s (assumed to be ASCII) contains at least
// one byte of non-whitespace.
func hasNonWhitespace(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		// Encoded-words can only be separated by linear whitespace which does
		// not include vertical tabs (\v).
		case ' ', '\t', '\n', '\r':
		default:
			return true
		}
	}
	return false
}
