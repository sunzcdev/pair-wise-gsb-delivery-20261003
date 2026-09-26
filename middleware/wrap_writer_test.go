package middleware

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// readerFromRecorder is a ResponseRecorder that also satisfies io.ReaderFrom,
// mirroring the real net/http response writer (which implements ReadFrom and
// is what httpFancyWriter.ReadFrom fast-paths to). Bytes copied via ReadFrom
// are recorded to the body so a test can observe whether they reached the
// original writer.
type readerFromRecorder struct {
	*httptest.ResponseRecorder
}

func (r *readerFromRecorder) ReadFrom(src io.Reader) (int64, error) {
	return io.Copy(r.ResponseRecorder.Body, src)
}

// minimalResponseWriter implements only http.ResponseWriter.
type minimalResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	code   int
}

func newMinimalResponseWriter() *minimalResponseWriter {
	return &minimalResponseWriter{header: make(http.Header)}
}

func (w *minimalResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *minimalResponseWriter) Write(b []byte) (int, error) {
	return w.body.Write(b)
}

func (w *minimalResponseWriter) WriteHeader(code int) {
	w.code = code
}

// The capabilities below are mixed into concrete test writer types via
// embedding, so every combination of Flusher/Hijacker/Pusher/ReaderFrom can
// be declared as a one-line struct.

type flusherCapability struct {
	flushed *bool
}

func (c flusherCapability) Flush() {
	if c.flushed != nil {
		*c.flushed = true
	}
}

type hijackerCapability struct{}

func (hijackerCapability) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, nil
}

type pusherCapability struct{}

func (pusherCapability) Push(string, *http.PushOptions) error {
	return nil
}

type readerFromCapability struct{}

func (readerFromCapability) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(io.Discard, r)
}

// Every Flusher/Hijacker/Pusher/ReaderFrom combination of test writer.
type wNone struct{ *minimalResponseWriter }
type wF struct {
	*minimalResponseWriter
	flusherCapability
}
type wH struct {
	*minimalResponseWriter
	hijackerCapability
}
type wP struct {
	*minimalResponseWriter
	pusherCapability
}
type wR struct {
	*minimalResponseWriter
	readerFromCapability
}
type wFH struct {
	*minimalResponseWriter
	flusherCapability
	hijackerCapability
}
type wFP struct {
	*minimalResponseWriter
	flusherCapability
	pusherCapability
}
type wFR struct {
	*minimalResponseWriter
	flusherCapability
	readerFromCapability
}
type wHP struct {
	*minimalResponseWriter
	hijackerCapability
	pusherCapability
}
type wHR struct {
	*minimalResponseWriter
	hijackerCapability
	readerFromCapability
}
type wPR struct {
	*minimalResponseWriter
	pusherCapability
	readerFromCapability
}
type wFHP struct {
	*minimalResponseWriter
	flusherCapability
	hijackerCapability
	pusherCapability
}
type wFHR struct {
	*minimalResponseWriter
	flusherCapability
	hijackerCapability
	readerFromCapability
}
type wFPR struct {
	*minimalResponseWriter
	flusherCapability
	pusherCapability
	readerFromCapability
}
type wHPR struct {
	*minimalResponseWriter
	hijackerCapability
	pusherCapability
	readerFromCapability
}
type wFHPR struct {
	*minimalResponseWriter
	flusherCapability
	hijackerCapability
	pusherCapability
	readerFromCapability
}

func newComboWriter(t *testing.T, combo string) http.ResponseWriter {
	t.Helper()
	m := newMinimalResponseWriter()
	switch combo {
	case "none":
		return &wNone{m}
	case "F":
		return &wF{m, flusherCapability{}}
	case "H":
		return &wH{m, hijackerCapability{}}
	case "P":
		return &wP{m, pusherCapability{}}
	case "R":
		return &wR{m, readerFromCapability{}}
	case "FH":
		return &wFH{m, flusherCapability{}, hijackerCapability{}}
	case "FP":
		return &wFP{m, flusherCapability{}, pusherCapability{}}
	case "FR":
		return &wFR{m, flusherCapability{}, readerFromCapability{}}
	case "HP":
		return &wHP{m, hijackerCapability{}, pusherCapability{}}
	case "HR":
		return &wHR{m, hijackerCapability{}, readerFromCapability{}}
	case "PR":
		return &wPR{m, pusherCapability{}, readerFromCapability{}}
	case "FHP":
		return &wFHP{m, flusherCapability{}, hijackerCapability{}, pusherCapability{}}
	case "FHR":
		return &wFHR{m, flusherCapability{}, hijackerCapability{}, readerFromCapability{}}
	case "FPR":
		return &wFPR{m, flusherCapability{}, pusherCapability{}, readerFromCapability{}}
	case "HPR":
		return &wHPR{m, hijackerCapability{}, pusherCapability{}, readerFromCapability{}}
	case "FHPR":
		return &wFHPR{m, flusherCapability{}, hijackerCapability{}, pusherCapability{}, readerFromCapability{}}
	}
	t.Fatalf("unknown combo %q", combo)
	return nil
}

