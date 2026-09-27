package goproxy_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// previewResult collects what a request filter observed through the preview
// helper, guarded against the race detector since filters run on the proxy
// server's goroutines.
type previewResult struct {
	mu      sync.Mutex
	peek    []byte
	peekErr error
	spilled bool
}

func (r *previewResult) set(peek []byte, err error, spilled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.peek = peek
	r.peekErr = err
	r.spilled = spilled
}

func (r *previewResult) get() ([]byte, error, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.peek, r.peekErr, r.spilled
}

func proxyClient(t *testing.T, proxy *goproxy.ProxyHttpServer) (*http.Client, func()) {
	t.Helper()
	proxySrv := httptest.NewServer(proxy)
	proxyURL, err := url.Parse(proxySrv.URL)
	require.NoError(t, err)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	return client, proxySrv.Close
}

// echoUploadServer returns a target server that reads the first headLen
// bytes of the request body, signals their arrival, then reads the rest and
// delivers the full body over the returned channel.
func echoUploadServer(t *testing.T, headLen int) (*httptest.Server, <-chan struct{}, <-chan []byte) {
	t.Helper()
	headSeen := make(chan struct{})
	received := make(chan []byte, 1)
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		head := make([]byte, headLen)
		if _, err := io.ReadFull(r.Body, head); err != nil {
			received <- nil
			return
		}
		once.Do(func() { close(headSeen) })
		rest, err := io.ReadAll(r.Body)
		if err != nil {
			received <- nil
			return
		}
		received <- append(head, rest...)
		w.WriteHeader(http.StatusOK)
	}))
	return srv, headSeen, received
}

// TestRequestBodyPreviewChainedFilters runs two request filters that both
// peek at the start of a slow chunked upload and replay it, then verifies
// the target server receives the exact original byte stream, and that the
// first chunk reaches the target before the client finished uploading.
func TestRequestBodyPreviewChainedFilters(t *testing.T) {
	chunk1 := []byte("first-chunk-of-the-upload--")
	chunk2 := []byte("second-chunk-with-the-rest-of-the-body")
	full := append(append([]byte{}, chunk1...), chunk2...)

	target, headSeen, received := echoUploadServer(t, len(chunk1))
	defer target.Close()

	var sniff, summary previewResult
	proxy := goproxy.NewProxyHttpServer()
	// Filter 1: content sniffing on the first bytes.
	proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		p := goproxy.NewRequestBodyPreview(req, goproxy.BodyPreviewOptions{MaxPreviewBytes: 8})
		b, err := p.Peek()
		sniff.set(b, err, p.SpilledToDisk())
		p.Replay()
		return req, nil
	})
	// Filter 2: redacted summary logging over a larger window.
	proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		p := goproxy.NewRequestBodyPreview(req, goproxy.BodyPreviewOptions{MaxPreviewBytes: 20})
		b, err := p.Peek()
		summary.set(b, err, p.SpilledToDisk())
		p.Replay()
		return req, nil
	})
	client, cleanup := proxyClient(t, proxy)
	defer cleanup()

	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, target.URL+"/upload", pr)
	require.NoError(t, err)

	streamed := make(chan bool, 1)
	go func() {
		_, _ = pw.Write(chunk1)
		select {
		case <-headSeen:
			streamed <- true
		case <-time.After(10 * time.Second):
			streamed <- false
			_ = pw.CloseWithError(errors.New("target did not receive first chunk in time"))
			return
		}
		_, _ = pw.Write(chunk2)
		_ = pw.Close()
	}()

	resp, err := client.Do(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.True(t, <-streamed, "target must receive the first chunk before the upload completes")
	require.Equal(t, full, <-received, "target must receive the exact original bytes")

	peek1, err1, _ := sniff.get()
	require.NoError(t, err1)
	require.Equal(t, full[:8], peek1)
	peek2, err2, _ := summary.get()
	require.NoError(t, err2)
	require.Equal(t, full[:20], peek2)
}

// TestRequestBodyPreviewSpillAndReject forces staging to spill to a temp
// file, then rejects the request with a local response and verifies the
// spill file is cleaned up.
func TestRequestBodyPreviewSpillAndReject(t *testing.T) {
	dir := t.TempDir()
	body := bytes.Repeat([]byte("sensitive-payload-"), 64)

	var observed previewResult
	proxy := goproxy.NewProxyHttpServer()
	proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		p := goproxy.NewRequestBodyPreview(req, goproxy.BodyPreviewOptions{
			MaxPreviewBytes: 64,
			MemoryBytes:     8,
			TempDir:         dir,
		})
		b, err := p.Peek()
		observed.set(b, err, p.SpilledToDisk())
		assert.NoError(t, p.Discard())
		return req, goproxy.NewResponse(req, goproxy.ContentTypeText, http.StatusForbidden, "rejected")
	})
	client, cleanup := proxyClient(t, proxy)
	defer cleanup()

	resp, err := client.Post("http://example.invalid/upload", "application/octet-stream", bytes.NewReader(body))
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)

	peek, err, spilled := observed.get()
	require.NoError(t, err)
	require.Equal(t, body[:64], peek)
	require.True(t, spilled, "staging must have spilled to a temp file")

	// Discard ran synchronously before the response was sent: the spill
	// file must already be gone.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "spill file must be removed after Discard")
}

