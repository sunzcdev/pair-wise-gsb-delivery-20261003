package goproxy

import (
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
)

const (
	// DefaultBodyPreviewMemoryBudget is the default amount of staged request
	// body bytes kept in memory before spilling over to a temporary file.
	DefaultBodyPreviewMemoryBudget = 256 * 1024
	// DefaultBodyPreviewMaxStagedBytes is the default cap on the total amount
	// of request body bytes staged for previewing.
	DefaultBodyPreviewMaxStagedBytes = 4 * 1024 * 1024
)

// ErrBodyPreviewClosed is returned when a released RequestBodyPreview is used.
var ErrBodyPreviewClosed = errors.New("goproxy: request body preview already released")

// BodyPreviewOptions configures ProxyCtx.PreviewRequestBody.
type BodyPreviewOptions struct {
	// PreviewBytes caps how many bytes a single Peek call may return.
	// Zero means no per-Peek cap (MaxStagedBytes still applies).
	PreviewBytes int
	// MemoryBudget is how many staged bytes are kept in memory before
	// spilling over to a temporary file.
	// Defaults to DefaultBodyPreviewMemoryBudget.
	MemoryBudget int
	// MaxStagedBytes caps the total amount of staged bytes, shared by all
	// filters previewing the same request. Defaults to
	// DefaultBodyPreviewMaxStagedBytes.
	MaxStagedBytes int64
	// TempDir is the directory used for spill files. Empty means os.TempDir().
	TempDir string
}

// RequestBodyPreview lets request filters look at the beginning of the
// request body without consuming it, and makes sure the bytes that were
// read are replayed to the destination server when the request is
// forwarded.
//
// Only the previewed prefix is buffered; the remainder of the body keeps
// streaming from the client, so the proxy never has to read the whole
// upload before forwarding it. The staged prefix is held in memory up to
// the configured memory budget and spills over to a temporary file beyond
// it. The total staged amount is capped separately.
//
// Ownership: once PreviewRequestBody returns, the preview owns req.Body -
// filters must not read from or close it directly. If the request is
// forwarded, the http.Transport closes the replaying body, which releases
// the original body and any staging resources. If a filter answers locally,
// replaces the request, or the caller cancels, everything is released once
// the request context is done. Release is idempotent.
//
// Peek must only be called from request filters, before the request is
// forwarded; calling it while the body is being sent upstream may corrupt
// the stream.
type RequestBodyPreview struct {
	mu          sync.Mutex
	src         io.ReadCloser
	mem         []byte
	file        *os.File
	filePath    string
	tempDir     string
	staged      int64
	srcEOF      bool
	srcErr      error
	previewMax  int
	memBudget   int
	maxStaged   int64
	released    bool
	releaseErr  error
	releaseOnce sync.Once
	done        chan struct{}
}

// PreviewRequestBody returns the RequestBodyPreview installed on req,
// creating and installing one if no filter has previewed this request yet.
// All filters previewing the same request share a single staging buffer, so
// the body is read from the client only once; the options of the first call
// win. The replaying body is installed as req.Body, so downstream filters
// and the destination server see the exact original byte stream.
func (ctx *ProxyCtx) PreviewRequestBody(req *http.Request, opts *BodyPreviewOptions) (*RequestBodyPreview, error) {
	if req == nil || req.Body == nil {
		return nil, errors.New("goproxy: cannot preview a request without a body")
	}
	if ctx.bodyPreviews == nil {
		ctx.bodyPreviews = make(map[*http.Request]*RequestBodyPreview)
	}
	if preview, ok := ctx.bodyPreviews[req]; ok {
		return preview, nil
	}
	preview, err := newRequestBodyPreview(req, opts)
	if err != nil {
		return nil, err
	}
	ctx.bodyPreviews[req] = preview
	return preview, nil
}

func newRequestBodyPreview(req *http.Request, opts *BodyPreviewOptions) (*RequestBodyPreview, error) {
	resolved := BodyPreviewOptions{}
	if opts != nil {
		resolved = *opts
	}
	if resolved.MemoryBudget <= 0 {
		resolved.MemoryBudget = DefaultBodyPreviewMemoryBudget
	}
	if resolved.MaxStagedBytes <= 0 {
		resolved.MaxStagedBytes = DefaultBodyPreviewMaxStagedBytes
	}
	if resolved.PreviewBytes < 0 {
		resolved.PreviewBytes = 0
	}
	preview := &RequestBodyPreview{
		src:        req.Body,
		tempDir:    resolved.TempDir,
		previewMax: resolved.PreviewBytes,
		memBudget:  resolved.MemoryBudget,
		maxStaged:  resolved.MaxStagedBytes,
		done:       make(chan struct{}),
	}
	req.Body = &bodyPreviewReplay{preview: preview}
	// Release staging resources and the original body when the request is
	// done being handled. This covers filters answering locally, filters
	// replacing the request, and the caller canceling the request - in all
	// of these the replaying body may never be closed by a RoundTripper.
	go func() {
		select {
		case <-req.Context().Done():
			preview.Release()
		case <-preview.done:
		}
	}()
	return preview, nil
}