// TestNewWrapResponseWriterPreservesReaderFromOnly is the minimal regression
// test: a writer implementing only http.ResponseWriter and io.ReaderFrom
// must keep its ReaderFrom identity after wrapping, for both HTTP/1 and
// HTTP/2. Previously the factory only preserved ReaderFrom when Flusher and
// Hijacker were also present, so the type assertion flipped from true to
// false after wrapping.
func TestNewWrapResponseWriterPreservesReaderFromOnly(t *testing.T) {
	type readerFromOnlyWriter struct {
		*minimalResponseWriter
		readerFromCapability
	}
	original := &readerFromOnlyWriter{newMinimalResponseWriter(), readerFromCapability{}}

	for _, protoMajor := range []int{1, 2} {
		t.Run("HTTP/"+strconv.Itoa(protoMajor), func(t *testing.T) {
			wrap := NewWrapResponseWriter(original, protoMajor)
			if _, ok := wrap.(io.ReaderFrom); !ok {
				t.Fatalf("wrapped writer lost io.ReaderFrom (protoMajor=%d)", protoMajor)
			}
			if _, ok := wrap.(http.Flusher); ok {
				t.Fatalf("wrapped writer falsely gained http.Flusher")
			}
			if _, ok := wrap.(http.Hijacker); ok {
				t.Fatalf("wrapped writer falsely gained http.Hijacker")
			}
			if _, ok := wrap.(http.Pusher); ok {
				t.Fatalf("wrapped writer falsely gained http.Pusher")
			}
			if wrap.Unwrap() != http.ResponseWriter(original) {
				t.Fatalf("Unwrap did not return the original writer")
			}
		})
	}
}

// TestNewWrapResponseWriterInterfaceCombinations verifies that the wrapper
// preserves exactly the optional interfaces applicable to the connection
// protocol: Flusher and ReaderFrom apply to both HTTP/1 and HTTP/2, Hijacker
// only to HTTP/1, and Pusher only to HTTP/2. In particular, a writer that
// does not support Hijack must never gain it through wrapping.
func TestNewWrapResponseWriterInterfaceCombinations(t *testing.T) {
	combos := []string{"none", "F", "H", "P", "R", "FH", "FP", "FR", "HP", "HR", "PR", "FHP", "FHR", "FPR", "HPR", "FHPR"}

	for _, protoMajor := range []int{1, 2} {
		for _, combo := range combos {
			has := func(c rune) bool { return strings.ContainsRune(combo, c) }
			wantFlush := has('F')
			wantReaderFrom := has('R')
			// Protocol-specific capabilities are only exposed on their own
			// protocol, even if the underlying writer has the method.
			wantHijack := protoMajor == 1 && has('H')
			wantPush := protoMajor == 2 && has('P')

			t.Run("HTTP/"+strconv.Itoa(protoMajor)+"/"+combo, func(t *testing.T) {
				original := newComboWriter(t, combo)
				wrap := NewWrapResponseWriter(original, protoMajor)

				if wrap.Unwrap() != original {
					t.Fatalf("Unwrap did not return the original writer")
				}
				if _, ok := wrap.(http.Flusher); ok != wantFlush {
					t.Fatalf("Flusher exposed=%v want %v", ok, wantFlush)
				}
				if _, ok := wrap.(io.ReaderFrom); ok != wantReaderFrom {
					t.Fatalf("ReaderFrom exposed=%v want %v", ok, wantReaderFrom)
				}
				if _, ok := wrap.(http.Hijacker); ok != wantHijack {
					t.Fatalf("Hijacker exposed=%v want %v", ok, wantHijack)
				}
				if _, ok := wrap.(http.Pusher); ok != wantPush {
					t.Fatalf("Pusher exposed=%v want %v", ok, wantPush)
				}
			})
		}
	}
}

