package fasthttp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zlib"
	"github.com/klauspost/compress/zstd"
	"github.com/valyala/fasthttp/fasthttputil"
)

// ----- test helpers -----

func mustGzip(t *testing.T, p []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write(p); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func mustDeflate(t *testing.T, p []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zlib.NewWriter(&b)
	if _, err := zw.Write(p); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func mustBrotli(t *testing.T, p []byte) []byte {
	t.Helper()
	return AppendBrotliBytes(nil, p)
}

func mustZstd(t *testing.T, p []byte) []byte {
	t.Helper()
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	return encoder.EncodeAll(p, nil)
}

func encodeByCoding(t *testing.T, coding string, p []byte) []byte {
	t.Helper()
	switch coding {
	case "gzip":
		return mustGzip(t, p)
	case "deflate":
		return mustDeflate(t, p)
	case "br":
		return mustBrotli(t, p)
	case "zstd":
		return mustZstd(t, p)
	default:
		t.Fatalf("unknown coding %q", coding)
		return nil
	}
}

// encodeChain compresses p with the given codings in Content-Encoding order:
// the first coding is applied last (outermost).
func encodeChain(t *testing.T, p []byte, codings ...string) []byte {
	t.Helper()
	encoded := p
	for i := len(codings) - 1; i >= 0; i-- {
		encoded = encodeByCoding(t, codings[i], encoded)
	}
	return encoded
}

// drainReader fully reads r with the given Read size (1 for byte-by-byte
// input) and returns the decoded bytes and terminal error.
func drainReader(t *testing.T, r io.Reader, readSize int) ([]byte, error) {
	t.Helper()
	return drainReaderRaw(r, readSize)
}

func newDecompressor(t *testing.T, r io.Reader, ce string, maxEncoded, maxDecoded int64) *ContentEncodingReader {
	t.Helper()
	cr, err := NewContentEncodingReader(r, ce, maxEncoded, maxDecoded)
	if err != nil {
		t.Fatalf("NewContentEncodingReader(%q): %v", ce, err)
	}
	return cr.(*ContentEncodingReader)
}

func decodeAll(t *testing.T, encoded []byte, ce string, readSize int, maxEncoded, maxDecoded int64) ([]byte, bool, error) {
	t.Helper()
	cr := newDecompressor(t, bytes.NewReader(encoded), ce, maxEncoded, maxDecoded)
	out, err := drainReader(t, cr, readSize)
	consumed := cr.FullyConsumed()
	if cerr := cr.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return out, consumed, err
}

var allCodings = []string{"gzip", "deflate", "br", "zstd"}

func newDecompressInmemoryListener(t *testing.T) *fasthttputil.InmemoryListener {
	t.Helper()
	return fasthttputil.NewInmemoryListener()
}

// mustEncodeChain is encodeChain without a *testing.T dependency so it can
// be used from goroutines in stress tests.
func mustEncodeChain(p []byte, codings ...string) []byte {
	encoded := p
	for i := len(codings) - 1; i >= 0; i-- {
		var b bytes.Buffer
		switch codings[i] {
		case "gzip":
			zw := gzip.NewWriter(&b)
			_, _ = zw.Write(encoded)
			_ = zw.Close()
		case "deflate":
			zw := zlib.NewWriter(&b)
			_, _ = zw.Write(encoded)
			_ = zw.Close()
		case "br":
			b.Write(AppendBrotliBytes(nil, encoded))
		case "zstd":
			enc, _ := zstd.NewWriter(nil)
			b.Write(enc.EncodeAll(encoded, nil))
			enc.Close()
		}
		encoded = b.Bytes()
	}
	return encoded
}

// drainReaderRaw fully reads r and returns its bytes and terminal error.
func drainReaderRaw(r io.Reader, readSize int) ([]byte, error) {
	if readSize <= 0 {
		readSize = 4096
	}
	var out bytes.Buffer
	buf := make([]byte, readSize)
	for {
		n, err := r.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			if err == io.EOF {
				return out.Bytes(), nil
			}
			return out.Bytes(), err
		}
	}
}

func TestContentEncodingReaderRoundtrip(t *testing.T) {
	t.Parallel()

	body := append(createFixedBody(4096), []byte(" tail")...)
	for _, coding := range allCodings {
		for _, readSize := range []int{1, 7, 4096} {
			coding, readSize := coding, readSize
			t.Run(coding+"/read"+itoa(readSize), func(t *testing.T) {
				t.Parallel()
				encoded := encodeByCoding(t, coding, body)
				out, consumed, err := decodeAll(t, encoded, coding, readSize, 0, 0)
				if err != nil {
					t.Fatalf("decode error: %v", err)
				}
				if !bytes.Equal(out, body) {
					t.Fatalf("decoded %d bytes, want %d", len(out), len(body))
				}
				if !consumed {
					t.Fatalf("FullyConsumed=false after clean EOF")
				}
			})
		}
	}
}

func TestContentEncodingReaderPassthrough(t *testing.T) {
	t.Parallel()

	body := createFixedBody(1024)
	for _, tc := range []struct {
		ce      string
		encoded []byte
	}{
		{"", body},
		{"identity", body},
		{" identity ", body},
		{"Identity", body},
		{"gzip, identity", mustGzip(t, body)},
		{" identity , gzip", mustGzip(t, body)},
	} {
		tc := tc
		t.Run(tc.ce, func(t *testing.T) {
			t.Parallel()
			out, _, err := decodeAll(t, tc.encoded, tc.ce, 1, int64(len(body))+10, int64(len(body)))
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.ce, err)
			}
			if !bytes.Equal(out, body) {
				t.Fatalf("passthrough data mismatch for %q", tc.ce)
			}
		})
	}

	if _, err := NewContentEncodingReader(bytes.NewReader(nil), "gzip, snappy", 0, 0); !errors.Is(err, ErrContentEncodingUnsupported) {
		t.Fatalf("want ErrContentEncodingUnsupported, got %v", err)
	}
}

