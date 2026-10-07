package fasthttp

import (
	"errors"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// ErrCompressedBodyTooLarge is returned by StreamDecoder when the compressed
// (network) input exceeds the limit passed to NewStreamDecoder.
//
// It is distinct from ErrBodyTooLarge, which reports the decompressed output
// limit, so callers can tell the two limits apart with errors.Is.
var ErrCompressedBodyTooLarge = errors.New("fasthttp: compressed body size exceeds the given limit")

// StreamDecoder incrementally decodes a compressed body stream.
//
// Unlike the whole-body helpers (BodyGunzip, BodyInflate, BodyUnbrotli,
// BodyUnzstd, BodyUncompressedWithLimit), it never buffers the full body:
// memory usage stays bounded by the decoder windows and the read buffers,
// not by the body size. This makes it suitable for large responses.
//
// Create it with NewStreamDecoder, read decoded bytes with Read and release
// the decoders with Close.
type StreamDecoder struct {
	in       *limitedInputReader
	out      *limitedOutputReader
	releases []func()
	err      error
	closed   bool
}

// NewStreamDecoder returns a StreamDecoder decoding the body stream r
// according to the given Content-Encoding header value.
//
// r must be bounded to the body, e.g. Response.BodyStream for a response
// read with Client.StreamResponseBody, so that FullyConsumed reflects
// whether the body boundary has been reached.
//
// Multiple comma-separated encodings are decoded in the order they were
// applied, i.e. the last token of the header value is decoded first.
// Supported tokens are "gzip", "deflate", "br", "zstd" and "identity".
// An empty contentEncoding passes the body through unchanged; the limits
// below still apply. Unknown tokens make NewStreamDecoder return
// ErrContentEncodingUnsupported.
//
// maxCompressedSize limits the bytes read from r and maxDecompressedSize
// limits the decoded bytes returned by Read. The limits are independent and
// are enforced while reading, not after the whole body has been consumed:
// Read returns ErrCompressedBodyTooLarge or ErrBodyTooLarge as soon as the
// respective limit is exceeded. A non-positive limit means no limit.
//
// For zstd bodies, maxDecompressedSize also caps the decoder window the
// same way BodyUnzstdWithLimit does.
//
// The caller must call Close once done, even on read errors. Read releases
// the decoders automatically once it returns a terminal error (including
// io.EOF), so truncated input, a corrupt checksum trailer and early Close
// all recycle the decoders.
func NewStreamDecoder(r io.Reader, contentEncoding string, maxCompressedSize, maxDecompressedSize int) (*StreamDecoder, error) {
	d := &StreamDecoder{
		in: &limitedInputReader{r: r, n: limitToRemaining(maxCompressedSize)},
	}

	// Content-Encoding lists encodings in the order they were applied,
	// so the last token is the outermost encoding and is decoded first.
	src := io.Reader(d.in)
	encodings := parseContentEncodingList(contentEncoding)
	for i := len(encodings) - 1; i >= 0; i-- {
		switch encodings[i] {
		case "gzip":
			zr, err := acquireGzipReader(src)
			if err != nil {
				d.release()
				return nil, err
			}
			src = zr
			d.releases = append(d.releases, func() { releaseGzipReader(zr) })
		case "deflate":
			zr, err := acquireFlateReader(src)
			if err != nil {
				d.release()
				return nil, err
			}
			src = zr
			d.releases = append(d.releases, func() { releaseFlateReader(zr) })
		case "br":
			zr := acquireBrotliReader(src)
			src = zr
			d.releases = append(d.releases, func() { releaseBrotliReader(zr) })
		case "zstd":
			if maxDecompressedSize > 0 {
				limited, err := acquireLimitedZstdReader(src, maxDecompressedSize)
				if err != nil {
					d.release()
					return nil, normalizeZstdLimitError(err)
				}
				src = limited.zr
				d.releases = append(d.releases, func() { releaseLimitedZstdReader(limited) })
			} else {
				zr, err := acquireZstdReader(src)
				if err != nil {
					d.release()
					return nil, err
				}
				src = zr
				d.releases = append(d.releases, func() { releaseZstdReader(zr) })
			}
		case "identity":
			// No decoding needed.
		default:
			d.release()
			return nil, ErrContentEncodingUnsupported
		}
	}

	d.out = &limitedOutputReader{r: src, n: limitToRemaining(maxDecompressedSize)}
	return d, nil
}

// Read reads decompressed bytes into p.
//
// It returns ErrCompressedBodyTooLarge once the compressed input limit and
// ErrBodyTooLarge once the decompressed output limit is exceeded. Once Read
// returns a non-nil error, the error is sticky and the decoders have been
// released.
func (d *StreamDecoder) Read(p []byte) (int, error) {
	if d.err != nil {
		return 0, d.err
	}
	if d.closed {
		return 0, io.ErrClosedPipe
	}
	n, err := d.out.Read(p)
	if err != nil {
		err = normalizeZstdLimitError(err)
		d.err = err
		d.release()
	}
	return n, err
}

// Close releases the decoders held by d. It is safe to call Close multiple
// times and after a failed Read.
//
// Closing before the decoded stream reached io.EOF leaves the rest of the
// raw body unread; use FullyConsumed to decide whether the underlying
// connection may be reused.
func (d *StreamDecoder) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	d.release()
	return nil
}

