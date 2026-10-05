package goproxy_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const digestTrailerName = "X-Content-Sha256"

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// recordTrailerHook is a ResponseTrailerHook that records every ResponseEnd
// it is asked about, so tests can assert how often and with what final state
// the hook ran.
type recordTrailerHook struct {
	name  string
	value string
	ends  chan goproxy.ResponseEnd
}

func newRecordTrailerHook(name, value string) *recordTrailerHook {
	return &recordTrailerHook{name: name, value: value, ends: make(chan goproxy.ResponseEnd, 10)}
}

func (h *recordTrailerHook) TrailerName() string { return h.name }

func (h *recordTrailerHook) TrailerValue(end goproxy.ResponseEnd) (string, bool) {
	h.ends <- end
	if !end.Complete() {
		return "", false
	}
	return h.value, true
}

func (h *recordTrailerHook) requireEnd(t *testing.T) goproxy.ResponseEnd {
	t.Helper()
	select {
	case end := <-h.ends:
		return end
	case <-time.After(5 * time.Second):
		t.Fatal("trailer hook was not called")
		return goproxy.ResponseEnd{}
	}
}

// declareDigest registers a digest trailer plus a recording hook on every response.
func declareDigest(proxy *goproxy.ProxyHttpServer, rec *recordTrailerHook) {
	proxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		ctx.DeclareResponseTrailer(goproxy.NewDigestTrailer(digestTrailerName, sha256.New()))
		if rec != nil {
			ctx.DeclareResponseTrailer(rec)
		}
		return resp
	})
}

// failAfterReader yields data and then fails with err, simulating an upstream
// body that breaks in the middle of the stream.
type failAfterReader struct {
	data []byte
	off  int
	err  error
}

func (r *failAfterReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, r.err
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

func doGet(t *testing.T, client *http.Client, rawURL string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, rawURL, nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp, string(body)
}

// TestDigestTrailerPlainHTTP1 checks the digest trailer end to end with a real
// HTTP/1 client: the upstream sends a Content-Length, which the proxy must
// drop in favor of chunked framing for the trailer to be emitted at all.
func TestDigestTrailerPlainHTTP1(t *testing.T) {
	const bodyText = "hello trailer"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, bodyText)
	}))
	defer upstream.Close()

	rec := newRecordTrailerHook("X-Record", "recorded")
	proxy := goproxy.NewProxyHttpServer()
	declareDigest(proxy, rec)

	client, proxySrv := oneShotProxy(proxy)
	defer proxySrv.Close()

	resp, body := doGet(t, client, upstream.URL+"/")
	require.Equal(t, bodyText, body)
	assert.Equal(t, sha256Hex(bodyText), resp.Trailer.Get(digestTrailerName),
		"client must receive the digest of the exact bytes it got")
	assert.Equal(t, "recorded", resp.Trailer.Get("X-Record"))
	assert.Contains(t, resp.TransferEncoding, "chunked",
		"trailers require chunked framing; Content-Length must be dropped")

	end := rec.requireEnd(t)
	assert.True(t, end.Complete())
	assert.Equal(t, int64(len(bodyText)), end.BytesCopied)
	assert.Len(t, rec.ends, 0, "hook must run at most once")
}

// TestDigestTrailerHTTP2Client checks the digest trailer with a real HTTP/2
// client talking to the proxy through a MITM'd h2 connection.
func TestDigestTrailerHTTP2Client(t *testing.T) {
	const bodyText = "hello h2 trailer"
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, bodyText)
	}))
	defer upstream.Close()

	rec := newRecordTrailerHook("X-Record", "recorded")
	proxy := goproxy.NewProxyHttpServer()
	proxy.AllowHTTP2 = true
	proxy.Tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	proxy.OnRequest().HandleConnect(goproxy.AlwaysMitm)
	declareDigest(proxy, rec)

	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	client := createProxyClientH2(t, proxySrv.URL)
	resp, body := doGet(t, client, upstream.URL+"/")
	require.Equal(t, 2, resp.ProtoMajor, "client should be talking HTTP/2 to the proxy")
	require.Equal(t, bodyText, body)
	assert.Equal(t, sha256Hex(bodyText), resp.Trailer.Get(digestTrailerName))
	assert.Equal(t, "recorded", resp.Trailer.Get("X-Record"))

	end := rec.requireEnd(t)
	assert.True(t, end.Complete())
	assert.Equal(t, int64(len(bodyText)), end.BytesCopied)
}

