package imapserver

import (
	"bytes"
	"compress/flate"
	"errors"
	"io"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// defaultCompressMaxDecompressedBytes is the default limit for the total
// number of decompressed bytes read from a single connection.
const defaultCompressMaxDecompressedBytes = 1 << 30 // 1 GiB

var errDecompressedLimit = errors.New("imapserver: decompressed data limit exceeded")

func (options *Options) compressMaxDecompressedBytes() int64 {
	if options.CompressMaxDecompressedBytes == 0 {
		return defaultCompressMaxDecompressedBytes
	}
	return options.CompressMaxDecompressedBytes
}

// handleCompress implements the COMPRESS command (RFC 4978).
//
// Compression begins right after the OK response: the response itself is
// sent uncompressed. COMPRESS may be issued in any state, but only once per
// connection. STARTTLS is rejected once compression is active (see
// canStartTLS).
func (c *Conn) handleCompress(tag string, dec *imapwire.Decoder) error {
	var mechanism string
	if !dec.ExpectSP() || !dec.ExpectAtom(&mechanism) || !dec.ExpectCRLF() {
		return dec.Err()
	}

	if c.compressed {
		return &imap.Error{
			Type: imap.StatusResponseTypeBad,
			Text: "Compression already active",
		}
	}
	if !strings.EqualFold(mechanism, "DEFLATE") {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "Unsupported compression mechanism",
		}
	}

	// The OK response itself must not be compressed: compression only
	// starts after it has been flushed.
	enc := newResponseEncoder(c)
	err := writeStatusResp(enc.Encoder, tag, &imap.StatusResponse{
		Type: imap.StatusResponseTypeOK,
		Text: "Compression now active",
	})
	enc.end()
	if err != nil {
		return err
	}

	// The client may have pipelined the COMPRESS command with compressed
	// data in a single write: drain bytes already buffered by bufio and
	// feed them to the decompressor so they are not lost.
	var buf bytes.Buffer
	if _, err := io.CopyN(&buf, c.br, int64(c.br.Buffered())); err != nil {
		panic(err) // unreachable
	}

	flateReader := flate.NewReader(io.MultiReader(&buf, c.rw))
	var r io.Reader = flateReader
	if limit := c.server.options.compressMaxDecompressedBytes(); limit >= 0 {
		r = &decompressLimitReader{r: flateReader, remaining: limit}
	}

	flateWriter, err := flate.NewWriter(c.rw, flate.DefaultCompression)
	if err != nil {
		return err // unreachable
	}

	c.compressed = true
	c.br.Reset(r)
	c.bw.Reset(flateFlushWriter{w: flateWriter})
	return nil
}

// decompressLimitReader caps the number of bytes produced by the underlying
// decompressor, to protect against decompression bombs.
type decompressLimitReader struct {
	r         io.Reader
	remaining int64
}

func (lr *decompressLimitReader) Read(b []byte) (int, error) {
	if lr.remaining <= 0 {
		return 0, errDecompressedLimit
	}
	if int64(len(b)) > lr.remaining {
		b = b[:lr.remaining]
	}
	n, err := lr.r.Read(b)
	lr.remaining -= int64(n)
	return n, err
}

// flateFlushWriter flushes the DEFLATE stream after each write. The
// connection's bufio.Writer performs a single write per flush, so this
// ensures that responses (including unsolicited ones, e.g. during IDLE)
// reach the client immediately.
type flateFlushWriter struct {
	w *flate.Writer
}

func (w flateFlushWriter) Write(b []byte) (int, error) {
	n, err := w.w.Write(b)
	if err != nil {
		return n, err
	}
	if err := w.w.Flush(); err != nil {
		return n, err
	}
	return n, nil
}
