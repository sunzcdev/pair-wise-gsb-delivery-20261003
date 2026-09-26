package middleware

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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

// readFromOnlyWriter hides an *httptest.ResponseRecorder behind the
// http.ResponseWriter interface and adds only io.ReaderFrom on top, so the
// value satisfies exactly http.ResponseWriter + io.ReaderFrom (no Flusher,
// Hijacker, or Pusher). readFromCalled records whether the fast path was used.
type readFromOnlyWriter struct {
	http.ResponseWriter
	recorder *readFromRecorder
}

func (w *readFromOnlyWriter) ReadFrom(r io.Reader) (int64, error) {
	return w.recorder.ReadFrom(r)
}

// readFromRecorder is a ResponseRecorder whose ReadFrom can be configured to
// stop after limit bytes and return err, simulating an underlying partial
// write failure.
type readFromRecorder struct {
	*httptest.ResponseRecorder
	called bool
	limit  int64
	err    error
}

func (r *readFromRecorder) ReadFrom(src io.Reader) (int64, error) {
	r.called = true
	if r.limit >= 0 {
		n, _ := io.CopyN(r.Body, src, r.limit)
		return n, r.err
	}
	return io.Copy(r.Body, src)
}

func newReadFromOnlyWriter() (*readFromOnlyWriter, *readFromRecorder) {
	recorder := &readFromRecorder{
		ResponseRecorder: &httptest.ResponseRecorder{
			HeaderMap: make(http.Header),
			Body:      new(bytes.Buffer),
		},
		limit: -1,
	}
	return &readFromOnlyWriter{ResponseWriter: recorder, recorder: recorder}, recorder
}

// TestNewWrapResponseWriterReaderFromOnly is a regression test: a writer
// implementing only http.ResponseWriter and io.ReaderFrom used to lose its
// io.ReaderFrom capability when wrapped, because the factory only preserved
// the fast path when Flusher, Hijacker and ReaderFrom were all present.
func TestNewWrapResponseWriterReaderFromOnly(t *testing.T) {
	w, recorder := newReadFromOnlyWriter()

	if _, ok := http.ResponseWriter(w).(io.ReaderFrom); !ok {
		t.Fatal("precondition failed: original writer should implement io.ReaderFrom")
	}

	wrap := NewWrapResponseWriter(w, 1)
	rf, ok := wrap.(io.ReaderFrom)
	if !ok {
		t.Fatal("wrapped writer lost the io.ReaderFrom fast path")
	}

	n, err := rf.ReadFrom(strings.NewReader("hello"))
	assertNoError(t, err)
	assertEqual(t, int64(5), n)
	if !recorder.called {
		t.Fatal("expected ReadFrom to fast-path to the underlying writer")
	}
	assertEqual(t, "hello", recorder.Body.String())
	assertEqual(t, http.StatusOK, wrap.Status())
	assertEqual(t, 5, wrap.BytesWritten())
}

