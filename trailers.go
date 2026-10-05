package goproxy

import (
	"encoding/hex"
	"hash"
	"io"
	"net/http"
)

// ResponseEnd describes how the proxy finished copying a response body to
// the client. It is handed to ResponseTrailerHook.TrailerValue so the hook
// can decide which trailer value — if any — the final state justifies.
type ResponseEnd struct {
	// BytesCopied is the number of body bytes the proxy wrote toward the client.
	BytesCopied int64
	// Err is the terminal copy error, if any: a read failure from the
	// upstream body (e.g. io.ErrUnexpectedEOF), a write failure to the
	// downstream client, or request cancellation. Err is nil only when the
	// body reached the client in full.
	Err error
}

// Complete reports whether the entire response body reached the client.
func (e ResponseEnd) Complete() bool { return e.Err == nil }

// ResponseTrailerHook computes one response trailer field once the response
// body has been copied to the client. Register hooks from a response filter
// with ProxyCtx.DeclareResponseTrailer.
type ResponseTrailerHook interface {
	// TrailerName returns the trailer field name emitted to the client.
	// It must be a valid trailer field name (e.g. not Content-Length).
	TrailerName() string
	// TrailerValue is called at most once per response, after the body copy
	// to the client has ended. Returning ok == false suppresses the trailer.
	TrailerValue(end ResponseEnd) (value string, ok bool)
}

// BodyObserver is an optional interface a ResponseTrailerHook can implement
// to inspect the response body bytes as they flow to the client.
type BodyObserver interface {
	// WrapBody returns a body that observes the copied bytes. The proxy
	// calls it once on the final resp.Body — after all response filters
	// ran — unless BodyAttached reports the hook is already observing a
	// body. A filter that replaces resp.Body can call WrapBody itself on
	// the original body to make the hook observe the upstream bytes
	// instead of the bytes sent to the client.
	WrapBody(body io.ReadCloser) io.ReadCloser
	// BodyAttached reports whether the hook is already observing a body.
	BodyAttached() bool
}

// DigestTrailer is a ResponseTrailerHook that emits a hex-encoded digest of
// the observed body bytes. The trailer is emitted only when the body reached
// the client completely, so its presence vouches for the integrity of the
// whole stream: a truncated, failed or cancelled response carries no digest.
type DigestTrailer struct {
	// Name is the trailer field name, e.g. "X-Content-Sha256".
	Name string
	// Hash computes the digest over the observed bytes, e.g. sha256.New().
	Hash hash.Hash

	attached bool
}

// NewDigestTrailer returns a DigestTrailer emitting the hex digest of the
// observed body bytes under the given trailer name.
func NewDigestTrailer(name string, h hash.Hash) *DigestTrailer {
	return &DigestTrailer{Name: name, Hash: h}
}

// TrailerName implements ResponseTrailerHook.
func (d *DigestTrailer) TrailerName() string { return d.Name }

// WrapBody implements BodyObserver. The proxy calls it on the final
// resp.Body; a filter that replaces the body may call it earlier on the
// original body to digest the upstream bytes instead.
func (d *DigestTrailer) WrapBody(body io.ReadCloser) io.ReadCloser {
	d.attached = true
	return &digestBody{ReadCloser: body, hash: d.Hash}
}

// BodyAttached implements BodyObserver.
func (d *DigestTrailer) BodyAttached() bool { return d.attached }

// TrailerValue implements ResponseTrailerHook.
func (d *DigestTrailer) TrailerValue(end ResponseEnd) (string, bool) {
	if !end.Complete() {
		return "", false
	}
	return hex.EncodeToString(d.Hash.Sum(nil)), true
}

type digestBody struct {
	io.ReadCloser
	hash hash.Hash
}

func (b *digestBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		_, _ = b.hash.Write(p[:n])
	}
	return n, err
}

// DeclareResponseTrailer registers hook to emit a trailer field when the
// current response finishes streaming to the client.
//
// Declaration must happen before the proxy commits the response headers,
// i.e. from a response filter (OnResponse); hooks declared later are
// ignored. Responses that may not carry a body — HEAD responses, 1xx, 204
// and 304 — never emit declared trailers, and their hooks are dropped
// without being called. Each hook runs at most once per response.
func (ctx *ProxyCtx) DeclareResponseTrailer(hook ResponseTrailerHook) {
	ctx.respTrailerHooks = append(ctx.respTrailerHooks, hook)
}

// armResponseTrailers prepares the hooks declared on ctx for emission and
// returns them, or nil when the response must not carry trailers (no hooks
// declared, or a response without a message body). Arming drops the
// Content-Length header — a length-framed response makes net/http silently
// drop trailers — and lets body-observing hooks wrap the final resp.Body,
// unless a filter already attached them to an earlier layer.
func armResponseTrailers(ctx *ProxyCtx, req *http.Request, resp *http.Response) []ResponseTrailerHook {
	hooks := ctx.respTrailerHooks
	if len(hooks) == 0 {
		return nil
	}
	if !responseBodyAllowed(req, resp) {
		// HEAD / 1xx / 204 / 304 responses end at the header terminator;
		// there is no trailer section to carry the declared fields.
		return nil
	}
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	if resp.Body != nil {
		for _, hook := range hooks {
			if obs, ok := hook.(BodyObserver); ok && !obs.BodyAttached() {
				resp.Body = obs.WrapBody(resp.Body)
			}
		}
	}
	return hooks
}

// finishResponseTrailers runs every hook exactly once against the final copy
// state and returns the trailer values to emit, keyed by canonical field
// name. Hooks that suppressed their value contribute nothing.
func finishResponseTrailers(hooks []ResponseTrailerHook, end ResponseEnd) http.Header {
	var trailer http.Header
	for _, hook := range hooks {
		if value, ok := hook.TrailerValue(end); ok {
			if trailer == nil {
				trailer = make(http.Header)
			}
			trailer.Set(hook.TrailerName(), value)
		}
	}
	return trailer
}

// announcedTrailerKeys returns the field names to announce in the Trailer
// header: the upstream trailer keys plus the declared hook names, deduplicated
// so a hook name colliding with an upstream trailer is announced only once.
func announcedTrailerKeys(trailer http.Header, hooks []ResponseTrailerHook) []string {
	keys := make([]string, 0, len(trailer)+len(hooks))
	for k := range trailer {
		keys = append(keys, k)
	}
	for _, hook := range hooks {
		name := http.CanonicalHeaderKey(hook.TrailerName())
		if _, ok := trailer[name]; !ok {
			keys = append(keys, name)
		}
	}
	return keys
}

// trailerHookFiller runs the declared hooks when the body stream ends and
// stores the emitted values into out. It is used on the MITM HTTP/1 path,
// where resp.Write copies the body internally and writes out (resp.Trailer)
// as the response trailers once the copy completes.
type trailerHookFiller struct {
	io.ReadCloser
	hooks  []ResponseTrailerHook
	out    http.Header
	copied int64
	done   bool
}

func (b *trailerHookFiller) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.copied += int64(n)
	if err != nil && !b.done {
		b.done = true
		end := ResponseEnd{BytesCopied: b.copied, Err: err}
		if err == io.EOF {
			end.Err = nil
		}
		for k, vs := range finishResponseTrailers(b.hooks, end) {
			b.out[k] = vs
		}
	}
	return n, err
}
