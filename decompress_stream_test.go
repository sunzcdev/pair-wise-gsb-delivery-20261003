package fasthttp

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
)

// byteByByteReader hands out a single byte per Read call to make sure the
// decoder chain copes with arbitrarily fragmented network input.
type byteByByteReader struct {
	b []byte
}

func (r *byteByByteReader) Read(p []byte) (int, error) {
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

func streamDecoderTestData() []byte {
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		sb.WriteString("the quick brown fox jumps over the lazy dog. ")
	}
	return []byte(sb.String())
}

func encodeForStreamDecoder(t *testing.T, encoding string, data []byte) []byte {
	t.Helper()
	switch encoding {
	case "gzip":
		return AppendGzipBytes(nil, data)
	case "deflate":
		return AppendDeflateBytes(nil, data)
	case "br":
		return AppendBrotliBytes(nil, data)
	case "zstd":
		return AppendZstdBytes(nil, data)
	default:
		t.Fatalf("unexpected encoding %q", encoding)
		return nil
	}
}

// encodeChain applies encodings in the listed order, matching the
// Content-Encoding header semantics.
func encodeChain(t *testing.T, encodings []string, data []byte) []byte {
	t.Helper()
	for _, enc := range encodings {
		data = encodeForStreamDecoder(t, enc, data)
	}
	return data
}

func drainStreamDecoder(t *testing.T, d *StreamDecoder) ([]byte, error) {
	t.Helper()
	defer d.Close() //nolint:errcheck
	return io.ReadAll(d)
}

func TestStreamDecoderRoundTrip(t *testing.T) {
	t.Parallel()

	data := streamDecoderTestData()
	chains := [][]string{
		{"gzip"},
		{"deflate"},
		{"br"},
		{"zstd"},
		{"identity"},
		{"gzip", "br"},
		{"br", "gzip"},
		{"deflate", "gzip", "br"},
		{"gzip", "gzip"},
	}
	for _, chain := range chains {
		header := strings.Join(chain, ", ")
		t.Run(header, func(t *testing.T) {
			t.Parallel()

			var encodings []string
			for _, enc := range chain {
				if enc != "identity" {
					encodings = append(encodings, enc)
				}
			}
			compressed := encodeChain(t, encodings, data)

			// Feed the decoder one byte at a time.
			d, err := NewStreamDecoder(&byteByByteReader{b: compressed}, header, 0, 0)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			decoded, err := drainStreamDecoder(t, d)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bytes.Equal(decoded, data) {
				t.Fatalf("unexpected decoded body: got %d bytes, want %d", len(decoded), len(data))
			}
		})
	}
}