// TestRequestBodyPreviewReplaceRequest verifies ownership when a filter
// replaces the request body after previewing: Discard releases the original
// body and staging resources, and the target receives the replacement.
func TestRequestBodyPreviewReplaceRequest(t *testing.T) {
	dir := t.TempDir()
	replacement := []byte("replacement-body")

	received := make(chan []byte, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			received <- nil
			return
		}
		received <- b
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	proxy := goproxy.NewProxyHttpServer()
	proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		p := goproxy.NewRequestBodyPreview(req, goproxy.BodyPreviewOptions{
			MaxPreviewBytes: 32,
			MemoryBytes:     4,
			TempDir:         dir,
		})
		_, _ = p.Peek()
		assert.NoError(t, p.Discard())
		req.Body = io.NopCloser(bytes.NewReader(replacement))
		req.ContentLength = int64(len(replacement))
		return req, nil
	})
	client, cleanup := proxyClient(t, proxy)
	defer cleanup()

	resp, err := client.Post(target.URL+"/upload", "application/octet-stream", bytes.NewReader(bytes.Repeat([]byte("x"), 512)))
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, replacement, <-received)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "spill file must be removed after Discard")
}

// TestRequestBodyPreviewCallerCancel verifies that when the caller cancels
// mid-upload after a spilling preview was replayed, the replay body cleans
// up the spill file.
func TestRequestBodyPreviewCallerCancel(t *testing.T) {
	dir := t.TempDir()

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	defer target.Close()

	proxy := goproxy.NewProxyHttpServer()
	proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		p := goproxy.NewRequestBodyPreview(req, goproxy.BodyPreviewOptions{
			MaxPreviewBytes: 64,
			MemoryBytes:     8,
			TempDir:         dir,
		})
		_, _ = p.Peek()
		p.Replay()
		return req, nil
	})
	client, cleanup := proxyClient(t, proxy)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL+"/upload", pr)
	require.NoError(t, err)

	go func() {
		_, _ = pw.Write(bytes.Repeat([]byte("y"), 256))
		cancel()
	}()
	resp, err := client.Do(req)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	_ = pw.Close()

	require.Eventually(t, func() bool {
		entries, rerr := os.ReadDir(dir)
		return rerr == nil && len(entries) == 0
	}, 5*time.Second, 10*time.Millisecond, "spill file must be removed after caller cancellation")
}

// TestRequestStreamingWithoutPreview verifies that filters not using the
// preview helper keep the original streaming behavior: the target starts
// receiving the body before the upload completes.
func TestRequestStreamingWithoutPreview(t *testing.T) {
	chunk1 := []byte("streaming-chunk-one---")
	chunk2 := []byte("streaming-chunk-two-and-rest")
	full := append(append([]byte{}, chunk1...), chunk2...)

	target, headSeen, received := echoUploadServer(t, len(chunk1))
	defer target.Close()

	proxy := goproxy.NewProxyHttpServer()
	proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		req.Header.Set("X-No-Preview", "1")
		return req, nil
	})
	client, cleanup := proxyClient(t, proxy)
	defer cleanup()

	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, target.URL+"/upload", pr)
	require.NoError(t, err)

	streamed := make(chan bool, 1)
	go func() {
		_, _ = pw.Write(chunk1)
		select {
		case <-headSeen:
			streamed <- true
		case <-time.After(10 * time.Second):
			streamed <- false
			_ = pw.CloseWithError(errors.New("target did not receive first chunk in time"))
			return
		}
		_, _ = pw.Write(chunk2)
		_ = pw.Close()
	}()

	resp, err := client.Do(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	require.True(t, <-streamed, "target must receive the first chunk before the upload completes")
	require.Equal(t, full, <-received)
}

// TestRequestBodyPreviewStageLimit verifies that the total staging limit
// truncates the preview window while Replay still forwards every byte.
func TestRequestBodyPreviewStageLimit(t *testing.T) {
	dir := t.TempDir()
	body := bytes.Repeat([]byte("0123456789"), 10)

	req := httptest.NewRequest(http.MethodPost, "http://example.invalid/upload", bytes.NewReader(body))
	p := goproxy.NewRequestBodyPreview(req, goproxy.BodyPreviewOptions{
		MaxPreviewBytes: 100,
		MaxStageBytes:   10,
		MemoryBytes:     4,
		TempDir:         dir,
	})
	head, err := p.Peek()
	require.NoError(t, err)
	require.Len(t, head, 10, "MaxStageBytes truncates the preview window")
	require.Equal(t, body[:10], head)
	assert.True(t, p.SpilledToDisk())

	p.Replay()
	got, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, body, got, "Replay must forward the exact original bytes")
	require.NoError(t, req.Body.Close())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "spill file must be removed once the replay body is consumed")
}

// TestRequestBodyPreviewPeekAfterRelease verifies the preview is inert
// after Replay or Discard.
func TestRequestBodyPreviewPeekAfterRelease(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://example.invalid/upload", bytes.NewReader([]byte("hello")))
	p := goproxy.NewRequestBodyPreview(req, goproxy.BodyPreviewOptions{})
	head, err := p.Peek()
	require.NoError(t, err)
	require.Equal(t, []byte("hello"), head)
	p.Replay()
	_, err = p.Peek()
	require.Error(t, err, "Peek after Replay must fail")
	require.NoError(t, p.Discard(), "Discard after Replay is a no-op")

	req2 := httptest.NewRequest(http.MethodPost, "http://example.invalid/upload", bytes.NewReader([]byte("world")))
	p2 := goproxy.NewRequestBodyPreview(req2, goproxy.BodyPreviewOptions{})
	require.NoError(t, p2.Discard())
	_, err = p2.Peek()
	require.Error(t, err, "Peek after Discard must fail")
}
