package fastjson

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// chunkedReader returns at most chunkSize bytes per Read call.
type chunkedReader struct {
	s         string
	chunkSize int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if len(r.s) == 0 {
		return 0, io.EOF
	}
	n := r.chunkSize
	if n > len(r.s) {
		n = len(r.s)
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, r.s[:n])
	r.s = r.s[n:]
	return n, nil
}

// dataAndEOFReader returns all the remaining data together with io.EOF
// on the first Read call.
type dataAndEOFReader struct {
	s string
}

func (r *dataAndEOFReader) Read(p []byte) (int, error) {
	if len(r.s) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.s)
	r.s = r.s[n:]
	return n, io.EOF
}

// errReader returns err after the given number of bytes.
type errReader struct {
	s   string
	err error
}

func (r *errReader) Read(p []byte) (int, error) {
	if len(r.s) == 0 {
		return 0, r.err
	}
	n := copy(p, r.s)
	r.s = r.s[n:]
	return n, nil
}

// countingReader counts Read calls and bytes read.
type countingReader struct {
	r     io.Reader
	reads int
	bytes int
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.reads++
	r.bytes += n
	return n, err
}

func scanAll(t *testing.T, sc *ReaderScanner) string {
	t.Helper()
	var bb bytes.Buffer
	for sc.Next() {
		fmt.Fprintf(&bb, "%s", sc.Value())
	}
	return bb.String()
}

