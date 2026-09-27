package goproxy

import (
	"errors"
	"io"
	"net/http"
	"os"
)

// Default staging budgets used by BodyPreviewOptions when a field is left
// at its zero value.
const (
	// DefaultMaxPreviewBytes caps how many bytes Peek stages and returns.
	DefaultMaxPreviewBytes = 32 << 10
	// DefaultMemoryBytes caps how many staged bytes are kept in RAM before
	// spilling to a temporary file.
	DefaultMemoryBytes = 256 << 10
)

// errPreviewReleased is returned by Peek after the preview has been released
// via Replay or Discard.
var errPreviewReleased = errors.New("goproxy: request body preview already released")

// BodyPreviewOptions configures a RequestBodyPreview.
type BodyPreviewOptions struct {
	// MaxPreviewBytes limits how many bytes from the start of the request
	// body Peek stages and returns. Values <= 0 select DefaultMaxPreviewBytes.
	MaxPreviewBytes int64
	// MemoryBytes is the in-memory staging budget. Staged bytes beyond it
	// spill to a temporary file. Values <= 0 select DefaultMemoryBytes.
	MemoryBytes int64
	// MaxStageBytes limits the total amount of body data staged across
	// memory and temp file. Values <= 0 leave staging bounded by
	// MaxPreviewBytes. When set below MaxPreviewBytes, Peek returns a
	// truncated prefix and the remainder streams through on Replay.
	MaxStageBytes int64
	// TempDir selects the directory for spill files. Empty uses os.TempDir.
	TempDir string
}

// RequestBodyPreview lets a request filter inspect the beginning of a
// request body without consuming it for later filters or the target server.
//
// Typical usage inside a ReqHandler:
//
//	p := goproxy.NewRequestBodyPreview(req, goproxy.BodyPreviewOptions{})
//	head, err := p.Peek()
//	if shouldReject(head) {
//		p.Discard() // closes the original body, removes any spill file
//		return req, goproxy.NewResponse(req, goproxy.ContentTypeText, http.StatusForbidden, "no")
//	}
//	p.Replay() // req.Body now replays the staged prefix, then streams the rest
//	return req, nil
//
// Ownership contract:
//   - Until Replay or Discard is called, the preview owns the staged
//     resources (memory buffer and optional temp file); the original body
//     remains owned by the request but must not be read directly.
//   - Replay installs a new req.Body that serves the staged bytes first and
//     then keeps streaming the remainder of the original body, so the proxy
//     never has to read the whole upload before forwarding. After Replay the
//     preview is inert: the new body owns the original body and the spill
//     file, and cleans both up on Close (including caller cancellation,
//     since http.Transport closes the request body when a request ends).
//   - Discard closes the original body and removes the spill file. Use it
//     when the filter answers locally or replaces req.Body / the request.
//
// Filters that never construct a RequestBodyPreview are unaffected and keep
// proxying bodies in the usual streaming fashion.
type RequestBodyPreview struct {
	req  *http.Request
	opts BodyPreviewOptions

	src     io.ReadCloser
	mem     []byte
	tmp     *os.File
	tmpName string
	eof     bool

	peeked  bool
	peekBuf []byte
	peekErr error

	released bool
}

// NewRequestBodyPreview wraps req so its body prefix can be inspected
// without disturbing downstream readers. The zero BodyPreviewOptions uses
// the documented defaults.
func NewRequestBodyPreview(req *http.Request, opts BodyPreviewOptions) *RequestBodyPreview {
	src := req.Body
	if src == nil {
		src = http.NoBody
	}
	return &RequestBodyPreview{req: req, opts: opts, src: src}
}

func (p *RequestBodyPreview) stageLimit() int64 {
	limit := p.opts.MaxPreviewBytes
	if limit <= 0 {
		limit = DefaultMaxPreviewBytes
	}
	if max := p.opts.MaxStageBytes; max > 0 && max < limit {
		limit = max
	}
	return limit
}

func (p *RequestBodyPreview) memBudget() int64 {
	if p.opts.MemoryBytes <= 0 {
		return DefaultMemoryBytes
	}
	return p.opts.MemoryBytes
}

// appendStaged stores b in the staging area, spilling to a temp file once
// the memory budget is exhausted. If the spill file cannot be created or
// written, it falls back to memory so that Replay stays byte-exact, and
// reports the error to the caller of Peek.
func (p *RequestBodyPreview) appendStaged(b []byte) error {
	if budget := p.memBudget() - int64(len(p.mem)); budget > 0 {
		n := min(int64(len(b)), budget)
		p.mem = append(p.mem, b[:n]...)
		b = b[n:]
	}
	if len(b) == 0 {
		return nil
	}
	if p.tmp == nil {
		f, err := os.CreateTemp(p.opts.TempDir, "goproxy-body-*")
		if err != nil {
			p.mem = append(p.mem, b...)
			return err
		}
		p.tmp = f
		p.tmpName = f.Name()
	}
	if _, err := p.tmp.Write(b); err != nil {
		p.mem = append(p.mem, b...)
		return err
	}
	return nil
}

func (p *RequestBodyPreview) staged() int64 {
	staged := int64(len(p.mem))
	if p.tmp != nil {
		if off, err := p.tmp.Seek(0, io.SeekCurrent); err == nil {
			staged += off
		}
	}
	return staged
}

