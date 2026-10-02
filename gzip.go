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

// FlushPolicy controls how the first Flush behaves while the response is
// still smaller than MinSize (and therefore has not yet committed to either
// gzip or plain encoding). Once the first Flush picks an encoding it cannot
// be changed for the rest of the response.
type FlushPolicy int

const (
	// FlushBuffered is the default. A Flush before the encoding is known is a
	// no-op: bytes keep buffering until MinSize is reached (or Close emits the
	// small response uncompressed).
	FlushBuffered FlushPolicy = iota

	// FlushAsGzip makes the first Flush commit to gzip immediately, handing the
	// currently buffered bytes (possibly none) to the gzip writer and flushing
	// them to the client. This enables streaming of responses that may stay
	// under MinSize.
	FlushAsGzip

	// FlushAsPlain makes the first Flush commit to passthrough immediately:
	// the buffered bytes are written uncompressed to the underlying
	// ResponseWriter and the response is never gzipped.
	FlushAsPlain
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
	ignore  bool   // If true, then we immediately passthru writes to the underlying ResponseWriter.
	closed  bool   // If true, Close already ran, so further Flush/Close calls must do nothing.

	// flushPolicy decides what happens when Flush is invoked before enough
	// bytes have been buffered to pick an encoding.
	flushPolicy FlushPolicy

	contentTypes []parsedContentType // Only compress if the response is one of these content-types. All are accepted if empty.
}

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

	// If we have already decided not to use GZIP, immediately passthrough.
	if w.ignore {
		return w.ResponseWriter.Write(b)
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
	// When no bytes are buffered yet, don't initialize the gzip writer: on
	// Close it would emit a gzip header (and trailer) even though the response
	// has no body. The gzip response headers are still committed, so a
	// HEAD-style response with a preset Content-Length advertises gzip.
	if len(w.buf) == 0 {
		w.commitGzipHeaders()
		return nil
	}
	return w.beginGzip()
}

// forceGzip commits the response to gzip encoding immediately, even when the
// buffer is empty (a zero-body flush) or still smaller than minSize. It is
// only used by the streaming first-flush policy. Once this returns the
// encoding can never switch back to plain, so the buffered bytes are handed
// over to the gzip writer exactly once.
func (w *GzipResponseWriter) forceGzip() error {
	return w.beginGzip()
}

// commitGzipHeaders advertises gzip encoding, drops a preset Content-Length
// (the compressed length differs) and flushes the saved status code.
func (w *GzipResponseWriter) commitGzipHeaders() {
	// Set the GZIP header.
	w.Header().Set(contentEncoding, "gzip")

	// if the Content-Length is already set, then calls to Write on gzip
	// will fail to set the Content-Length header since its already set
	// See: https://github.com/golang/go/issues/14975.
	w.Header().Del(contentLength)

	// Write the status code to the underlying response, if one was saved.
	if w.code != 0 {
		w.ResponseWriter.WriteHeader(w.code)
		// Ensure that no other WriteHeader's happen.
		w.code = 0
	}
}

// beginGzip sets the gzip headers, commits the pending status code, obtains a
// gzip writer and drains the buffer into it. The buffer is cleared only after
// it has been fully written, so a failed write never silently drops bytes and
// a retry never writes them twice.
func (w *GzipResponseWriter) beginGzip() error {
	w.commitGzipHeaders()

	// Initialize the GZIP response, creating it lazily when the buffer is
	// empty so a zero-body flush still emits a valid gzip stream header.
	if w.gw == nil {
		w.init()
	}

	if len(w.buf) == 0 {
		return nil
	}

	n, err := w.gw.Write(w.buf)
	// This should never happen (per io.Writer docs), but if the write didn't
	// accept the entire buffer but returned no specific error, we have no clue
	// what's going on, so abort just to be safe.
	if err == nil && n < len(w.buf) {
		return io.ErrShortWrite
	}
	if err != nil {
		return err
	}

	// Hand ownership of the bytes to the gzip writer; never write them again.
	w.buf = nil
	return nil
}

