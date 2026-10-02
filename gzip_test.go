package gziphandler

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// FlushError is the method picked up by http.ResponseController.Flush on
// Go 1.20 and newer.
var _ interface{ FlushError() error } = (*GzipResponseWriter)(nil)

const (
	smallTestBody = "aaabbcaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbc"
	testBody      = "aaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbccc aaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbccc aaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbccc aaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbccc aaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbccc aaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbccc aaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbcccaaabbbccc"
)

func TestParseEncodings(t *testing.T) {
	examples := map[string]codings{

		// Examples from RFC 2616
		"compress, gzip":                     {"compress": 1.0, "gzip": 1.0},
		"":                                   {},
		"*":                                  {"*": 1.0},
		"compress;q=0.5, gzip;q=1.0":         {"compress": 0.5, "gzip": 1.0},
		"gzip;q=1.0, identity; q=0.5, *;q=0": {"gzip": 1.0, "identity": 0.5, "*": 0.0},

		// More random stuff
		"AAA;q=1":     {"aaa": 1.0},
		"BBB ; q = 2": {"bbb": 1.0},
	}

	for eg, exp := range examples {
		act, _ := parseEncodings(eg)
		assert.Equal(t, exp, act)
	}
}

func TestGzipHandler(t *testing.T) {
	// This just exists to provide something for GzipHandler to wrap.
	handler := newTestHandler(testBody)

	// requests without accept-encoding are passed along as-is

	req1, _ := http.NewRequest("GET", "/whatever", nil)
	resp1 := httptest.NewRecorder()
	handler.ServeHTTP(resp1, req1)
	res1 := resp1.Result()

	assert.Equal(t, 200, res1.StatusCode)
	assert.Equal(t, "", res1.Header.Get("Content-Encoding"))
	assert.Equal(t, "Accept-Encoding", res1.Header.Get("Vary"))
	assert.Equal(t, testBody, resp1.Body.String())

	// but requests with accept-encoding:gzip are compressed if possible

	req2, _ := http.NewRequest("GET", "/whatever", nil)
	req2.Header.Set("Accept-Encoding", "gzip")
	resp2 := httptest.NewRecorder()
	handler.ServeHTTP(resp2, req2)
	res2 := resp2.Result()

	assert.Equal(t, 200, res2.StatusCode)
	assert.Equal(t, "gzip", res2.Header.Get("Content-Encoding"))
	assert.Equal(t, "Accept-Encoding", res2.Header.Get("Vary"))
	assert.Equal(t, gzipStrLevel(testBody, gzip.DefaultCompression), resp2.Body.Bytes())

	// content-type header is correctly set based on uncompressed body

	req3, _ := http.NewRequest("GET", "/whatever", nil)
	req3.Header.Set("Accept-Encoding", "gzip")
	res3 := httptest.NewRecorder()
	handler.ServeHTTP(res3, req3)

	assert.Equal(t, http.DetectContentType([]byte(testBody)), res3.Header().Get("Content-Type"))
}

func TestGzipHandlerSmallBodyNoCompression(t *testing.T) {
	handler := newTestHandler(smallTestBody)

	req, _ := http.NewRequest("GET", "/whatever", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	res := resp.Result()

	// with less than 1400 bytes the response should not be gzipped

	assert.Equal(t, 200, res.StatusCode)
	assert.Equal(t, "", res.Header.Get("Content-Encoding"))
	assert.Equal(t, "Accept-Encoding", res.Header.Get("Vary"))
	assert.Equal(t, smallTestBody, resp.Body.String())

}

func TestGzipHandlerAlreadyCompressed(t *testing.T) {
	handler := newTestHandler(testBody)

	req, _ := http.NewRequest("GET", "/gzipped", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	assert.Equal(t, testBody, res.Body.String())
}

func TestNewGzipLevelHandler(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, testBody)
	})

	for lvl := gzip.BestSpeed; lvl <= gzip.BestCompression; lvl++ {
		wrapper, err := NewGzipLevelHandler(lvl)
		if !assert.Nil(t, err, "NewGzipLevleHandler returned error for level:", lvl) {
			continue
		}

		req, _ := http.NewRequest("GET", "/whatever", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		resp := httptest.NewRecorder()
		wrapper(handler).ServeHTTP(resp, req)
		res := resp.Result()

		assert.Equal(t, 200, res.StatusCode)
		assert.Equal(t, "gzip", res.Header.Get("Content-Encoding"))
		assert.Equal(t, "Accept-Encoding", res.Header.Get("Vary"))
		assert.Equal(t, gzipStrLevel(testBody, lvl), resp.Body.Bytes())
	}
}

func TestNewGzipLevelHandlerReturnsErrorForInvalidLevels(t *testing.T) {
	var err error
	_, err = NewGzipLevelHandler(-42)
	assert.NotNil(t, err)

	_, err = NewGzipLevelHandler(42)
	assert.NotNil(t, err)
}