// TestNewWrapResponseWriterInterfaceCombinations verifies that every
// applicable combination of the optional interfaces (http.Flusher,
// http.Hijacker, http.Pusher, io.ReaderFrom) survives wrapping, and that the
// wrapper never gains a capability the underlying writer lacks. Hijacker is
// an HTTP/1-only capability; Pusher is HTTP/2-only.
func TestNewWrapResponseWriterInterfaceCombinations(t *testing.T) {
	type capabilities struct {
		flusher    bool
		hijacker   bool
		pusher     bool
		readerFrom bool
	}

	newRecorder := func() *httptest.ResponseRecorder {
		return &httptest.ResponseRecorder{
			HeaderMap: make(http.Header),
			Body:      new(bytes.Buffer),
		}
	}

	tests := []struct {
		name       string
		protoMajor int
		make       func(http.ResponseWriter) http.ResponseWriter
		want       capabilities
	}{
		// HTTP/1.x: Flusher, Hijacker and ReaderFrom may appear in any
		// combination; Pusher never applies.
		{"http1/none", 1, func(w http.ResponseWriter) http.ResponseWriter {
			// Hide the recorder behind the interface: a bare
			// httptest.ResponseRecorder implements http.Flusher.
			return struct{ http.ResponseWriter }{w}
		}, capabilities{}},
		{"http1/flusher", 1, func(w http.ResponseWriter) http.ResponseWriter {
			return struct {
				http.ResponseWriter
				flusherStub
			}{w, flusherStub{}}
		}, capabilities{flusher: true}},
		{"http1/hijacker", 1, func(w http.ResponseWriter) http.ResponseWriter {
			return struct {
				http.ResponseWriter
				hijackerStub
			}{w, hijackerStub{}}
		}, capabilities{hijacker: true}},
		{"http1/readerFrom", 1, func(w http.ResponseWriter) http.ResponseWriter {
			return struct {
				http.ResponseWriter
				readerFromStub
			}{w, readerFromStub{dst: w}}
		}, capabilities{readerFrom: true}},
		{"http1/flusher+hijacker", 1, func(w http.ResponseWriter) http.ResponseWriter {
			return struct {
				http.ResponseWriter
				flusherStub
				hijackerStub
			}{w, flusherStub{}, hijackerStub{}}
		}, capabilities{flusher: true, hijacker: true}},
		{"http1/flusher+readerFrom", 1, func(w http.ResponseWriter) http.ResponseWriter {
			return struct {
				http.ResponseWriter
				flusherStub
				readerFromStub
			}{w, flusherStub{}, readerFromStub{dst: w}}
		}, capabilities{flusher: true, readerFrom: true}},
		{"http1/hijacker+readerFrom", 1, func(w http.ResponseWriter) http.ResponseWriter {
			return struct {
				http.ResponseWriter
				hijackerStub
				readerFromStub
			}{w, hijackerStub{}, readerFromStub{dst: w}}
		}, capabilities{hijacker: true, readerFrom: true}},
		{"http1/flusher+hijacker+readerFrom", 1, func(w http.ResponseWriter) http.ResponseWriter {
			return struct {
				http.ResponseWriter
				flusherStub
				hijackerStub
				readerFromStub
			}{w, flusherStub{}, hijackerStub{}, readerFromStub{dst: w}}
		}, capabilities{flusher: true, hijacker: true, readerFrom: true}},

		// HTTP/2: Flusher, Pusher and ReaderFrom may appear in any
		// combination; Hijacker never applies.
		{"http2/none", 2, func(w http.ResponseWriter) http.ResponseWriter {
			return struct{ http.ResponseWriter }{w}
		}, capabilities{}},
		{"http2/flusher", 2, func(w http.ResponseWriter) http.ResponseWriter {
			return struct {
				http.ResponseWriter
				flusherStub
			}{w, flusherStub{}}
		}, capabilities{flusher: true}},
		{"http2/pusher", 2, func(w http.ResponseWriter) http.ResponseWriter {
			return struct {
				http.ResponseWriter
				pusherStub
			}{w, pusherStub{}}
		}, capabilities{pusher: true}},
		{"http2/readerFrom", 2, func(w http.ResponseWriter) http.ResponseWriter {
			return struct {
				http.ResponseWriter
				readerFromStub
			}{w, readerFromStub{dst: w}}
		}, capabilities{readerFrom: true}},
		{"http2/flusher+pusher", 2, func(w http.ResponseWriter) http.ResponseWriter {
			return struct {
				http.ResponseWriter
				flusherStub
				pusherStub
			}{w, flusherStub{}, pusherStub{}}
		}, capabilities{flusher: true, pusher: true}},
		{"http2/flusher+readerFrom", 2, func(w http.ResponseWriter) http.ResponseWriter {
			return struct {
				http.ResponseWriter
				flusherStub
				readerFromStub
			}{w, flusherStub{}, readerFromStub{dst: w}}
		}, capabilities{flusher: true, readerFrom: true}},
		{"http2/pusher+readerFrom", 2, func(w http.ResponseWriter) http.ResponseWriter {
			return struct {
				http.ResponseWriter
				pusherStub
				readerFromStub
			}{w, pusherStub{}, readerFromStub{dst: w}}
		}, capabilities{pusher: true, readerFrom: true}},
		{"http2/flusher+pusher+readerFrom", 2, func(w http.ResponseWriter) http.ResponseWriter {
			return struct {
				http.ResponseWriter
				flusherStub
				pusherStub
				readerFromStub
			}{w, flusherStub{}, pusherStub{}, readerFromStub{dst: w}}
		}, capabilities{flusher: true, pusher: true, readerFrom: true}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			original := tc.make(newRecorder())
			wrap := NewWrapResponseWriter(original, tc.protoMajor)

			_, gotFlusher := wrap.(http.Flusher)
			assertEqual(t, tc.want.flusher, gotFlusher)

			_, gotHijacker := wrap.(http.Hijacker)
			assertEqual(t, tc.want.hijacker, gotHijacker)

			_, gotPusher := wrap.(http.Pusher)
			assertEqual(t, tc.want.pusher, gotPusher)

			_, gotReaderFrom := wrap.(io.ReaderFrom)
			assertEqual(t, tc.want.readerFrom, gotReaderFrom)

			// Unwrap must always return the exact writer that was wrapped.
			assertEqual(t, original, wrap.Unwrap())

			// ResponseController must keep working through the wrapper.
			controller := http.NewResponseController(wrap)
			if tc.want.flusher {
				assertNoError(t, controller.Flush())
			}
			if !tc.want.hijacker {
				// A writer without Hijack support must not pretend to
				// have one after wrapping.
				if _, _, err := controller.Hijack(); err == nil {
					t.Fatal("expected Hijack to fail for a non-hijacker writer")
				}
			}
		})
	}
}

