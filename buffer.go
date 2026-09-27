package scs

import (
	"bufio"
	"errors"
	"net"
	"net/http"
)

// bufferedResponseWriter buffers the response status code, headers and body
// (up to a fixed limit) instead of writing them directly to the underlying
// http.ResponseWriter. This allows the session data to be committed after
// the handler has returned, so that handlers may modify the session data
// after writing to the response. The buffered response is only sent to the
// client if the final session commit succeeds.
type bufferedResponseWriter struct {
	w              http.ResponseWriter
	request        *http.Request
	sessionManager *SessionManager
	limit          int
	header         http.Header
	body           []byte
	statusCode     int
	streamed       bool
	wroteHeader    bool
	hijacked       bool
}

func (bw *bufferedResponseWriter) Header() http.Header {
	if bw.streamed {
		return bw.w.Header()
	}
	return bw.header
}

func (bw *bufferedResponseWriter) Write(b []byte) (int, error) {
	if bw.streamed {
		return bw.w.Write(b)
	}

	// If writing b would exceed the buffer limit, commit the session and
	// switch to streaming the remainder of the response directly to the
	// client.
	if len(bw.body)+len(b) > bw.limit {
		bw.stream()
		return bw.w.Write(b)
	}

	bw.body = append(bw.body, b...)
	return len(b), nil
}

func (bw *bufferedResponseWriter) WriteHeader(code int) {
	if bw.streamed {
		if !bw.wroteHeader {
			bw.wroteHeader = true
			bw.w.WriteHeader(code)
		}
		return
	}

	if bw.statusCode == 0 {
		bw.statusCode = code
	}
}

// Flush commits the session immediately, sends any buffered response to the
// client, and switches to streaming the remainder of the response directly.
func (bw *bufferedResponseWriter) Flush() {
	bw.stream()
	if f, ok := bw.w.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack commits the session, sends any buffered response to the client, and
// then hijacks the underlying connection.
func (bw *bufferedResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := bw.w.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("scs: the underlying ResponseWriter does not support hijacking")
	}

	bw.stream()
	bw.hijacked = true
	return h.Hijack()
}

func (bw *bufferedResponseWriter) Unwrap() http.ResponseWriter {
	return bw.w
}

// stream commits the session and switches to pass-through mode, sending any
// buffered response to the client. It is a no-op if the writer is already
// streaming.
func (bw *bufferedResponseWriter) stream() {
	if bw.streamed {
		return
	}

	err := bw.sessionManager.commitAndAddSessionCookie(bw, bw.request)
	if err != nil {
		bw.streamed = true
		bw.sessionManager.ErrorFunc(bw.w, bw.request, err)
		return
	}

	bw.flush()
	bw.streamed = true
}

// flush sends the buffered headers, status code and body to the underlying
// ResponseWriter.
func (bw *bufferedResponseWriter) flush() {
	for key, values := range bw.header {
		for _, value := range values {
			bw.w.Header().Add(key, value)
		}
	}

	if bw.statusCode != 0 {
		bw.w.WriteHeader(bw.statusCode)
		bw.wroteHeader = true
	}

	if len(bw.body) > 0 {
		bw.w.Write(bw.body)
		bw.body = nil
	}
}
