package goproxy_test

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"hash"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/stretchr/testify/require"
)

const digestTrailer = "X-Content-Sha256"

// failAfterReader returns bytes up to failAt, then fails mid-stream with a
// non-EOF error (rather than cleanly closing), simulating a truncated upstream.
type failAfterReader struct {
	data   []byte
	offset int
	failAt int
}

func (r *failAfterReader) Read(p []byte) (int, error) {
	if r.offset >= r.failAt {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, r.data[r.offset:r.failAt])
	r.offset += n
	return n, nil
}

func (r *failAfterReader) Close() error { return nil }

func addDigestOnResponse(ctx *goproxy.ProxyCtx, fired *atomic.Int32, sawComplete *atomic.Bool) error {
	return ctx.AddContentDigestTrailer(digestTrailer, sha256.New,
		func(result goproxy.CopyResult, h hash.Hash, _ *goproxy.ProxyCtx) string {
			fired.Add(1)
			if result.Complete() {
				sawComplete.Store(true)
				return hex.EncodeToString(h.Sum(nil))
			}
			return ""
		})
}

// registerDigest adds an OnResponse handler that installs the digest hook.
func registerDigest(proxy *goproxy.ProxyHttpServer, fired *atomic.Int32, sawComplete *atomic.Bool) {
	proxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		if err := addDigestOnResponse(ctx, fired, sawComplete); err != nil {
			panic(err)
		}
		return resp
	})
}

func requireDigestMatches(t *testing.T, resp *http.Response, body string) {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	require.Equal(t, hex.EncodeToString(sum[:]), resp.Trailer.Get(digestTrailer),
		"real client must observe the digest trailer after the body")
}

func TestDigestTrailerHTTP1Complete(t *testing.T) {
	const body = "the quick brown fox jumps over the lazy dog"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Deliberately set Content-Length: announcing a trailer must make the
		// proxy drop it and switch to chunked, or the trailer is silently lost.
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()

	proxy := goproxy.NewProxyHttpServer()
	var fired atomic.Int32
	var complete atomic.Bool
	registerDigest(proxy, &fired, &complete)
	client, ps := oneShotProxy(proxy)
	defer ps.Close()

	resp, err := client.Get(upstream.URL + "/")
	require.NoError(t, err)
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, body, string(got))

	requireDigestMatches(t, resp, body)
	// The upstream declared Content-Length; with a trailer the proxy must
	// reframe as chunked (HTTP/1.1) rather than silently dropping the trailer.
	require.Contains(t, resp.TransferEncoding, "chunked")
	require.Equal(t, int32(1), fired.Load(), "hook must run exactly once")
	require.True(t, complete.Load())
}

func TestDigestTrailerHTTP1TruncatedUpstream(t *testing.T) {
	const body = "0123456789ABCDEF-truncated-here"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()

	proxy := goproxy.NewProxyHttpServer()
	var fired atomic.Int32
	var complete atomic.Bool
	proxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		resp.Body = &failAfterReader{data: []byte(body), failAt: 8}
		// Register after replacing the body: the digest must cover the body
		// the client actually receives (the replacement layer).
		require.NoError(t, addDigestOnResponse(ctx, &fired, &complete))
		return resp
	})
	client, ps := oneShotProxy(proxy)
	defer ps.Close()

	resp, err := client.Get(upstream.URL + "/")
	require.NoError(t, err)
	defer resp.Body.Close()

	got, _ := io.ReadAll(resp.Body)
	require.Equal(t, "01234567", string(got))
	require.Empty(t, resp.Trailer.Get(digestTrailer),
		"unexpected upstream EOF must not produce a verified digest")
	require.Equal(t, int32(1), fired.Load())
	require.False(t, complete.Load(), "hook must observe an incomplete copy")
}