// TestNewWrapResponseWriterResponseController verifies that
// http.ResponseController keeps reaching the underlying capabilities through
// the new wrapper combinations.
func TestNewWrapResponseWriterResponseController(t *testing.T) {
	flushed := false
	original := &wFR{newMinimalResponseWriter(), flusherCapability{flushed: &flushed}, readerFromCapability{}}
	wrap := NewWrapResponseWriter(original, 1)

	if err := http.NewResponseController(wrap).Flush(); err != nil {
		t.Fatalf("ResponseController.Flush: %v", err)
	}
	if !flushed {
		t.Fatalf("Flush did not reach the underlying writer")
	}

	// A writer without Flusher must not gain Flush through wrapping.
	bare := NewWrapResponseWriter(&wNone{newMinimalResponseWriter()}, 1)
	if err := http.NewResponseController(bare).Flush(); err == nil {
		t.Fatalf("ResponseController.Flush unexpectedly succeeded on a non-flushing writer")
	}
}

// readerOnlyWriter is a ResponseWriter + io.ReaderFrom that records the
// bytes delivered via the ReadFrom fast path.
type readerOnlyWriter struct {
	*minimalResponseWriter
	readFromCalled bool
}

func (w *readerOnlyWriter) ReadFrom(r io.Reader) (int64, error) {
	w.readFromCalled = true
	return io.Copy(w.minimalResponseWriter, r)
}

// TestReaderFromFastPathConsistency checks Status, BytesWritten, and data
// delivery on the direct ReadFrom fast path (no tee, no discard).
func TestReaderFromFastPathConsistency(t *testing.T) {
	original := &readerOnlyWriter{minimalResponseWriter: newMinimalResponseWriter()}
	wrap := NewWrapResponseWriter(original, 1)

	const input = "hello world"
	n, err := wrap.(io.ReaderFrom).ReadFrom(strings.NewReader(input))
	assertNoError(t, err)

	if !original.readFromCalled {
		t.Fatalf("ReadFrom did not fast-path to the underlying ReaderFrom")
	}
	assertEqual(t, int64(len(input)), n)
	assertEqual(t, len(input), wrap.BytesWritten())
	assertEqual(t, http.StatusOK, wrap.Status())
	assertEqual(t, input, original.body.String())
}

// TestReaderFromTeeConsistency makes sure the tee path bypasses the
// underlying ReaderFrom, tees all bytes, and counts them exactly once.
func TestReaderFromTeeConsistency(t *testing.T) {
	original := &readerOnlyWriter{minimalResponseWriter: newMinimalResponseWriter()}
	wrap := NewWrapResponseWriter(original, 1)

	var tee bytes.Buffer
	wrap.Tee(&tee)

	const input = "hello world"
	n, err := wrap.(io.ReaderFrom).ReadFrom(strings.NewReader(input))
	assertNoError(t, err)

	if original.readFromCalled {
		t.Fatalf("teed ReadFrom must not use the underlying ReadFrom fast path")
	}
	assertEqual(t, int64(len(input)), n)
	assertEqual(t, len(input), wrap.BytesWritten())
	assertEqual(t, input, tee.String())
	assertEqual(t, input, original.body.String())
	assertEqual(t, http.StatusOK, wrap.Status())
}

// TestReaderFromDiscardConsistency verifies that Discard suppresses body
// writes (including the underlying ReadFrom fast path) while still counting
// the bytes.
func TestReaderFromDiscardConsistency(t *testing.T) {
	original := &readerOnlyWriter{minimalResponseWriter: newMinimalResponseWriter()}
	wrap := NewWrapResponseWriter(original, 1)
	wrap.Discard()

	const input = "hello world"
	n, err := wrap.(io.ReaderFrom).ReadFrom(strings.NewReader(input))
	assertNoError(t, err)

	if original.readFromCalled {
		t.Fatalf("discarded ReadFrom must not use the underlying ReadFrom fast path")
	}
	assertEqual(t, int64(len(input)), n)
	assertEqual(t, len(input), wrap.BytesWritten())
	assertEqual(t, 0, original.body.Len())
}

// partialReadFromWriter writes a fixed prefix to its body via ReadFrom and
// then returns an error, simulating the underlying connection failing after
// a partial write.
type partialReadFromWriter struct {
	*minimalResponseWriter
	limit int64
	err   error
}