// Capability stubs used to build writers with arbitrary interface
// combinations. readerFromStub streams into the wrapped writer's body so the
// fast path remains observable; the others are no-ops.
type flusherStub struct{}

type hijackerStub struct{}

type pusherStub struct{}

type readerFromStub struct{ dst io.Writer }

func (flusherStub) Flush() {}

func (hijackerStub) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("hijack not supported")
}

func (pusherStub) Push(string, *http.PushOptions) error {
	return http.ErrNotSupported
}

func (s readerFromStub) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(s.dst, r)
}

// TestWrapResponseWriterReadFromSemantics verifies that the ReadFrom fast
// path exposed by the factory keeps Status, BytesWritten, Tee, Discard and
// underlying partial-write errors consistent.
func TestWrapResponseWriterReadFromSemantics(t *testing.T) {
	const input = "hello world"

	newWrap := func() (WrapResponseWriter, *readFromRecorder) {
		w, recorder := newReadFromOnlyWriter()
		wrap := NewWrapResponseWriter(w, 1)
		if _, ok := wrap.(io.ReaderFrom); !ok {
			t.Fatal("wrapped writer should implement io.ReaderFrom")
		}
		return wrap, recorder
	}

	t.Run("StatusAndBytesWritten", func(t *testing.T) {
		wrap, recorder := newWrap()
		n, err := wrap.(io.ReaderFrom).ReadFrom(strings.NewReader(input))
		assertNoError(t, err)
		assertEqual(t, int64(len(input)), n)
		assertEqual(t, http.StatusOK, wrap.Status())
		assertEqual(t, len(input), wrap.BytesWritten())
		assertEqual(t, input, recorder.Body.String())
	})

	t.Run("Tee", func(t *testing.T) {
		wrap, recorder := newWrap()
		var tee bytes.Buffer
		wrap.Tee(&tee)
		n, err := wrap.(io.ReaderFrom).ReadFrom(strings.NewReader(input))
		assertNoError(t, err)
		assertEqual(t, int64(len(input)), n)
		assertEqual(t, len(input), wrap.BytesWritten())
		assertEqual(t, input, tee.String())
		assertEqual(t, input, recorder.Body.String())
	})

	t.Run("Discard", func(t *testing.T) {
		wrap, recorder := newWrap()
		wrap.Discard()
		n, err := wrap.(io.ReaderFrom).ReadFrom(strings.NewReader(input))
		assertNoError(t, err)
		assertEqual(t, int64(len(input)), n)
		assertEqual(t, len(input), wrap.BytesWritten())
		assertEqual(t, 0, recorder.Body.Len())
	})

	t.Run("PartialWriteError", func(t *testing.T) {
		wrap, recorder := newWrap()
		errBoom := errors.New("boom")
		recorder.limit = 3
		recorder.err = errBoom
		n, err := wrap.(io.ReaderFrom).ReadFrom(strings.NewReader(input))
		assertEqual(t, errBoom, err)
		assertEqual(t, int64(3), n)
		assertEqual(t, 3, wrap.BytesWritten())
		assertEqual(t, http.StatusOK, wrap.Status())
		assertEqual(t, input[:3], recorder.Body.String())
	})
}
