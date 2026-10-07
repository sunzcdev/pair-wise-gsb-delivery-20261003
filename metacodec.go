package rpc2

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"io"
	"sync"
)

// MetadataCodec is a Codec that carries a bounded per-request metadata area
// alongside the regular request header and body. Responses never carry
// metadata. Codecs implementing this interface are detected automatically by
// the client and server; plain Codec implementations (such as NewGobCodec)
// keep working unchanged.
type MetadataCodec interface {
	Codec

	// ReadRequestMetadata returns the metadata decoded from the current
	// request header. It is called after ReadHeader and before
	// ReadRequestBody. The returned map is a fresh copy owned by the call.
	ReadRequestMetadata() (Metadata, error)

	// WriteRequestWithMetadata is like Codec.WriteRequest but also sends the
	// per-request metadata. It must reject metadata that violates
	// MaxMetadataKeys or MaxMetadataLen before touching the connection.
	WriteRequestWithMetadata(r *Request, body interface{}, md Metadata) error
}

// maxHeaderFrameLen is the hard upper bound for the on-wire header frame.
// It keeps the encoded metadata (plus the small header envelope) inside
// MaxMetadataLen plus a fixed margin. A larger declared length means the peer
// is not speaking the protocol and terminates the connection.
const maxHeaderFrameLen = MaxMetadataLen + 4096

// metaHeader is the first gob-encoded frame of every message.
type metaHeader struct {
	Seq     uint64
	Method  string
	Error   string
	MetaLen uint32
	Meta    []byte
	HasMeta bool
}

// NewMetadataCodec returns a MetadataCodec that frames each message as two
// length-prefixed gob values: a header (seq/method/error plus bounded
// metadata) followed by the request or response body.
func NewMetadataCodec(conn io.ReadWriteCloser) MetadataCodec {
	return &metaCodec{rwc: conn}
}

type metaCodec struct {
	rwc io.ReadWriteCloser

	writeMu sync.Mutex

	readMu sync.Mutex
	header metaHeader
	md     Metadata
	mdErr  error
}

// writeFrame writes one length-prefixed gob blob.
func (c *metaCodec) writeFrame(buf *bytes.Buffer, v interface{}) error {
	buf.Reset()
	enc := gob.NewEncoder(buf)
	if err := enc.Encode(v); err != nil {
		return err
	}
	var lenBuf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(lenBuf[:], uint64(buf.Len()))
	if _, err := c.rwc.Write(lenBuf[:n]); err != nil {
		return err
	}
	if _, err := c.rwc.Write(buf.Bytes()); err != nil {
		return err
	}
	return nil
}

func (c *metaCodec) writeMessage(h *metaHeader, body interface{}) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	var buf bytes.Buffer
	if err := c.writeFrame(&buf, h); err != nil {
		return err
	}
	// The body frame may be nil for notifications/error replies that carry
	// no payload. gob cannot encode nil interface, so encode an empty struct.
	if body == nil {
		body = struct{}{}
	}
	if err := c.writeFrame(&buf, body); err != nil {
		return err
	}
	return nil
}

// readFrame reads one length-prefixed blob, rejecting frames whose declared
// size exceeds limit (0 means unlimited). The returned buffer is owned by the
// caller.
func (c *metaCodec) readFrame(limit int) ([]byte, error) {
	length, err := binary.ReadUvarint(newByteReader(c.rwc))
	if err != nil {
		return nil, err
	}
	if limit > 0 && length > uint64(limit) {
		// Consume and discard the oversized blob so the stream stays aligned
		// for diagnostics, then report a protocol error that tears down the
		// connection: no trusted message can follow a malicious header.
		if _, err := io.CopyN(io.Discard, c.rwc, int64(length)); err != nil {
			return nil, err
		}
		return nil, ErrMetadataTooLarge
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(c.rwc, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// uvarintReader adapts an io.Reader to binary.ByteReader.
type byteReader struct{ r io.Reader }

func newByteReader(r io.Reader) io.ByteReader {
	if br, ok := r.(io.ByteReader); ok {
		return br
	}
	return &byteReader{r: r}
}

func (x *byteReader) ReadByte() (byte, error) {
	var b [1]byte
	_, err := io.ReadFull(x.r, b[:])
	return b[0], err
}

func (c *metaCodec) ReadHeader(req *Request, resp *Response) error {
	c.readMu.Lock()
	defer c.readMu.Unlock()

	c.md = nil
	c.mdErr = nil
	c.header = metaHeader{}

	frame, err := c.readFrame(maxHeaderFrameLen)
	if err != nil {
		return err
	}
	if err := gob.NewDecoder(bytes.NewReader(frame)).Decode(&c.header); err != nil {
		return err
	}

	if c.header.Method != "" {
		req.Seq = c.header.Seq
		req.Method = c.header.Method
	} else {
		resp.Seq = c.header.Seq
		resp.Error = c.header.Error
	}

	c.md, c.mdErr = c.decodeMetadata()
	return nil
}

// decodeMetadata validates and decodes the metadata carried in the header.
// Length and key-count limits are enforced here, before the request body is
// read and before any handler can run.
func (c *metaCodec) decodeMetadata() (Metadata, error) {
	if !c.header.HasMeta {
		return nil, nil
	}
	if c.header.MetaLen > MaxMetadataLen {
		return nil, ErrMetadataTooLarge
	}
	if len(c.header.Meta) != int(c.header.MetaLen) {
		return nil, ErrMetadataTooLarge
	}
	var md Metadata
	if err := gob.NewDecoder(bytes.NewReader(c.header.Meta)).Decode(&md); err != nil {
		return nil, err
	}
	if len(md) > MaxMetadataKeys {
		return nil, ErrTooManyMetadataKeys
	}
	return md.clone(), nil
}

func (c *metaCodec) ReadRequestMetadata() (Metadata, error) {
	if c.mdErr != nil {
		return nil, c.mdErr
	}
	return c.md.clone(), nil
}

func (c *metaCodec) ReadRequestBody(body interface{}) error {
	return c.readBody(body)
}

func (c *metaCodec) ReadResponseBody(body interface{}) error {
	return c.readBody(body)
}

func (c *metaCodec) readBody(body interface{}) error {
	frame, err := c.readFrame(0)
	if err != nil {
		return err
	}
	if body == nil {
		return nil
	}
	return gob.NewDecoder(bytes.NewReader(frame)).Decode(body)
}

func (c *metaCodec) WriteRequest(r *Request, body interface{}) error {
	return c.WriteRequestWithMetadata(r, body, nil)
}

func (c *metaCodec) WriteRequestWithMetadata(r *Request, body interface{}, md Metadata) error {
	h := metaHeader{Seq: r.Seq, Method: r.Method}
	if md != nil {
		if len(md) > MaxMetadataKeys {
			return ErrTooManyMetadataKeys
		}
		var buf bytes.Buffer
		if err := gob.NewEncoder(&buf).Encode(md); err != nil {
			return err
		}
		if buf.Len() > MaxMetadataLen {
			return ErrMetadataTooLarge
		}
		h.HasMeta = true
		h.MetaLen = uint32(buf.Len())
		h.Meta = buf.Bytes()
	}
	return c.writeMessage(&h, body)
}

func (c *metaCodec) WriteResponse(r *Response, body interface{}) error {
	h := &metaHeader{Seq: r.Seq, Error: r.Error}
	return c.writeMessage(h, body)
}

func (c *metaCodec) Close() error {
	return c.rwc.Close()
}
