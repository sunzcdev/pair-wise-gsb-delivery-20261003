package gziphandler // import "github.com/NYTimes/gziphandler"

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

const (
	vary            = "Vary"
	acceptEncoding  = "Accept-Encoding"
	contentEncoding = "Content-Encoding"
	contentType     = "Content-Type"
	contentLength   = "Content-Length"
)

type codings map[string]float64

const (
	// DefaultQValue is the default qvalue to assign to an encoding if no explicit qvalue is set.
	// This is actually kind of ambiguous in RFC 2616, so hopefully it's correct.
	// The examples seem to indicate that it is.
	DefaultQValue = 1.0

	// DefaultMinSize is the default minimum size until we enable gzip compression.
	// 1500 bytes is the MTU size for the internet since that is the largest size allowed at the network layer.
	// If you take a file that is 1300 bytes and compress it to 800 bytes, it’s still transmitted in that same 1500 byte packet regardless, so you’ve gained nothing.
	// That being the case, you should restrict the gzip compression to files with a size greater than a single packet, 1400 bytes (1.4KB) is a safe value.
	DefaultMinSize = 1400
)

// gzipWriterPools stores a sync.Pool for each compression level for reuse of
// gzip.Writers. Use poolIndex to covert a compression level to an index into
// gzipWriterPools.
var gzipWriterPools [gzip.BestCompression - gzip.BestSpeed + 2]*sync.Pool

func init() {
	for i := gzip.BestSpeed; i <= gzip.BestCompression; i++ {
		addLevelPool(i)
	}
	addLevelPool(gzip.DefaultCompression)
}

// poolIndex maps a compression level to its index into gzipWriterPools. It
// assumes that level is a valid gzip compression level.
func poolIndex(level int) int {
	// gzip.DefaultCompression == -1, so we need to treat it special.
	if level == gzip.DefaultCompression {
		return gzip.BestCompression - gzip.BestSpeed + 1
	}
	return level - gzip.BestSpeed
}

func addLevelPool(level int) {
	gzipWriterPools[poolIndex(level)] = &sync.Pool{
		New: func() interface{} {
			// NewWriterLevel only returns error on a bad level, we are guaranteeing
			// that this will be a valid level so it is okay to ignore the returned
			// error.
			w, _ := gzip.NewWriterLevel(nil, level)
			return w
		},
	}
}

// GzipResponseWriter provides an http.ResponseWriter interface, which gzips
// bytes before writing them to the underlying response. This doesn't close the
// writers, so don't forget to do that.
// It can be configured to skip response smaller than minSize.
type GzipResponseWriter struct {
	http.ResponseWriter
	index int // Index for gzipWriterPools.
	gw    *gzip.Writer

	code int // Saves the WriteHeader value.

	minSize int    // Specifies the minimum response size to gzip. If the response length is bigger than this value, it is compressed.
	buf     []byte // Holds the first part of the write before reaching the minSize or the end of the write.

	// encoding records the response encoding, once it has been decided.
	// It starts as encodingUndecided and can only transition to
	// encodingGzip or encodingPlain. The first Flush with a flushStrategy
	// may force the decision; after that it never changes.
	encoding encodingState

	contentTypes []parsedContentType // Only compress if the response is one of these content-types. All are accepted if empty.

	// flushStrategy, if set, is consulted on the first Flush while the
	// encoding is still undecided.
	flushStrategy FlushStrategy
}

// encodingState records whether the buffered response is going to be gzip
// compressed or passed through untouched.
type encodingState uint8

const (
	encodingUndecided encodingState = iota
	encodingGzip
	encodingPlain
)

type GzipResponseWriterWithCloseNotify struct {
	*GzipResponseWriter
}

func (w GzipResponseWriterWithCloseNotify) CloseNotify() <-chan bool {
	return w.ResponseWriter.(http.CloseNotifier).CloseNotify()
}

