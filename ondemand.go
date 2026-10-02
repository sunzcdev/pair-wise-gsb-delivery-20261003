package nosurf

import (
	"net"
	"net/http"
)

// requestState holds the per-request bookkeeping used by CSRFHandler.
// It is stored in the request context and is shared between the
// context implementations (pre/post Go 1.7).
type requestState struct {
	handler   *CSRFHandler
	writer    *onDemandResponseWriter
	realToken []byte
	// hasCookie reports whether the request carried a valid-length CSRF cookie.
	hasCookie bool
}

// onDemandResponseWriter wraps an http.ResponseWriter so that nosurf can tell
// whether the response headers have already been committed.
// In on-demand mode a token requested after that point can no longer be paired
// with a Set-Cookie header, so GetToken() refuses to issue one.
type onDemandResponseWriter struct {
	http.ResponseWriter
	committed bool
}

func newOnDemandResponseWriter(w http.ResponseWriter) *onDemandResponseWriter {
	return &onDemandResponseWriter{ResponseWriter: w}
}

func (w *onDemandResponseWriter) WriteHeader(code int) {
	w.committed = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *onDemandResponseWriter) Write(b []byte) (int, error) {
	w.committed = true
	return w.ResponseWriter.Write(b)
}

// Committed reports whether the response headers have been sent.
func (w *onDemandResponseWriter) Committed() bool {
	return w.committed
}

// Flush implements http.Flusher when the underlying writer supports it.
// Flushing commits the response headers.
func (w *onDemandResponseWriter) Flush() {
	w.committed = true
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Hijack implements http.Hijacker when the underlying writer supports it.
func (w *onDemandResponseWriter) Hijack() (net.Conn, interface{}, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hijacker.Hijack()
}

// Push implements http.Pusher when the underlying writer supports it.
func (w *onDemandResponseWriter) Push(target string, opts *http.PushOptions) error {
	pusher, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, opts)
}