func (w *partialReadFromWriter) ReadFrom(r io.Reader) (int64, error) {
	n, _ := io.CopyN(w.minimalResponseWriter, r, w.limit)
	return n, w.err
}

// TestReaderFromPartialWriteError verifies that a partial write error from
// the underlying ReadFrom is propagated, and that BytesWritten reflects only
// the bytes that actually made it to the underlying writer.
func TestReaderFromPartialWriteError(t *testing.T) {
	writeErr := errors.New("connection reset")
	original := &partialReadFromWriter{
		minimalResponseWriter: newMinimalResponseWriter(),
		limit:                 5,
		err:                   writeErr,
	}
	wrap := NewWrapResponseWriter(original, 1)

	const input = "hello world"
	n, err := wrap.(io.ReaderFrom).ReadFrom(strings.NewReader(input))

	if !errors.Is(err, writeErr) {
		t.Fatalf("ReadFrom error = %v, want %v", err, writeErr)
	}
	assertEqual(t, int64(5), n)
	assertEqual(t, 5, wrap.BytesWritten())
	assertEqual(t, "hello", original.body.String())
	assertEqual(t, http.StatusOK, wrap.Status())
}

func TestWrapResponseWriterStatusWhenFlushed(t *testing.T) {
	for _, writer := range []struct {
		name string
		wrap func(http.ResponseWriter) WrapResponseWriter
	}{
		{"flushWriter", func(w http.ResponseWriter) WrapResponseWriter {
			return &flushWriter{basicWriter{ResponseWriter: w}}
		}},
		{"flushHijackWriter", func(w http.ResponseWriter) WrapResponseWriter {
			return &flushHijackWriter{basicWriter{ResponseWriter: w}}
		}},
		{"httpFancyWriter", func(w http.ResponseWriter) WrapResponseWriter {
			return &httpFancyWriter{basicWriter{ResponseWriter: w}}
		}},
		{"http2FancyWriter", func(w http.ResponseWriter) WrapResponseWriter {
			return &http2FancyWriter{basicWriter{ResponseWriter: w}}
		}},
	} {
		t.Run(writer.name, func(t *testing.T) {
			for _, status := range []struct {
				name string
				code int
				want int
			}{
				{"implicit", 0, http.StatusOK},
				{"explicit", http.StatusCreated, http.StatusCreated},
			} {
				t.Run(status.name, func(t *testing.T) {
					for _, mode := range []struct {
						name    string
						discard bool
						tee     bool
					}{
						{"passthrough", false, false},
						{"tee", false, true},
						{"discard", true, false},
						{"discard-with-tee", true, true},
					} {
						t.Run(mode.name, func(t *testing.T) {
							original := &httptest.ResponseRecorder{
								HeaderMap: make(http.Header),
								Body:      new(bytes.Buffer),
							}
							wrap := writer.wrap(original)
							var tee bytes.Buffer
							if mode.tee {
								wrap.Tee(&tee)
							}
							if mode.discard {
								wrap.Discard()
							}
							if status.code != 0 {
								wrap.WriteHeader(status.code)
							}

							wrap.(http.Flusher).Flush()
							assertEqual(t, status.want, wrap.Status())
							assertEqual(t, !mode.discard, original.Flushed)
							assertEqual(t, 0, wrap.BytesWritten())

							// Flushing commits the status, even before any body is written.
							wrap.WriteHeader(http.StatusInternalServerError)
							_, err := wrap.Write([]byte("body"))
							assertNoError(t, err)
							assertNoError(t, http.NewResponseController(wrap).Flush())
							assertEqual(t, status.want, wrap.Status())
							assertEqual(t, 4, wrap.BytesWritten())
							if mode.tee {
								assertEqual(t, "body", tee.String())
							}
							if mode.discard {
								assertEqual(t, false, original.Flushed)
								assertEqual(t, 0, original.Code)
								assertEqual(t, "", original.Body.String())
								original.WriteHeader(http.StatusAccepted)
								_, err := original.Write([]byte("replacement"))
								assertNoError(t, err)
								assertEqual(t, http.StatusAccepted, original.Code)
								assertEqual(t, "replacement", original.Body.String())
							} else {
								assertEqual(t, status.want, original.Code)
								assertEqual(t, "body", original.Body.String())
							}
						})
					}
				})
			}
		})
	}
}