func TestContentEncodingReaderChained(t *testing.T) {
	t.Parallel()

	cases := [][]string{
		{"gzip", "deflate"},
		{"deflate", "gzip"},
		{"br", "gzip"},
		{"zstd", "br", "gzip"},
		{"gzip", "br", "deflate"},
	}
	body := createFixedBody(8192)
	for _, codings := range cases {
		codings := codings
		t.Run(joinCodings(codings), func(t *testing.T) {
			t.Parallel()
			encoded := encodeChain(t, body, codings...)
			for _, readSize := range []int{1, 32, 8192} {
				ce := joinCodings(codings)
				out, consumed, err := decodeAll(t, encoded, ce, readSize, 0, 0)
				if err != nil {
					t.Fatalf("readSize=%d: %v", readSize, err)
				}
				if !bytes.Equal(out, body) {
					t.Fatalf("readSize=%d: decoded %d bytes, want %d", readSize, len(out), len(body))
				}
				if !consumed {
					t.Fatalf("FullyConsumed=false after clean chained EOF")
				}
			}
		})
	}
}

// A highly compressible body ("zip bomb") must be stopped by the decoded-size
// limit regardless of how tiny the encoded input is.
func TestContentEncodingReaderZipBombDecodedLimit(t *testing.T) {
	t.Parallel()

	const decodedLimit = 4096
	body := bytes.Repeat([]byte{'A'}, 1<<20)
	for _, coding := range allCodings {
		coding := coding
		t.Run(coding, func(t *testing.T) {
			t.Parallel()
			encoded := encodeByCoding(t, coding, body)
			if len(encoded) >= len(body)/10 {
				t.Fatalf("test fixture not a high-ratio payload: %d -> %d", len(body), len(encoded))
			}

			r := bytes.NewReader(encoded)
			cr := newDecompressor(t, r, coding, 0, decodedLimit)
			out, err := drainReader(t, cr, 4096)
			if !errors.Is(err, ErrBodyTooLarge) {
				t.Fatalf("want ErrBodyTooLarge, got %v (decoded %d bytes)", err, len(out))
			}
			if int64(len(out)) != decodedLimit+1 {
				t.Fatalf("want exactly %d decoded bytes at the error, got %d", decodedLimit+1, len(out))
			}
			if cr.FullyConsumed() {
				t.Fatalf("FullyConsumed=true after decoded limit; connection must not be reusable")
			}
			if err := cr.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	}
}

// Memory bound: a zip bomb must never materialize its full decompressed
// body regardless of how large a buffer the caller hands to Read. The
// decoder is forced to ask for its natural chunk size, while the bytes
// visible to the caller never exceed limit+1 and the encoded bytes pulled
// stay tiny.
func TestContentEncodingReaderZipBombMemoryBound(t *testing.T) {
	t.Parallel()

	const decodedLimit = 2048
	body := bytes.Repeat([]byte{'Q'}, 4<<20)
	for _, coding := range allCodings {
		coding := coding
		t.Run(coding, func(t *testing.T) {
			t.Parallel()
			encoded := encodeByCoding(t, coding, body)
			// A 16 MiB caller buffer lets the decoder request its natural
			// chunk size; the reader must still clamp output to limit+1.
			src := newByteAtATimeReader(encoded)
			cr := newDecompressor(t, src, coding, 0, decodedLimit)
			buf := make([]byte, 16<<20)
			var total int64
			var maxChunk int
			for {
				n, err := cr.Read(buf)
				if n > maxChunk {
					maxChunk = n
				}
				total += int64(n)
				if err != nil {
					if !errors.Is(err, ErrBodyTooLarge) {
						t.Fatalf("want ErrBodyTooLarge, got %v after %d bytes", err, total)
					}
					break
				}
			}
			if total != decodedLimit+1 {
				t.Fatalf("decoded %d bytes total, want %d", total, decodedLimit+1)
			}
			// No single read can hand the caller more than limit+1, which
			// bounds the decoded working set independently of decoder
			// chunk sizes.
			if maxChunk > decodedLimit+1 {
				t.Fatalf("single read returned %d bytes, max allowed %d", maxChunk, decodedLimit+1)
			}
			if cr.FullyConsumed() {
				t.Fatal("FullyConsumed must be false after a decoded limit")
			}
			if err := cr.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func joinCodings(codings []string) string {
	var b bytes.Buffer
	for i, c := range codings {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(c)
	}
	return b.String()
}

// byteAtATimeReader delivers exactly one encoded byte per Read.
type byteAtATimeReader struct {
	b []byte
}

func newByteAtATimeReader(b []byte) *byteAtATimeReader {
	return &byteAtATimeReader{b: b}
}

func (r *byteAtATimeReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.b[0]
	r.b = r.b[1:]
	return 1, nil
}

// maxRequestReader records the largest Read request size and total bytes
// pulled from the network.
type maxRequestReader struct {
	r         io.Reader
	maxReq    int
	totalRead int64
}

func (r *maxRequestReader) Read(p []byte) (int, error) {
	if len(p) > r.maxReq {
		r.maxReq = len(p)
	}
	n, err := r.r.Read(p)
	r.totalRead += int64(n)
	return n, err
}

func TestContentEncodingReaderEncodedLimitExactVsPlusOne(t *testing.T) {
	t.Parallel()

	body := createFixedBody(2048)
	encoded := mustGzip(t, body)

	// Exactly the encoded length: the complete stream is within the limit
	// and decoding succeeds.
	out, consumed, err := decodeAll(t, encoded, "gzip", 4096, int64(len(encoded)), 0)
	if err != nil {
		t.Fatalf("limit == encoded length: %v", err)
	}
	if !bytes.Equal(out, body) || !consumed {
		t.Fatalf("limit == encoded length: decoded=%d consumed=%v", len(out), consumed)
	}

	// One byte less: limit is crossed before the stream ends.
	_, _, err = decodeAll(t, encoded, "gzip", 4096, int64(len(encoded))-1, 0)
	if !errors.Is(err, ErrEncodedBodyTooLarge) {
		t.Fatalf("want ErrEncodedBodyTooLarge at encoded-1, got %v", err)
	}
}

func TestContentEncodingReaderDecodedLimitExactVsPlusOne(t *testing.T) {
	t.Parallel()

	body := createFixedBody(1024)
	for _, coding := range allCodings {
		coding := coding
		t.Run(coding, func(t *testing.T) {
			t.Parallel()
			encoded := encodeByCoding(t, coding, body)

			out, consumed, err := decodeAll(t, encoded, coding, 4096, 0, int64(len(body)))
			if err != nil {
				t.Fatalf("limit == decoded length: %v", err)
			}
			if !bytes.Equal(out, body) || !consumed {
				t.Fatalf("limit == decoded length: decoded=%d consumed=%v", len(out), consumed)
			}

			out, _, err = decodeAll(t, encoded, coding, 4096, 0, int64(len(body))-1)
			if !errors.Is(err, ErrBodyTooLarge) {
				t.Fatalf("want ErrBodyTooLarge at decoded-1, got %v", err)
			}
			if len(out) != len(body) {
				t.Fatalf("want %d decoded bytes at the error, got %d", len(body), len(out))
			}
		})
	}
}

// Errors must be distinguishable.
func TestContentEncodingReaderDistinctLimitErrors(t *testing.T) {
	t.Parallel()

	body := bytes.Repeat([]byte{'Z'}, 1<<16)
	encoded := mustGzip(t, body)

	if errors.Is(ErrBodyTooLarge, ErrEncodedBodyTooLarge) {
		t.Fatal("limit errors must not match each other")
	}

	_, _, err := decodeAll(t, encoded, "gzip", 64, int64(len(encoded))/2, 0)
	if !errors.Is(err, ErrEncodedBodyTooLarge) {
		t.Fatalf("want ErrEncodedBodyTooLarge, got %v", err)
	}

	_, _, err = decodeAll(t, encoded, "gzip", 64, 0, 128)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("want ErrBodyTooLarge, got %v", err)
	}
}

// Prompt limiting: a large compressed payload under a tight decoded limit
// must stop before the whole decompressed body is produced, even though a
// highly compressed stream may fit in a few network reads.
func TestContentEncodingReaderPromptDecodedLimit(t *testing.T) {
	t.Parallel()

	body := bytes.Repeat([]byte{1, 2, 3, 4}, 1<<18)
	for _, coding := range allCodings {
		coding := coding
		t.Run(coding, func(t *testing.T) {
			t.Parallel()
			encoded := encodeByCoding(t, coding, body)
			cr := newDecompressor(t, bytes.NewReader(encoded), coding, 0, 1024)
			out, err := drainReader(t, cr, 512)
			if !errors.Is(err, ErrBodyTooLarge) {
				t.Fatalf("want ErrBodyTooLarge, got %v", err)
			}
			if int64(len(out)) != 1025 {
				t.Fatalf("decoded %d bytes before the limit error, want 1025", len(out))
			}
			if !cr.FullyConsumed() {
				// Expected: the oversized body is not read to its boundary.
			} else {
				t.Fatalf("FullyConsumed must be false after a decoded limit")
			}
			_ = cr.Close()
		})
	}
}

func TestContentEncodingReaderTruncated(t *testing.T) {
	t.Parallel()

	body := createFixedBody(4096)
	for _, coding := range allCodings {
		coding := coding
		t.Run(coding, func(t *testing.T) {
			t.Parallel()
			encoded := encodeByCoding(t, coding, body)
			for _, cut := range []int{1, len(encoded) / 2, len(encoded) - 1} {
				cut := cut
				out, consumed, err := decodeAll(t, encoded[:cut], coding, 4096, 0, 0)
				if err == nil || errors.Is(err, io.EOF) {
					t.Fatalf("cut=%d: expected decode error, got %v", cut, err)
				}
				if errors.Is(err, ErrBodyTooLarge) || errors.Is(err, ErrEncodedBodyTooLarge) {
					t.Fatalf("cut=%d: truncation must not look like a limit error: %v", cut, err)
				}
				if consumed {
					t.Fatalf("cut=%d: FullyConsumed must be false on truncated input", cut)
				}
				// A cut near the trailer may still have yielded every
				// decoded byte; the truncation is caught when the missing
				// trailer/end marker is requested. Only the tiny prefix is
				// bounded by the encoded prefix size.
				if cut == 1 && int64(len(out)) > int64(cut)*64+64 {
					t.Fatalf("cut=%d decoded %d bytes", cut, len(out))
				}
			}
		})
	}
}

func TestContentEncodingReaderBadTrailer(t *testing.T) {
	t.Parallel()

	body := createFixedBody(2048)
	// gzip and zlib carry an integrity trailer at the end of the body.
	for _, tc := range []struct {
		coding    string
		trailerSz int
	}{
		{"gzip", 8},
		{"deflate", 4},
	} {
		tc := tc
		t.Run(tc.coding, func(t *testing.T) {
			t.Parallel()
			encoded := encodeByCoding(t, tc.coding, body)
			corrupt := append([]byte(nil), encoded...)
			// Flip a byte inside the checksum trailer, not after it.
			pos := len(corrupt) - tc.trailerSz
			if encoded[pos] == 0 {
				corrupt[pos] = 1
			} else {
				corrupt[pos] = 0
			}

			out, consumed, err := decodeAll(t, corrupt, tc.coding, 4096, 0, 0)
			if err == nil {
				t.Fatalf("corrupt trailer accepted; decoded %d bytes", len(out))
			}
			if errors.Is(err, ErrBodyTooLarge) {
				t.Fatalf("integrity error mistaken for a size limit: %v", err)
			}
			if !consumed {
				t.Fatalf("trailer error occurs at the response boundary; FullyConsumed should be true")
			}
		})
	}
}

func TestContentEncodingReaderTrailingData(t *testing.T) {
	t.Parallel()

	body := createFixedBody(1024)
	encoded := mustGzip(t, body)
	trailing := append(append([]byte(nil), encoded...), 'X')

	out, consumed, err := decodeAll(t, trailing, "gzip", 4096, 0, 0)
	if err == nil {
		t.Fatalf("trailing byte accepted; decoded %d bytes", len(out))
	}
	if consumed {
		t.Fatalf("FullyConsumed must be false when bytes follow the encoded stream")
	}
}

func TestContentEncodingReaderReleasesDecoders(t *testing.T) {
	body := createFixedBody(1024)
	encoded := encodeChain(t, body, "gzip", "deflate", "br")

	type cleanup struct {
		coding string
		pooled bool
	}
	cases := []struct {
		name       string
		maxEncoded int64
		maxDecoded int64
		cutTo      int // when positive, truncate the encoded input
		pooledAll  bool
	}{
		{"clean EOF", 0, 0, 0, true},
		{"decoded limit", 0, 8, 0, false},
		{"encoded limit", 4, 0, 0, false},
		{"truncated", 0, 0, len(encoded) / 2, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Uses the global decoder pools via decoderReleaseHook; keep
			// the subtests serial so hook installation is not shared.
			var cleanups []cleanup
			decoderReleaseHook = func(coding string, pooled bool) {
				cleanups = append(cleanups, cleanup{coding, pooled})
			}
			defer func() { decoderReleaseHook = nil }()

			input := encoded
			if tc.cutTo > 0 {
				input = encoded[:tc.cutTo]
			}
			// Construction may already pull some bytes through gzip header
			// probing, so an early limit can surface there too; cleanup
			// must happen either way.
			cr, err := NewContentEncodingReader(bytes.NewReader(input), "gzip,deflate,br", tc.maxEncoded, tc.maxDecoded)
			if err == nil {
				_, err = drainReader(t, cr, 128)
			}
			if tc.pooledAll && err != nil {
				t.Fatalf("unexpected error in %q: %v", tc.name, err)
			}
			if !tc.pooledAll && err == nil {
				t.Fatalf("%q path was supposed to fail with a terminal error", tc.name)
			}
			if cr != nil {
				if cerr := cr.Close(); cerr != nil {
					t.Fatalf("Close: %v", cerr)
				}
				// Close must be idempotent.
				if cerr := cr.Close(); cerr != nil {
					t.Fatalf("second Close: %v", cerr)
				}
			}

			// gzip construction itself pulls inner headers, so a very
			// tight limit can stop gzip from being created at all; the
			// inner layers still had to be cleaned up.
			wantCodings := []string{"gzip", "deflate", "br"}
			if cr == nil {
				wantCodings = []string{"deflate", "br"}
			}
			want := map[string]bool{}
			for _, c := range wantCodings {
				want[c] = false
			}
			var pooledCount int
			for _, c := range cleanups {
				want[c.coding] = true
				if c.pooled {
					pooledCount++
				}
			}
			for coding, seen := range want {
				if !seen {
					t.Fatalf("decoder for %q was not cleaned up in %q path (cleanups=%v)", coding, tc.name, cleanups)
				}
			}
			if tc.pooledAll && pooledCount != len(cleanups) {
				t.Fatalf("clean EOF must pool every decoder, got %v", cleanups)
			}
			if !tc.pooledAll && pooledCount == len(cleanups) {
				t.Fatalf("%q must not pool decoders from an incomplete stream, got %v", tc.name, cleanups)
			}
		})
	}
}

func TestContentEncodingReaderEarlyClosePropagates(t *testing.T) {
	t.Parallel()

	body := createFixedBody(8192)
	encoded := mustGzip(t, body)
	tracker := &maxRequestReader{r: bytes.NewReader(encoded)}
	underlying := &closeTrackingReader{r: tracker}

	cr := newDecompressor(t, underlying, "gzip", 0, 0)
	buf := make([]byte, 64)
	n, err := cr.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatalf("first Read: %v", err)
	}
	if n == 0 {
		t.Fatal("expected some decoded bytes before early close")
	}
	partial := buf[:n]
	if cr.FullyConsumed() {
		t.Fatal("FullyConsumed must be false after a partial read")
	}
	if err := cr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !underlying.closed {
		t.Fatal("Close did not propagate to the underlying response body")
	}
	// gzip reads ahead when it reaches EOF; the body may already be fully
	// pulled from the network even though the caller stopped early, which is
	// exactly why the caller must decide reuse via FullyConsumed rather than
	// bytes read. Here the caller only got 64 bytes of a 8 KiB body, so
	// FullyConsumed=false already proves the boundary was not reached.
	if int64(len(partial)) >= int64(len(body)) {
		t.Fatalf("first Read unexpectedly decoded the whole %d-byte body", len(body))
	}

	// Reading after Close must fail.
	if _, err := cr.Read(buf); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Read after Close: want io.ErrClosedPipe, got %v", err)
	}
}

type closeTrackingReader struct {
	r      io.Reader
	closed bool
	err    error
}

func (r *closeTrackingReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, io.ErrClosedPipe
	}
	return r.r.Read(p)
}

func (r *closeTrackingReader) Close() error {
	r.closed = true
	return nil
}

func (r *closeTrackingReader) CloseWithError(err error) error {
	r.err = err
	return r.Close()
}

func TestContentEncodingReaderCloseWithErrorDiscardsConn(t *testing.T) {
	t.Parallel()

	body := createFixedBody(4096)
	encoded := mustGzip(t, body)
	underlying := &closeTrackingReader{r: bytes.NewReader(encoded)}

	cr := newDecompressor(t, underlying, "gzip", 0, 16)
	buf := make([]byte, 64)
	if _, err := cr.Read(buf); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("want ErrBodyTooLarge, got %v", err)
	}
	// The caller reports why it stopped; a streamed client body would now
	// discard the connection instead of pooling it.
	if err := cr.CloseWithError(ErrBodyTooLarge); err != nil {
		t.Fatalf("CloseWithError: %v", err)
	}
	if !errors.Is(underlying.err, ErrBodyTooLarge) {
		t.Fatalf("underlying CloseWithError got %v, want %v", underlying.err, ErrBodyTooLarge)
	}
}

