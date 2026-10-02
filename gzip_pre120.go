//go:build !go1.20

package gziphandler

import "net/http"

// flushUnderlying flushes the wrapped ResponseWriter when it implements
// http.Flusher. Before Go 1.20 an http.Flusher cannot report failures, so
// nothing can be returned here; FlushError only surfaces errors coming from
// the gzip writer itself on these versions.
func (w *GzipResponseWriter) flushUnderlying() error {
	if fw, ok := w.ResponseWriter.(http.Flusher); ok {
		fw.Flush()
	}
	return nil
}
