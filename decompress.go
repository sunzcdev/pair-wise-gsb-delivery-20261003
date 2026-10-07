package fasthttp

import (
	"errors"
	"io"
	"strings"
	"sync"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zlib"
	"github.com/klauspost/compress/zstd"
	brotli "github.com/molecule-man/go-brrr"
)

// ErrEncodedBodyTooLarge is returned by a streaming content-encoding decoder
// when the encoded body read from the network exceeds the caller-supplied
// limit on compressed bytes.
//
// It is distinct from [ErrBodyTooLarge], which is returned when the decoded
// body exceeds the limit on decompressed bytes.
var ErrEncodedBodyTooLarge = errors.New("fasthttp: encoded body size exceeds the given limit")

// errContentEncodingTrailingData is returned when bytes remain after a
// self-terminating encoded stream even though the response framing promised
// the stream was the whole body. The original response boundary cannot be
// trusted in that case.
var errContentEncodingTrailingData = errors.New("fasthttp: unexpected data after content-encoded stream")

// NewContentEncodingReader wraps r with the decoder chain described by
// contentEncoding, which is the value of the 'Content-Encoding' header,
// e.g. "gzip", "br" or "gzip, deflate".
//
// Multiple codings are unwrapped in the order mandated by RFC 9110: the
// first coding listed is applied to the wire bytes first and is decoded
// first. The supported codings are the same ones supported by the rest of
// the package: "gzip", "deflate", "br" and "zstd"; "identity" and an empty
// value mean no decoding. Unknown codings result in
// [ErrContentEncodingUnsupported].
//
// maxEncodedSize, when positive, bounds the number of encoded bytes pulled
// from r; reading one byte more than the limit returns
// [ErrEncodedBodyTooLarge]. maxDecodedSize, when positive, bounds the number
// of decoded bytes handed to callers; producing one byte more than the
// limit returns [ErrBodyTooLarge]. The check happens while decoding, without
// buffering the whole body first. A value of 0 or less disables the
// respective bound.
//
// Limit errors, truncated input and failed integrity checks release the
// pooled decoders. The caller must always Close the returned reader.
//
// If r implements [io.Closer] or [ReadCloserWithError], Close (or
// [ReadCloserWithError.CloseWithError]) is propagated to it, which releases
// the underlying response body and its connection. The connection is only
// reusable when the original response boundary was read reliably: after a
// clean end of stream, after an integrity-check failure on the final
// trailer, or whenever [ContentEncodingReader.FullyConsumed] is true.
// Truncated input and limit errors leave bytes unread, so the connection
// must be discarded; passing the read error to CloseWithError does that
// when r is a streamed response body returned by [Client.Do].
func NewContentEncodingReader(r io.Reader, contentEncoding string, maxEncodedSize, maxDecodedSize int64) (io.ReadCloser, error) {
	codings, err := parseContentEncodingCodings(contentEncoding)
	if err != nil {
		return nil, err
	}

	cr := &ContentEncodingReader{
		raw:          r,
		input:        &limitedBytesReader{r: r, limit: maxEncodedSize},
		decodedLimit: maxDecodedSize,
		layers:       make([]*decodingLayer, 0, len(codings)),
	}

	current := io.Reader(cr.input)
	// The first listed coding was applied to the plain body last, so it is
	// the outermost encoding on the wire and must be decoded first: build
	// layers in listed order, each new layer wrapping the previous one.
	for i := 0; i < len(codings); i++ {
		layerSource := current
		var boundary interface{ atBoundary() (bool, error) }
		if codings[i] == "br" {
			// Withhold the last byte so the boundary probe after a clean
			// brotli EOF can still observe trailing wire bytes; at real
			// source EOF the withheld byte is released.
			withheld := &withheldByteReader{r: current}
			layerSource = withheld
			boundary = withheld
		}
		// Defer decoder creation to the first Read. gzip and zlib readers
		// pull a header during construction, which would drive the whole
		// inner chain before the outer layer is in place; lazy setup keeps
		// every decoder reading only after the chain above it exists.
		layer := &decodingLayer{kind: codings[i], src: layerSource, maxDecoded: maxDecodedSize, boundary: boundary}
		cr.layers = append(cr.layers, layer)
		current = layer
	}
	cr.decoded = current
	return cr, nil
}