// Encoded limit enforcement with byte-at-a-time delivery: the limit+1 byte
// must be the first rejected one, even on a fully in-bounds stream.
func TestContentEncodingReaderEncodedLimitByteAtATime(t *testing.T) {
	t.Parallel()

	body := createFixedBody(1024)
	for _, coding := range allCodings {
		coding := coding
		t.Run(coding, func(t *testing.T) {
			t.Parallel()
			encoded := encodeByCoding(t, coding, body)

			// Within limit by exactly one byte less than the full encoded
			// length fails, because the stream needs all of its bytes.
			src := newByteAtATimeReader(encoded)
			cr := newDecompressor(t, src, coding, int64(len(encoded))-1, 0)
			_, err := drainReader(t, cr, 1)
			if !errors.Is(err, ErrEncodedBodyTooLarge) {
				t.Fatalf("encoded-1: want ErrEncodedBodyTooLarge, got %v", err)
			}
			_ = cr.Close()

			// Exact length succeeds byte by byte.
			out, consumed, err := decodeAll(t, encoded, coding, 1, int64(len(encoded)), 0)
			if err != nil {
				t.Fatalf("exact encoded limit: %v", err)
			}
			if !bytes.Equal(out, body) || !consumed {
				t.Fatalf("exact encoded limit: decoded=%d consumed=%v", len(out), consumed)
			}
		})
	}
}