func TestMustNewGzipLevelHandlerWillPanic(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("panic was not called")
		}
	}()

	_ = MustNewGzipLevelHandler(-42)
}

func TestGzipHandlerNoBody(t *testing.T) {
	tests := []struct {
		statusCode      int
		contentEncoding string
		emptyBody       bool
		body            []byte
	}{
		// Body must be empty.
		{http.StatusNoContent, "", true, nil},
		{http.StatusNotModified, "", true, nil},
		// Body is going to get gzip'd no matter what.
		{http.StatusOK, "", true, []byte{}},
		{http.StatusOK, "gzip", false, []byte(testBody)},
	}

	for num, test := range tests {
		handler := GzipHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(test.statusCode)
			if test.body != nil {
				w.Write(test.body)
			}
		}))

		rec := httptest.NewRecorder()
		// TODO: in Go1.7 httptest.NewRequest was introduced this should be used
		// once 1.6 is not longer supported.
		req := &http.Request{
			Method:     "GET",
			URL:        &url.URL{Path: "/"},
			Proto:      "HTTP/1.1",
			ProtoMinor: 1,
			RemoteAddr: "192.0.2.1:1234",
			Header:     make(http.Header),
		}
		req.Header.Set("Accept-Encoding", "gzip")
		handler.ServeHTTP(rec, req)

		body, err := ioutil.ReadAll(rec.Body)
		if err != nil {
			t.Fatalf("Unexpected error reading response body: %v", err)
		}

		header := rec.Header()
		assert.Equal(t, test.contentEncoding, header.Get("Content-Encoding"), fmt.Sprintf("for test iteration %d", num))
		assert.Equal(t, "Accept-Encoding", header.Get("Vary"), fmt.Sprintf("for test iteration %d", num))
		if test.emptyBody {
			assert.Empty(t, body, fmt.Sprintf("for test iteration %d", num))
		} else {
			assert.NotEmpty(t, body, fmt.Sprintf("for test iteration %d", num))
			assert.NotEqual(t, test.body, body, fmt.Sprintf("for test iteration %d", num))
		}
	}
}

func TestGzipHandlerContentLength(t *testing.T) {
	testBodyBytes := []byte(testBody)
	tests := []struct {
		bodyLen   int
		bodies    [][]byte
		emptyBody bool
	}{
		{len(testBody), [][]byte{testBodyBytes}, false},
		// each of these writes is less than the DefaultMinSize
		{len(testBody), [][]byte{testBodyBytes[:200], testBodyBytes[200:]}, false},
		// without a defined Content-Length it should still gzip
		{0, [][]byte{testBodyBytes[:200], testBodyBytes[200:]}, false},
		// simulate a HEAD request with an empty write (to populate headers)
		{len(testBody), [][]byte{nil}, true},
	}

	// httptest.NewRecorder doesn't give you access to the Content-Length
	// header so instead, we create a server on a random port and make
	// a request to that instead
	ln, err := net.Listen("tcp", "127.0.0.1:")
	if err != nil {
		t.Fatalf("failed creating listen socket: %v", err)
	}
	defer ln.Close()
	srv := &http.Server{
		Handler: nil,
	}
	go srv.Serve(ln)

	for num, test := range tests {
		srv.Handler = GzipHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if test.bodyLen > 0 {
				w.Header().Set("Content-Length", strconv.Itoa(test.bodyLen))
			}
			for _, b := range test.bodies {
				w.Write(b)
			}
		}))
		req := &http.Request{
			Method: "GET",
			URL:    &url.URL{Path: "/", Scheme: "http", Host: ln.Addr().String()},
			Header: make(http.Header),
			Close:  true,
		}
		req.Header.Set("Accept-Encoding", "gzip")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Unexpected error making http request in test iteration %d: %v", num, err)
		}
		defer res.Body.Close()

		body, err := ioutil.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("Unexpected error reading response body in test iteration %d: %v", num, err)
		}

		l, err := strconv.Atoi(res.Header.Get("Content-Length"))
		if err != nil {
			t.Fatalf("Unexpected error parsing Content-Length in test iteration %d: %v", num, err)
		}
		if test.emptyBody {
			assert.Empty(t, body, fmt.Sprintf("for test iteration %d", num))
			assert.Equal(t, 0, l, fmt.Sprintf("for test iteration %d", num))
		} else {
			assert.Len(t, body, l, fmt.Sprintf("for test iteration %d", num))
		}
		assert.Equal(t, "gzip", res.Header.Get("Content-Encoding"), fmt.Sprintf("for test iteration %d", num))
		assert.NotEqual(t, test.bodyLen, l, fmt.Sprintf("for test iteration %d", num))
	}
}

func TestGzipHandlerMinSizeMustBePositive(t *testing.T) {
	_, err := NewGzipLevelAndMinSize(gzip.DefaultCompression, -1)
	assert.Error(t, err)
}