// TestDigestTrailerMitmHTTP1 checks the digest trailer through a MITM'd
// HTTP/1.1 connection, where the proxy writes the response with resp.Write.
func TestDigestTrailerMitmHTTP1(t *testing.T) {
	const bodyText = "hello mitm trailer"
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, bodyText)
	}))
	defer upstream.Close()

	rec := newRecordTrailerHook("X-Record", "recorded")
	proxy := goproxy.NewProxyHttpServer()
	proxy.Tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	proxy.OnRequest().HandleConnect(goproxy.AlwaysMitm)
	declareDigest(proxy, rec)

	client, proxySrv := oneShotProxy(proxy)
	defer proxySrv.Close()

	resp, body := doGet(t, client, upstream.URL+"/")
	require.Equal(t, 1, resp.ProtoMajor)
	require.Equal(t, bodyText, body)
	assert.Equal(t, sha256Hex(bodyText), resp.Trailer.Get(digestTrailerName))
	assert.Equal(t, "recorded", resp.Trailer.Get("X-Record"))

	end := rec.requireEnd(t)
	assert.True(t, end.Complete())
	assert.Equal(t, int64(len(bodyText)), end.BytesCopied)
}

// TestDigestTrailerSuppressedOnUpstreamError replaces the body with a reader
// that fails mid-stream: the client must not receive a "body verified"
// digest for the truncated response.
func TestDigestTrailerSuppressedOnUpstreamError(t *testing.T) {
	const partial = "partial-body"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "full-body-that-never-arrives")
	}))
	defer upstream.Close()

	rec := newRecordTrailerHook("X-Record", "recorded")
	proxy := goproxy.NewProxyHttpServer()
	declareDigest(proxy, rec)
	proxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		resp.Body = io.NopCloser(&failAfterReader{data: []byte(partial), err: io.ErrUnexpectedEOF})
		return resp
	})

	client, proxySrv := oneShotProxy(proxy)
	defer proxySrv.Close()

	resp, body := doGet(t, client, upstream.URL+"/")
	assert.Equal(t, partial, body)
	assert.Empty(t, resp.Trailer.Get(digestTrailerName),
		"an incomplete body must not carry a verification digest")
	assert.Empty(t, resp.Trailer.Get("X-Record"))

	end := rec.requireEnd(t)
	assert.False(t, end.Complete())
	assert.True(t, errors.Is(end.Err, io.ErrUnexpectedEOF), "unexpected error: %v", end.Err)
	assert.Equal(t, int64(len(partial)), end.BytesCopied)
	assert.Len(t, rec.ends, 0, "hook must run at most once")
}

// TestDigestTrailerSuppressedOnClientCancel cancels the request while the
// body is streaming: the downstream write fails and the hook must observe a
// non-complete end, so no verification digest is produced.
func TestDigestTrailerSuppressedOnClientCancel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; ; i++ {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			_, _ = fmt.Fprintf(w, "chunk-%d\n", i)
			flusher.Flush()
			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	rec := newRecordTrailerHook("X-Record", "recorded")
	proxy := goproxy.NewProxyHttpServer()
	declareDigest(proxy, rec)

	client, proxySrv := oneShotProxy(proxy)
	defer proxySrv.Close()

	reqCtx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, upstream.URL+"/", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)

	// Read one chunk, then cancel: the proxy's next write to the client fails.
	_, err = resp.Body.Read(make([]byte, 64))
	require.NoError(t, err)
	cancel()
	_ = resp.Body.Close()

	end := rec.requireEnd(t)
	assert.False(t, end.Complete(), "a cancelled stream must not report a complete body")
	assert.Error(t, end.Err)
}