func TestReaderScanner(t *testing.T) {
	var sc ReaderScanner

	t.Run("success", func(t *testing.T) {
		sc.Init(strings.NewReader(`[] {} "" 123`))
		s := scanAll(t, &sc)
		if err := sc.Error(); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if s != `[]{}""123` {
			t.Fatalf("unexpected string obtained; got %q; want %q", s, `[]{}""123`)
		}
	})

	t.Run("adjacent-containers", func(t *testing.T) {
		sc.Init(strings.NewReader(`[]{}["a"]{"k":"v"}"str" 1 2.5 true false null`))
		s := scanAll(t, &sc)
		if err := sc.Error(); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		want := `[]{}["a"]{"k":"v"}"str"12.5truefalsenull`
		if s != want {
			t.Fatalf("unexpected string obtained; got %q; want %q", s, want)
		}
	})

	t.Run("scalar-at-eof", func(t *testing.T) {
		sc.Init(strings.NewReader(`1.5e+10`))
		s := scanAll(t, &sc)
		if err := sc.Error(); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if s != `1.5e+10` {
			t.Fatalf("unexpected string obtained; got %q; want %q", s, `1.5e+10`)
		}
	})

	t.Run("trailing-whitespace", func(t *testing.T) {
		sc.Init(strings.NewReader("123 \n\t\r "))
		s := scanAll(t, &sc)
		if err := sc.Error(); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if s != `123` {
			t.Fatalf("unexpected string obtained; got %q; want %q", s, `123`)
		}
	})

	t.Run("empty", func(t *testing.T) {
		sc.Init(strings.NewReader(""))
		if sc.Next() {
			t.Fatalf("Next must return false")
		}
		if err := sc.Error(); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
	})

	t.Run("tiny-chunks", func(t *testing.T) {
		// Escapes, exponents and nesting must be handled correctly
		// even when the reader returns one or two bytes at a time.
		input := `{"a\"b\\c": [1, 1.5e-10, {"d": "x}]"}], "u\ty": 7} ` +
			`"esc\"aped\\" 2.5E+4 [[]] -12`
		want := `{"a\"b\\c":[1,1.5e-10,{"d":"x}]"}],"u\ty":7}` +
			`"esc\"aped\\"2.5E+4[[]]-12`
		for _, chunkSize := range []int{1, 2, 3} {
			sc.Init(&chunkedReader{s: input, chunkSize: chunkSize})
			s := scanAll(t, &sc)
			if err := sc.Error(); err != nil {
				t.Fatalf("chunkSize=%d: unexpected error: %s", chunkSize, err)
			}
			if s != want {
				t.Fatalf("chunkSize=%d: unexpected string obtained; got %q; want %q", chunkSize, s, want)
			}
		}
	})

	t.Run("data-with-eof", func(t *testing.T) {
		// The data returned together with io.EOF must be consumed
		// before treating the stream as finished.
		sc.Init(&dataAndEOFReader{s: `[1,2] "x" 42`})
		s := scanAll(t, &sc)
		if err := sc.Error(); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if s != `[1,2]"x"42` {
			t.Fatalf("unexpected string obtained; got %q; want %q", s, `[1,2]"x"42`)
		}
	})

	t.Run("truncated", func(t *testing.T) {
		for _, input := range []string{
			`{"a": [1, 2`,
			`[1, 2`,
			`"abc`,
			`"abc\`,
			`{"a": 1} [2`,
		} {
			sc.Init(strings.NewReader(input))
			for sc.Next() {
			}
			err := sc.Error()
			if err == nil {
				t.Fatalf("input %q: expecting non-nil error", input)
			}
			if !strings.Contains(err.Error(), "truncated") {
				t.Fatalf("input %q: unexpected error; got %q; want truncation error", input, err)
			}
			if sc.Next() {
				t.Fatalf("input %q: Next must return false after error", input)
			}
		}
	})

	t.Run("read-error", func(t *testing.T) {
		errTest := errors.New("test read error")
		sc.Init(&errReader{s: `[1, 2`, err: errTest})
		for sc.Next() {
		}
		err := sc.Error()
		if err == nil {
			t.Fatalf("expecting non-nil error")
		}
		if !errors.Is(err, errTest) {
			t.Fatalf("unexpected error; got %q; want it to wrap %q", err, errTest)
		}
	})

	t.Run("parse-error", func(t *testing.T) {
		sc.Init(strings.NewReader(`[] sdfdsfdf`))
		for sc.Next() {
		}
		if err := sc.Error(); err == nil {
			t.Fatalf("expecting non-nil error")
		}
		if sc.Next() {
			t.Fatalf("Next must return false")
		}
	})

	t.Run("max-value-size", func(t *testing.T) {
		sc.Init(strings.NewReader(`[1,2,3] "abcdef" 42`))
		sc.MaxValueSize = 7
		if !sc.Next() {
			t.Fatalf("Next must return true for the first value")
		}
		if s := sc.Value().String(); s != `[1,2,3]` {
			t.Fatalf("unexpected value; got %q; want %q", s, `[1,2,3]`)
		}
		if sc.Next() {
			t.Fatalf("Next must return false on too big value")
		}
		err := sc.Error()
		if err == nil {
			t.Fatalf("expecting non-nil error")
		}
		if !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("unexpected error; got %q; want size limit error", err)
		}
	})

	t.Run("max-value-size-truncated-input", func(t *testing.T) {
		// The limit must be enforced even before the value is complete.
		sc.Init(strings.NewReader(`"` + strings.Repeat("a", 100)))
		sc.MaxValueSize = 10
		if sc.Next() {
			t.Fatalf("Next must return false on too big value")
		}
		if err := sc.Error(); err == nil {
			t.Fatalf("expecting non-nil error")
		}
	})

	t.Run("stop-early", func(t *testing.T) {
		// After the caller stops iterating, the scanner must not read
		// the remaining input.
		tail := strings.Repeat(" ", readerScannerBufSize) + strings.Repeat("1", 100000)
		cr := &countingReader{r: strings.NewReader(`{"a":1} ` + tail)}
		sc.Init(cr)
		if !sc.Next() {
			t.Fatalf("Next must return true")
		}
		if s := sc.Value().String(); s != `{"a":1}` {
			t.Fatalf("unexpected value; got %q; want %q", s, `{"a":1}`)
		}
		reads := cr.reads
		if cr.bytes >= len(tail) {
			t.Fatalf("the scanner read too much input; got %d bytes", cr.bytes)
		}
		// Stop iterating. The number of reads must not grow.
		if cr.reads != reads {
			t.Fatalf("unexpected reads after stopping iteration")
		}
		if err := sc.Error(); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
	})

	t.Run("reuse", func(t *testing.T) {
		for i := range 3 {
			sc.Init(strings.NewReader(fmt.Sprintf(`[%d] "%d"`, i, i)))
			s := scanAll(t, &sc)
			if err := sc.Error(); err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			want := fmt.Sprintf(`[%d]"%d"`, i, i)
			if s != want {
				t.Fatalf("unexpected string obtained; got %q; want %q", s, want)
			}
		}
	})
}