// FullyConsumed reports whether the raw input has been read to its end.
//
// If it returns false after Close, the body boundary has not been reliably
// reached and the underlying connection must not be reused for another
// request.
func (d *StreamDecoder) FullyConsumed() bool {
	return d.in.consumed
}

func (d *StreamDecoder) release() {
	for i := len(d.releases) - 1; i >= 0; i-- {
		d.releases[i]()
	}
	d.releases = nil
}

func normalizeZstdLimitError(err error) error {
	if errors.Is(err, zstd.ErrWindowSizeExceeded) || errors.Is(err, zstd.ErrDecoderSizeExceeded) {
		return ErrBodyTooLarge
	}
	return err
}

func parseContentEncodingList(header string) []string {
	if header == "" {
		return nil
	}
	parts := strings.Split(header, ",")
	encodings := make([]string, 0, len(parts))
	for _, part := range parts {
		if enc := strings.ToLower(strings.TrimSpace(part)); enc != "" {
			encodings = append(encodings, enc)
		}
	}
	return encodings
}

func limitToRemaining(limit int) int64 {
	if limit <= 0 {
		return -1
	}
	return int64(limit)
}

// limitedInputReader caps the compressed bytes read from r and remembers
// whether r has been read to its end.
type limitedInputReader struct {
	r        io.Reader
	n        int64 // remaining bytes before the limit; -1 means no limit
	consumed bool  // r returned io.EOF
	failed   bool  // the limit has been exceeded
}

func (l *limitedInputReader) Read(p []byte) (int, error) {
	if l.failed {
		return 0, ErrCompressedBodyTooLarge
	}
	if len(p) == 0 {
		return 0, nil
	}
	if l.n == 0 {
		// The input reached its limit. This is fine only if the source
		// is at its end; a single byte more exceeds the limit.
		for {
			var b [1]byte
			nr, err := l.r.Read(b[:])
			if nr > 0 {
				l.failed = true
				return 0, ErrCompressedBodyTooLarge
			}
			if err == io.EOF {
				l.consumed = true
			}
			if err != nil {
				return 0, err
			}
		}
	}
	if l.n > 0 && int64(len(p)) > l.n {
		p = p[:l.n]
	}
	nr, err := l.r.Read(p)
	if l.n > 0 {
		l.n -= int64(nr)
	}
	if err == io.EOF {
		l.consumed = true
	}
	return nr, err
}

// limitedOutputReader caps the decompressed bytes delivered from r.
type limitedOutputReader struct {
	r      io.Reader
	n      int64 // remaining bytes before the limit; -1 means no limit
	failed bool  // the limit has been exceeded
}

func (l *limitedOutputReader) Read(p []byte) (int, error) {
	if l.failed {
		return 0, ErrBodyTooLarge
	}
	if len(p) == 0 {
		return 0, nil
	}
	if l.n == 0 {
		// Exactly the limit has been delivered. Probe for one more byte
		// to distinguish a body of exactly the limit from a larger one.
		for {
			var b [1]byte
			nr, err := l.r.Read(b[:])
			if nr > 0 {
				l.failed = true
				return 0, ErrBodyTooLarge
			}
			if err != nil {
				return 0, err
			}
		}
	}
	if l.n > 0 && int64(len(p)) > l.n {
		p = p[:l.n]
	}
	nr, err := l.r.Read(p)
	if l.n > 0 {
		l.n -= int64(nr)
	}
	return nr, err
}