// parseContentEncodingCodings splits a Content-Encoding header value into
// its codings in listed order.
func parseContentEncodingCodings(contentEncoding string) ([]string, error) {
	contentEncoding = strings.TrimSpace(contentEncoding)
	if contentEncoding == "" {
		return nil, nil
	}
	parts := strings.Split(contentEncoding, ",")
	codings := make([]string, 0, len(parts))
	for _, part := range parts {
		coding := strings.ToLower(strings.TrimSpace(part))
		switch coding {
		case "", "identity":
			continue
		case "gzip", "deflate", "br", "zstd":
		default:
			return nil, ErrContentEncodingUnsupported
		}
		codings = append(codings, coding)
	}
	return codings, nil
}

// ContentEncodingReader is a streaming decoder for a Content-Encoded body.
type ContentEncodingReader struct {
	raw   io.Reader
	input *limitedBytesReader

	// decoded is the outermost decoder; reads from it produce plain bytes.
	decoded      io.Reader
	layers       []*decodingLayer
	decodedLimit int64
	decodedRead  int64

	finished bool
	consumed bool
	closed   bool

	closeOnce sync.Once
	closeErr  error
}

// Read implements [io.Reader].
func (r *ContentEncodingReader) Read(p []byte) (int, error) {
	if r.finished {
		return 0, io.EOF
	}
	if r.closed {
		return 0, io.ErrClosedPipe
	}

	// Ask the decoder for at most the remaining allowance plus one byte,
	// regardless of how big the caller buffer is. The extra byte is enough
	// to tell an exactly-at-limit stream from an oversized one, and keeps
	// the decoded working set bounded to the limit plus one chunk.
	readP := p
	if r.decodedLimit > 0 {
		remaining := r.decodedLimit + 1 - r.decodedRead
		if remaining <= 0 {
			r.finished = true
			r.discard()
			return 0, ErrBodyTooLarge
		}
		if int64(len(readP)) > remaining {
			readP = readP[:remaining]
		}
	}

	n, err := r.decoded.Read(readP)
	r.decodedRead += int64(n)

	if r.decodedLimit > 0 && r.decodedRead > r.decodedLimit {
		r.finished = true
		r.discard()
		return n, ErrBodyTooLarge
	}
	// The encoded-size limit takes precedence over any decoder error: the
	// decoder may have read the (limit+1)th byte and turned the resulting
	// failure into a format/truncation error of its own.
	if inputErr := r.checkInputLimit(); inputErr != nil {
		r.finished = true
		r.discard()
		return n, inputErr
	}
	if err != nil {
		consumed := false
		if err == io.EOF {
			err = r.checkBoundary()
			consumed = err == nil
		} else if r.failedAtTrailer(err) {
			// Integrity failures are detected while validating the final
			// trailer, i.e. after every encoded byte was consumed. Any
			// other terminal error (truncated input, bad header, unknown
			// format) happened mid-stream and the boundary is untrusted.
			consumed = r.rawInputAtEOF()
		}
		// Any terminal decoder error (truncated input, bad checksum,
		// unknown format, ...) makes the decoders unusable.
		r.finished = true
		r.consumed = r.consumed || consumed
		if consumed {
			r.release()
		} else {
			r.discard()
		}
		return n, err
	}
	return n, nil
}

// checkInputLimit turns over-limit input consumption into
// ErrEncodedBodyTooLarge as soon as it happens. Decoders are allowed to read
// ahead of what they decode (gzip probes the next member, brotli pulls input
// in 32 KiB chunks), so the limit can already be breached even though the
// decoder's Read returned some plain bytes and no error.
func (r *ContentEncodingReader) checkInputLimit() error {
	if r.input.limit <= 0 || r.input.read <= r.input.limit {
		return nil
	}
	return ErrEncodedBodyTooLarge
}