// TestNoTrailerForBodilessResponse checks that HEAD and 204 responses never
// announce or emit declared trailers, and never run the hooks.
func TestNoTrailerForBodilessResponse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		status int
	}{
		{name: "head", method: http.MethodHead, status: http.StatusOK},
		{name: "no content", method: http.MethodGet, status: http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, "body")
			}))
			defer upstream.Close()

			rec := newRecordTrailerHook("X-Record", "recorded")
			proxy := goproxy.NewProxyHttpServer()
			declareDigest(proxy, rec)

			client, proxySrv := oneShotProxy(proxy)
			defer proxySrv.Close()

			req, err := http.NewRequestWithContext(context.Background(), tc.method, upstream.URL+"/", nil)
			require.NoError(t, err)
			resp, err := client.Do(req)
			require.NoError(t, err)
			_, _ = io.Copy(io.Discard, resp.Body)
			require.NoError(t, resp.Body.Close())

			assert.Empty(t, resp.Header.Get("Trailer"),
				"bodiless responses must not announce trailers")
			assert.Empty(t, resp.Trailer.Get(digestTrailerName))

			// The hook runs (if at all) before the response finishes
			// reaching the client; give stragglers a chance to show up.
			time.Sleep(50 * time.Millisecond)
			assert.Len(t, rec.ends, 0, "bodiless responses must not run trailer hooks")
		})
	}
}

// TestDigestTrailerLayerSelection checks that a filter replacing the body
// controls which byte stream the digest covers: the bytes sent to the client
// by default, or the upstream bytes when it wraps the original body itself.
func TestDigestTrailerLayerSelection(t *testing.T) {
	const (
		upstreamBody = "upstream-bytes"
		replacedBody = "replaced-bytes"
	)

	newUpstream := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, upstreamBody)
		}))
	}

	t.Run("downstream bytes by default", func(t *testing.T) {
		upstream := newUpstream()
		defer upstream.Close()

		proxy := goproxy.NewProxyHttpServer()
		proxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
			ctx.DeclareResponseTrailer(goproxy.NewDigestTrailer(digestTrailerName, sha256.New()))
			resp.Body = io.NopCloser(strings.NewReader(replacedBody))
			return resp
		})

		client, proxySrv := oneShotProxy(proxy)
		defer proxySrv.Close()

		resp, body := doGet(t, client, upstream.URL+"/")
		require.Equal(t, replacedBody, body)
		assert.Equal(t, sha256Hex(replacedBody), resp.Trailer.Get(digestTrailerName),
			"by default the digest covers the bytes sent to the client")
	})

	t.Run("upstream bytes when the filter wraps the original body", func(t *testing.T) {
		upstream := newUpstream()
		defer upstream.Close()

		var filterSaw string
		proxy := goproxy.NewProxyHttpServer()
		proxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
			digest := goproxy.NewDigestTrailer(digestTrailerName, sha256.New())
			ctx.DeclareResponseTrailer(digest)
			// Attach the digest to the original body before replacing it:
			// the trailer then describes the upstream bytes.
			raw, err := io.ReadAll(digest.WrapBody(resp.Body))
			if err != nil {
				t.Errorf("cannot read upstream body: %v", err)
				return resp
			}
			filterSaw = string(raw)
			resp.Body = io.NopCloser(strings.NewReader(replacedBody))
			return resp
		})

		client, proxySrv := oneShotProxy(proxy)
		defer proxySrv.Close()

		resp, body := doGet(t, client, upstream.URL+"/")
		require.Equal(t, replacedBody, body)
		assert.Equal(t, upstreamBody, filterSaw)
		assert.Equal(t, sha256Hex(upstreamBody), resp.Trailer.Get(digestTrailerName),
			"the digest covers the upstream bytes the filter chose to observe")
	})
}

// TestDigestTrailerCoexistsWithUpstreamTrailer checks that declared trailers
// are merged with the trailers the upstream server itself sent.
func TestDigestTrailerCoexistsWithUpstreamTrailer(t *testing.T) {
	const bodyText = "hello combined trailers"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Trailer", "X-Upstream-Trailer")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, bodyText)
		w.Header().Set("X-Upstream-Trailer", "upstream-value")
	}))
	defer upstream.Close()

	proxy := goproxy.NewProxyHttpServer()
	declareDigest(proxy, nil)

	client, proxySrv := oneShotProxy(proxy)
	defer proxySrv.Close()

	resp, body := doGet(t, client, upstream.URL+"/")
	require.Equal(t, bodyText, body)
	assert.Equal(t, "upstream-value", resp.Trailer.Get("X-Upstream-Trailer"),
		"upstream trailers must still be forwarded")
	assert.Equal(t, sha256Hex(bodyText), resp.Trailer.Get(digestTrailerName),
		"declared trailers must be emitted alongside upstream trailers")
}