// Write appends data to the gzip writer.
func (w *GzipResponseWriter) Write(b []byte) (int, error) {
	// GZIP responseWriter is initialized. Use the GZIP responseWriter.
	if w.gw != nil {
		return w.gw.Write(b)
	}

	// If the encoding has already been decided, route the write accordingly.
	// It can never change after the first decision.
	switch w.encoding {
	case encodingPlain:
		// Immediately passthru writes to the underlying ResponseWriter.
		return w.ResponseWriter.Write(b)
	case encodingGzip:
		// Gzip was selected before any byte was buffered (e.g. by an empty
		// write followed by a real one), so initialize the writer now.
		w.init()
		return w.gw.Write(b)
	}

	// Save the write into a buffer for later use in GZIP responseWriter (if content is long enough) or at close with regular responseWriter.
	// On the first write, w.buf changes from nil to a valid slice
	w.buf = append(w.buf, b...)

	var (
		cl, _ = strconv.Atoi(w.Header().Get(contentLength))
		ct    = w.Header().Get(contentType)
		ce    = w.Header().Get(contentEncoding)
	)
	// Only continue if they didn't already choose an encoding or a known unhandled content length or type.
	if ce == "" && (cl == 0 || cl >= w.minSize) && (ct == "" || handleContentType(w.contentTypes, ct)) {
		// If the current buffer is less than minSize and a Content-Length isn't set, then wait until we have more data.
		if len(w.buf) < w.minSize && cl == 0 {
			return len(b), nil
		}
		// If the Content-Length is larger than minSize or the current buffer is larger than minSize, then continue.
		if cl >= w.minSize || len(w.buf) >= w.minSize {
			// If a Content-Type wasn't specified, infer it from the current buffer.
			if ct == "" {
				ct = http.DetectContentType(w.buf)
				w.Header().Set(contentType, ct)
			}
			// If the Content-Type is acceptable to GZIP, initialize the GZIP writer.
			if handleContentType(w.contentTypes, ct) {
				if err := w.startGzip(); err != nil {
					return 0, err
				}
				return len(b), nil
			}
		}
	}
	// If we got here, we should not GZIP this response.
	if err := w.startPlain(); err != nil {
		return 0, err
	}
	return len(b), nil
}

// startGzip initializes a GZIP writer and writes the buffer.
func (w *GzipResponseWriter) startGzip() error {
	w.encoding = encodingGzip

	// Set the GZIP header.
	w.Header().Set(contentEncoding, "gzip")

	// if the Content-Length is already set, then calls to Write on gzip
	// will fail to set the Content-Length header since its already set
	// See: https://github.com/golang/go/issues/14975.
	w.Header().Del(contentLength)

	// Write the header to gzip response.
	if w.code != 0 {
		w.ResponseWriter.WriteHeader(w.code)
		// Ensure that no other WriteHeader's happen
		w.code = 0
	}

	// Initialize and flush the buffer into the gzip response if there are any bytes.
	// If there aren't any, we shouldn't initialize it yet because on Close it will
	// write the gzip header even if nothing was ever written.
	if len(w.buf) > 0 {
		// Initialize the GZIP response.
		w.init()
		buf := w.buf
		w.buf = nil
		n, err := w.gw.Write(buf)

		// This should never happen (per io.Writer docs), but if the write didn't
		// accept the entire buffer but returned no specific error, we have no clue
		// what's going on, so abort just to be safe.
		if err == nil && n < len(buf) {
			err = io.ErrShortWrite
		}
		return err
	}
	return nil
}

// startPlain writes to sent bytes and buffer the underlying ResponseWriter without gzip.
func (w *GzipResponseWriter) startPlain() error {
	w.encoding = encodingPlain

	if w.code != 0 {
		w.ResponseWriter.WriteHeader(w.code)
		// Ensure that no other WriteHeader's happen
		w.code = 0
	}
	// If Write was never called then don't call Write on the underlying ResponseWriter.
	if w.buf == nil {
		return nil
	}
	buf := w.buf
	w.buf = nil
	n, err := w.ResponseWriter.Write(buf)
	// This should never happen (per io.Writer docs), but if the write didn't
	// accept the entire buffer but returned no specific error, we have no clue
	// what's going on, so abort just to be safe.
	if err == nil && n < len(buf) {
		err = io.ErrShortWrite
	}
	return err
}

// WriteHeader just saves the response code until close or GZIP effective writes.
func (w *GzipResponseWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}

// init graps a new gzip writer from the gzipWriterPool and writes the correct
// content encoding header.
func (w *GzipResponseWriter) init() {
	// Bytes written during ServeHTTP are redirected to this gzip writer
	// before being written to the underlying response.
	gzw := gzipWriterPools[w.index].Get().(*gzip.Writer)
	gzw.Reset(w.ResponseWriter)
	w.gw = gzw
}

