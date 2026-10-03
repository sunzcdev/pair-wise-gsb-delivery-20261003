package gziphandler

import (
	"bytes"
	"compress/gzip"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fetchTrailerResponse starts an httptest.Server wrapping h with the gzip
// middleware, requests it with an explicit Accept-Encoding: gzip header (so
// the transport does not transparently decompress), reads the body to EOF
// and returns the response along with the raw body bytes.
func fetchTrailerResponse(t *testing.T, h http.Handler) (*http.Response, []byte) {
	t.Helper()

	srv := httptest.NewServer(GzipHandler(h))
	t.Cleanup(srv.Close)

	req, err := http.NewRequest("GET", srv.URL, nil)
	require.NoError(t, err)
	req.Header.Set("Accept-Encoding", "gzip")

	res, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer res.Body.Close()

	raw, err := ioutil.ReadAll(res.Body)
	require.NoError(t, err)

	return res, raw
}

// decodeBody gunzips raw if the response was gzipped and returns the plain
// body bytes.
func decodeBody(t *testing.T, res *http.Response, raw []byte) []byte {
	t.Helper()

	if res.Header.Get("Content-Encoding") != "gzip" {
		return raw
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	require.NoError(t, err)
	body, err := ioutil.ReadAll(zr)
	require.NoError(t, err)
	require.NoError(t, zr.Close())
	return body
}

func TestTrailerSmallBodyPassthrough(t *testing.T) {
	// The first (and only) write never reaches minSize, so the response is
	// committed at Close. The trailer value is set before that happens and
	// must still be delivered only as a trailer.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Trailer", "X-Checksum")
		w.Write([]byte(smallTestBody))
		w.Header().Set("X-Checksum", "abc123")
	})

	res, raw := fetchTrailerResponse(t, handler)

	assert.Equal(t, "abc123", res.Trailer.Get("X-Checksum"))
	assert.Empty(t, res.Header.Get("X-Checksum"))
	assert.Equal(t, smallTestBody, string(decodeBody(t, res, raw)))
}

func TestTrailerLargeBodyGzipped(t *testing.T) {
	// The body exceeds minSize, so it is compressed. The trailer must be
	// delivered at the tail and the decompressed body must match exactly.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Trailer", "X-Checksum")
		w.Write([]byte(testBody))
		w.Header().Set("X-Checksum", "def456")
	})

	res, raw := fetchTrailerResponse(t, handler)

	assert.Equal(t, "gzip", res.Header.Get("Content-Encoding"))
	assert.Equal(t, "def456", res.Trailer.Get("X-Checksum"))
	assert.Empty(t, res.Header.Get("X-Checksum"))
	assert.Equal(t, testBody, string(decodeBody(t, res, raw)))
}

func TestTrailerEarlyFlush(t *testing.T) {
	// Flushing before minSize is reached is a no-op; the trailer set
	// afterwards must still be delivered only at the tail.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Trailer", "X-Checksum")
		w.Write([]byte(smallTestBody))
		w.(http.Flusher).Flush()
		w.Header().Set("X-Checksum", "flush1")
	})

	res, raw := fetchTrailerResponse(t, handler)

	assert.Equal(t, "flush1", res.Trailer.Get("X-Checksum"))
	assert.Empty(t, res.Header.Get("X-Checksum"))
	assert.Equal(t, smallTestBody, string(decodeBody(t, res, raw)))
}

func TestTrailerFlushAfterGzipStarted(t *testing.T) {
	// Flushing once the gzip writer is active commits the headers early;
	// trailer values set afterwards must still arrive as trailers.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Trailer", "X-Checksum")
		w.Write([]byte(testBody))
		w.(http.Flusher).Flush()
		w.Header().Set("X-Checksum", "flush2")
	})

	res, raw := fetchTrailerResponse(t, handler)

	assert.Equal(t, "gzip", res.Header.Get("Content-Encoding"))
	assert.Equal(t, "flush2", res.Trailer.Get("X-Checksum"))
	assert.Empty(t, res.Header.Get("X-Checksum"))
	assert.Equal(t, testBody, string(decodeBody(t, res, raw)))
}

func TestTrailerEmptyBody(t *testing.T) {
	// No body is ever written; the declared trailer must still be sent.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Trailer", "X-Checksum")
		w.Header().Set("X-Checksum", "empty0")
	})

	res, raw := fetchTrailerResponse(t, handler)

	assert.Equal(t, "empty0", res.Trailer.Get("X-Checksum"))
	assert.Empty(t, res.Header.Get("X-Checksum"))
	assert.Empty(t, decodeBody(t, res, raw))
}

func TestTrailerPrefix(t *testing.T) {
	// Trailers declared via http.TrailerPrefix must be passed through as
	// trailers, both for passthrough and compressed responses.
	for _, tc := range []struct {
		name string
		body string
	}{
		{"passthrough", smallTestBody},
		{"gzipped", testBody},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// The key must be present before the underlying response
				// is committed; for compressed responses that happens as
				// soon as the gzip writer starts streaming.
				w.Header().Set(http.TrailerPrefix+"X-Checksum", "prefix1")
				w.Write([]byte(tc.body))
			})

			res, raw := fetchTrailerResponse(t, handler)

			assert.Equal(t, "prefix1", res.Trailer.Get("X-Checksum"))
			assert.Empty(t, res.Header.Get("X-Checksum"))
			assert.Equal(t, tc.body, string(decodeBody(t, res, raw)))
		})
	}
}

func TestTrailerForbiddenFieldsRejected(t *testing.T) {
	// Fields which may not appear in trailers (RFC 7230, section 4.1.2)
	// must not be extracted into trailers.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Trailer", "Content-Type, Content-Length, X-Checksum")
		w.Write([]byte(smallTestBody))
		w.Header().Set("X-Checksum", "ok789")
	})

	res, raw := fetchTrailerResponse(t, handler)

	assert.Equal(t, "ok789", res.Trailer.Get("X-Checksum"))
	assert.Empty(t, res.Trailer.Get("Content-Type"))
	assert.Empty(t, res.Trailer.Get("Content-Length"))
	assert.Equal(t, "text/plain", res.Header.Get("Content-Type"))
	assert.Equal(t, smallTestBody, string(decodeBody(t, res, raw)))
}
