package message

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
)

type UnknownCharsetError struct {
	e error
}

func (u UnknownCharsetError) Unwrap() error { return u.e }

func (u UnknownCharsetError) Error() string {
	return "unknown charset: " + u.e.Error()
}

// IsUnknownCharset returns a boolean indicating whether the error is known to
// report that the charset advertised by the entity is unknown.
func IsUnknownCharset(err error) bool {
	return errors.As(err, new(UnknownCharsetError))
}

// CharsetReader, if non-nil, defines a function to generate charset-conversion
// readers, converting from the provided charset into UTF-8. Charsets are always
// lower-case. utf-8 and us-ascii charsets are handled by default. One of the
// the CharsetReader's result values must be non-nil.
//
// Importing github.com/emersion/go-message/charset will set CharsetReader to
// a function that handles most common charsets. Alternatively, CharsetReader
// can be set to e.g. golang.org/x/net/html/charset.NewReaderLabel.
var CharsetReader func(charset string, input io.Reader) (io.Reader, error)

// charsetReader calls CharsetReader if non-nil.
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	charset = strings.ToLower(charset)
	if charset == "utf-8" || charset == "us-ascii" {
		return input, nil
	}
	if CharsetReader != nil {
		r, err := CharsetReader(charset, input)
		if err != nil {
			return r, UnknownCharsetError{err}
		}
		return r, nil
	}
	return input, UnknownCharsetError{fmt.Errorf("message: unhandled charset %q", charset)}
}

// decodeHeader decodes an internationalized header field. If it fails, it
// returns the input string and the error.
func decodeHeader(s string) (string, error) {
	wordDecoder := mime.WordDecoder{CharsetReader: charsetReader}
	dec, err := wordDecoder.DecodeHeader(s)
	if err != nil {
		return s, err
	}
	return dec, nil
}

// A HeaderDiagnostic describes an issue encountered while decoding a header
// field with DecodeHeaderRecover.
type HeaderDiagnostic struct {
	// Start and End are byte offsets in the original header field value
	// delimiting the fragment that could not be decoded.
	Start, End int
	// Err is the error that occurred while decoding the fragment.
	Err error
}

// Error formats the diagnostic.
func (d HeaderDiagnostic) Error() string {
	return fmt.Sprintf("message: cannot decode header field fragment at bytes %d-%d: %v", d.Start, d.End, d.Err)
}

// Unwrap returns the underlying decoding error.
func (d HeaderDiagnostic) Unwrap() error { return d.Err }

// DecodeHeaderRecover decodes an internationalized header field, recovering
// from malformed encoded-words. Fragments that cannot be decoded are kept
// verbatim in the returned string and reported in the returned diagnostics.
// Valid fragments are always decoded, even when adjacent fragments are
// broken. Whitespace between adjacent encoded-words is dropped, as specified
// by RFC 2047.
//
// Diagnostic positions are byte offsets in the original header field value,
// not in the decoded string.
func DecodeHeaderRecover(s string) (string, []HeaderDiagnostic) {
	wordDecoder := mime.WordDecoder{CharsetReader: charsetReader}

	var sb strings.Builder
	var diags []HeaderDiagnostic
	pos := 0
	prevWord := false
	for pos < len(s) {
		rel := strings.Index(s[pos:], "=?")
		if rel < 0 {
			sb.WriteString(s[pos:])
			break
		}
		start := pos + rel
		end, ok := encodedWordEnd(s, start)
		if !ok {
			// Not an encoded-word: copy "=?" literally and move on.
			sb.WriteString(s[pos : start+2])
			prevWord = false
			pos = start + 2
			continue
		}

		plain := s[pos:start]
		if prevWord && isLWSP(plain) {
			// Whitespace between two encoded-words is not displayed.
			plain = ""
		}
		sb.WriteString(plain)

		word := s[start:end]
		dec, err := wordDecoder.Decode(word)
		if err != nil {
			diags = append(diags, HeaderDiagnostic{Start: start, End: end, Err: err})
			sb.WriteString(word)
		} else {
			sb.WriteString(dec)
		}
		prevWord = true
		pos = end
	}
	return sb.String(), diags
}

// encodedWordEnd returns the offset just past the end of the encoded-word
// starting at s[start]. s[start:] must begin with "=?". It reports false if
// there is no complete encoded-word at this position.
func encodedWordEnd(s string, start int) (int, bool) {
	body := s[start+2:]
	qmarks := 0
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			// Encoded-words cannot contain whitespace.
			return 0, false
		}
		if c == '?' {
			qmarks++
			// An encoded-word is "=?charset?encoding?text?=", so the
			// closing "?=" is preceded by at least 3 question marks.
			if qmarks >= 3 && i+1 < len(body) && body[i+1] == '=' {
				return start + 2 + i + 2, true
			}
		}
	}
	return 0, false
}

// isLWSP reports whether s is non-empty and contains only linear whitespace.
func isLWSP(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\r', '\n':
		default:
			return false
		}
	}
	return true
}

func encodeHeader(s string) string {
	return mime.QEncoding.Encode("utf-8", s)
}