// Close will close the gzip.Writer and will put it back in the gzipWriterPool.
func (w *GzipResponseWriter) Close() error {
	switch w.encoding {
	case encodingPlain:
		return nil
	case encodingGzip:
		// gw may be nil if gzip was selected through the write path with no
		// buffered bytes; no gzip header was written, so there is no trailer
		// to write and nothing to return to the pool.
		if w.gw == nil {
			return nil
		}
		err := w.gw.Close()
		gzipWriterPools[w.index].Put(w.gw)
		w.gw = nil
		return err
	}

	// GZIP not triggered yet, write out regular response.
	err := w.startPlain()
	// Returns the error if any at write.
	if err != nil {
		err = fmt.Errorf("gziphandler: write to regular responseWriter at close gets error: %q", err.Error())
	}
	return err
}

// Flush flushes buffered data to the client. If a FlushStrategy is
// configured and the encoding is still undecided, the first Flush gets to
// finalize the encoding choice (see WithFlushStrategy). Afterwards it
// flushes the underlying *gzip.Writer (when compressing), and then the
// underlying http.ResponseWriter. This makes GzipResponseWriter an
// http.Flusher.
//
// Use FlushError to observe failures while flushing.
func (w *GzipResponseWriter) Flush() {
	_ = w.FlushError()
}

// FlushError behaves like Flush but returns an error, making the
// GzipResponseWriter compatible with http.ResponseController.Flush on
// Go 1.20 and newer. Errors from flushing the gzip writer or the underlying
// ResponseWriter are returned to the caller.
func (w *GzipResponseWriter) FlushError() error {
	if w.encoding == encodingUndecided {
		// Without a strategy, Flush remains a no-op until the write path or
		// Close decides whether a plain or gzipped response is served.
		if w.flushStrategy == nil {
			return nil
		}

		switch w.flushStrategy(len(w.buf)) {
		case FlushGzip:
			// Gzip can only be selected while the response is still eligible
			// for it; a preset Content-Encoding, a Content-Length below
			// minSize or an unhandled Content-Type forces passthrough.
			if w.gzipEligible() {
				if err := w.commitGzip(); err != nil {
					return err
				}
				break
			}
			fallthrough
		case FlushIdentity:
			if err := w.startPlain(); err != nil {
				return err
			}
		default:
			// FlushDefer: keep buffering and don't touch the underlying
			// ResponseWriter yet.
			return nil
		}
	}

	if w.gw != nil {
		if err := w.gw.Flush(); err != nil {
			return err
		}
	}

	return w.flushUnderlying()
}

// gzipEligible reports whether the current response may still be gzip
// compressed, mirroring the gating logic used by Write.
func (w *GzipResponseWriter) gzipEligible() bool {
	var (
		cl, _ = strconv.Atoi(w.Header().Get(contentLength))
		ct    = w.Header().Get(contentType)
		ce    = w.Header().Get(contentEncoding)
	)
	if ce != "" {
		return false
	}
	if cl != 0 && cl < w.minSize {
		return false
	}
	if ct != "" && !handleContentType(w.contentTypes, ct) {
		return false
	}
	return true
}

// commitGzip starts gzip compression unconditionally. Unlike startGzip
// (used by the write path) it initializes the gzip writer even when nothing
// has been buffered yet, so a zero-body first Flush still emits the gzip
// header.
func (w *GzipResponseWriter) commitGzip() error {
	w.encoding = encodingGzip

	w.Header().Set(contentEncoding, "gzip")

	// if the Content-Length is already set, then calls to Write on gzip
	// will fail to set the Content-Length header since its already set
	// See: https://github.com/golang/go/issues/14975.
	w.Header().Del(contentLength)

	// Infer the Content-Type from the current buffer, just like Write does.
	if w.Header().Get(contentType) == "" && len(w.buf) > 0 {
		w.Header().Set(contentType, http.DetectContentType(w.buf))
	}

	// Write the saved status code before any body byte goes out.
	if w.code != 0 {
		w.ResponseWriter.WriteHeader(w.code)
		w.code = 0
	}

	buf := w.buf
	w.buf = nil
	w.init()

	if len(buf) == 0 {
		return nil
	}
	n, err := w.gw.Write(buf)
	if err == nil && n < len(buf) {
		err = io.ErrShortWrite
	}
	return err
}

// Hijack implements http.Hijacker. If the underlying ResponseWriter is a
// Hijacker, its Hijack method is returned. Otherwise an error is returned.
func (w *GzipResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("http.Hijacker interface is not supported")
}

// verify Hijacker interface implementation
var _ http.Hijacker = &GzipResponseWriter{}

// MustNewGzipLevelHandler behaves just like NewGzipLevelHandler except that in
// an error case it panics rather than returning an error.
func MustNewGzipLevelHandler(level int) func(http.Handler) http.Handler {
	wrap, err := NewGzipLevelHandler(level)
	if err != nil {
		panic(err)
	}
	return wrap
}