func TestDigestTrailerBodilessAndHead(t *testing.T) {
	cases := []struct {
		name   string
		method string
		status int
	}{
		{"head", http.MethodHead, http.StatusOK},
		{"no-content", http.MethodGet, http.StatusNoContent},
		{"not-modified", http.MethodGet, http.StatusNotModified},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", "11")
				w.WriteHeader(tc.status)
			}))
			defer upstream.Close()

			proxy := goproxy.NewProxyHttpServer()
			var fired atomic.Int32
			var complete atomic.Bool
			registerDigest(proxy, &fired, &complete)
			client, ps := oneShotProxy(proxy)
			defer ps.Close()

			req, err := http.NewRequestWithContext(context.Background(), tc.method, upstream.URL+"/", nil)
			require.NoError(t, err)
			resp, err := client.Do(req)
			require.NoError(t, err)
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()

			require.Empty(t, resp.Trailer.Get(digestTrailer),
				"HEAD/bodiless responses must not carry a digest trailer")
			require.NotContains(t, resp.Header.Get("Trailer"), digestTrailer,
				"digest trailer must not even be announced")
			require.Zero(t, fired.Load(), "hook must not run for a bodiless response")
			require.False(t, complete.Load())
		})
	}
}

func TestDigestTrailerReplacedBodyCoversReplacement(t *testing.T) {
	const original = "ORIGINAL-upstream-bytes"
	const replacement = "replacement-body-bytes"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, original)
	}))
	defer upstream.Close()

	proxy := goproxy.NewProxyHttpServer()
	// First a handler replaces the body; then the digest handler runs and hashes
	// the replacement, proving which byte layer the digest is computed over.
	proxy.OnResponse().DoFunc(func(resp *http.Response, _ *goproxy.ProxyCtx) *http.Response {
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(strings.NewReader(replacement))
		return resp
	})
	var fired atomic.Int32
	var complete atomic.Bool
	registerDigest(proxy, &fired, &complete)

	client, ps := oneShotProxy(proxy)
	defer ps.Close()

	resp, err := client.Get(upstream.URL + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, replacement, string(got))

	requireDigestMatches(t, resp, replacement)
	require.True(t, complete.Load())
}

func TestAddResponseTrailerValidation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hi")
	}))
	defer upstream.Close()

	for _, bad := range []string{"Transfer-Encoding", "Content-Length", "Trailer", ""} {
		bad := bad
		t.Run("forbidden/"+bad, func(t *testing.T) {
			proxy := goproxy.NewProxyHttpServer()
			var registerErr error
			var fired atomic.Int32
			proxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
				registerErr = ctx.AddResponseTrailer(bad,
					func(goproxy.CopyResult, hash.Hash, *goproxy.ProxyCtx) string {
						fired.Add(1)
						return "x"
					})
				return resp
			})
			client, ps := oneShotProxy(proxy)
			defer ps.Close()
			resp, err := client.Get(upstream.URL + "/")
			require.NoError(t, err)
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()

			require.Error(t, registerErr, "invalid/forbidden trailer name %q must be rejected", bad)
			require.Zero(t, fired.Load())
		})
	}
}

func TestDigestTrailerHTTP2Complete(t *testing.T) {
	const body = "http2-body-with-a-digest-trailer!!"
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, 2, r.ProtoMajor, "upstream must be negotiated over HTTP/2")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	backend.EnableHTTP2 = true
	backend.StartTLS()
	defer backend.Close()

	proxy := goproxy.NewProxyHttpServer()
	proxy.AllowHTTP2 = true
	proxy.Tr = &http.Transport{
		ForceAttemptHTTP2: true,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"h2"},
		},
	}
	proxy.OnRequest().HandleConnect(goproxy.AlwaysMitm)
	var fired atomic.Int32
	var complete atomic.Bool
	registerDigest(proxy, &fired, &complete)
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()
	client2 := createProxyClientH2(t, proxySrv.URL)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, backend.URL+"/h2", nil)
	require.NoError(t, err)
	resp, err := client2.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 2, resp.ProtoMajor)

	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, body, string(got))

	requireDigestMatches(t, resp, body)
	require.Equal(t, int32(1), fired.Load())
	require.True(t, complete.Load())
}

