package goproxy_test

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elazarl/goproxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func proxyClient(t *testing.T, proxyURL string) *http.Client {
	t.Helper()
	parsed, err := url.Parse(proxyURL)
	require.NoError(t, err)
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(parsed)}}
}

func waitForEmptyDir(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(dir)
		if err == nil && len(entries) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	entries, _ := os.ReadDir(dir)
	t.Fatalf("temporary directory %s was not cleaned up, %d entries left", dir, len(entries))
}

// slowChunkedPOST sends payload through client in 1KiB chunks with delays,
// using chunked transfer encoding. After the first chunk is sent it waits
// for the gate channel to be closed (or 5s), proving the other side started
// receiving before the upload finished. It returns the response.
func slowChunkedPOST(t *testing.T, client *http.Client, rawURL string, payload []byte, gate <-chan struct{}) *http.Response {
	t.Helper()
	pipeReader, pipeWriter := io.Pipe()
	req, err := http.NewRequest(http.MethodPost, rawURL, pipeReader)
	require.NoError(t, err)

	type result struct {
		resp *http.Response
		err  error
	}
	respCh := make(chan result, 1)
	go func() {
		resp, err := client.Do(req)
		respCh <- result{resp, err}
	}()

	const chunk = 1024
	first := min(chunk, len(payload))
	_, err = pipeWriter.Write(payload[:first])
	require.NoError(t, err)

	select {
	case <-gate:
	case <-time.After(5 * time.Second):
		t.Fatal("target did not receive any body bytes while the upload was still in progress")
	}

	for off := first; off < len(payload); off += chunk {
		end := min(off+chunk, len(payload))
		if _, err := pipeWriter.Write(payload[off:end]); err != nil {
			t.Fatalf("writing chunk at offset %d: %v", off, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.NoError(t, pipeWriter.Close())

	select {
	case res := <-respCh:
		require.NoError(t, res.err)
		return res.resp
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for response")
		return nil
	}
}

// TestBodyPreviewChainedFiltersSlowChunkedUpload chains two request filters
// that both peek at the beginning of the body (one identifies the content,
// the other records a redacted summary) and verifies that the target server
// receives the exact original bytes, streamed before the upload completes.
func TestBodyPreviewChainedFiltersSlowChunkedUpload(t *testing.T) {
	payload := []byte(bytes.Repeat([]byte("The quick brown fox 0123456789;"), 400)) // 12400 bytes

	firstByteCh := make(chan struct{})
	gotBodyCh := make(chan []byte, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first := make([]byte, 1)
		if _, err := io.ReadFull(r.Body, first); err != nil {
			gotBodyCh <- nil
			return
		}
		close(firstByteCh)
		rest, _ := io.ReadAll(r.Body)
		gotBodyCh <- append(first, rest...)
	}))
	defer target.Close()

	tempDir := t.TempDir()

	var detectedType atomic.Value
	var summaryHash atomic.Value
	var summaryLen atomic.Int64
	var firstPreview, secondPreview atomic.Value

	proxy := goproxy.NewProxyHttpServer()
	// First filter: identify the content from the body prefix.
	proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		preview, err := ctx.PreviewRequestBody(req, &goproxy.BodyPreviewOptions{
			PreviewBytes:   128,
			MemoryBudget:   32,
			MaxStagedBytes: 4096,
			TempDir:        tempDir,
		})
		if err != nil {
			return req, goproxy.NewResponse(req, "text/plain", http.StatusInternalServerError, err.Error())
		}
		head, err := preview.Peek(64)
		if err != nil {
			return req, goproxy.NewResponse(req, "text/plain", http.StatusInternalServerError, err.Error())
		}
		detectedType.Store(http.DetectContentType(head))
		firstPreview.Store(preview)
		return req, nil
	})
	// Second filter: record a redacted summary (hash + length) of the prefix.
	proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		preview, err := ctx.PreviewRequestBody(req, nil)
		if err != nil {
			return req, goproxy.NewResponse(req, "text/plain", http.StatusInternalServerError, err.Error())
		}
		head, err := preview.Peek(128)
		if err != nil {
			return req, goproxy.NewResponse(req, "text/plain", http.StatusInternalServerError, err.Error())
		}
		sum := sha1.Sum(head)
		summaryHash.Store(hex.EncodeToString(sum[:]))
		summaryLen.Store(int64(len(head)))
		secondPreview.Store(preview)
		return req, nil
	})
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	resp := slowChunkedPOST(t, proxyClient(t, proxySrv.URL), target.URL+"/upload", payload, firstByteCh)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	// The target received exactly the original bytes.
	require.Equal(t, payload, <-gotBodyCh)

	// Both filters saw the same prefix of the same shared staging buffer.
	assert.Equal(t, "text/plain; charset=utf-8", detectedType.Load().(string))
	assert.Equal(t, int64(128), summaryLen.Load())
	expectedSum := sha1.Sum(payload[:128])
	assert.Equal(t, hex.EncodeToString(expectedSum[:]), summaryHash.Load().(string))
	assert.Same(t, firstPreview.Load(), secondPreview.Load())

	// The 128 staged bytes exceeded the 32 byte memory budget and spilled.
	preview := firstPreview.Load().(*goproxy.RequestBodyPreview)
	assert.True(t, preview.Spilled())
	assert.Equal(t, int64(128), preview.StagedBytes())

	// Forwarding is done: the spill file is gone.
	waitForEmptyDir(t, tempDir)
}