// checkBoundary verifies that the encoded response body ended exactly where
// the decoder said the stream ended. A successful decode alone does not prove
// that every wire byte was consumed for every coding, so one more byte is
// requested from the limited input reader: EOF confirms a reliable boundary,
// anything else means the framing is off or a byte was hidden past the end
// of the encoded stream.
func (r *ContentEncodingReader) checkBoundary() error {
	// Prefer the outermost decoder's direct input for the boundary check.
	// Brotli buffers input internally, so probing the raw reader alone would
	// miss bytes the brotli reader swallowed past the end of its stream.
	if len(r.layers) > 0 && r.layers[0].boundary != nil {
		atBoundary, err := r.layers[0].boundary.atBoundary()
		if atBoundary && err == nil {
			r.consumed = true
			return nil
		}
		if err != nil && err != io.EOF {
			return err
		}
		return errContentEncodingTrailingData
	}
	var probe [1]byte
	n, err := r.input.Read(probe[:])
	if n > 0 || errors.Is(err, ErrEncodedBodyTooLarge) {
		return errContentEncodingTrailingData
	}
	if err == io.EOF {
		r.consumed = true
		return nil
	}
	if err == nil {
		// A well-behaved reader never returns (0, nil), but treat it as
		// "no proof" rather than spinning here.
		return io.ErrNoProgress
	}
	return err
}

// rawInputAtEOF reports whether the limited input reader is already at EOF.
// It is used after a decoder error to distinguish a failure detected on the
// final trailer bytes (the response boundary was fully read) from
// truncation (bytes remain unread).
func (r *ContentEncodingReader) rawInputAtEOF() bool {
	var probe [1]byte
	n, err := r.input.Read(probe[:])
	return n == 0 && err == io.EOF
}

// failedAtTrailer reports whether err is an integrity failure detected while
// validating the final trailer bytes rather than a mid-stream failure.
func (r *ContentEncodingReader) failedAtTrailer(err error) bool {
	return errors.Is(err, gzip.ErrChecksum) || errors.Is(err, zlib.ErrChecksum)
}

// FullyConsumed reports whether the encoded body provided by the underlying
// reader was read up to the original response boundary. It only becomes
// meaningful once Read reports io.EOF or a terminal error.
//
// A true result means the connection carrying the body may be reused for
// another request; a false result means unread bytes may still be buffered
// or on the wire, so the connection must be discarded.
func (r *ContentEncodingReader) FullyConsumed() bool {
	return r.consumed
}

// Close releases the decoders and, when the underlying reader supports it,
// closes the response body. Decoders are recycled even when the caller stops
// reading before EOF.
func (r *ContentEncodingReader) Close() error {
	return r.CloseWithError(nil)
}

// CloseWithError implements [ReadCloserWithError]. A non-nil err signals
// that the caller gave up before the response boundary, which makes
// streamed response bodies discard the connection instead of pooling it.
func (r *ContentEncodingReader) CloseWithError(err error) error {
	r.closeOnce.Do(func() {
		r.closed = true
		// Callers that stop before a clean EOF may leave half-consumed
		// decoder state; never return such decoders to the pools.
		if r.finished && r.consumed {
			r.release()
		} else {
			r.discard()
		}
		switch src := r.raw.(type) {
		case ReadCloserWithError:
			r.closeErr = src.CloseWithError(err)
		case io.Closer:
			r.closeErr = src.Close()
		}
	})
	return r.closeErr
}

func (r *ContentEncodingReader) release() {
	for i := range r.layers {
		r.layers[i].release()
	}
}

// discard drops the decoders instead of returning them to the pools.
func (r *ContentEncodingReader) discard() {
	for i := range r.layers {
		r.layers[i].discard()
	}
}

// decodingLayer is one coding in the decoder chain. Exactly one decoder
// field is set, matching kind.
type decodingLayer struct {
	kind string

	src        io.Reader
	maxDecoded int64

	// boundary, when set, can report whether the decoder's direct input
	// ended exactly at the decoded stream boundary.
	boundary interface{ atBoundary() (bool, error) }

	gzip      *gzip.Reader
	flate     io.ReadCloser
	br        *brotli.Reader
	zstd      *limitedZstdReader
	zstdPlain *zstd.Decoder

	released bool
}

