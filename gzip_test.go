package gziphandler

import (
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
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

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

// --------------------------------------------------------------------
// Completion callback tests

var errForcedUnderlyingWrite = errors.New("forced underlying write failure")

// failAtWriteWriter wraps a ResponseWriter and fails (or short-writes) once
// the number of underlying Write calls reaches failAt (1-based). Every later
// call fails too. failAt == 0 means never fail.
type failAtWriteWriter struct {
	http.ResponseWriter
	failAt  int
	short   bool
	calls   int
	written int64
}

func (w *failAtWriteWriter) Write(b []byte) (int, error) {
	w.calls++
	if w.failAt > 0 && w.calls >= w.failAt {
		if w.short {
			n := len(b) - 1
			if n < 0 {
				n = 0
			}
			nw, _ := w.ResponseWriter.Write(b[:n])
			w.written += int64(nw)
			return nw, nil
		}
		return 0, errForcedUnderlyingWrite
	}
	n, err := w.ResponseWriter.Write(b)
	w.written += int64(n)
	return n, err
}

func newFailWriter(failAt int, short bool) *failAtWriteWriter {
	return &failAtWriteWriter{
		ResponseWriter: httptest.NewRecorder(),
		failAt:         failAt,
		short:          short,
	}
}

func serveCompletionBody(t *testing.T, w http.ResponseWriter, fn CompletionFunc, handlerFn func(http.ResponseWriter, *http.Request)) {
	t.Helper()
	wrapper, err := GzipHandlerWithOpts(OnCompletion(fn))
	if !assert.NoError(t, err) {
		return
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	wrapper(http.HandlerFunc(handlerFn)).ServeHTTP(w, req)
}

func bodyWriteHandler(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}
}

func TestCompletionNormalGzip(t *testing.T) {
	rec := httptest.NewRecorder()
	var fired int
	var stats CompletionStats

	serveCompletionBody(t, rec, func(s CompletionStats) {
		fired++
		stats = s
	}, bodyWriteHandler(testBody))

	assert.Equal(t, 1, fired, "callback must run exactly once")
	assert.Equal(t, int64(len(testBody)), stats.UncompressedBytes)
	assert.Equal(t, int64(rec.Body.Len()), stats.WrittenBytes)
	assert.Equal(t, "gzip", stats.ContentEncoding)
	assert.NoError(t, stats.FirstError)
	assert.NoError(t, stats.CloseError)

	// The gzip trailer must be present, i.e. the stream fully closed.
	gr, err := gzip.NewReader(rec.Body)
	assert.NoError(t, err)
	out, err := ioutil.ReadAll(gr)
	assert.NoError(t, err)
	assert.Equal(t, testBody, string(out))
}

func TestCompletionNormalPlain(t *testing.T) {
	rec := httptest.NewRecorder()
	var fired int
	var stats CompletionStats

	serveCompletionBody(t, rec, func(s CompletionStats) {
		fired++
		stats = s
	}, bodyWriteHandler(smallTestBody))

	assert.Equal(t, 1, fired)
	assert.Equal(t, int64(len(smallTestBody)), stats.UncompressedBytes)
	assert.Equal(t, int64(len(smallTestBody)), stats.WrittenBytes)
	assert.Equal(t, "identity", stats.ContentEncoding)
	assert.NoError(t, stats.FirstError)
	assert.NoError(t, stats.CloseError)
	assert.Equal(t, smallTestBody, rec.Body.String())
}

func TestCompletionNotCalledWhenGzipNotAccepted(t *testing.T) {
	var fired int
	wrapper, err := GzipHandlerWithOpts(OnCompletion(func(CompletionStats) { fired++ }))
	assert.NoError(t, err)

	req := httptest.NewRequest("GET", "/", nil) // no Accept-Encoding: gzip
	rec := httptest.NewRecorder()
	wrapper(http.HandlerFunc(bodyWriteHandler(testBody))).ServeHTTP(rec, req)

	assert.Equal(t, 0, fired)
	assert.Equal(t, testBody, rec.Body.String())
}

// probeWriteCount serves the handler once without failures and returns how
// many Writes the gzip machinery issued to the underlying ResponseWriter.
func probeWriteCount(t *testing.T, handlerFn func(http.ResponseWriter, *http.Request)) *failAtWriteWriter {
	fw := newFailWriter(0, false)
	serveCompletionBody(t, fw, func(CompletionStats) {}, handlerFn)
	return fw
}

func TestCompletionTrailerWriteFailure(t *testing.T) {
	// Find out how many underlying writes a successful gzip response takes,
	// then fail only the final one (the gzip trailer written on Close).
	probe := probeWriteCount(t, bodyWriteHandler(testBody))

	fw := newFailWriter(probe.calls, false)
	var stats CompletionStats
	serveCompletionBody(t, fw, func(s CompletionStats) { stats = s }, bodyWriteHandler(testBody))

	assert.Equal(t, "gzip", stats.ContentEncoding)
	assert.Equal(t, int64(len(testBody)), stats.UncompressedBytes)
	assert.Equal(t, errForcedUnderlyingWrite, stats.FirstError)
	assert.Equal(t, errForcedUnderlyingWrite, stats.CloseError)

	// The trailer never made it to the wire, so the stream is not complete.
	gr, err := gzip.NewReader(fw.ResponseWriter.(*httptest.ResponseRecorder).Body)
	assert.NoError(t, err)
	_, err = io.Copy(ioutil.Discard, gr)
	assert.Error(t, err)
}

func TestCompletionEarlyWriteFailure(t *testing.T) {
	// The very first underlying write is the gzip header write.
	fw := newFailWriter(1, false)
	var stats CompletionStats
	serveCompletionBody(t, fw, func(s CompletionStats) { stats = s }, bodyWriteHandler(testBody))

	assert.Equal(t, "gzip", stats.ContentEncoding)
	assert.Equal(t, int64(0), stats.UncompressedBytes, "no handler byte must be accepted")
	assert.Equal(t, errForcedUnderlyingWrite, stats.FirstError)
	assert.Equal(t, errForcedUnderlyingWrite, stats.CloseError)
	assert.Equal(t, int64(0), stats.WrittenBytes)
}

func TestCompletionFlushFailure(t *testing.T) {
	// Determine which underlying write the Flush triggers.
	flushCall := 0
	probeFw := newFailWriter(0, false)
	flushingHandler := func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, testBody)
		w.(http.Flusher).Flush()
		if p, ok := w.(*GzipResponseWriterWithCloseNotify); ok {
			_ = p
		}
	}
	// The fail writer lives inside the middleware, so expose the call count
	// by writing to it through the gzip wrapper: use a recorder hook instead.
	serveCompletionBody(t, &flushProbeWriter{failAtWriteWriter: probeFw, onFlushCalls: &flushCall}, func(CompletionStats) {}, flushingHandler)

	fw := newFailWriter(flushCall, false)
	var stats CompletionStats
	serveCompletionBody(t, fw, func(s CompletionStats) { stats = s }, flushingHandler)

	assert.Equal(t, "gzip", stats.ContentEncoding)
	assert.Equal(t, int64(len(testBody)), stats.UncompressedBytes)
	assert.Equal(t, errForcedUnderlyingWrite, stats.FirstError, "Flush error must be captured")
	assert.Equal(t, errForcedUnderlyingWrite, stats.CloseError)
}

