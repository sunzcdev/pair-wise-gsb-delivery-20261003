//go:build go1.20

package gziphandler

import "net/http"

// flushUnderlying flushes the wrapped ResponseWriter through an
// http.ResponseController so that flush failures can be observed. When the
// underlying connection does not support flushing, http.ErrNotSupported is
// reported to the caller, matching the FlushError contract.
func (w *GzipResponseWriter) flushUnderlying() error {
	return http.NewResponseController(w.ResponseWriter).Flush()
}

// Unwrap exposes the wrapped ResponseWriter to http.ResponseController (via
// its rwUnwrapper lookup), so that methods not handled here (Hijack,
// SetReadDeadline, EnableFullDuplex, ...) are delegated transparently.
func (w *GzipResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
