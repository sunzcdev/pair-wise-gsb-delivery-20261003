package scs

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"net/http"
)

// ErrResponseTooLarge is returned by Write when the LoadAndSave middleware is
// buffering the response (see SessionManager.BufferSize) and the buffered
// body would exceed the configured buffer size limit.
var ErrResponseTooLarge = errors.New("scs: buffered response body exceeds the configured BufferSize limit")

// bufferedResponseWriter buffers the response status code and body in memory
// so that the session can be committed after the handler has returned, and
// the buffered response is only sent to the client if the commit succeeds.
// Header writes are passed straight through to the underlying ResponseWriter's
// header map, which is safe because nothing is sent to the client until
// WriteHeader is called on the underlying ResponseWriter.
type bufferedResponseWriter struct {
	http.ResponseWriter
	status int
	buf    *bytes.Buffer
	limit  int
}

func (bw *bufferedResponseWriter) Write(b []byte) (int, error) {
	if bw.status == 0 {
		bw.status = http.StatusOK
	}

	if bw.buf.Len()+len(b) > bw.limit {
		return 0, ErrResponseTooLarge
	}

	return bw.buf.Write(b)
}

func (bw *bufferedResponseWriter) WriteHeader(code int) {
	if bw.status == 0 {
		bw.status = code
	}
}

// Flush is a no-op. Flushing part of the response to the client before the
// handler has returned would defeat the purpose of buffering, so the buffered
// response is always sent in one go after the session has been committed.
func (bw *bufferedResponseWriter) Flush() {}

// Hijack always returns an error. Hijacking the connection is not supported
// while the response is being buffered.
func (bw *bufferedResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("scs: hijacking the connection is not supported when response buffering is enabled")
}

func (bw *bufferedResponseWriter) Unwrap() http.ResponseWriter {
	return bw.ResponseWriter
}