func TestGzipHandlerMinSize(t *testing.T) {
	responseLength := 0
	b := []byte{'x'}

	wrapper, _ := NewGzipLevelAndMinSize(gzip.DefaultCompression, 128)
	handler := wrapper(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			// Write responses one byte at a time to ensure that the flush
			// mechanism, if used, is working properly.
			for i := 0; i < responseLength; i++ {
				n, err := w.Write(b)
				assert.Equal(t, 1, n)
				assert.Nil(t, err)
			}
		},
	))

	r, _ := http.NewRequest("GET", "/whatever", &bytes.Buffer{})
	r.Header.Add("Accept-Encoding", "gzip")

	// Short response is not compressed
	responseLength = 127
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Result().Header.Get(contentEncoding) == "gzip" {
		t.Error("Expected uncompressed response, got compressed")
	}

	// Long response is not compressed
	responseLength = 128
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Result().Header.Get(contentEncoding) != "gzip" {
		t.Error("Expected compressed response, got uncompressed")
	}
}

func TestGzipDoubleClose(t *testing.T) {
	// reset the pool for the default compression so we can make sure duplicates
	// aren't added back by double close
	addLevelPool(gzip.DefaultCompression)

	handler := GzipHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// call close here and it'll get called again interally by
		// NewGzipLevelHandler's handler defer
		w.Write([]byte("test"))
		w.(io.Closer).Close()
	}))

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	// the second close shouldn't have added the same writer
	// so we pull out 2 writers from the pool and make sure they're different
	w1 := gzipWriterPools[poolIndex(gzip.DefaultCompression)].Get()
	w2 := gzipWriterPools[poolIndex(gzip.DefaultCompression)].Get()
	// assert.NotEqual looks at the value and not the address, so we use regular ==
	assert.False(t, w1 == w2)
}

type panicOnSecondWriteHeaderWriter struct {
	http.ResponseWriter
	headerWritten bool
}

func (w *panicOnSecondWriteHeaderWriter) WriteHeader(s int) {
	if w.headerWritten {
		panic("header already written")
	}
	w.headerWritten = true
	w.ResponseWriter.WriteHeader(s)
}

func TestGzipHandlerDoubleWriteHeader(t *testing.T) {
	handler := GzipHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "15000")
		// Specifically write the header here
		w.WriteHeader(304)
		// Ensure that after a Write the header isn't triggered again on close
		w.Write(nil)
	}))
	wrapper := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w = &panicOnSecondWriteHeaderWriter{
			ResponseWriter: w,
		}
		handler.ServeHTTP(w, r)
	})

	rec := httptest.NewRecorder()
	// TODO: in Go1.7 httptest.NewRequest was introduced this should be used
	// once 1.6 is not longer supported.
	req := &http.Request{
		Method:     "GET",
		URL:        &url.URL{Path: "/"},
		Proto:      "HTTP/1.1",
		ProtoMinor: 1,
		RemoteAddr: "192.0.2.1:1234",
		Header:     make(http.Header),
	}
	req.Header.Set("Accept-Encoding", "gzip")
	wrapper.ServeHTTP(rec, req)
	body, err := ioutil.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("Unexpected error reading response body: %v", err)
	}
	assert.Empty(t, body)
	header := rec.Header()
	assert.Equal(t, "gzip", header.Get("Content-Encoding"))
	assert.Equal(t, "Accept-Encoding", header.Get("Vary"))
	assert.Equal(t, 304, rec.Code)
}

func TestStatusCodes(t *testing.T) {
	handler := GzipHandler(http.NotFoundHandler())
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	result := w.Result()
	if result.StatusCode != 404 {
		t.Errorf("StatusCode should have been 404 but was %d", result.StatusCode)
	}
}

func TestFlushBeforeWrite(t *testing.T) {
	b := []byte(testBody)
	handler := GzipHandler(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.WriteHeader(http.StatusNotFound)
		rw.(http.Flusher).Flush()
		rw.Write(b)
	}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	res := w.Result()
	assert.Equal(t, http.StatusNotFound, res.StatusCode)
	assert.Equal(t, "gzip", res.Header.Get("Content-Encoding"))
	assert.NotEqual(t, b, w.Body.Bytes())
}

// newHeldGzipWriter builds a handler that parks after exposing its
// GzipResponseWriter, so a test can drive Write/Flush/Close directly. The
// returned channel must be closed at the end of the test to let the handler
// (and its deferred Close) finish.
func newHeldGzipWriter(t *testing.T, strategy FlushStrategy) (*GzipResponseWriter, *httptest.ResponseRecorder, chan<- struct{}) {
	wrapper, err := GzipHandlerWithOpts(CompressionLevel(gzip.BestSpeed), MinSize(10000), WithFlushStrategy(strategy))
	if err != nil {
		t.Fatalf("GzipHandlerWithOpts: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(acceptEncoding, "gzip")

	done := make(chan struct{})
	ready := make(chan *GzipResponseWriter)
	handler := wrapper(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ready <- w.(*GzipResponseWriter)
		<-done
	}))
	go handler.ServeHTTP(rec, req)

	return <-ready, rec, done
}

