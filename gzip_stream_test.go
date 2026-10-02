package gziphandler

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	streamChunk1 = "first-fragment, smaller than MinSize"
	streamChunk2 = "second-fragment"
)

// chunkedReader decodes an HTTP/1.1 chunked response body. It blocks on
// incomplete chunks, so bytes flushed by the server are observable before the
// whole response finishes.
type chunkedReader struct {
	br    *bufio.Reader
	chunk []byte
	done  bool
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.done {
		return 0, io.EOF
	}
	if len(c.chunk) > 0 {
		n := copy(p, c.chunk)
		c.chunk = c.chunk[n:]
		return n, nil
	}

	line, err := c.br.ReadString('\n')
	if err != nil {
		return 0, err
	}
	size, err := strconv.ParseUint(strings.TrimSpace(line), 16, 64)
	if err != nil {
		return 0, err
	}
	if size == 0 {
		for {
			trailer, err := c.br.ReadString('\n')
			if err != nil {
				return 0, err
			}
			if strings.TrimSpace(trailer) == "" {
				c.done = true
				return 0, io.EOF
			}
		}
	}

	c.chunk = make([]byte, size)
	if _, err := io.ReadFull(c.br, c.chunk); err != nil {
		return 0, err
	}
	if _, err := c.br.Discard(2); err != nil {
		return 0, err
	}
	return c.Read(p)
}

type rawResponse struct {
	conn   net.Conn
	status int
	header http.Header
	body   *chunkedReader
}

func (r *rawResponse) close() { r.conn.Close() }

// newChunkedServer starts a real HTTP/1.1 server and returns its URL.
func newChunkedServer(t *testing.T, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:")
	require.NoError(t, err)
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() {
		srv.Close()
		ln.Close()
	})
	return "http://" + ln.Addr().String()
}

// rawGet performs a request over a raw TCP connection and parses only the
// status line and headers, returning a chunked-decoding body reader. This lets
// a test observe exactly which bytes each Flush pushed onto the wire.
func rawGet(t *testing.T, baseURL string) *rawResponse {
	t.Helper()
	addr := strings.TrimPrefix(baseURL, "http://")
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)

	req := "GET / HTTP/1.1\r\nHost: " + addr + "\r\nAccept-Encoding: gzip\r\n\r\n"
	_, err = conn.Write([]byte(req))
	require.NoError(t, err)

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	require.NoError(t, err)
	require.Contains(t, resp.TransferEncoding, "chunked")

	return &rawResponse{
		conn:   conn,
		status: resp.StatusCode,
		header: resp.Header,
		body:   &chunkedReader{br: br},
	}
}

// readAtLeast reads exactly n bytes, failing if they do not arrive before the
// deadline, proving they were sent by a single Flush rather than buffered.
func readAtLeast(t *testing.T, conn net.Conn, r io.Reader, n int) []byte {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	out, err := io.ReadAll(io.LimitReader(r, int64(n)))
	require.NoError(t, err)
	require.Len(t, out, n)
	require.NoError(t, conn.SetReadDeadline(time.Time{}))
	return out
}

func streamWrapper(t *testing.T, policy FlushPolicy) func(http.Handler) http.Handler {
	t.Helper()
	wrapper, err := GzipHandlerWithOpts(
		CompressionLevel(gzip.DefaultCompression),
		MinSize(1400),
		StreamingFlush(policy),
	)
	require.NoError(t, err)
	return wrapper
}

func gzipRequest(t *testing.T) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	return req
}