// Network input must never be over-read past the encoded limit by more than
// the single probe byte used to check the boundary.
func TestContentEncodingReaderEncodedLimitNetworkBound(t *testing.T) {
	t.Parallel()

	// Mix highly repetitive and pseudo-random data so the encoded body is
	// itself large; otherwise the whole compressed stream fits in a few
	// dozen bytes and a 64-byte network limit could never be crossed.
	body := append(bytes.Repeat([]byte("network-bound-pattern-"), 2048),
		createIncompressibleBody(1<<16)...)
	for _, coding := range allCodings {
		coding := coding
		t.Run(coding, func(t *testing.T) {
			t.Parallel()
			encoded := encodeByCoding(t, coding, body)
			limit := int64(len(encoded) / 2)
			if limit < 8 {
				limit = 8
			}
			tracker := &maxRequestReader{r: bytes.NewReader(encoded)}
			cr := newDecompressor(t, tracker, coding, limit, 0)
			_, err := drainReader(t, cr, 37)
			if !errors.Is(err, ErrEncodedBodyTooLarge) {
				t.Fatalf("want ErrEncodedBodyTooLarge, got %v", err)
			}
			_ = cr.Close()
			if tracker.totalRead > limit+1 {
				t.Fatalf("pulled %d encoded bytes with a %d-byte limit", tracker.totalRead, limit)
			}
		})
	}
}

