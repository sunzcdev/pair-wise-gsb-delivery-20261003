package fastjson

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// chunkReader returns data in chunks of chunkSize bytes and counts
// the number of Read calls.
type chunkReader struct {
	data      []byte
	chunkSize int
	reads     int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := r.chunkSize
	if n > len(r.data) {
		n = len(r.data)
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, r.data[:n])
	r.data = r.data[n:]
	r.reads++
	return n, nil
}

// eofWithDataReader returns all the remaining data together with io.EOF.
type eofWithDataReader struct {
	data []byte
}

func (r *eofWithDataReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, io.EOF
}

// errorReader returns data first and err afterwards.
type errorReader struct {
	data []byte
	err  error
}

func (r *errorReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

// errorWithDataReader returns data together with a non-EOF error.
type errorWithDataReader struct {
	data []byte
	err  error
}

func (r *errorWithDataReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, r.err
}

func readAllValues(t *testing.T, r *Reader) string {
	t.Helper()
	var bb bytes.Buffer
	for r.Next() {
		fmt.Fprintf(&bb, "%s", r.Value())
	}
	return bb.String()
}

func TestReader(t *testing.T) {
	var r Reader

	t.Run("success", func(t *testing.T) {
		r.Init(strings.NewReader(`[] {} "" 123`))
		s := readAllValues(t, &r)
		if err := r.Error(); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if s != `[]{}""123` {
			t.Fatalf("unexpected string obtained; got %q; want %q", s, `[]{}""123`)
		}
	})

	t.Run("adjacent-containers", func(t *testing.T) {
		r.Init(strings.NewReader(`{}[] {"a":1}[1,2]["x"]`))
		s := readAllValues(t, &r)
		if err := r.Error(); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if s != `{}[]{"a":1}[1,2]["x"]` {
			t.Fatalf("unexpected string obtained; got %q", s)
		}
	})

	t.Run("whitespace-separated-scalars", func(t *testing.T) {
		r.Init(strings.NewReader(`1 2.5 3e4 true false null "s"`))
		s := readAllValues(t, &r)
		if err := r.Error(); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if s != `12.53e4truefalsenull"s"` {
			t.Fatalf("unexpected string obtained; got %q", s)
		}
	})

	t.Run("scalar-at-eof", func(t *testing.T) {
		r.Init(strings.NewReader(`[1] 42`))
		s := readAllValues(t, &r)
		if err := r.Error(); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if s != `[1]42` {
			t.Fatalf("unexpected string obtained; got %q", s)
		}
	})

	t.Run("empty-input", func(t *testing.T) {
		r.Init(strings.NewReader(""))
		if r.Next() {
			t.Fatalf("Next must return false on empty input")
		}
		if err := r.Error(); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
	})

	t.Run("whitespace-only-input", func(t *testing.T) {
		r.Init(strings.NewReader("  \t\n "))
		if r.Next() {
			t.Fatalf("Next must return false on whitespace-only input")
		}
		if err := r.Error(); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
	})

	t.Run("error", func(t *testing.T) {
		r.Init(strings.NewReader(`[] sdfdsfdf`))
		for r.Next() {
		}
		if err := r.Error(); err == nil {
			t.Fatalf("expecting non-nil error")
		}
		if r.Next() {
			t.Fatalf("Next must return false")
		}
	})

	t.Run("reuse", func(t *testing.T) {
		for i := range 3 {
			r.Init(strings.NewReader(fmt.Sprintf(`[%d] "%d"`, i, i)))
			s := readAllValues(t, &r)
			if err := r.Error(); err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			want := fmt.Sprintf(`[%d]"%d"`, i, i)
			if s != want {
				t.Fatalf("unexpected string obtained; got %q; want %q", s, want)
			}
		}
	})
}

func TestReaderTinyChunks(t *testing.T) {
	// The input contains escapes, exponents and nested containers.
	input := `{"a":[{"b":[1,2,{"c":"d"}]}]}` + "\n" +
		`1.5e-10 "x\ny\"z" true null [3]{}` + "\r\n\t"
	want := `{"a":[{"b":[1,2,{"c":"d"}]}]}` +
		`1.5e-10"x\ny\"z"truenull[3]{}`

	for _, chunkSize := range []int{1, 2} {
		t.Run(fmt.Sprintf("chunk-%d", chunkSize), func(t *testing.T) {
			cr := &chunkReader{data: []byte(input), chunkSize: chunkSize}
			var r Reader
			r.Init(cr)
			s := readAllValues(t, &r)
			if err := r.Error(); err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			if s != want {
				t.Fatalf("unexpected string obtained; got %q; want %q", s, want)
			}
		})
	}
}

func TestReaderDataWithEOF(t *testing.T) {
	// The reader returns data together with io.EOF. The data must be
	// consumed before the EOF is handled.
	rdr := &eofWithDataReader{data: []byte(`[1] 42`)}
	var r Reader
	r.Init(rdr)
	s := readAllValues(t, &r)
	if err := r.Error(); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if s != `[1]42` {
		t.Fatalf("unexpected string obtained; got %q; want %q", s, `[1]42`)
	}
}

func TestReaderTruncated(t *testing.T) {
	for _, input := range []string{
		`{"a": [1, 2`,
		`[1, 2`,
		`"abc`,
		`{"a": "b`,
		`{} [`,
	} {
		t.Run(input, func(t *testing.T) {
			var r Reader
			r.Init(strings.NewReader(input))
			for r.Next() {
			}
			err := r.Error()
			if err == nil {
				t.Fatalf("expecting non-nil error on truncated input %q", input)
			}
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("unexpected error for truncated input %q: %v; want io.ErrUnexpectedEOF", input, err)
			}
			if r.Next() {
				t.Fatalf("Next must return false")
			}
		})
	}
}