// TestStreamingFlushAsGzipFirstFragment verifies the first Flush below MinSize
// emits the current fragment as gzip immediately. A second flush neither
// resends bytes nor writes the trailer, and the final stream decodes to both
// fragments in order.
func TestStreamingFlushAsGzipFirstFragment(t *testing.T) {
	firstFlushed := make(chan struct{})
	gotFirst := make(chan struct{})

	h := streamWrapper(t, FlushAsGzip)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, streamChunk1)
		require.NoError(t, http.NewResponseController(w).Flush())
		close(firstFlushed)

		select {
		case <-gotFirst:
		case <-time.After(5 * time.Second):
			require.Fail(t, "timed out waiting for client to receive first fragment")
		}

		// A second flush must neither duplicate the buffer nor add a trailer.
		require.NoError(t, http.NewResponseController(w).Flush())
		_, _ = io.WriteString(w, streamChunk2)
	}))

	res := rawGet(t, newChunkedServer(t, h))
	defer res.close()
	require.Equal(t, "gzip", res.header.Get("Content-Encoding"))

	// While the handler is blocked after its first Flush, the gzip header must
	// already be readable on the wire.
	<-firstFlushed
	prefix := readAtLeast(t, res.conn, res.body, 10)
	assert.Equal(t, []byte{0x1f, 0x8b}, prefix[:2], "gzip magic header")
	close(gotFirst)

	gr, err := gzip.NewReader(io.MultiReader(bytes.NewReader(prefix), res.body))
	require.NoError(t, err)
	body, err := io.ReadAll(gr)
	require.NoError(t, err)
	assert.Equal(t, streamChunk1+streamChunk2, string(body))
}

// TestStreamingFlushAsGzipZeroBody verifies a Flush with no buffered bytes
// commits gzip (emitting a valid stream header) and preserves a previously
// written status code; later writes decode correctly.
func TestStreamingFlushAsGzipZeroBody(t *testing.T) {
	h := streamWrapper(t, FlushAsGzip)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		require.NoError(t, http.NewResponseController(w).Flush())
		_, _ = io.WriteString(w, streamChunk1)
	}))

	res := rawGet(t, newChunkedServer(t, h))
	defer res.close()

	assert.Equal(t, http.StatusAccepted, res.status)
	assert.Equal(t, "gzip", res.header.Get("Content-Encoding"))

	gr, err := gzip.NewReader(res.body)
	require.NoError(t, err)
	body, err := io.ReadAll(gr)
	require.NoError(t, err)
	assert.Equal(t, streamChunk1, string(body))
}

// TestStreamingFlushAsPlain verifies the first Flush can instead commit to
// passthrough and deliver the current fragment uncompressed immediately,
// without duplicating it when the rest of the body is sent later.
func TestStreamingFlushAsPlain(t *testing.T) {
	firstFlushed := make(chan struct{})
	gotFirst := make(chan struct{})

	h := streamWrapper(t, FlushAsPlain)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, streamChunk1)
		require.NoError(t, http.NewResponseController(w).Flush())
		close(firstFlushed)
		<-gotFirst
		_, _ = io.WriteString(w, streamChunk2)
	}))

	res := rawGet(t, newChunkedServer(t, h))
	defer res.close()
	assert.Equal(t, "", res.header.Get("Content-Encoding"))

	<-firstFlushed
	first := readAtLeast(t, res.conn, res.body, len(streamChunk1))
	assert.Equal(t, streamChunk1, string(first))
	close(gotFirst)

	rest, err := io.ReadAll(res.body)
	require.NoError(t, err)
	assert.Equal(t, streamChunk2, string(rest))
}

// TestStreamingFlushAsGzipPresetContentLength verifies a preset
// Content-Length is dropped when the first Flush forces gzip.
func TestStreamingFlushAsGzipPresetContentLength(t *testing.T) {
	h := streamWrapper(t, FlushAsGzip)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10000")
		_, _ = io.WriteString(w, streamChunk1)
		require.NoError(t, http.NewResponseController(w).Flush())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, gzipRequest(t))

	res := rec.Result()
	assert.Equal(t, "gzip", res.Header.Get("Content-Encoding"))
	assert.Equal(t, "", res.Header.Get("Content-Length"))
}