func decodeGzip(t *testing.T, b []byte) []byte {
	t.Helper()
	gr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	out, err := ioutil.ReadAll(gr)
	if err != nil {
		// A flushed-but-not-closed gzip stream is truncated; whatever was
		// decoded up to the missing trailer is still valid output.
		if err != io.ErrUnexpectedEOF && err != io.EOF {
			t.Fatalf("reading gzip body: %v", err)
		}
	}
	return out
}

func TestFirstFlushCommitsGzipBelowMinSize(t *testing.T) {
	gw, rec, done := newHeldGzipWriter(t, FlushGzipImmediately)
	defer close(done)

	first := []byte("first fragment")
	if n, err := gw.Write(first); err != nil || n != len(first) {
		t.Fatalf("Write: n=%d err=%v", n, err)
	}
	assert.Equal(t, encodingUndecided, gw.encoding, "still buffered until the first flush")
	assert.False(t, rec.Flushed)

	if err := gw.FlushError(); err != nil {
		t.Fatalf("FlushError: %v", err)
	}

	assert.Equal(t, encodingGzip, gw.encoding, "first flush commits gzip")
	assert.True(t, rec.Flushed)
	assert.Equal(t, "gzip", rec.Header().Get(contentEncoding))

	// The whole current fragment is already observable after the first flush,
	// and the buffer must not be written a second time later.
	firstSeen := decodeGzip(t, append([]byte(nil), rec.Body.Bytes()...))
	assert.Equal(t, first, firstSeen)

	second := []byte("second fragment")
	if _, err := gw.Write(second); err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	assert.Equal(t, append(first, second...), decodeGzip(t, rec.Body.Bytes()))
}

func TestFirstFlushCommitsIdentity(t *testing.T) {
	gw, rec, done := newHeldGzipWriter(t, FlushAsIdentity)
	defer close(done)

	first := []byte("first fragment")
	gw.Write(first)
	assert.Equal(t, encodingUndecided, gw.encoding)

	if err := gw.FlushError(); err != nil {
		t.Fatalf("FlushError: %v", err)
	}

	assert.Equal(t, encodingPlain, gw.encoding, "first flush commits passthrough")
	assert.True(t, rec.Flushed)
	assert.Equal(t, "", rec.Header().Get(contentEncoding))
	assert.Equal(t, first, rec.Body.Bytes(), "buffered bytes handed out on first flush")

	// Later writes can never switch back to gzip.
	second := []byte("second fragment")
	gw.Write(second)
	if err := gw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assert.Equal(t, "", rec.Header().Get(contentEncoding))
	assert.Equal(t, append(first, second...), rec.Body.Bytes())
}

func TestZeroBodyFirstFlushGzip(t *testing.T) {
	gw, rec, done := newHeldGzipWriter(t, FlushGzipImmediately)
	defer close(done)

	gw.WriteHeader(http.StatusAccepted)
	if err := gw.FlushError(); err != nil {
		t.Fatalf("FlushError: %v", err)
	}

	assert.Equal(t, encodingGzip, gw.encoding)
	assert.Equal(t, http.StatusAccepted, rec.Code, "saved status is written")
	assert.Equal(t, "gzip", rec.Header().Get(contentEncoding))
	assert.True(t, rec.Flushed)

	// The gzip stream already exists; a later write just extends it and Close
	// emits exactly one gzip trailer.
	if _, err := gw.Write([]byte("late body")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	out := decodeGzip(t, rec.Body.Bytes())
	assert.Equal(t, []byte("late body"), out)

	// Exactly one gzip member: a second member cannot start inside the body.
	br := bytes.NewReader(rec.Body.Bytes())
	gr1, err := gzip.NewReader(br)
	if err != nil {
		t.Fatalf("first gzip.NewReader: %v", err)
	}
	if _, err := ioutil.ReadAll(gr1); err != nil {
		t.Fatalf("read first member: %v", err)
	}
	if _, err := gzip.NewReader(br); err == nil {
		t.Fatal("expected no second gzip member, got one")
	}
}

func TestZeroBodyFirstFlushIdentity(t *testing.T) {
	gw, rec, done := newHeldGzipWriter(t, FlushAsIdentity)
	defer close(done)

	gw.WriteHeader(http.StatusAccepted)
	if err := gw.FlushError(); err != nil {
		t.Fatalf("FlushError: %v", err)
	}

	assert.Equal(t, encodingPlain, gw.encoding)
	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.Equal(t, "", rec.Header().Get(contentEncoding))
	assert.Empty(t, rec.Body.Bytes())
	assert.True(t, rec.Flushed)

	gw.Write([]byte("plain late body"))
	if err := gw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assert.Equal(t, []byte("plain late body"), rec.Body.Bytes())
}

func TestFirstFlushGzipWithPresetContentLength(t *testing.T) {
	// A preset Content-Length below MinSize means compression is ineligible,
	// so the first flush is forced to passthrough and the Content-Length is
	// preserved.
	handler, err := GzipHandlerWithOpts(MinSize(10000), WithFlushStrategy(FlushGzipImmediately))
	if err != nil {
		t.Fatalf("GzipHandlerWithOpts: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(acceptEncoding, "gzip")

	h := handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := []byte("short")
		w.Header().Set(contentLength, strconv.Itoa(len(body)))
		w.Write(body)
		w.(http.Flusher).Flush()
	}))
	h.ServeHTTP(rec, req)

	assert.Equal(t, "", rec.Header().Get(contentEncoding))
	assert.Equal(t, strconv.Itoa(len("short")), rec.Header().Get(contentLength))
	assert.Equal(t, []byte("short"), rec.Body.Bytes())
}