// NewGzipLevelHandler returns a wrapper function (often known as middleware)
// which can be used to wrap an HTTP handler to transparently gzip the response
// body if the client supports it (via the Accept-Encoding header). Responses will
// be encoded at the given gzip compression level. An error will be returned only
// if an invalid gzip compression level is given, so if one can ensure the level
// is valid, the returned error can be safely ignored.
func NewGzipLevelHandler(level int) (func(http.Handler) http.Handler, error) {
	return NewGzipLevelAndMinSize(level, DefaultMinSize)
}

// NewGzipLevelAndMinSize behave as NewGzipLevelHandler except it let the caller
// specify the minimum size before compression.
func NewGzipLevelAndMinSize(level, minSize int) (func(http.Handler) http.Handler, error) {
	return GzipHandlerWithOpts(CompressionLevel(level), MinSize(minSize))
}

func GzipHandlerWithOpts(opts ...option) (func(http.Handler) http.Handler, error) {
	c := &config{
		level:   gzip.DefaultCompression,
		minSize: DefaultMinSize,
	}

	for _, o := range opts {
		o(c)
	}

	if err := c.validate(); err != nil {
		return nil, err
	}

	return func(h http.Handler) http.Handler {
		index := poolIndex(c.level)

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add(vary, acceptEncoding)
			if acceptsGzip(r) {
				gw := &GzipResponseWriter{
					ResponseWriter: w,
					index:          index,
					minSize:        c.minSize,
					contentTypes:   c.contentTypes,
					flushStrategy:  c.flushStrategy,
				}
				defer gw.Close()

				if _, ok := w.(http.CloseNotifier); ok {
					gwcn := GzipResponseWriterWithCloseNotify{gw}
					h.ServeHTTP(gwcn, r)
				} else {
					h.ServeHTTP(gw, r)
				}

			} else {
				h.ServeHTTP(w, r)
			}
		})
	}, nil
}

// Parsed representation of one of the inputs to ContentTypes.
// See https://golang.org/pkg/mime/#ParseMediaType
type parsedContentType struct {
	mediaType string
	params    map[string]string
}

// equals returns whether this content type matches another content type.
func (pct parsedContentType) equals(mediaType string, params map[string]string) bool {
	if pct.mediaType != mediaType {
		return false
	}
	// if pct has no params, don't care about other's params
	if len(pct.params) == 0 {
		return true
	}

	// if pct has any params, they must be identical to other's.
	if len(pct.params) != len(params) {
		return false
	}
	for k, v := range pct.params {
		if w, ok := params[k]; !ok || v != w {
			return false
		}
	}
	return true
}

// Used for functional configuration.
type config struct {
	minSize       int
	level         int
	contentTypes  []parsedContentType
	flushStrategy FlushStrategy
}

func (c *config) validate() error {
	if c.level != gzip.DefaultCompression && (c.level < gzip.BestSpeed || c.level > gzip.BestCompression) {
		return fmt.Errorf("invalid compression level requested: %d", c.level)
	}

	if c.minSize < 0 {
		return fmt.Errorf("minimum size must be more than zero")
	}

	return nil
}

type option func(c *config)

// FlushDecision is returned by a FlushStrategy to decide what happens on
// the first Flush while the response encoding is still undecided.
type FlushDecision uint8

const (
	// FlushDefer keeps buffering: the first Flush is a no-op and the
	// encoding is decided later, exactly like the default behavior.
	FlushDefer FlushDecision = iota

	// FlushGzip commits to gzip on the first Flush. The currently buffered
	// bytes are immediately emitted as gzip, even if the response is still
	// smaller than MinSize. If the response is no longer eligible for gzip
	// (a Content-Encoding is already set, the preset Content-Length is
	// smaller than MinSize, or the Content-Type is not handled), the
	// response is sent as plain instead.
	FlushGzip

	// FlushIdentity commits to passthrough (identity) on the first Flush.
	// The currently buffered bytes are written to the underlying
	// ResponseWriter without compression.
	FlushIdentity
)

// FlushStrategy is consulted once, on the first Flush, while the response
// encoding is still undecided. buffered is the number of bytes written so
// far that are still held in the MinSize buffer (zero for a Flush that
// happens before any Write). The returned decision commits the response to
// gzip or identity encoding for its remainder; the encoding can never
// change afterwards.
type FlushStrategy func(buffered int) FlushDecision