// TestBodyPreviewSpillThenRejectCleansUp rejects the request in a filter
// after the preview spilled to a temporary file, and verifies the file is
// removed and the target is never hit.
func TestBodyPreviewSpillThenRejectCleansUp(t *testing.T) {
	var targetHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
	}))
	defer target.Close()

	tempDir := t.TempDir()
	var spilled atomic.Bool

	proxy := goproxy.NewProxyHttpServer()
	proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		preview, err := ctx.PreviewRequestBody(req, &goproxy.BodyPreviewOptions{
			PreviewBytes:   512,
			MemoryBudget:   16,
			MaxStagedBytes: 1024,
			TempDir:        tempDir,
		})
		if err != nil {
			return req, goproxy.NewResponse(req, "text/plain", http.StatusInternalServerError, err.Error())
		}
		if _, err := preview.Peek(512); err != nil {
			return req, goproxy.NewResponse(req, "text/plain", http.StatusInternalServerError, err.Error())
		}
		spilled.Store(preview.Spilled())
		return req, goproxy.NewResponse(req, "text/plain", http.StatusForbidden, "denied")
	})
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	payload := bytes.Repeat([]byte("x"), 4096)
	req, err := http.NewRequest(http.MethodPost, target.URL+"/upload", bytes.NewReader(payload))
	require.NoError(t, err)
	resp, err := proxyClient(t, proxySrv.URL).Do(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.True(t, spilled.Load(), "preview should have spilled to a temp file")
	assert.Equal(t, int64(0), targetHits.Load(), "target must not be hit on local response")
	waitForEmptyDir(t, tempDir)
}

// TestBodyPreviewLimits verifies the separate preview-length and
// total-staging caps, the memory-to-file spill, and that the replaying body
// reproduces the full original stream.
func TestBodyPreviewLimits(t *testing.T) {
	tempDir := t.TempDir()

	data := make([]byte, 300000)
	for i := range data {
		data[i] = byte(i % 251)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://example.com/upload",
		io.NopCloser(bytes.NewReader(data)))
	require.NoError(t, err)

	proxyCtx := &goproxy.ProxyCtx{}
	preview, err := proxyCtx.PreviewRequestBody(req, &goproxy.BodyPreviewOptions{
		MemoryBudget:   64,
		MaxStagedBytes: 250,
		TempDir:        tempDir,
	})
	require.NoError(t, err)

	// Total staging cap: only 250 bytes are staged even though more was asked.
	head, err := preview.Peek(1000)
	require.NoError(t, err)
	require.Len(t, head, 250)
	assert.Equal(t, data[:250], head)
	assert.Equal(t, int64(250), preview.StagedBytes())
	assert.True(t, preview.Spilled())

	// The replaying body yields the staged prefix followed by the live
	// stream, reproducing the original body exactly.
	replayed, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, data, replayed)

	// Canceling the caller releases the staging resources.
	spillPath := preview.TempFilePath()
	require.NotEmpty(t, spillPath)
	cancel()
	waitForEmptyDir(t, tempDir)

	_, err = preview.Peek(1)
	assert.ErrorIs(t, err, goproxy.ErrBodyPreviewClosed)
}

// TestBodyPreviewPerPeekCap verifies that PreviewBytes caps each Peek
// independently of the total staging cap.
func TestBodyPreviewPerPeekCap(t *testing.T) {
	data := bytes.Repeat([]byte("abcd"), 1000)
	req, err := http.NewRequest(http.MethodPost, "http://example.com/upload",
		io.NopCloser(bytes.NewReader(data)))
	require.NoError(t, err)

	proxyCtx := &goproxy.ProxyCtx{}
	preview, err := proxyCtx.PreviewRequestBody(req, &goproxy.BodyPreviewOptions{
		PreviewBytes:   100,
		MemoryBudget:   1 << 20,
		MaxStagedBytes: 1 << 20,
	})
	require.NoError(t, err)

	head, err := preview.Peek(1000)
	require.NoError(t, err)
	assert.Len(t, head, 100)
	assert.Equal(t, data[:100], head)
	assert.False(t, preview.Spilled())

	replayed, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, data, replayed)
}

// TestNoPreviewKeepsStreaming verifies that filters not using the preview
// API keep the original streaming behavior: the target starts receiving
// body bytes before the upload completes.
func TestNoPreviewKeepsStreaming(t *testing.T) {
	payload := bytes.Repeat([]byte("stream me gently;"), 500)

	firstByteCh := make(chan struct{})
	gotBodyCh := make(chan []byte, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first := make([]byte, 1)
		if _, err := io.ReadFull(r.Body, first); err != nil {
			gotBodyCh <- nil
			return
		}
		close(firstByteCh)
		rest, _ := io.ReadAll(r.Body)
		gotBodyCh <- append(first, rest...)
	}))
	defer target.Close()

	var filterCalls atomic.Int64
	proxy := goproxy.NewProxyHttpServer()
	proxy.OnRequest().DoFunc(func(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
		filterCalls.Add(1)
		return req, nil
	})
	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	resp := slowChunkedPOST(t, proxyClient(t, proxySrv.URL), target.URL+"/upload", payload, firstByteCh)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	require.Equal(t, payload, <-gotBodyCh)
	assert.Equal(t, int64(1), filterCalls.Load())
}