func TestDigestTrailerHTTP2TruncatedUpstream(t *testing.T) {
	const body = "h2-stream-that-gets-cut-short!!"
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, 2, r.ProtoMajor)
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		flusher.Flush()
		_, _ = io.WriteString(w, body)
	}))
	backend.EnableHTTP2 = true
	backend.StartTLS()
	defer backend.Close()

	proxy := goproxy.NewProxyHttpServer()
	proxy.AllowHTTP2 = true
	proxy.Tr = &http.Transport{
		ForceAttemptHTTP2: true,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"h2"},
		},
	}
	proxy.OnRequest().HandleConnect(goproxy.AlwaysMitm)
	var fired atomic.Int32
	var complete atomic.Bool
	proxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		resp.Body = &failAfterReader{data: []byte(body), failAt: 7}
		require.NoError(t, addDigestOnResponse(ctx, &fired, &complete))
		return resp
	})
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()
	client := createProxyClientH2(t, proxySrv.URL)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, backend.URL+"/h2-cut", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	_, _ = io.ReadAll(resp.Body)
	require.Empty(t, resp.Trailer.Get(digestTrailer),
		"truncated HTTP/2 stream must not yield a verified digest")
	require.Equal(t, int32(1), fired.Load())
	require.False(t, complete.Load())
}

func TestDigestTrailerHTTP1ClientCancel(t *testing.T) {
	const chunk = "0123456789ABCDEF"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 1000; i++ {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
			flusher.Flush()
			time.Sleep(2 * time.Millisecond)
		}
	}))
	defer upstream.Close()

	proxy := goproxy.NewProxyHttpServer()
	var fired atomic.Int32
	var complete atomic.Bool
	registerDigest(proxy, &fired, &complete)
	client, ps := oneShotProxy(proxy)
	defer ps.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL+"/slow", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)

	buf := make([]byte, len(chunk))
	_, _ = io.ReadFull(resp.Body, buf)
	cancel()
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Once the proxy notices the broken downstream write it must not certify
	// completeness. Give the server-side hook a moment to have fired.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && fired.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	require.Equal(t, int32(1), fired.Load(), "hook should still run exactly once")
	require.False(t, complete.Load(),
		"downstream write failure / client cancel must not be reported complete")
}

func TestDigestTrailerCoexistsWithUpstreamTrailers(t *testing.T) {
	const body = "grpc-style-payload"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Trailer", "Grpc-Status")
		w.Header().Set("Content-Type", "application/grpc")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
		w.Header().Set("Grpc-Status", "0")
	}))
	defer upstream.Close()

	proxy := goproxy.NewProxyHttpServer()
	var fired atomic.Int32
	var complete atomic.Bool
	registerDigest(proxy, &fired, &complete)
	client, ps := oneShotProxy(proxy)
	defer ps.Close()

	resp, err := client.Get(upstream.URL + "/rpc")
	require.NoError(t, err)
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, body, string(got))

	require.Equal(t, "0", resp.Trailer.Get("Grpc-Status"),
		"original upstream trailer must still be forwarded")
	requireDigestMatches(t, resp, body)
	require.Equal(t, int32(1), fired.Load())
}

func TestDigestTrailerHTTP2CoexistsWithUnannouncedUpstreamTrailer(t *testing.T) {
	const body = "late-trailer-payload"
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, 2, r.ProtoMajor)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
		// Unannounced upstream trailer: forwarded via TrailerPrefix, whereas
		// the digest hook is pre-announced. Both must reach the client.
		w.Header().Set(http.TrailerPrefix+"X-Late-Upstream", "late-value")
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()

	proxy := goproxy.NewProxyHttpServer()
	proxy.AllowHTTP2 = true
	proxy.Tr = &http.Transport{
		ForceAttemptHTTP2: true,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"h2"},
		},
	}
	proxy.OnRequest().HandleConnect(goproxy.AlwaysMitm)
	var fired atomic.Int32
	var complete atomic.Bool
	registerDigest(proxy, &fired, &complete)
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()
	client := createProxyClientH2(t, proxySrv.URL)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, upstream.URL+"/late", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, body, string(got))

	require.Equal(t, "late-value", resp.Trailer.Get("X-Late-Upstream"))
	requireDigestMatches(t, resp, body)
	require.Equal(t, int32(1), fired.Load())
}