func TestStreamDecoderNoEncoding(t *testing.T) {
	t.Parallel()

	data := streamDecoderTestData()
	d, err := NewStreamDecoder(bytes.NewReader(data), "", 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	decoded, err := drainStreamDecoder(t, d)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(decoded, data) {
		t.Fatalf("unexpected body: got %d bytes, want %d", len(decoded), len(data))
	}
	if !d.FullyConsumed() {
		t.Fatal("expected the raw input to be fully consumed")
	}
}

func TestStreamDecoderExactLimits(t *testing.T) {
	t.Parallel()

	data := streamDecoderTestData()
	compressed := AppendGzipBytes(nil, data)

	t.Run("output limit exact", func(t *testing.T) {
		t.Parallel()
		d, err := NewStreamDecoder(bytes.NewReader(compressed), "gzip", 0, len(data))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		decoded, err := drainStreamDecoder(t, d)
		if err != nil {
			t.Fatalf("unexpected error for a body of exactly the limit: %v", err)
		}
		if !bytes.Equal(decoded, data) {
			t.Fatalf("unexpected decoded body: got %d bytes, want %d", len(decoded), len(data))
		}
	})

	t.Run("output limit minus one", func(t *testing.T) {
		t.Parallel()
		d, err := NewStreamDecoder(bytes.NewReader(compressed), "gzip", 0, len(data)-1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		decoded, err := drainStreamDecoder(t, d)
		if !errors.Is(err, ErrBodyTooLarge) {
			t.Fatalf("expected ErrBodyTooLarge, got %v", err)
		}
		if len(decoded) != len(data)-1 {
			t.Fatalf("expected %d decoded bytes before the error, got %d", len(data)-1, len(decoded))
		}
	})

	t.Run("input limit exact", func(t *testing.T) {
		t.Parallel()
		d, err := NewStreamDecoder(bytes.NewReader(compressed), "gzip", len(compressed), 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		decoded, err := drainStreamDecoder(t, d)
		if err != nil {
			t.Fatalf("unexpected error for input of exactly the limit: %v", err)
		}
		if !bytes.Equal(decoded, data) {
			t.Fatalf("unexpected decoded body: got %d bytes, want %d", len(decoded), len(data))
		}
		if !d.FullyConsumed() {
			t.Fatal("expected the raw input to be fully consumed")
		}
	})

	t.Run("input limit minus one", func(t *testing.T) {
		t.Parallel()
		d, err := NewStreamDecoder(bytes.NewReader(compressed), "gzip", len(compressed)-1, 0)
		if err == nil {
			_, err = drainStreamDecoder(t, d)
		}
		if !errors.Is(err, ErrCompressedBodyTooLarge) {
			t.Fatalf("expected ErrCompressedBodyTooLarge, got %v", err)
		}
	})
}

func TestStreamDecoderHighCompressionRatio(t *testing.T) {
	t.Parallel()

	// 8 MiB of zeros compress to roughly 8 KiB with gzip.
	data := make([]byte, 8<<20)
	compressed := AppendGzipBytes(nil, data)
	if len(compressed) > 1<<20 {
		t.Fatalf("expected a highly compressible body, got %d compressed bytes", len(compressed))
	}

	const maxDecompressed = 64 << 10
	d, err := NewStreamDecoder(bytes.NewReader(compressed), "gzip", 0, maxDecompressed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	decoded, err := drainStreamDecoder(t, d)
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("expected ErrBodyTooLarge, got %v", err)
	}
	if len(decoded) != maxDecompressed {
		t.Fatalf("expected exactly %d decoded bytes, got %d", maxDecompressed, len(decoded))
	}
	// The limit must be enforced while reading: the decoder stopped long
	// before the whole compressed input was consumed.
	if d.FullyConsumed() {
		t.Fatal("the decoder consumed the whole input before hitting the output limit")
	}

	// The compressed input limit triggers independently and early as well.
	d, err = NewStreamDecoder(bytes.NewReader(compressed), "gzip", 1024, 0)
	if err == nil {
		_, err = drainStreamDecoder(t, d)
	}
	if !errors.Is(err, ErrCompressedBodyTooLarge) {
		t.Fatalf("expected ErrCompressedBodyTooLarge, got %v", err)
	}
}

func TestStreamDecoderMemoryBound(t *testing.T) {
	t.Parallel()

	// 64 MiB of zeros compress to roughly 64 KiB. Decoding the stream,
	// byte by byte and through two stacked encodings, must not retain
	// memory proportional to the decompressed size.
	data := make([]byte, 64<<20)
	compressed := encodeChain(t, []string{"deflate", "gzip"}, data)
	t.Logf("compressed size: %d bytes", len(compressed))

	runtime.GC()
	var m1, m2 runtime.MemStats
	runtime.ReadMemStats(&m1)

	d, err := NewStreamDecoder(&byteByByteReader{b: compressed}, "deflate, gzip", 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var total int
	buf := make([]byte, 32<<10)
	for {
		n, err := d.Read(buf)
		total += n
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatalf("unexpected close error: %v", err)
	}
	if total != len(data) {
		t.Fatalf("unexpected decoded size: got %d, want %d", total, len(data))
	}

	runtime.GC()
	runtime.ReadMemStats(&m2)
	if grown := int64(m2.HeapAlloc) - int64(m1.HeapAlloc); grown > 16<<20 {
		t.Fatalf("decoding retained too much memory: heap grew by %d bytes for a %d bytes body", grown, len(data))
	}
}

func TestStreamDecoderTruncatedInput(t *testing.T) {
	t.Parallel()

	data := streamDecoderTestData()
	compressed := AppendGzipBytes(nil, data)
	truncated := compressed[:len(compressed)/2]

	for gzipReaderPool.Get() != nil { //nolint:revive
		// Drain the pool so the recycling check below is meaningful.
	}

	d, err := NewStreamDecoder(bytes.NewReader(truncated), "gzip", 0, 0)
	if err == nil {
		_, err = drainStreamDecoder(t, d)
	}
	if err == nil {
		t.Fatal("expected an error for truncated input")
	}
	if errors.Is(err, ErrBodyTooLarge) || errors.Is(err, ErrCompressedBodyTooLarge) {
		t.Fatalf("unexpected limit error for truncated input: %v", err)
	}

	// The decoder must have been recycled even though the stream failed.
	if gzipReaderPool.Get() == nil {
		t.Fatal("gzip decoder was not recycled after truncated input")
	}
}

func TestStreamDecoderBadChecksum(t *testing.T) {
	t.Parallel()

	data := streamDecoderTestData()
	compressed := AppendGzipBytes(nil, data)
	// Corrupt the CRC32 in the gzip trailer.
	compressed[len(compressed)-5] ^= 0xff

	for gzipReaderPool.Get() != nil { //nolint:revive
		// Drain the pool so the recycling check below is meaningful.
	}

	d, err := NewStreamDecoder(bytes.NewReader(compressed), "gzip", 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, err = drainStreamDecoder(t, d)
	if !errors.Is(err, gzip.ErrChecksum) {
		t.Fatalf("expected gzip.ErrChecksum, got %v", err)
	}

	if gzipReaderPool.Get() == nil {
		t.Fatal("gzip decoder was not recycled after a checksum error")
	}
}

func TestStreamDecoderEarlyClose(t *testing.T) {
	t.Parallel()

	data := streamDecoderTestData()
	compressed := AppendGzipBytes(nil, data)

	for gzipReaderPool.Get() != nil { //nolint:revive
		// Drain the pool so the recycling check below is meaningful.
	}

	d, err := NewStreamDecoder(bytes.NewReader(compressed), "gzip", 0, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	buf := make([]byte, 16)
	if _, err := d.Read(buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("unexpected close error: %v", err)
	}
	// Close is idempotent.
	if err := d.Close(); err != nil {
		t.Fatalf("unexpected close error: %v", err)
	}

	// The body boundary has not been reached, so the connection carrying
	// this body must not be reused.
	if d.FullyConsumed() {
		t.Fatal("expected the raw input to be partially unread after early close")
	}
	if _, err := d.Read(buf); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("expected io.ErrClosedPipe after close, got %v", err)
	}

	if gzipReaderPool.Get() == nil {
		t.Fatal("gzip decoder was not recycled after early close")
	}
}

func TestStreamDecoderZstdWindowLimit(t *testing.T) {
	t.Parallel()

	// A frame encoded with a window larger than the decoder cap derived
	// from the output limit must fail with ErrBodyTooLarge.
	data := make([]byte, 12<<20)
	for i := range data {
		data[i] = byte(i / 64)
	}
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf, zstd.WithWindowSize(16<<20))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := zw.Write(data); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d, err := NewStreamDecoder(bytes.NewReader(buf.Bytes()), "zstd", 0, 4096)
	if err == nil {
		_, err = drainStreamDecoder(t, d)
	}
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("expected ErrBodyTooLarge, got %v", err)
	}
}

func TestStreamDecoderUnsupportedEncoding(t *testing.T) {
	t.Parallel()

	if _, err := NewStreamDecoder(bytes.NewReader(nil), "compress", 0, 0); !errors.Is(err, ErrContentEncodingUnsupported) {
		t.Fatalf("expected ErrContentEncodingUnsupported, got %v", err)
	}

	// An unsupported layer in a multi-encoding header releases the
	// decoders acquired for the layers processed before it.
	for gzipReaderPool.Get() != nil { //nolint:revive
		// Drain the pool so the recycling check below is meaningful.
	}
	compressed := AppendGzipBytes(nil, streamDecoderTestData())
	if _, err := NewStreamDecoder(bytes.NewReader(compressed), "compress, gzip", 0, 0); !errors.Is(err, ErrContentEncodingUnsupported) {
		t.Fatalf("expected ErrContentEncodingUnsupported, got %v", err)
	}
	if gzipReaderPool.Get() == nil {
		t.Fatal("gzip decoder was not recycled after a construction failure")
	}
}