// flushProbeWriter records the underlying Write call count at Flush time.
type flushProbeWriter struct {
	*failAtWriteWriter
	onFlushCalls *int
}

func (w *flushProbeWriter) Flush() {
	*w.onFlushCalls = w.calls
	if f, ok := w.failAtWriteWriter.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func TestCompletionShortWriteNotComplete(t *testing.T) {
	// Gzip path: short-write the final (trailer) underlying write.
	probe := probeWriteCount(t, bodyWriteHandler(testBody))
	fw := newFailWriter(probe.calls, true)
	var stats CompletionStats
	serveCompletionBody(t, fw, func(s CompletionStats) { stats = s }, bodyWriteHandler(testBody))

	assert.Error(t, stats.FirstError)
	assert.Error(t, stats.CloseError)
	assert.Equal(t, int64(len(testBody)), stats.UncompressedBytes)
	expected := fw.ResponseWriter.(*httptest.ResponseRecorder).Body.Len()
	assert.Equal(t, int64(expected), stats.WrittenBytes)

	// Plain path: a short passthrough write must be counted as partial and
	// reported as io.ErrShortWrite.
	rec := httptest.NewRecorder()
	plain := &plainShortWriter{ResponseWriter: rec}
	var plainStats CompletionStats
	serveCompletionBody(t, plain, func(s CompletionStats) { plainStats = s }, bodyWriteHandler(smallTestBody))

	assert.Equal(t, "identity", plainStats.ContentEncoding)
	assert.Equal(t, int64(len(smallTestBody)-1), plainStats.UncompressedBytes)
	assert.Equal(t, int64(len(smallTestBody)-1), plainStats.WrittenBytes)
	assert.Equal(t, io.ErrShortWrite, plainStats.FirstError)
	assert.NoError(t, plainStats.CloseError)
}

type plainShortWriter struct {
	http.ResponseWriter
	calls int
}

func (w *plainShortWriter) Write(b []byte) (int, error) {
	w.calls++
	if w.calls == 1 {
		n, err := w.ResponseWriter.Write(b[:len(b)-1])
		return n, err
	}
	return w.ResponseWriter.Write(b)
}

func TestCompletionHandlerPanic(t *testing.T) {
	rec := httptest.NewRecorder()
	var fired int
	var stats CompletionStats

	assert.Panics(t, func() {
		serveCompletionBody(t, rec, func(s CompletionStats) {
			fired++
			stats = s
		}, func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, testBody)
			panic("boom")
		})
	})

	assert.Equal(t, 1, fired, "callback still runs exactly once on panic")
	assert.Equal(t, "gzip", stats.ContentEncoding)
	assert.Equal(t, int64(len(testBody)), stats.UncompressedBytes)
	assert.NoError(t, stats.FirstError)
	assert.NoError(t, stats.CloseError, "the deferred close still finishes the trailer")

	gr, err := gzip.NewReader(rec.Body)
	assert.NoError(t, err)
	out, err := ioutil.ReadAll(gr)
	assert.NoError(t, err)
	assert.Equal(t, testBody, string(out))
}

