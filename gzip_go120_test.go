//go:build go1.20

package gziphandler

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestFlushErrorIsObservableThroughResponseController verifies that
// http.ResponseController.Flush reaches the gzip handler's FlushError and
// surfaces the underlying flush failure, both when the first flush commits
// to gzip and when it commits to passthrough.
func TestFlushErrorIsObservableThroughResponseController(t *testing.T) {
	for _, tc := range []struct {
		name     string
		strategy FlushStrategy
		gzipped  bool
	}{
		{"gzip first flush", FlushGzipImmediately, true},
		{"identity first flush", FlushAsIdentity, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw, rec, done := newHeldGzipWriter(t, tc.strategy)
			defer close(done)

			failing := &failingFlushWriter{ResponseWriter: rec}
			gw.ResponseWriter = failing

			gw.Write([]byte("some buffered body"))
			err := http.NewResponseController(gw).Flush()
			if err == nil || err.Error() != "boom: flush failed" {
				t.Fatalf("expected the underlying flush error, got %v", err)
			}
			assert.Equal(t, 1, failing.flushed, "underlying flush must be attempted exactly once")

			// Even after a failed flush, the encoding is final: the buffer
			// is not replayed, and no extra trailer is written by Close on
			// the plain path.
			if tc.gzipped {
				assert.Equal(t, encodingGzip, gw.encoding)
				assert.Equal(t, "gzip", rec.Header().Get(contentEncoding))
			} else {
				assert.Equal(t, encodingPlain, gw.encoding)
				assert.Equal(t, "", rec.Header().Get(contentEncoding))
				assert.Equal(t, []byte("some buffered body"), rec.Body.Bytes())
			}
		})
	}
}

// TestFlushErrorUnsupported verifies that an underlying ResponseWriter which
// cannot flush reports http.ErrNotSupported through the controller.
func TestFlushErrorUnsupported(t *testing.T) {
	gw, rec, done := newHeldGzipWriter(t, FlushAsIdentity)
	defer close(done)

	// httptest.ResponseRecorder implements the plain Flusher only; replace
	// it with a bare header map writer that supports no flushing at all.
	gw.ResponseWriter = &nonFlushingWriter{header: rec.Header()}

	gw.Write([]byte("x"))
	err := http.NewResponseController(gw).Flush()
	if !errors.Is(err, http.ErrNotSupported) {
		t.Fatalf("expected http.ErrNotSupported, got %v", err)
	}
	assert.Equal(t, encodingPlain, gw.encoding)
}

type nonFlushingWriter struct {
	header http.Header
}

func (w *nonFlushingWriter) Header() http.Header         { return w.header }
func (w *nonFlushingWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *nonFlushingWriter) WriteHeader(int)             {}

var _ http.ResponseWriter = (*nonFlushingWriter)(nil)