// FlushGzipImmediately is a FlushStrategy which commits to gzip on the
// first Flush, no matter how little has been buffered.
var FlushGzipImmediately FlushStrategy = func(int) FlushDecision { return FlushGzip }

// FlushAsIdentity is a FlushStrategy which commits to passthrough on the
// first Flush.
var FlushAsIdentity FlushStrategy = func(int) FlushDecision { return FlushIdentity }

// WithFlushStrategy configures the FlushStrategy used on the first Flush.
// Without it, Flush stays a no-op until enough data has been written to
// reach MinSize, preserving the default buffering behavior.
func WithFlushStrategy(strategy FlushStrategy) option {
	return func(c *config) {
		c.flushStrategy = strategy
	}
}

func MinSize(size int) option {
	return func(c *config) {
		c.minSize = size
	}
}

func CompressionLevel(level int) option {
	return func(c *config) {
		c.level = level
	}
}

// ContentTypes specifies a list of content types to compare
// the Content-Type header to before compressing. If none
// match, the response will be returned as-is.
//
// Content types are compared in a case-insensitive, whitespace-ignored
// manner.
//
// A MIME type without any other directive will match a content type
// that has the same MIME type, regardless of that content type's other
// directives. I.e., "text/html" will match both "text/html" and
// "text/html; charset=utf-8".
//
// A MIME type with any other directive will only match a content type
// that has the same MIME type and other directives. I.e.,
// "text/html; charset=utf-8" will only match "text/html; charset=utf-8".
//
// By default, responses are gzipped regardless of
// Content-Type.
func ContentTypes(types []string) option {
	return func(c *config) {
		c.contentTypes = []parsedContentType{}
		for _, v := range types {
			mediaType, params, err := mime.ParseMediaType(v)
			if err == nil {
				c.contentTypes = append(c.contentTypes, parsedContentType{mediaType, params})
			}
		}
	}
}

// GzipHandler wraps an HTTP handler, to transparently gzip the response body if
// the client supports it (via the Accept-Encoding header). This will compress at
// the default compression level.
func GzipHandler(h http.Handler) http.Handler {
	wrapper, _ := NewGzipLevelHandler(gzip.DefaultCompression)
	return wrapper(h)
}

// acceptsGzip returns true if the given HTTP request indicates that it will
// accept a gzipped response.
func acceptsGzip(r *http.Request) bool {
	acceptedEncodings, _ := parseEncodings(r.Header.Get(acceptEncoding))
	return acceptedEncodings["gzip"] > 0.0
}

// returns true if we've been configured to compress the specific content type.
func handleContentType(contentTypes []parsedContentType, ct string) bool {
	// If contentTypes is empty we handle all content types.
	if len(contentTypes) == 0 {
		return true
	}

	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}

	for _, c := range contentTypes {
		if c.equals(mediaType, params) {
			return true
		}
	}

	return false
}

// parseEncodings attempts to parse a list of codings, per RFC 2616, as might
// appear in an Accept-Encoding header. It returns a map of content-codings to
// quality values, and an error containing the errors encountered. It's probably
// safe to ignore those, because silently ignoring errors is how the internet
// works.
//
// See: http://tools.ietf.org/html/rfc2616#section-14.3.
func parseEncodings(s string) (codings, error) {
	c := make(codings)
	var e []string

	for _, ss := range strings.Split(s, ",") {
		coding, qvalue, err := parseCoding(ss)

		if err != nil {
			e = append(e, err.Error())
		} else {
			c[coding] = qvalue
		}
	}

	// TODO (adammck): Use a proper multi-error struct, so the individual errors
	//                 can be extracted if anyone cares.
	if len(e) > 0 {
		return c, fmt.Errorf("errors while parsing encodings: %s", strings.Join(e, ", "))
	}

	return c, nil
}

// parseCoding parses a single conding (content-coding with an optional qvalue),
// as might appear in an Accept-Encoding header. It attempts to forgive minor
// formatting errors.
func parseCoding(s string) (coding string, qvalue float64, err error) {
	for n, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		qvalue = DefaultQValue

		if n == 0 {
			coding = strings.ToLower(part)
		} else if strings.HasPrefix(part, "q=") {
			qvalue, err = strconv.ParseFloat(strings.TrimPrefix(part, "q="), 64)

			if qvalue < 0.0 {
				qvalue = 0.0
			} else if qvalue > 1.0 {
				qvalue = 1.0
			}
		}
	}

	if coding == "" {
		err = fmt.Errorf("empty content-coding")
	}

	return
}