func TestResponseBodyUncompressedStream(t *testing.T) {
	t.Parallel()

	body := createFixedBody(4096)
	encoded := mustGzip(t, body)
	var resp Response
	resp.Header.SetContentEncoding("gzip")
	resp.SetBody(encoded)

	r, err := resp.BodyUncompressedStreamWithLimit(int64(len(encoded)), int64(len(body)))
	if err != nil {
		t.Fatalf("BodyUncompressedStreamWithLimit: %v", err)
	}
	out, err := drainReader(t, r, 512)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !bytes.Equal(out, body) {
		t.Fatalf("decoded %d bytes, want %d", len(out), len(body))
	}
	cr := r.(*ContentEncodingReader)
	if !cr.FullyConsumed() {
		t.Fatal("FullyConsumed=false on a complete buffered response")
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Chained codings on a request too.
	chained := encodeChain(t, body, "br", "gzip")
	var req Request
	req.Header.SetContentEncoding("br,gzip")
	req.SetBody(chained)
	rr, err := req.BodyUncompressedStream()
	if err != nil {
		t.Fatalf("BodyUncompressedStream: %v", err)
	}
	out, err = drainReader(t, rr, 1)
	if err != nil {
		t.Fatalf("chained byte-at-a-time drain: %v", err)
	}
	if !bytes.Equal(out, body) {
		t.Fatalf("chained decoded %d bytes, want %d", len(out), len(body))
	}
	if err := rr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestContentEncodingReaderOldWholeBodyHelpersStillWork(t *testing.T) {
	t.Parallel()

	body := createFixedBody(2048)
	if got, err := AppendGunzipBytes(nil, mustGzip(t, body)); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("AppendGunzipBytes: %v", err)
	}
	if got, err := AppendInflateBytes(nil, mustDeflate(t, body)); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("AppendInflateBytes: %v", err)
	}
	if got, err := AppendUnbrotliBytes(nil, mustBrotli(t, body)); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("AppendUnbrotliBytes: %v", err)
	}
	if got, err := AppendUnzstdBytes(nil, mustZstd(t, body)); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("AppendUnzstdBytes: %v", err)
	}
}