// TestStreamingFlushAsPlainPresetStatus verifies a saved status code is
// committed on the first passthrough flush and buffered bytes are written once.
func TestStreamingFlushAsPlainPresetStatus(t *testing.T) {
	h := streamWrapper(t, FlushAsPlain)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, streamChunk1)
		require.NoError(t, http.NewResponseController(w).Flush())
		_, _ = io.WriteString(w, streamChunk2)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, gzipRequest(t))

	res := rec.Result()
	assert.Equal(t, http.StatusTeapot, res.StatusCode)
	assert.Equal(t, "", res.Header.Get("Content-Encoding"))
	assert.Equal(t, streamChunk1+streamChunk2, rec.Body.String())
}

var errFlushFailed = errors.New("simulated flush failure")

// failingFlushWriter surfaces an error from FlushError.
type failingFlushWriter struct {
	header  http.Header
	flushed bool
}

func (f *failingFlushWriter) Header() http.Header         { return f.header }
func (f *failingFlushWriter) Write(b []byte) (int, error) { return len(b), nil }
func (f *failingFlushWriter) WriteHeader(int)             {}
func (f *failingFlushWriter) FlushError() error {
	f.flushed = true
	return errFlushFailed
}

// TestStreamingFlushErrorObservable verifies an underlying flush failure is
// surfaced through http.ResponseController.Flush rather than swallowed.
func TestStreamingFlushErrorObservable(t *testing.T) {
	fw := &failingFlushWriter{header: http.Header{}}
	gw := &GzipResponseWriter{
		ResponseWriter: fw,
		index:          poolIndex(gzip.DefaultCompression),
		minSize:        DefaultMinSize,
		flushPolicy:    FlushAsGzip,
	}

	_, err := gw.Write([]byte("hi"))
	require.NoError(t, err)

	err = http.NewResponseController(gw).Flush()
	assert.True(t, errors.Is(err, errFlushFailed), "expected flush failure, got %v", err)
	assert.True(t, fw.flushed)
}

// nonFlushingWriter implements http.ResponseWriter without flushing support.
type nonFlushingWriter struct {
	header http.Header
}

func (n *nonFlushingWriter) Header() http.Header         { return n.header }
func (n *nonFlushingWriter) Write(b []byte) (int, error) { return len(b), nil }
func (n *nonFlushingWriter) WriteHeader(int)             {}

// TestStreamingFlushErrorNotSupported verifies FlushError reports
// http.ErrNotSupported when the underlying writer cannot flush.
func TestStreamingFlushErrorNotSupported(t *testing.T) {
	gw := &GzipResponseWriter{
		ResponseWriter: &nonFlushingWriter{header: http.Header{}},
		index:          poolIndex(gzip.DefaultCompression),
		minSize:        DefaultMinSize,
		flushPolicy:    FlushAsGzip,
	}
	_, err := gw.Write([]byte("hi"))
	require.NoError(t, err)
	assert.True(t, errors.Is(gw.FlushError(), http.ErrNotSupported))
}

// TestStreamingFlushAfterClose verifies a Flush after the deferred Close
// neither writes a second gzip trailer nor panics, and the body is one gzip
// member ending cleanly in EOF.
func TestStreamingFlushAfterClose(t *testing.T) {
	var gw *GzipResponseWriter
	h := streamWrapper(t, FlushAsGzip)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gw = w.(*GzipResponseWriter)
		_, _ = io.WriteString(w, streamChunk1)
		require.NoError(t, http.NewResponseController(w).Flush())
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, gzipRequest(t))

	assert.NotPanics(t, func() { gw.Flush() })
	assert.NoError(t, gw.Close())

	gr, err := gzip.NewReader(rec.Body)
	require.NoError(t, err)
	body, err := io.ReadAll(gr)
	require.NoError(t, err)
	assert.Equal(t, streamChunk1, string(body))
	_, err = gr.Read(make([]byte, 1))
	assert.True(t, errors.Is(err, io.EOF))
}

// TestStreamingFlushInvalidPolicy verifies configuration validation.
func TestStreamingFlushInvalidPolicy(t *testing.T) {
	_, err := GzipHandlerWithOpts(StreamingFlush(FlushPolicy(99)))
	assert.Error(t, err)
}
