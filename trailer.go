package goproxy

import (
	"errors"
	"hash"
	"io"
	"net/http"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// CopyResult reports the outcome of copying a proxied response body to the
// client. It is passed to a ResponseTrailerFunc after the copy ends.
type CopyResult struct {
	// BytesWritten is the number of body bytes successfully written to the
	// client, and therefore the number of bytes covered by any digest the
	// trailer registration requested.
	BytesWritten int64
	// ReadErr is the non-EOF error returned while reading the (possibly
	// filter-replaced) response body. A clean end of stream returns nil; an
	// unexpected EOF is reported here as io.ErrUnexpectedEOF. nil does not by
	// itself prove completeness when WriteErr is set.
	ReadErr error
	// WriteErr is the error returned while writing to the client, including a
	// client cancellation. nil means every byte read from the body was
	// accepted downstream.
	WriteErr error
}

// Complete reports whether the entire response body was delivered to the
// client without error. A trailer that vouches for stream completeness must
// only be emitted when Complete is true.
func (r CopyResult) Complete() bool {
	return r.ReadErr == nil && r.WriteErr == nil
}

// ResponseTrailerFunc decides the value of a response trailer once the body
// copy has finished. It runs at most once per response, after the body (and
// any requested hash) has been finalized but before the trailer section is
// committed.
//
// Returning a non-empty value emits the trailer with that value. Returning
// "" suppresses the trailer even though it was pre-announced; this is the
// correct choice when result.Complete() is false, since an empty or partial
// digest must never be presented as proof of an intact body.
//
// The hash argument is the per-request instance supplied when the trailer was
// registered (nil for a plain registration), already updated with exactly the
// bytes written to the client.
type ResponseTrailerFunc func(result CopyResult, hash hash.Hash, ctx *ProxyCtx) string

// responseTrailer is a registered end-of-response trailer hook.
type responseTrailer struct {
	name string
	fn   ResponseTrailerFunc
	// newHash, when non-nil, is invoked once per response to obtain a fresh
	// hash that sees every byte of the final, post-filter body that the proxy
	// writes to the client.
	newHash func() hash.Hash
	hash    hash.Hash
	fired   bool
}

// AddResponseTrailer declares a response trailer and registers a hook that
// supplies its value after the response body has been copied to the client.
//
// name must be a valid trailer header name; it is canonicalized. The
// declaration MUST happen before the response headers are committed, i.e.
// from an OnResponse handler. Calling it later, with an invalid/forbidden
// trailer name (such as "Content-Length", "Trailer", "Transfer-Encoding"),
// or on a context without a response, returns an error and registers nothing.
//
// No body is hashed: use AddContentDigestTrailer for that. The hook runs at
// most once. On a HEAD request, a 1xx/204/304 response, or a WebSocket
// upgrade the hook does not run and no trailer is announced, since those
// exchanges carry no copyable response body.
func (ctx *ProxyCtx) AddResponseTrailer(name string, fn ResponseTrailerFunc) error {
	return ctx.registerResponseTrailer(name, fn, nil)
}

// AddContentDigestTrailer is like AddResponseTrailer, but newHash is called
// once per response and fed every body byte the proxy successfully writes to
// the client. The hash therefore covers exactly the final, post-filter bytes
// the client receives: if an OnResponse handler replaces resp.Body, the hash
// covers the replacement, not the original upstream payload. The per-response
// hash is passed to fn for encoding (e.g. hex).
// When the copy is incomplete (unexpected upstream EOF, downstream write
// failure, or client cancellation) the hash reflects only the bytes that
// made it downstream and result.Complete() is false, so fn can withhold the
// digest instead of certifying a truncated stream.
func (ctx *ProxyCtx) AddContentDigestTrailer(name string, newHash func() hash.Hash, fn ResponseTrailerFunc) error {
	if newHash == nil {
		return errors.New("goproxy: AddContentDigestTrailer requires a non-nil hash factory")
	}
	return ctx.registerResponseTrailer(name, fn, newHash)
}

func (ctx *ProxyCtx) registerResponseTrailer(name string, fn ResponseTrailerFunc, newHash func() hash.Hash) error {
	if fn == nil {
		return errors.New("goproxy: response trailer hook must not be nil")
	}
	if ctx.Resp == nil {
		return errors.New("goproxy: response trailer must be declared from an OnResponse handler")
	}
	if ctx.headersCommitted {
		return errors.New("goproxy: response trailer " + name + " must be declared before the response headers are written")
	}
	canonical := http.CanonicalHeaderKey(name)
	if canonical == "" || !httpguts.ValidTrailerHeader(canonical) {
		return errors.New("goproxy: " + name + " is not a legal trailer header name")
	}
	ctx.trailerHooks = append(ctx.trailerHooks, &responseTrailer{
		name:    canonical,
		fn:      fn,
		newHash: newHash,
	})
	return nil
}

// prepareTrailerHooks finalizes hooks for a response that is about to be
// committed. It returns nil (and changes nothing) when the response carries
// no copyable body, so HEAD, bodiless status codes, and WebSocket upgrades are
// never framed as trailers.
func (proxy *ProxyHttpServer) prepareTrailerHooks(ctx *ProxyCtx, resp *http.Response) []*responseTrailer {
	if len(ctx.trailerHooks) == 0 {
		return nil
	}
	if resp.Body == nil || !responseBodyAllowed(ctx.Req, resp) || isWebSocketHandshake(resp.Header) {
		return nil
	}
	hooks := ctx.trailerHooks
	for _, h := range hooks {
		if h.newHash != nil {
			h.hash = h.newHash()
		}
	}
	return hooks
}

// announceTrailerHooks merges hook trailer names into the response headers
// before WriteHeader. The declaration forces HTTP/1.1 chunked framing (and an
// HTTP/2 trailing HEADERS frame), which is incompatible with Content-Length,
// so Content-Length is removed when any hook trailer will be emitted.
func announceTrailerHooks(w http.ResponseWriter, hooks []*responseTrailer) {
	if len(hooks) == 0 {
		return
	}
	w.Header().Del("Content-Length")
	existing := make(map[string]bool)
	for _, v := range w.Header()["Trailer"] {
		for _, part := range strings.Split(v, ",") {
			if name := strings.TrimSpace(part); name != "" {
				existing[http.CanonicalHeaderKey(name)] = true
			}
		}
	}
	for _, h := range hooks {
		if !existing[h.name] {
			existing[h.name] = true
			w.Header().Add("Trailer", h.name)
		}
	}
}

// digestWriter feeds successfully-written body bytes to the registered hook
// hashes. It must wrap the client writer: only bytes accepted downstream are
// hashed, so a failed Write cannot contribute to a "verified" digest.
type digestWriter struct {
	w     io.Writer
	hooks []*responseTrailer
}

func (d *digestWriter) Write(p []byte) (int, error) {
	n, err := d.w.Write(p)
	if n > 0 {
		for _, h := range d.hooks {
			if h.hash != nil {
				_, _ = h.hash.Write(p[:n])
			}
		}
	}
	return n, err
}

// copyBody copies src to dst while feeding hook hashes, distinguishing read
// failures (e.g. unexpected upstream EOF) from write failures (e.g. client
// cancellation). When flushEveryWrite is non-nil it is invoked after every
// successful write, matching the streaming path. The returned CopyResult is
// suitable for hook consumption.
func copyBody(dst io.Writer, src io.Reader, hooks []*responseTrailer, flushEveryWrite func() error) (CopyResult, error) {
	var result CopyResult
	if len(hooks) > 0 {
		dst = &digestWriter{w: dst, hooks: hooks}
	}

	buf := make([]byte, 32*1024)
	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[0:nr])
			if nw < 0 || nw > nr {
				nw = 0
			}
			result.BytesWritten += int64(nw)
			if ew != nil {
				result.WriteErr = ew
				break
			}
			if nr != nw {
				result.WriteErr = io.ErrShortWrite
				break
			}
			if flushEveryWrite != nil {
				if ferr := flushEveryWrite(); ferr != nil && result.WriteErr == nil {
					result.WriteErr = ferr
					break
				}
			}
		}
		if er != nil {
			if er != io.EOF {
				result.ReadErr = er
			}
			break
		}
	}
	return result, firstCopyError(result)
}

func firstCopyError(result CopyResult) error {
	if result.WriteErr != nil {
		return result.WriteErr
	}
	return result.ReadErr
}

// fireTrailerHooks runs each hook at most once and writes any non-empty value
// under the pre-announced trailer name. A panicking hook is contained so one
// bad hook cannot corrupt the response for the others.
func (proxy *ProxyHttpServer) fireTrailerHooks(w http.ResponseWriter, ctx *ProxyCtx, hooks []*responseTrailer, result CopyResult) {
	for _, h := range hooks {
		if h.fired {
			continue
		}
		h.fired = true
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					ctx.Warnf("response trailer hook %q panicked: %v", h.name, rec)
				}
			}()
			value := h.fn(result, h.hash, ctx)
			if value != "" {
				w.Header().Set(h.name, value)
			}
		}()
	}
}
