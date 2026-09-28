package dkim

import (
	"fmt"
	"io"
	"strings"
)

// Canonicalization is a canonicalization algorithm.
type Canonicalization string

const (
	CanonicalizationSimple  Canonicalization = "simple"
	CanonicalizationRelaxed                  = "relaxed"
)

type canonicalizer interface {
	CanonicalizeHeader(s string) string
	CanonicalizeBody(w io.Writer, d *diagnostics) io.WriteCloser
}

var canonicalizers = map[Canonicalization]canonicalizer{
	CanonicalizationSimple:  new(simpleCanonicalizer),
	CanonicalizationRelaxed: new(relaxedCanonicalizer),
}

// crlfFixer fixes any lone LF without a preceding CR.
type crlfFixer struct {
	cr bool
}

func (cf *crlfFixer) Fix(b []byte) []byte {
	res := make([]byte, 0, len(b))
	for _, ch := range b {
		prevCR := cf.cr
		cf.cr = false
		switch ch {
		case '\r':
			cf.cr = true
		case '\n':
			if !prevCR {
				res = append(res, '\r')
			}
		}
		res = append(res, ch)
	}
	return res
}

type simpleCanonicalizer struct{}

func (c *simpleCanonicalizer) CanonicalizeHeader(s string) string {
	return s
}

type simpleBodyCanonicalizer struct {
	w         io.Writer
	crlfBuf   []byte
	crlfFixer crlfFixer
	d         *diagnostics
	in        int
	out       int
}

func (c *simpleBodyCanonicalizer) Write(b []byte) (int, error) {
	written := len(b)
	b = append(c.crlfBuf, b...)

	b = c.crlfFixer.Fix(b)

	end := len(b)
	// If it ends with \r, maybe the next write will begin with \n
	if end > 0 && b[end-1] == '\r' {
		end--
	}
	// Keep all \r\n sequences
	for end >= 2 {
		prev := b[end-2]
		cur := b[end-1]
		if prev != '\r' || cur != '\n' {
			break
		}
		end -= 2
	}

	c.crlfBuf = b[end:]

	var err error
	if end > 0 {
		_, err = c.w.Write(b[:end])
	}

	c.d.emit(DiagnosticEvent{
		Stage:       DiagnosticStageBody,
		InputOffset: c.in,
		InputLength: written,
		OutputBytes: end,
		Summary:     "canonicalized body chunk",
	})
	c.in += written
	c.out += end

	return written, err
}

func (c *simpleBodyCanonicalizer) Close() error {
	dropped := countTrailingLineBreaks(c.crlfBuf)

	// Flush crlfBuf if it ends with a single \r (without a matching \n)
	if len(c.crlfBuf) > 0 && c.crlfBuf[len(c.crlfBuf)-1] == '\r' {
		if _, err := c.w.Write(c.crlfBuf); err != nil {
			return err
		}
		c.out += len(c.crlfBuf)
	}
	c.crlfBuf = nil

	if _, err := c.w.Write([]byte(crlf)); err != nil {
		return err
	}
	c.out += len(crlf)

	c.d.emit(DiagnosticEvent{
		Stage:       DiagnosticStageBody,
		InputOffset: 0,
		InputLength: c.in,
		OutputBytes: c.out,
		Summary:     fmt.Sprintf("simple body: discarded %d trailing line break(s), wrote final CRLF", dropped),
	})
	return nil
}

func (c *simpleCanonicalizer) CanonicalizeBody(w io.Writer, d *diagnostics) io.WriteCloser {
	return &simpleBodyCanonicalizer{w: w, d: d}
}

type relaxedCanonicalizer struct{}

func (c *relaxedCanonicalizer) CanonicalizeHeader(s string) string {
	k, v, ok := strings.Cut(s, ":")
	if !ok {
		return strings.TrimSpace(strings.ToLower(s)) + ":" + crlf
	}

	k = strings.TrimSpace(strings.ToLower(k))
	v = strings.Join(strings.FieldsFunc(v, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}), " ")
	return k + ":" + v + crlf
}

type relaxedBodyCanonicalizer struct {
	w         io.Writer
	crlfBuf   []byte
	wsp       bool
	written   bool
	crlfFixer crlfFixer
	d         *diagnostics
	in        int
	out       int
}

func (c *relaxedBodyCanonicalizer) Write(b []byte) (int, error) {
	written := len(b)

	b = c.crlfFixer.Fix(b)

	canonical := make([]byte, 0, len(b))
	for _, ch := range b {
		if ch == ' ' || ch == '\t' {
			c.wsp = true
		} else if ch == '\r' || ch == '\n' {
			c.wsp = false
			c.crlfBuf = append(c.crlfBuf, ch)
		} else {
			if len(c.crlfBuf) > 0 {
				canonical = append(canonical, c.crlfBuf...)
				c.crlfBuf = c.crlfBuf[:0]
			}
			if c.wsp {
				canonical = append(canonical, ' ')
				c.wsp = false
			}

			canonical = append(canonical, ch)
		}
	}

	if !c.written && len(canonical) > 0 {
		c.written = true
	}

	_, err := c.w.Write(canonical)

	c.d.emit(DiagnosticEvent{
		Stage:       DiagnosticStageBody,
		InputOffset: c.in,
		InputLength: written,
		OutputBytes: len(canonical),
		Summary:     "canonicalized body chunk",
	})
	c.in += written
	c.out += len(canonical)

	return written, err
}

func (c *relaxedBodyCanonicalizer) Close() error {
	dropped := countTrailingLineBreaks(c.crlfBuf)

	var summary string
	if c.written {
		if _, err := c.w.Write([]byte(crlf)); err != nil {
			return err
		}
		c.out += len(crlf)
		summary = fmt.Sprintf("relaxed body: discarded %d trailing line break(s)", dropped)
	} else {
		summary = "relaxed body: empty body, no output"
	}

	c.d.emit(DiagnosticEvent{
		Stage:       DiagnosticStageBody,
		InputOffset: 0,
		InputLength: c.in,
		OutputBytes: c.out,
		Summary:     summary,
	})
	return nil
}

func (c *relaxedCanonicalizer) CanonicalizeBody(w io.Writer, d *diagnostics) io.WriteCloser {
	return &relaxedBodyCanonicalizer{w: w, d: d}
}

// countTrailingLineBreaks counts the number of CRLF or lone LF line breaks
// in b. It is used to report how many trailing line breaks were discarded
// during body canonicalization.
func countTrailingLineBreaks(b []byte) int {
	n := 0
	for i := 0; i < len(b); i++ {
		if b[i] == '\r' && i+1 < len(b) && b[i+1] == '\n' {
			i++
		}
		if b[i] == '\r' || b[i] == '\n' {
			n++
		}
	}
	return n
}

type limitedWriter struct {
	W io.Writer
	N int64
}

func (w *limitedWriter) Write(b []byte) (int, error) {
	if w.N <= 0 {
		return len(b), nil
	}

	skipped := 0
	if int64(len(b)) > w.N {
		b = b[:w.N]
		skipped = int(int64(len(b)) - w.N)
	}

	n, err := w.W.Write(b)
	w.N -= int64(n)
	return n + skipped, err
}