func TestCompletionDoubleCloseAfterStarted(t *testing.T) {
	addLevelPool(gzip.DefaultCompression)

	var fired int
	var stats CompletionStats
	handler := GzipHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, testBody)
		// Explicit close; the middleware's deferred Close runs second.
		assert.NoError(t, w.(io.Closer).Close())
	}))

	wrapper, err := GzipHandlerWithOpts(OnCompletion(func(s CompletionStats) {
		fired++
		stats = s
	}))
	assert.NoError(t, err)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	wrapper(handler).ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, 1, fired, "callback runs once even with double close")
	assert.NoError(t, stats.CloseError)

	pool := gzipWriterPools[poolIndex(gzip.DefaultCompression)]
	w1 := pool.Get().(*gzip.Writer)
	w2 := pool.Get().(*gzip.Writer)
	assert.False(t, w1 == w2, "same writer must not be returned to the pool twice")

	// A recycled writer must still be usable.
	var buf bytes.Buffer
	w1.Reset(&buf)
	_, werr := io.WriteString(w1, "recycled")
	assert.NoError(t, werr)
	assert.NoError(t, w1.Close())
}

func TestCompletionHijack(t *testing.T) {
	var (
		mu    sync.Mutex
		fired int
		stats CompletionStats
	)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	assert.NoError(t, err)

	wrapper, err := GzipHandlerWithOpts(OnCompletion(func(s CompletionStats) {
		mu.Lock()
		defer mu.Unlock()
		fired++
		stats = s
	}))
	assert.NoError(t, err)

	srv := &http.Server{
		Handler: wrapper(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hj, ok := w.(http.Hijacker)
			assert.True(t, ok)
			conn, bufrw, err := hj.Hijack()
			assert.NoError(t, err)
			defer conn.Close()

			reply := "HTTP/1.1 200 OK\r\nContent-Length: 5\r\nConnection: close\r\n\r\nhello"
			bufrw.WriteString(reply)
			bufrw.Flush()
		})),
	}
	go srv.Serve(ln)
	defer srv.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	assert.NoError(t, err)
	defer conn.Close()
	io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\nAccept-Encoding: gzip\r\n\r\n")
	raw, err := ioutil.ReadAll(conn)
	assert.NoError(t, err)

	assert.True(t, strings.HasSuffix(string(raw), "hello"), "no gzip trailer must be written after hijack: %q", raw)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, fired)
	assert.Equal(t, "identity", stats.ContentEncoding)
	assert.Equal(t, int64(0), stats.UncompressedBytes)
	assert.Equal(t, int64(0), stats.WrittenBytes)
	assert.NoError(t, stats.FirstError)
	assert.NoError(t, stats.CloseError)
}