// Peek stages up to the configured preview window from the start of the
// body and returns it. The returned slice is a copy owned by the caller.
//
// Peek may return fewer bytes than the body holds: when the body is shorter
// than the window, when MaxStageBytes truncates staging, or when a read
// error occurs. Such truncation is not an error; the unstaged remainder
// still streams through on Replay. If spilling to a temp file fails, Peek
// still returns the full window (staged in memory as a fallback) together
// with the spill error, and Replay remains byte-exact.
//
// Peek is idempotent: later calls return the same window without reading
// more from the body.
func (p *RequestBodyPreview) Peek() ([]byte, error) {
	if p.released {
		return nil, errPreviewReleased
	}
	if p.peeked {
		return p.peekBuf, p.peekErr
	}
	p.peeked = true

	limit := p.stageLimit()
	chunk := make([]byte, 0, min(limit, 32<<10))
	for p.staged() < limit && !p.eof {
		need := limit - p.staged()
		if int64(cap(chunk)) > need {
			chunk = chunk[:need]
		} else {
			chunk = chunk[:cap(chunk)]
		}
		n, err := p.src.Read(chunk)
		if n > 0 {
			if serr := p.appendStaged(chunk[:n]); serr != nil && p.peekErr == nil {
				p.peekErr = serr
			}
		}
		switch {
		case errors.Is(err, io.EOF):
			p.eof = true
		case err != nil:
			p.eof = true
			if p.peekErr == nil {
				p.peekErr = err
			}
		}
	}

	window := p.staged()
	if max := p.opts.MaxPreviewBytes; max > 0 && window > max {
		window = max
	}
	p.peekBuf = make([]byte, window)
	n := copy(p.peekBuf, p.mem)
	if rest := p.peekBuf[n:]; len(rest) > 0 && p.tmp != nil {
		if _, err := p.tmp.ReadAt(rest, 0); err != nil && p.peekErr == nil {
			p.peekErr = err
		}
	}
	return p.peekBuf, p.peekErr
}

// SpilledToDisk reports whether staging overflowed the memory budget into a
// temporary file.
func (p *RequestBodyPreview) SpilledToDisk() bool {
	return p.tmp != nil
}

// Replay replaces req.Body with a body that serves the staged prefix first
// and then continues streaming the remainder of the original body, and
// releases the preview. The byte stream seen by downstream filters and the
// target server is identical to the original one; ContentLength is left
// untouched. GetBody is cleared because a partially consumed stream cannot
// be reconstructed for redirects or retries.
//
// After Replay the preview no longer owns anything: closing the new body
// closes the original one and removes the spill file, if any.
func (p *RequestBodyPreview) Replay() {
	if p.released {
		return
	}
	p.released = true
	if p.staged() == 0 {
		// Nothing was consumed; keep the original body untouched.
		return
	}
	rb := &replayBody{mem: p.mem, src: p.src}
	if p.tmp != nil {
		if _, err := p.tmp.Seek(0, io.SeekStart); err == nil {
			rb.tmp = p.tmp
			rb.tmpName = p.tmpName
		} else {
			// Cannot rewind the spill file; fold its contents back into
			// memory so the replayed stream stays byte-exact.
			spilled, rerr := io.ReadAll(io.NewSectionReader(p.tmp, 0, p.staged()-int64(len(p.mem))))
			if rerr == nil {
				rb.mem = append(rb.mem, spilled...)
			}
			p.releaseTmp()
		}
	}
	p.req.Body = rb
	p.req.GetBody = nil
	p.mem, p.tmp, p.src = nil, nil, nil
}

// Discard closes the original body and removes the spill file, if any, and
// releases the preview. Call it when the filter answers with a local
// response or replaces req.Body (or the whole request): in both cases the
// original body would otherwise leak. Discard after Replay is a no-op.
func (p *RequestBodyPreview) Discard() error {
	if p.released {
		return nil
	}
	p.released = true
	p.releaseTmp()
	err := p.src.Close()
	p.src, p.mem = nil, nil
	return err
}

func (p *RequestBodyPreview) releaseTmp() {
	if p.tmp != nil {
		name := p.tmpName
		_ = p.tmp.Close()
		_ = os.Remove(name)
		p.tmp = nil
	}
}

// replayBody is the request body installed by Replay. It serves the staged
// prefix (memory, then spill file) and then keeps reading the remainder of
// the original body, so uploads are forwarded without being fully read
// first. It owns the original body and the spill file: Close releases both,
// which also covers caller cancellation, since http.Transport closes the
// request body when the request ends.
type replayBody struct {
	mem     []byte
	tmp     *os.File
	tmpName string
	src     io.ReadCloser
	closed  bool
}

func (b *replayBody) Read(p []byte) (int, error) {
	if b.closed {
		return 0, errors.New("goproxy: read on closed replay body")
	}
	for {
		if len(b.mem) > 0 {
			n := copy(p, b.mem)
			b.mem = b.mem[n:]
			return n, nil
		}
		if b.tmp != nil {
			n, err := b.tmp.Read(p)
			if n > 0 {
				// A non-EOF error, if any, resurfaces on the next Read.
				return n, nil
			}
			if errors.Is(err, io.EOF) {
				b.releaseTmp()
				continue
			}
			return n, err
		}
		return b.src.Read(p)
	}
}

func (b *replayBody) Close() error {
	if b.closed {
		return nil
	}
	b.closed = true
	b.releaseTmp()
	return b.src.Close()
}

func (b *replayBody) releaseTmp() {
	if b.tmp != nil {
		name := b.tmpName
		_ = b.tmp.Close()
		_ = os.Remove(name)
		b.tmp = nil
	}
}