// Peek returns up to n bytes from the beginning of the request body,
// staging more from the client if needed. The returned slice may be shorter
// than n when the per-Peek preview cap or the total staging cap is hit, or
// when the body itself is shorter. Successive Peeks of the same or smaller
// length are served from the staging buffer without touching the client.
func (p *RequestBodyPreview) Peek(n int) ([]byte, error) {
	if n < 0 {
		n = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.released {
		return nil, ErrBodyPreviewClosed
	}
	if p.previewMax > 0 && n > p.previewMax {
		n = p.previewMax
	}
	if int64(n) > p.maxStaged {
		n = int(p.maxStaged)
	}
	if err := p.stageLocked(int64(n)); err != nil {
		return nil, err
	}
	return p.readStagedLocked(0, int64(n))
}

// StagedBytes reports how many body bytes are currently staged.
func (p *RequestBodyPreview) StagedBytes() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.staged
}

// Spilled reports whether staging spilled over from memory to a temporary
// file.
func (p *RequestBodyPreview) Spilled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.file != nil
}

// TempFilePath returns the path of the spill file, or "" if staging still
// fits in memory. The file is removed when the preview is released.
func (p *RequestBodyPreview) TempFilePath() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.filePath
}

// Release closes the original body and removes all staging resources,
// including the spill file. It is idempotent and safe to call concurrently;
// filters normally don't need to call it - see the type documentation for
// the ownership rules.
func (p *RequestBodyPreview) Release() error {
	p.releaseOnce.Do(func() {
		p.mu.Lock()
		p.released = true
		if p.file != nil {
			p.file.Close()
		}
		if p.filePath != "" {
			os.Remove(p.filePath)
		}
		src := p.src
		p.mu.Unlock()
		if src != nil {
			p.releaseErr = src.Close()
		}
		close(p.done)
	})
	return p.releaseErr
}

// stageLocked reads from the original body until at least target bytes are
// staged, the staging cap is hit, or the body is exhausted.
func (p *RequestBodyPreview) stageLocked(target int64) error {
	if target > p.maxStaged {
		target = p.maxStaged
	}
	buf := make([]byte, 32*1024)
	for p.staged < target && !p.srcEOF && p.srcErr == nil {
		remain := target - p.staged
		if remain > int64(len(buf)) {
			remain = int64(len(buf))
		}
		n, err := p.src.Read(buf[:remain])
		if n > 0 {
			if aerr := p.appendLocked(buf[:n]); aerr != nil {
				p.srcErr = aerr
				return aerr
			}
		}
		if err != nil {
			if err == io.EOF {
				p.srcEOF = true
			} else {
				p.srcErr = err
				return err
			}
		}
	}
	return nil
}

// appendLocked stages b, keeping up to the memory budget in memory and
// spilling the rest to a temporary file.
func (p *RequestBodyPreview) appendLocked(b []byte) error {
	total := int64(len(b))
	if room := p.memBudget - len(p.mem); room > 0 {
		n := min(room, len(b))
		p.mem = append(p.mem, b[:n]...)
		b = b[n:]
	}
	if len(b) > 0 {
		if p.file == nil {
			f, err := os.CreateTemp(p.tempDir, "goproxy-body-preview-*")
			if err != nil {
				return err
			}
			p.file = f
			p.filePath = f.Name()
		}
		if _, err := p.file.Write(b); err != nil {
			return err
		}
	}
	p.staged += total
	return nil
}

// readStagedLocked copies up to maxLen staged bytes starting at offset off.
func (p *RequestBodyPreview) readStagedLocked(off, maxLen int64) ([]byte, error) {
	if off >= p.staged || maxLen <= 0 {
		return nil, nil
	}
	if off+maxLen > p.staged {
		maxLen = p.staged - off
	}
	out := make([]byte, maxLen)
	memLen := int64(len(p.mem))
	written := int64(0)
	if off < memLen {
		n := min(memLen-off, maxLen)
		copy(out, p.mem[off:off+n])
		written += n
	}
	if written < maxLen {
		if p.file == nil {
			return nil, errors.New("goproxy: staged data missing spill file")
		}
		if _, err := p.file.ReadAt(out[written:], off+written-memLen); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// bodyPreviewReplay is installed as req.Body while a preview is active. It
// serves the staged prefix first and then streams the remainder from the
// original body, so the destination server receives the exact original
// bytes.
type bodyPreviewReplay struct {
	preview *RequestBodyPreview
	pos     int64
}

func (r *bodyPreviewReplay) Read(buf []byte) (int, error) {
	p := r.preview
	p.mu.Lock()
	if r.pos < p.staged {
		data, err := p.readStagedLocked(r.pos, int64(len(buf)))
		if err != nil {
			p.mu.Unlock()
			return 0, err
		}
		n := copy(buf, data)
		r.pos += int64(len(data))
		p.mu.Unlock()
		return n, nil
	}
	released := p.released
	src := p.src
	p.mu.Unlock()
	if released {
		return 0, ErrBodyPreviewClosed
	}
	return src.Read(buf)
}

func (r *bodyPreviewReplay) Close() error {
	return r.preview.Release()
}