// init lazily creates the pooled decoder on first use. gzip and zlib read a
// header during construction, which would eagerly drive the whole inner
// chain before the outer layer exists; deferring setup keeps every decoder
// reading only after the chain above it is in place.
func (l *decodingLayer) init() error {
	src := l.src
	maxDecodedSize := l.maxDecoded
	switch l.kind {
	case "gzip":
		zr, err := acquireGzipReader(src)
		if err != nil {
			return err
		}
		l.gzip = zr
	case "deflate":
		zr, err := acquireFlateReader(src)
		if err != nil {
			return err
		}
		l.flate = zr
	case "br":
		l.br = acquireBrotliReader(src)
	case "zstd":
		if maxDecodedSize > 0 {
			limited, err := acquireLimitedZstdReader(src, int64ToInt(maxDecodedSize))
			if err != nil {
				return err
			}
			l.zstd = limited
		} else {
			zr, err := acquireZstdReader(src)
			if err != nil {
				return err
			}
			l.zstdPlain = zr
		}
	}
	l.src = nil
	return nil
}

// int64ToInt clamps an int64 bound into an int on 32-bit platforms.
func int64ToInt(n int64) int {
	const maxInt = int64(^uint(0) >> 1)
	if n > maxInt {
		return int(maxInt)
	}
	return int(n)
}

func (l *decodingLayer) initialized() bool {
	return l.gzip != nil || l.flate != nil || l.br != nil || l.zstd != nil || l.zstdPlain != nil
}

func (l *decodingLayer) Read(p []byte) (int, error) {
	if !l.initialized() {
		if err := l.init(); err != nil {
			return 0, err
		}
	}
	switch l.kind {
	case "gzip":
		return l.gzip.Read(p)
	case "deflate":
		return l.flate.Read(p)
	case "br":
		return l.br.Read(p)
	case "zstd":
		if l.zstd != nil {
			n, err := l.zstd.zr.Read(p)
			if errors.Is(err, zstd.ErrWindowSizeExceeded) || errors.Is(err, zstd.ErrDecoderSizeExceeded) {
				return n, ErrBodyTooLarge
			}
			return n, err
		}
		return l.zstdPlain.Read(p)
	default:
		panic("BUG: unknown content coding " + l.kind)
	}
}

func (l *decodingLayer) release() {
	if l.released {
		return
	}
	l.released = true
	if decoderReleaseHook != nil {
		decoderReleaseHook(l.kind, true)
	}
	switch l.kind {
	case "gzip":
		if l.gzip != nil {
			releaseGzipReader(l.gzip)
		}
	case "deflate":
		if l.flate != nil {
			releaseFlateReader(l.flate)
		}
	case "br":
		if l.br != nil {
			releaseBrotliReader(l.br)
		}
	case "zstd":
		if l.zstd != nil {
			releaseLimitedZstdReader(l.zstd)
		} else if l.zstdPlain != nil {
			releaseZstdReader(l.zstdPlain)
		}
	}
}

// discard closes the decoder without pooling it. A Reset after a partial or
// failed stream is not guaranteed to leave every pooled decoder reusable, so
// error paths pay the cost of dropping it.
func (l *decodingLayer) discard() {
	if l.released {
		return
	}
	l.released = true
	if decoderReleaseHook != nil {
		decoderReleaseHook(l.kind, false)
	}
	switch l.kind {
	case "gzip":
		if l.gzip != nil {
			releaseGzipReader(l.gzip)
		}
	case "deflate":
		if l.flate != nil {
			releaseFlateReader(l.flate)
		}
	case "br":
		// Close releases the ring buffer; do not put the reader back.
		if l.br != nil {
			_ = l.br.Close()
		}
	case "zstd":
		if l.zstd != nil {
			l.zstd.zr.Close()
		} else if l.zstdPlain != nil {
			l.zstdPlain.Close()
		}
	}
}

