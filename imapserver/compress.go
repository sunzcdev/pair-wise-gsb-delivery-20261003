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

// maxDecompressedSize is the maximum total amount of decompressed data a
// client may send on a connection once COMPRESS=DEFLATE is enabled. It
// protects the server against decompression bombs. It's a variable so that
// tests can lower it.
var maxDecompressedSize int64 = 1 << 30 // 1 GiB

var errDecompressedSizeExceeded = errors.New("imapserver: maximum decompressed size exceeded")

// canCompress reports whether the COMPRESS command can be used on this
// connection. COMPRESS is available in any state (including before
// authentication and after STARTTLS), but only once per connection.
func (c *Conn) canCompress() bool {
	return !c.compressed
}

func (c *Conn) handleCompress(tag string, dec *imapwire.Decoder) error {
	var mechanism string
	if !dec.ExpectSP() || !dec.ExpectAtom(&mechanism) || !dec.ExpectCRLF() {
		return dec.Err()
	}

	if !strings.EqualFold(mechanism, "DEFLATE") {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Text: "Unsupported compression algorithm",
		}
	}
	if !c.canCompress() {
		return &imap.Error{
			Type: imap.StatusResponseTypeBad,
			Text: "Compression is already active",
		}
	}

	// Do not allow to write uncompressed data past this point: keep
	// c.encMutex locked until the end
	enc := newResponseEncoder(c)
	defer enc.end()

	// The tagged OK response itself is sent uncompressed: compression only
	// begins after it has been flushed.
	err := writeStatusResp(enc.Encoder, tag, &imap.StatusResponse{
		Type: imap.StatusResponseTypeOK,
		Text: "DEFLATE compression active",
	})
	if err != nil {
		return err
	}

	// Drain cleartext data buffered by our bufio.Reader: these bytes have
	// been read from the network before compression was enabled and must be
	// prepended to the compressed stream.
	var buf bytes.Buffer
	if _, err := io.CopyN(&buf, c.br, int64(c.br.Buffered())); err != nil {
		panic(err) // unreachable
	}

	c.mutex.Lock()
	conn := c.conn
	c.mutex.Unlock()
	rw := c.server.options.wrapReadWriter(conn)

	flateReader := flate.NewReader(io.MultiReader(&buf, rw))
	c.br.Reset(&decompressReader{r: flateReader})

	flateWriter, err := flate.NewWriter(rw, flate.DefaultCompression)
	if err != nil {
		panic(err) // unreachable
	}
	c.bw.Reset(&compressWriter{w: flateWriter})

	c.compressed = true
	c.flateWriter = flateWriter

	return nil
}

// closeCompress terminates the DEFLATE stream, if any. It must be called
// before the underlying connection is closed.
func (c *Conn) closeCompress() {
	c.encMutex.Lock()
	defer c.encMutex.Unlock()
	if c.flateWriter != nil {
		c.flateWriter.Close()
		c.flateWriter = nil
	}
}

// decompressReader inflates a DEFLATE stream and enforces a limit on the
// total amount of decompressed data.
type decompressReader struct {
	r io.Reader
	n int64
}

func (r *decompressReader) Read(p []byte) (int, error) {
	if r.n >= maxDecompressedSize {
		return 0, errDecompressedSizeExceeded
	}
	n, err := r.r.Read(p)
	r.n += int64(n)
	if err == io.ErrUnexpectedEOF {
		// The DEFLATE stream has no final block: a clean connection close
		// surfaces as an unexpected EOF. Translate it to a regular EOF.
		err = io.EOF
	}
	return n, err
}

// compressWriter deflates data and flushes the compressor after each write,
// so that responses (including unilateral updates) are sent to the network
// immediately.
type compressWriter struct {
	w *flate.Writer
}

func (w *compressWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if err != nil {
		return n, err
	}
	return n, w.w.Flush()
}