func TestReaderReadError(t *testing.T) {
	errCustom := errors.New("custom read error")

	t.Run("error-after-data", func(t *testing.T) {
		var r Reader
		r.Init(&errorReader{data: []byte(`{"a":1} `), err: errCustom})
		if !r.Next() {
			t.Fatalf("Next must return true")
		}
		if s := r.Value().String(); s != `{"a":1}` {
			t.Fatalf("unexpected value: %q", s)
		}
		if r.Next() {
			t.Fatalf("Next must return false")
		}
		if err := r.Error(); !errors.Is(err, errCustom) {
			t.Fatalf("unexpected error: %v; want %v", err, errCustom)
		}
	})

	t.Run("error-mid-value", func(t *testing.T) {
		var r Reader
		r.Init(&errorReader{data: []byte(`{"a":`), err: errCustom})
		if r.Next() {
			t.Fatalf("Next must return false")
		}
		if err := r.Error(); !errors.Is(err, errCustom) {
			t.Fatalf("unexpected error: %v; want %v", err, errCustom)
		}
	})

	t.Run("error-with-data", func(t *testing.T) {
		// The data returned together with the error must be consumed first.
		var r Reader
		r.Init(&errorWithDataReader{data: []byte(`[1,2]`), err: errCustom})
		if !r.Next() {
			t.Fatalf("Next must return true")
		}
		if s := r.Value().String(); s != `[1,2]` {
			t.Fatalf("unexpected value: %q", s)
		}
		if r.Next() {
			t.Fatalf("Next must return false")
		}
		if err := r.Error(); !errors.Is(err, errCustom) {
			t.Fatalf("unexpected error: %v; want %v", err, errCustom)
		}
	})
}

func TestReaderMaxValueSize(t *testing.T) {
	t.Run("too-big-container", func(t *testing.T) {
		var r Reader
		r.MaxValueSize = 10
		r.Init(strings.NewReader(`{"a": "12345678901234567890"}`))
		if r.Next() {
			t.Fatalf("Next must return false")
		}
		err := r.Error()
		if err == nil {
			t.Fatalf("expecting non-nil error")
		}
		if !strings.Contains(err.Error(), "too big") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("too-big-scalar", func(t *testing.T) {
		var r Reader
		r.MaxValueSize = 5
		r.Init(strings.NewReader(`123456789`))
		if r.Next() {
			t.Fatalf("Next must return false")
		}
		if err := r.Error(); err == nil {
			t.Fatalf("expecting non-nil error")
		}
	})

	t.Run("fits", func(t *testing.T) {
		var r Reader
		r.MaxValueSize = 10
		r.Init(strings.NewReader(`[1,2,3] 12345`))
		s := readAllValues(t, &r)
		if err := r.Error(); err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if s != `[1,2,3]12345` {
			t.Fatalf("unexpected string obtained; got %q", s)
		}
	})

	t.Run("not-reset-by-init", func(t *testing.T) {
		var r Reader
		r.MaxValueSize = 3
		r.Init(strings.NewReader(`12345`))
		if r.Next() {
			t.Fatalf("Next must return false")
		}
		if err := r.Error(); err == nil {
			t.Fatalf("expecting non-nil error")
		}
	})
}

func TestReaderStopsReading(t *testing.T) {
	// The reader must not read the remaining input after the caller
	// stops iterating.
	first := `{"a":1}`
	tail := strings.Repeat(" ", 10000) + `[1,2,3]` + strings.Repeat("x", 10000)
	cr := &chunkReader{data: []byte(first + tail), chunkSize: 1}

	var r Reader
	r.Init(cr)
	if !r.Next() {
		t.Fatalf("Next must return true")
	}
	if s := r.Value().String(); s != first {
		t.Fatalf("unexpected value: %q; want %q", s, first)
	}
	if cr.reads != len(first) {
		t.Fatalf("unexpected number of reads: %d; want %d; the reader must not read past the first value", cr.reads, len(first))
	}

	// The second value is read on demand too.
	if !r.Next() {
		t.Fatalf("Next must return true")
	}
	if s := r.Value().String(); s != `[1,2,3]` {
		t.Fatalf("unexpected value: %q", s)
	}
	wantReads := len(first) + 10000 + len(`[1,2,3]`)
	if cr.reads != wantReads {
		t.Fatalf("unexpected number of reads: %d; want %d", cr.reads, wantReads)
	}
}

func TestReaderMaxValueSizeStopsReading(t *testing.T) {
	// The reader must stop reading once the value size limit is exceeded.
	cr := &chunkReader{data: []byte(strings.Repeat("1", 100000)), chunkSize: 4096}
	var r Reader
	r.MaxValueSize = 100
	r.Init(cr)
	if r.Next() {
		t.Fatalf("Next must return false")
	}
	if err := r.Error(); err == nil {
		t.Fatalf("expecting non-nil error")
	}
	if cr.reads > 2 {
		t.Fatalf("too many reads: %d; the reader must stop after exceeding MaxValueSize", cr.reads)
	}
}