// startPlain writes to sent bytes and buffer the underlying ResponseWriter without gzip.
func (w *GzipResponseWriter) startPlain() error {
	if w.code != 0 {
		w.ResponseWriter.WriteHeader(w.code)
		// Ensure that no other WriteHeader's happen
		w.code = 0
	}
	w.ignore = true
	// If Write was never called then don't call Write on the underlying ResponseWriter.
	if w.buf == nil {
		return nil
	}
	n, err := w.ResponseWriter.Write(w.buf)
	w.buf = nil
	// This should never happen (per io.Writer docs), but if the write didn't
	// accept the entire buffer but returned no specific error, we have no clue
	// what's going on, so abort just to be safe.
	if err == nil && n < len(w.buf) {
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
	if w.closed {
		return nil
	}
	w.closed = true

	if w.ignore {
		return nil
	}

	if w.gw == nil {
		// GZIP not triggered yet, write out regular response.
		err := w.startPlain()
		// Returns the error if any at write.
		if err != nil {
			err = fmt.Errorf("gziphandler: write to regular responseWriter at close gets error: %q", err.Error())
		}
		return err
	}

	err := w.gw.Close()
	gzipWriterPools[w.index].Put(w.gw)
	w.gw = nil
	return err
}

// Flush flushes the underlying *gzip.Writer and then the underlying
// http.ResponseWriter. This makes GzipResponseWriter an http.Flusher.
//
// The first Flush, before enough bytes have been buffered to pick an
// encoding, is governed by the handler's FlushPolicy. After that choice the
// encoding is fixed for the lifetime of the response. Flush itself discards
// errors; use FlushError (or an http.ResponseController) to observe them.
func (w *GzipResponseWriter) Flush() {
	_ = w.FlushError()
}

// FlushError flushes the underlying *gzip.Writer and then the underlying
// http.ResponseWriter, returning any error. Implementing FlushError lets
// http.ResponseController.Flush surface failures of the underlying flush
// instead of silently dropping them.
func (w *GzipResponseWriter) FlushError() error {
	if w.closed {
		return nil
	}

	// The encoding has not been chosen yet; the first Flush commits it.
	if w.gw == nil && !w.ignore {
		// FlushBuffered (the default): a Flush before the encoding is known is
		// a no-op, including not flushing the underlying writer (which could
		// prematurely commit a status code). Bytes keep buffering until
		// MinSize is reached or Close runs.
		if w.flushPolicy == FlushBuffered {
			return nil
		}
		if err := w.commitFirstFlush(); err != nil {
			return err
		}
	}

	if w.gw != nil {
		if err := w.gw.Flush(); err != nil {
			return err
		}
	}

	// Prefer http.NewResponseController so an underlying Unwrap chain is
	// honored, falling back to the legacy http.Flusher interface.
	rc := http.NewResponseController(w.ResponseWriter)
	if err := rc.Flush(); err != nil {
		if err == http.ErrNotSupported {
			if fw, ok := w.ResponseWriter.(http.Flusher); ok {
				fw.Flush()
				return nil
			}
		}
		return err
	}
	return nil
}

// commitFirstFlush applies the configured FlushPolicy to the first Flush call,
// deciding once and for all whether the response is gzipped or passed through.
func (w *GzipResponseWriter) commitFirstFlush() error {
	switch w.flushPolicy {
	case FlushAsGzip:
		// Never double-encode a response that already chose an encoding.
		if ce := w.Header().Get(contentEncoding); ce != "" {
			return w.startPlain()
		}
		return w.forceGzip()

	case FlushAsPlain:
		return w.startPlain()
	}
	// FlushBuffered is handled by the caller before reaching this point.
	return nil
}

// Unwrap exposes the wrapped http.ResponseWriter so that
// http.NewResponseController (and other interface-unwrapping helpers) can reach
// optional interfaces such as FlusherWithError on the underlying connection.
func (w *GzipResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
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
					flushPolicy:    c.flushPolicy,
					contentTypes:   c.contentTypes,
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
	minSize      int
	level        int
	contentTypes []parsedContentType
	flushPolicy  FlushPolicy
}

func (c *config) validate() error {
	if c.level != gzip.DefaultCompression && (c.level < gzip.BestSpeed || c.level > gzip.BestCompression) {
		return fmt.Errorf("invalid compression level requested: %d", c.level)
	}

	if c.minSize < 0 {
		return fmt.Errorf("minimum size must be more than zero")
	}

	switch c.flushPolicy {
	case FlushBuffered, FlushAsGzip, FlushAsPlain:
	default:
		return fmt.Errorf("invalid flush policy requested: %d", c.flushPolicy)
	}

	return nil
}

type option func(c *config)

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

// StreamingFlush sets the FlushPolicy applied to the first Flush of a
// response while it is still below MinSize. Use FlushAsGzip to start gzip
// encoding on that first flush (streaming the current bytes immediately), or
// FlushAsPlain to commit to an uncompressed passthrough response. The default
// FlushBuffered keeps the historical behavior of buffering until the encoding
// is determined. The choice made by the first flush is permanent.
func StreamingFlush(policy FlushPolicy) option {
	return func(c *config) {
		c.flushPolicy = policy
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