func TestDigestTrailerHTTP1StreamingFlush(t *testing.T) {
	const events = "data: one\n\ndata: two\n\ndata: three\n\n"
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, line := range []string{"data: one\n\n", "data: two\n\n", "data: three\n\n"} {
			_, _ = io.WriteString(w, line)
			flusher.Flush()
		}
		<-release
	}))
	defer upstream.Close()

	proxy := goproxy.NewProxyHttpServer()
	var fired atomic.Int32
	var complete atomic.Bool
	registerDigest(proxy, &fired, &complete)
	client, ps := oneShotProxy(proxy)
	defer ps.Close()

	resp, err := client.Get(upstream.URL + "/stream")
	require.NoError(t, err)
	defer resp.Body.Close()

	// Each SSE event must arrive before the server handler returns, proving
	// the hooked copy path still flushes incrementally.
	buf := make([]byte, len(events))
	_, err = io.ReadFull(resp.Body, buf)
	require.NoError(t, err)
	require.Equal(t, events, string(buf))

	// Ending the upstream handler closes the stream; the client then sees trailers.
	close(release)
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Empty(t, got)
	requireDigestMatches(t, resp, events)
	require.True(t, complete.Load())
	require.Equal(t, int32(1), fired.Load())
}

func TestDigestTrailerHTTP1EmptyBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	proxy := goproxy.NewProxyHttpServer()
	var fired atomic.Int32
	var complete atomic.Bool
	registerDigest(proxy, &fired, &complete)
	client, ps := oneShotProxy(proxy)
	defer ps.Close()

	resp, err := client.Get(upstream.URL + "/empty")
	require.NoError(t, err)
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Empty(t, got)

	var empty [32]byte
	sum := sha256.Sum256(empty[:0])
	require.Equal(t, hex.EncodeToString(sum[:]), resp.Trailer.Get(digestTrailer),
		"an empty 200 body is a valid, complete body and still gets a digest")
	require.True(t, complete.Load())
	require.Equal(t, int32(1), fired.Load())
}

// registerPlainTrailer installs a non-hashing response trailer hook.
func registerPlainTrailer(proxy *goproxy.ProxyHttpServer, name string, fn goproxy.ResponseTrailerFunc) error {
	var regErr error
	proxy.OnResponse().DoFunc(func(resp *http.Response, ctx *goproxy.ProxyCtx) *http.Response {
		regErr = ctx.AddResponseTrailer(name, fn)
		return resp
	})
	return regErr
}

// TestAddResponseTrailerAfterCommitRejected proves the "declare before header
// commit" contract: once the response-end hook runs (headers and body already
// committed to the client), a further declaration must be rejected.
func TestAddResponseTrailerLateDeclarationRejected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "late")
	}))
	defer upstream.Close()

	proxy := goproxy.NewProxyHttpServer()
	var lateErr error
	registerPlainTrailer(proxy, "X-Phase",
		func(result goproxy.CopyResult, _ hash.Hash, ctx *goproxy.ProxyCtx) string {
			lateErr = ctx.AddResponseTrailer("X-Too-Late",
				func(goproxy.CopyResult, hash.Hash, *goproxy.ProxyCtx) string { return "v" })
			return "done"
		})

	client, ps := oneShotProxy(proxy)
	defer ps.Close()
	resp, err := client.Get(upstream.URL + "/")
	require.NoError(t, err)
	got, _ := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	require.Equal(t, int64(4), got)
	require.Equal(t, "done", resp.Trailer.Get("X-Phase"))
	require.Error(t, lateErr, "declaring a trailer after commit must be rejected")
	require.Empty(t, resp.Trailer.Get("X-Too-Late"))
}