func TestFirstFlushGzipLargeContentLength(t *testing.T) {
	// A preset Content-Length at or above MinSize keeps gzip eligible, so
	// the first flush emits the buffered prefix as gzip and drops CL.
	gw, rec, done := newHeldGzipWriter(t, FlushGzipImmediately)
	defer close(done)
	gw.Header().Set(contentLength, "20000")
	gw.Write([]byte("prefix"))
	if err := gw.FlushError(); err != nil {
		t.Fatalf("FlushError: %v", err)
	}

	assert.Equal(t, encodingGzip, gw.encoding)
	assert.Equal(t, "gzip", rec.Header().Get(contentEncoding))
	assert.Equal(t, "", rec.Header().Get(contentLength))
	assert.Equal(t, []byte("prefix"), decodeGzip(t, rec.Body.Bytes()))
}

func TestFirstFlushRespectsPresetContentEncoding(t *testing.T) {
	gw, rec, done := newHeldGzipWriter(t, FlushGzipImmediately)
	defer close(done)
	gw.Header().Set(contentEncoding, "gzip")
	gw.Write([]byte("already-encoded"))
	if err := gw.FlushError(); err != nil {
		t.Fatalf("FlushError: %v", err)
	}

	assert.Equal(t, encodingPlain, gw.encoding)
	assert.Equal(t, "gzip", rec.Header().Get(contentEncoding), "existing encoding is untouched")
	assert.Equal(t, []byte("already-encoded"), rec.Body.Bytes())
}

func TestFirstFlushRespectsContentTypeFilter(t *testing.T) {
	wrapper, err := GzipHandlerWithOpts(
		ContentTypes([]string{"application/json"}),
		WithFlushStrategy(FlushGzipImmediately),
	)
	if err != nil {
		t.Fatalf("GzipHandlerWithOpts: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(acceptEncoding, "gzip")
	wrapper(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(contentType, "text/plain")
		w.Write([]byte("not json"))
		w.(http.Flusher).Flush()
	})).ServeHTTP(rec, req)

	assert.Equal(t, "", rec.Header().Get(contentEncoding))
	assert.Equal(t, []byte("not json"), rec.Body.Bytes())
}

func TestFlushWithoutStrategyStaysNoOp(t *testing.T) {
	gw, rec, done := newHeldGzipWriter(t, nil)
	defer close(done)
	gw.WriteHeader(http.StatusTeapot)
	gw.Write([]byte("buffered"))
	if err := gw.FlushError(); err != nil {
		t.Fatalf("FlushError: %v", err)
	}

	assert.Equal(t, encodingUndecided, gw.encoding)
	assert.False(t, rec.Flushed)
	assert.Equal(t, http.StatusOK, rec.Code, "nothing reaches the underlying writer yet")

	if err := gw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assert.Equal(t, http.StatusTeapot, rec.Code)
	assert.Equal(t, []byte("buffered"), rec.Body.Bytes())
}

func TestFlushStrategyDeferKeepsBuffering(t *testing.T) {
	gw, rec, done := newHeldGzipWriter(t, func(int) FlushDecision { return FlushDefer })
	defer close(done)
	gw.Write([]byte("buffered"))
	gw.FlushError()

	assert.Equal(t, encodingUndecided, gw.encoding)
	assert.False(t, rec.Flushed)

	// A later flush may still commit.
	gw.flushStrategy = FlushGzipImmediately
	if err := gw.FlushError(); err != nil {
		t.Fatalf("second FlushError: %v", err)
	}
	assert.Equal(t, encodingGzip, gw.encoding)
	assert.Equal(t, []byte("buffered"), decodeGzip(t, rec.Body.Bytes()))
}

func TestEncodingCannotChangeAfterFirstFlush(t *testing.T) {
	gw, _, done := newHeldGzipWriter(t, FlushAsIdentity)
	defer close(done)
	gw.Write([]byte("x"))
	gw.FlushError()

	// Even swapping the strategy after the decision has no effect.
	gw.flushStrategy = FlushGzipImmediately
	if err := gw.FlushError(); err != nil {
		t.Fatalf("FlushError: %v", err)
	}
	assert.Equal(t, encodingPlain, gw.encoding)
	assert.Nil(t, gw.gw)
}