// Pool reuse across many successful and failing streams, concurrently, must
// stay correct: every clean stream round-trips and every limit/truncation
// still produces its error.
func TestContentEncodingReaderPoolReuseStress(t *testing.T) {
	t.Parallel()

	cases := []struct {
		ce      string
		codings []string
	}{
		{"gzip", []string{"gzip"}},
		{"deflate", []string{"deflate"}},
		{"br", []string{"br"}},
		{"zstd", []string{"zstd"}},
		{"br,gzip", []string{"br", "gzip"}},
		{"gzip,deflate", []string{"gzip", "deflate"}},
		{"zstd,br,gzip", []string{"zstd", "br", "gzip"}},
	}
	bodies := [][]byte{
		createFixedBody(1),
		createFixedBody(128),
		createFixedBody(1 << 14),
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 64)
	for worker := range 16 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for iter := range 100 {
				c := cases[(worker+iter)%len(cases)]
				body := bodies[(worker+iter)%len(bodies)]
				encoded := mustEncodeChain(body, c.codings...)

				switch iter % 4 {
				case 0:
					// Full clean stream.
					r, err := NewContentEncodingReader(bytes.NewReader(encoded), c.ce, 0, 0)
					if err != nil {
						errCh <- fmt.Errorf("clean %s: %w", c.ce, err)
						return
					}
					out, err := drainReaderRaw(r, 1+(iter%64))
					_ = r.Close()
					if err != nil || !bytes.Equal(out, body) {
						errCh <- fmt.Errorf("clean %s: n=%d err=%v", c.ce, len(out), err)
						return
					}
				case 1:
					// Decoded limit.
					if int64(len(body)) <= 8 {
						continue
					}
					r, _ := NewContentEncodingReader(bytes.NewReader(encoded), c.ce, 0, 8)
					_, err := drainReaderRaw(r, 4)
					_ = r.Close()
					if !errors.Is(err, ErrBodyTooLarge) {
						errCh <- fmt.Errorf("decoded-limit %s: %v", c.ce, err)
						return
					}
				case 2:
					// Encoded limit.
					if len(encoded) <= 4 {
						continue
					}
					r, _ := NewContentEncodingReader(bytes.NewReader(encoded), c.ce, 4, 0)
					_, err := drainReaderRaw(r, 4)
					_ = r.Close()
					if !errors.Is(err, ErrEncodedBodyTooLarge) {
						errCh <- fmt.Errorf("encoded-limit %s: %v", c.ce, err)
						return
					}
				case 3:
					// Truncation.
					cut := len(encoded) / 2
					if len(encoded)-cut < 2 {
						continue
					}
					r, err := NewContentEncodingReader(bytes.NewReader(encoded[:cut]), c.ce, 0, 0)
					if err == nil {
						_, err = drainReaderRaw(r, 16)
					}
					_ = r.Close()
					if err == nil {
						errCh <- fmt.Errorf("truncation %s accepted", c.ce)
						return
					}
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

// End-to-end: streamed HTTP response + content-encoding decoder. The
// connection stays usable after a fully consumed body and is discarded (the
// client surfaces an error or opens a new connection) after a decoded limit.
func TestContentEncodingReaderClientStreaming(t *testing.T) {
	t.Parallel()

	const bodySize = 1 << 14
	body := createFixedBody(bodySize)
	gzipped := mustGzip(t, body)

	srvCh := make(chan struct{})
	ln := newDecompressInmemoryListener(t)
	var accepted atomic.Int64
	go func() {
		defer close(srvCh)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				// Serve sequential keep-alive requests on one connection.
				for {
					req := AcquireRequest()
					if err := req.Read(br); err != nil {
						ReleaseRequest(req)
						return
					}
					ReleaseRequest(req)
					fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Encoding: gzip\r\nContent-Length: %d\r\nConnection: keep-alive\r\n\r\n", len(gzipped))
					c.Write(gzipped)
				}
			}(conn)
		}
	}()
	t.Cleanup(func() { ln.Close(); <-srvCh })

	c := &Client{
		Dial: func(_ string) (net.Conn, error) { return ln.Dial() },
	}

	// First request: fully consume the decoded body; connection is reusable.
	req := AcquireRequest()
	resp := AcquireResponse()
	resp.StreamBody = true
	req.SetRequestURI("http://example.com/")
	if err := c.Do(req, resp); err != nil {
		t.Fatalf("first Do: %v", err)
	}
	dec, err := NewContentEncodingReader(resp.BodyStream(), "gzip", 0, int64(bodySize))
	if err != nil {
		t.Fatalf("NewContentEncodingReader: %v", err)
	}
	got, derr := drainReaderRaw(dec, 128)
	if derr != nil || !bytes.Equal(got, body) {
		t.Fatalf("decoded: n=%d err=%v", len(got), derr)
	}
	if !dec.(*ContentEncodingReader).FullyConsumed() {
		t.Fatal("FullyConsumed=false after clean stream")
	}
	if err := dec.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ReleaseRequest(req)
	ReleaseResponse(resp)

	// Second request on the same pooled connection must succeed.
	req2 := AcquireRequest()
	resp2 := AcquireResponse()
	resp2.StreamBody = true
	req2.SetRequestURI("http://example.com/")
	if err := c.Do(req2, resp2); err != nil {
		t.Fatalf("reused connection Do: %v", err)
	}
	if got := accepted.Load(); got != 1 {
		t.Fatalf("expected the second request to reuse the single connection, got %d", got)
	}
	dec2, err := NewContentEncodingReader(resp2.BodyStream(), "gzip", 0, bodySize/2)
	if err != nil {
		t.Fatalf("NewContentEncodingReader(2): %v", err)
	}
	_, derr = drainReaderRaw(dec2, 128)
	if !errors.Is(derr, ErrBodyTooLarge) {
		t.Fatalf("want ErrBodyTooLarge, got %v", derr)
	}
	cr2 := dec2.(*ContentEncodingReader)
	if cr2.FullyConsumed() {
		t.Fatal("FullyConsumed must be false after a decoded limit")
	}
	// Report the reason so the client discards the unread connection.
	if err := cr2.CloseWithError(derr); err != nil {
		t.Fatalf("CloseWithError: %v", err)
	}
	ReleaseRequest(req2)
	ReleaseResponse(resp2)

	// A third request must still work because the client discarded the
	// tainted connection and opened a fresh one.
	req3 := AcquireRequest()
	defer ReleaseRequest(req3)
	resp3 := AcquireResponse()
	defer ReleaseResponse(resp3)
	resp3.StreamBody = true
	req3.SetRequestURI("http://example.com/")
	if err := c.Do(req3, resp3); err != nil {
		t.Fatalf("fresh connection Do: %v", err)
	}
	if got := accepted.Load(); got != 2 {
		t.Fatalf("expected a fresh connection after the discarded body, got %d total", got)
	}
	dec3, err := NewContentEncodingReader(resp3.BodyStream(), "gzip", 0, 0)
	if err != nil {
		t.Fatalf("NewContentEncodingReader(3): %v", err)
	}
	got, derr = drainReaderRaw(dec3, 4096)
	if derr != nil || !bytes.Equal(got, body) {
		t.Fatalf("third decode: n=%d err=%v", len(got), derr)
	}
	if err := dec3.Close(); err != nil {
		t.Fatalf("Close(3): %v", err)
	}
}