func TestBasicWritesTeesWritesWithoutDiscard(t *testing.T) {
	// explicitly create the struct instead of NewRecorder to control the value of Code
	original := &httptest.ResponseRecorder{
		HeaderMap: make(http.Header),
		Body:      new(bytes.Buffer),
	}
	wrap := &basicWriter{ResponseWriter: original}

	var buf bytes.Buffer
	wrap.Tee(&buf)

	_, err := wrap.Write([]byte("hello world"))
	assertNoError(t, err)

	assertEqual(t, 200, original.Code)
	assertEqual(t, []byte("hello world"), original.Body.Bytes())
	assertEqual(t, []byte("hello world"), buf.Bytes())
	assertEqual(t, 11, wrap.BytesWritten())
}

func TestBasicWriterDiscardsWritesToOriginalResponseWriter(t *testing.T) {
	t.Run("With Tee", func(t *testing.T) {
		// explicitly create the struct instead of NewRecorder to control the value of Code
		original := &httptest.ResponseRecorder{
			HeaderMap: make(http.Header),
			Body:      new(bytes.Buffer),
		}
		wrap := &basicWriter{ResponseWriter: original}

		var buf bytes.Buffer
		wrap.Tee(&buf)
		wrap.Discard()

		_, err := wrap.Write([]byte("hello world"))
		assertNoError(t, err)

		assertEqual(t, 0, original.Code) // wrapper shouldn't call WriteHeader implicitly
		assertEqual(t, 0, original.Body.Len())
		assertEqual(t, []byte("hello world"), buf.Bytes())
		assertEqual(t, 11, wrap.BytesWritten())
	})

	t.Run("Without Tee", func(t *testing.T) {
		// explicitly create the struct instead of NewRecorder to control the value of Code
		original := &httptest.ResponseRecorder{
			HeaderMap: make(http.Header),
			Body:      new(bytes.Buffer),
		}
		wrap := &basicWriter{ResponseWriter: original}
		wrap.Discard()

		_, err := wrap.Write([]byte("hello world"))
		assertNoError(t, err)

		assertEqual(t, 0, original.Code) // wrapper shouldn't call WriteHeader implicitly
		assertEqual(t, 0, original.Body.Len())
		assertEqual(t, 11, wrap.BytesWritten())
	})
}

// TestHttpFancyWriterReadFromByteCountWithTee is a regression test for
// https://github.com/go-chi/chi/issues/1067.
// httpFancyWriter.ReadFrom was adding n to basicWriter.bytes even when the
// write went through basicWriter.Write (which already increments the counter),
// resulting in double-counting the bytes when a Tee writer was set.
func TestHttpFancyWriterReadFromByteCountWithTee(t *testing.T) {
	original := &httptest.ResponseRecorder{
		HeaderMap: make(http.Header),
		Body:      new(bytes.Buffer),
	}
	f := &httpFancyWriter{basicWriter: basicWriter{ResponseWriter: original}}

	var teeBuf bytes.Buffer
	f.Tee(&teeBuf)

	const input = "hello world"
	n, err := f.ReadFrom(strings.NewReader(input))
	assertNoError(t, err)
	assertEqual(t, int64(len(input)), n)
	// Before the fix, BytesWritten() returned 22 (double-counted).
	assertEqual(t, len(input), f.BytesWritten())
	assertEqual(t, []byte(input), teeBuf.Bytes())
}

// TestHttpFancyWriterReadFromHonorsDiscard verifies that Discard() suppresses
// the body even when it is written via ReadFrom/io.Copy. Before the fix, the
// non-tee ReadFrom path streamed straight to the original ResponseWriter's
// ReaderFrom, bypassing the discard flag entirely.
func TestHttpFancyWriterReadFromHonorsDiscard(t *testing.T) {
	original := &readerFromRecorder{&httptest.ResponseRecorder{
		HeaderMap: make(http.Header),
		Body:      new(bytes.Buffer),
	}}
	f := &httpFancyWriter{basicWriter: basicWriter{ResponseWriter: original}}
	f.Discard()

	const input = "hello world"
	n, err := f.ReadFrom(strings.NewReader(input))
	assertNoError(t, err)
	assertEqual(t, int64(len(input)), n)
	// The data must NOT reach the original writer once Discard() is set.
	assertEqual(t, 0, original.Body.Len())
	// BytesWritten still reflects the bytes consumed.
	assertEqual(t, len(input), f.BytesWritten())
}