func TestSubsequentGzipFlushStreamsMoreData(t *testing.T) {
	gw, rec, done := newHeldGzipWriter(t, FlushGzipImmediately)
	defer close(done)

	gw.Write([]byte("one"))
	if err := gw.FlushError(); err != nil {
		t.Fatalf("first FlushError: %v", err)
	}
	firstLen := rec.Body.Len()

	gw.Write([]byte("two"))
	if err := gw.FlushError(); err != nil {
		t.Fatalf("second FlushError: %v", err)
	}
	if rec.Body.Len() <= firstLen {
		t.Fatalf("second flush should emit more gzip bytes: %d <= %d", rec.Body.Len(), firstLen)
	}

	if err := gw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assert.Equal(t, []byte("onetwo"), decodeGzip(t, rec.Body.Bytes()))

	// A second Close must not emit another gzip trailer.
	if err := gw.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestLegacyFlushSwallowsErrors(t *testing.T) {
	gw, rec, done := newHeldGzipWriter(t, FlushAsIdentity)
	defer close(done)

	failing := &failingFlushWriter{ResponseWriter: rec}
	gw.ResponseWriter = failing
	gw.Write([]byte("x"))

	// The http.Flusher method does not return errors; it must not panic.
	gw.Flush()
	assert.Equal(t, 1, failing.flushed)
}

func TestImplementCloseNotifier(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set(acceptEncoding, "gzip")
	GzipHandler(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		_, ok := rw.(http.CloseNotifier)
		assert.True(t, ok, "response writer must implement http.CloseNotifier")
	})).ServeHTTP(&mockRWCloseNotify{}, request)
}

func TestImplementFlusherAndCloseNotifier(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set(acceptEncoding, "gzip")
	GzipHandler(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		_, okCloseNotifier := rw.(http.CloseNotifier)
		assert.True(t, okCloseNotifier, "response writer must implement http.CloseNotifier")
		_, okFlusher := rw.(http.Flusher)
		assert.True(t, okFlusher, "response writer must implement http.Flusher")
	})).ServeHTTP(&mockRWCloseNotify{}, request)
}

func TestNotImplementCloseNotifier(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set(acceptEncoding, "gzip")
	GzipHandler(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		_, ok := rw.(http.CloseNotifier)
		assert.False(t, ok, "response writer must not implement http.CloseNotifier")
	})).ServeHTTP(httptest.NewRecorder(), request)
}

type mockRWCloseNotify struct{}

func (m *mockRWCloseNotify) CloseNotify() <-chan bool {
	panic("implement me")
}

func (m *mockRWCloseNotify) Header() http.Header {
	return http.Header{}
}

func (m *mockRWCloseNotify) Write([]byte) (int, error) {
	panic("implement me")
}

func (m *mockRWCloseNotify) WriteHeader(int) {
	panic("implement me")
}

// failingFlushWriter fails every flush, and records whether flushing was
// attempted at all.
type failingFlushWriter struct {
	http.ResponseWriter
	flushed int
}

func (w *failingFlushWriter) Write(b []byte) (int, error) {
	return w.ResponseWriter.Write(b)
}

func (w *failingFlushWriter) FlushError() error {
	w.flushed++
	return errors.New("boom: flush failed")
}

func TestIgnoreSubsequentWriteHeader(t *testing.T) {
	handler := GzipHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.WriteHeader(404)
	}))
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	result := w.Result()
	if result.StatusCode != 500 {
		t.Errorf("StatusCode should have been 500 but was %d", result.StatusCode)
	}
}

func TestDontWriteWhenNotWrittenTo(t *testing.T) {
	// When using gzip as middleware without ANY writes in the handler,
	// ensure the gzip middleware doesn't touch the actual ResponseWriter
	// either.

	handler0 := GzipHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	}))

	handler1 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler0.ServeHTTP(w, r)
		w.WriteHeader(404) // this only works if gzip didn't do a WriteHeader(200)
	})

	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	handler1.ServeHTTP(w, r)

	result := w.Result()
	if result.StatusCode != 404 {
		t.Errorf("StatusCode should have been 404 but was %d", result.StatusCode)
	}
}