// decoderReleaseHook is used by internal tests to observe decoder cleanup.
// pooled reports whether the decoder went back to a pool (true) or was
// dropped after an incomplete or failed stream (false).
var decoderReleaseHook func(coding string, pooled bool)

// limitedBytesReader is an [io.LimitedReader] variant that returns
// [ErrEncodedBodyTooLarge] once one byte more than limit has been requested
// from the underlying reader. A limit of 0 or less disables limiting.
type limitedBytesReader struct {
	r     io.Reader
	limit int64
	read  int64
}

func (r *limitedBytesReader) Read(p []byte) (int, error) {
	if r.limit <= 0 {
		return r.r.Read(p)
	}
	remaining := r.limit + 1 - r.read
	if remaining <= 0 {
		return 0, ErrEncodedBodyTooLarge
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := r.r.Read(p)
	r.read += int64(n)
	if r.read > r.limit {
		return n, ErrEncodedBodyTooLarge
	}
	return n, err
}

// withheldByteReader mirrors the whole-body brotliSliceReader approach for
// streaming input: it never hands over the very last byte read from the
// underlying reader. The brotli decoder succeeds without consuming that byte
// when it marks the end of a self-terminating stream; if the byte turns out
// to be part of the stream, the decoder asks for more input and receives it.
//
// Unlike the slice variant, input is delivered in a private buffer so the
// invariant also holds with byte-at-a-time network delivery.
type withheldByteReader struct {
	r io.Reader
	// pending holds bytes read from r but not yet handed to the decoder.
	// Its last byte is always withheld.
	pending []byte
	atEOF   bool
}

// atBoundary reports whether the brotli decoder consumed the stream exactly
// where its input ended: the withheld buffer is empty after source EOF. A
// non-empty pending buffer means bytes arrived past the decoded stream.
func (r *withheldByteReader) atBoundary() (bool, error) {
	if len(r.pending) > 0 {
		return false, nil
	}
	if r.atEOF {
		return true, nil
	}
	var probe [1]byte
	n, err := r.Read(probe[:])
	if n > 0 {
		return false, nil
	}
	return err == io.EOF, err
}

func (r *withheldByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.atEOF && len(r.pending) == 0 {
		return 0, io.EOF
	}
	// Serve pending bytes, still withholding the final one.
	if len(r.pending) > 1 {
		n := copy(p, r.pending[:len(r.pending)-1])
		r.pending = r.pending[n:]
		return n, nil
	}
	if r.atEOF && len(r.pending) == 1 {
		p[0] = r.pending[0]
		r.pending = r.pending[1:]
		return 1, io.EOF
	}

	// Refill pending from the underlying reader into a private buffer. Read
	// until two bytes are available (so one can stay withheld) or the source
	// ends; the decoder requests 32 KiB chunks but network delivery may be a
	// single byte at a time.
	bufp := withheldByteBufPool.Get().(*[]byte) //nolint:forcetypeassert
	buf := *bufp
	if len(buf) < len(p)+1 {
		buf = make([]byte, len(p)+1)
	}
	defer withheldByteBufPool.Put(bufp)

	total := len(r.pending)
	buf = append(buf[:0], r.pending...)
	var readErr error
	for total < 2 {
		n, err := r.r.Read(buf[total:cap(buf)])
		total += n
		if err != nil {
			readErr = err
			break
		}
	}
	buf = buf[:total]
	if total == 0 {
		return 0, readErr
	}
	// Once the source has reported EOF there is no later byte to withhold
	// against: hand everything over with the source error so the decoder can
	// finalize. Remember that the source ended for the boundary probe.
	if readErr != nil {
		given := copy(p, buf)
		r.pending = append(r.pending[:0], buf[given:]...)
		if len(r.pending) == 0 {
			r.atEOF = true
		}
		return given, readErr
	}
	given := copy(p, buf[:total-1])
	// Preserve every byte not handed over (at least the withheld final one).
	r.pending = append(r.pending[:0], buf[given:total]...)
	return given, readErr
}

var withheldByteBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 32*1024+1)
		return &b
	},
}