var contentTypeTests = []struct {
	name                 string
	contentType          string
	acceptedContentTypes []string
	expectedGzip         bool
}{
	{
		name:                 "Always gzip when content types are empty",
		contentType:          "",
		acceptedContentTypes: []string{},
		expectedGzip:         true,
	},
	{
		name:                 "MIME match",
		contentType:          "application/json",
		acceptedContentTypes: []string{"application/json"},
		expectedGzip:         true,
	},
	{
		name:                 "MIME no match",
		contentType:          "text/xml",
		acceptedContentTypes: []string{"application/json"},
		expectedGzip:         false,
	},
	{
		name:                 "MIME match with no other directive ignores non-MIME directives",
		contentType:          "application/json; charset=utf-8",
		acceptedContentTypes: []string{"application/json"},
		expectedGzip:         true,
	},
	{
		name:                 "MIME match with other directives requires all directives be equal, different charset",
		contentType:          "application/json; charset=ascii",
		acceptedContentTypes: []string{"application/json; charset=utf-8"},
		expectedGzip:         false,
	},
	{
		name:                 "MIME match with other directives requires all directives be equal, same charset",
		contentType:          "application/json; charset=utf-8",
		acceptedContentTypes: []string{"application/json; charset=utf-8"},
		expectedGzip:         true,
	},
	{
		name:                 "MIME match with other directives requires all directives be equal, missing charset",
		contentType:          "application/json",
		acceptedContentTypes: []string{"application/json; charset=ascii"},
		expectedGzip:         false,
	},
	{
		name:                 "MIME match case insensitive",
		contentType:          "Application/Json",
		acceptedContentTypes: []string{"application/json"},
		expectedGzip:         true,
	},
	{
		name:                 "MIME match ignore whitespace",
		contentType:          "application/json;charset=utf-8",
		acceptedContentTypes: []string{"application/json;            charset=utf-8"},
		expectedGzip:         true,
	},
}

func TestContentTypes(t *testing.T) {
	for _, tt := range contentTypeTests {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Header().Set("Content-Type", tt.contentType)
			io.WriteString(w, testBody)
		})

		wrapper, err := GzipHandlerWithOpts(ContentTypes(tt.acceptedContentTypes))
		if !assert.Nil(t, err, "NewGzipHandlerWithOpts returned error", tt.name) {
			continue
		}

		req, _ := http.NewRequest("GET", "/whatever", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		resp := httptest.NewRecorder()
		wrapper(handler).ServeHTTP(resp, req)
		res := resp.Result()

		assert.Equal(t, 200, res.StatusCode)
		if tt.expectedGzip {
			assert.Equal(t, "gzip", res.Header.Get("Content-Encoding"), tt.name)
		} else {
			assert.NotEqual(t, "gzip", res.Header.Get("Content-Encoding"), tt.name)
		}
	}
}

// readAvailableChunks reads complete HTTP/1.1 chunked chunks from r until no
// more data arrives within the idle timeout. It returns the concatenated
// chunk payloads (without framing) and whether the stream ended (a
// zero-length terminator chunk or EOF, as happens when the server closes
// the connection).
func readAvailableChunks(t *testing.T, conn net.Conn, r *bufio.Reader, idle time.Duration) ([]byte, bool) {
	t.Helper()
	var out []byte
	for {
		conn.SetReadDeadline(time.Now().Add(idle))
		line, err := r.ReadString('\n')
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return out, false
			}
			// EOF (possibly with a closed-connection error) means the
			// server ended the stream.
			if line == "" {
				return out, true
			}
			t.Fatalf("reading chunk size line %q: %v", line, err)
		}
		n, err := strconv.ParseUint(strings.TrimSpace(line), 16, 64)
		if err != nil {
			t.Fatalf("parsing chunk size %q: %v", line, err)
		}
		if n == 0 {
			io.ReadFull(r, make([]byte, 2))
			return out, true
		}
		chunk := make([]byte, n)
		if _, err := io.ReadFull(r, chunk); err != nil {
			t.Fatalf("reading %d byte chunk: %v", n, err)
		}
		io.ReadFull(r, make([]byte, 2))
		out = append(out, chunk...)
	}
}

// gunzipAvailable decompresses as much of a possibly truncated gzip stream
// as it can, returning the decoded bytes.
func gunzipAvailable(t *testing.T, raw []byte) []byte {
	t.Helper()
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip.NewReader on first fragment: %v", err)
	}
	var out bytes.Buffer
	_, _ = io.Copy(&out, gr)
	return out.Bytes()
}

// TestStreamingFlushFirstFragment verifies against a real, chunk-reading
// client that the first Flush hands out the entire currently buffered body
// fragment even when it is far below MinSize: with the gzip strategy the
// client gunzips exactly the first fragment, and with the identity strategy
// the raw first chunk is the fragment itself.
func TestStreamingFlushFirstFragment(t *testing.T) {
	const minSize = 4096
	frag1 := strings.Repeat("a", 100)
	frag2 := strings.Repeat("b", 100)

	for _, tc := range []struct {
		name     string
		strategy FlushStrategy
		gzipped  bool
	}{
		{"gzip on first flush", FlushGzipImmediately, true},
		{"identity on first flush", FlushAsIdentity, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proceed1 := make(chan struct{})

			wrapper, err := GzipHandlerWithOpts(
				CompressionLevel(gzip.BestSpeed),
				MinSize(minSize),
				WithFlushStrategy(tc.strategy),
			)
			if err != nil {
				t.Fatalf("GzipHandlerWithOpts: %v", err)
			}

			handler := wrapper(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusAccepted)
				flusher := w.(http.Flusher)

				if _, err := io.WriteString(w, frag1); err != nil {
					t.Errorf("write frag1: %v", err)
				}
				flusher.Flush()
				<-proceed1

				if _, err := io.WriteString(w, frag2); err != nil {
					t.Errorf("write frag2: %v", err)
				}
				flusher.Flush()
				// Return: the wrapper's deferred Close emits the gzip
				// trailer (when compressing) and the server ends the
				// chunked stream.
			}))

			// Use an unstarted server and Start it (rather than NewServer)
			// to get a plaintext HTTP server we can speak to directly over
			// a raw TCP connection.
			srv := httptest.NewUnstartedServer(handler)
			srv.Start()
			defer srv.Close()

			conn, err := net.Dial("tcp", srv.Listener.Addr().String())
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()

			if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: example\r\nAccept-Encoding: gzip\r\nConnection: close\r\n\r\n"); err != nil {
				t.Fatalf("write request: %v", err)
			}

			r := bufio.NewReader(conn)
			reqForParse := &http.Request{Method: "GET", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1}
			resp, err := http.ReadResponse(r, reqForParse)
			if err != nil {
				t.Fatalf("read response headers: %v", err)
			}

			assert.Equal(t, http.StatusAccepted, resp.StatusCode)
			assert.Equal(t, []string{"chunked"}, resp.TransferEncoding)
			if tc.gzipped {
				assert.Equal(t, "gzip", resp.Header.Get("Content-Encoding"))
			} else {
				assert.Equal(t, "", resp.Header.Get("Content-Encoding"))
			}

			first, done := readAvailableChunks(t, conn, r, 250*time.Millisecond)
			assert.False(t, done, "stream must stay open after the first flush")
			if tc.gzipped {
				assert.Equal(t, []byte(frag1), gunzipAvailable(t, first),
					"first flush delivers the complete current fragment as gzip")
			} else {
				assert.Equal(t, []byte(frag1), first,
					"first flush delivers the complete current fragment verbatim")
			}

			close(proceed1)
			rest, done := readAvailableChunks(t, conn, r, 5*time.Second)
			assert.True(t, done, "stream finishes after the handler returns")

			if tc.gzipped {
				fullRaw := append(append([]byte(nil), first...), rest...)
				fullReader := bytes.NewReader(fullRaw)
				gr, err := gzip.NewReader(fullReader)
				if err != nil {
					t.Fatalf("gzip.NewReader full body: %v", err)
				}
				decoded, err := ioutil.ReadAll(gr)
				if err != nil {
					t.Fatalf("read full body: %v", err)
				}
				assert.Equal(t, frag1+frag2, string(decoded))

				// The second wire fragment must start right after the bytes
				// that encoded the first fragment: decoding the first
				// fragment must yield exactly frag1 with no prefix of frag2.
				firstDecoded := gunzipAvailable(t, first)
				assert.Equal(t, []byte(frag1), firstDecoded,
					"the first fragment is complete and only frag1")
				if _, err := gzip.NewReader(fullReader); err == nil {
					t.Fatal("expected exactly one gzip member, found a second one")
				}
			} else {
				assert.Equal(t, []byte(frag2), rest)
			}
		})
	}
}

// --------------------------------------------------------------------

func BenchmarkGzipHandler_S2k(b *testing.B)   { benchmark(b, false, 2048) }
func BenchmarkGzipHandler_S20k(b *testing.B)  { benchmark(b, false, 20480) }
func BenchmarkGzipHandler_S100k(b *testing.B) { benchmark(b, false, 102400) }
func BenchmarkGzipHandler_P2k(b *testing.B)   { benchmark(b, true, 2048) }
func BenchmarkGzipHandler_P20k(b *testing.B)  { benchmark(b, true, 20480) }
func BenchmarkGzipHandler_P100k(b *testing.B) { benchmark(b, true, 102400) }

// --------------------------------------------------------------------

func gzipStrLevel(s string, lvl int) []byte {
	var b bytes.Buffer
	w, _ := gzip.NewWriterLevel(&b, lvl)
	io.WriteString(w, s)
	w.Close()
	return b.Bytes()
}

func benchmark(b *testing.B, parallel bool, size int) {
	bin, err := ioutil.ReadFile("testdata/benchmark.json")
	if err != nil {
		b.Fatal(err)
	}

	req, _ := http.NewRequest("GET", "/whatever", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	handler := newTestHandler(string(bin[:size]))

	if parallel {
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				runBenchmark(b, req, handler)
			}
		})
	} else {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			runBenchmark(b, req, handler)
		}
	}
}

func runBenchmark(b *testing.B, req *http.Request, handler http.Handler) {
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if code := res.Code; code != 200 {
		b.Fatalf("Expected 200 but got %d", code)
	} else if blen := res.Body.Len(); blen < 500 {
		b.Fatalf("Expected complete response body, but got %d bytes", blen)
	}
}

func newTestHandler(body string) http.Handler {
	return GzipHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gzipped":
			w.Header().Set("Content-Encoding", "gzip")
			io.WriteString(w, body)
		default:
			io.WriteString(w, body)
		}
	}))
}
