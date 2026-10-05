package hyperloglog

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// A streaming frame wraps one Sketch binary encoding so that several sketches
// can be concatenated in a single file or stream and read back one at a time.
// A frame is self-delimiting and independent of the inner encoding's internal
// length fields: the inner bytes are produced and consumed by
// MarshalBinary and UnmarshalBinary, which keep accepting every historical
// inner encoding version.
//
// The wire format of a frame is, in order, all integers big endian:
//
//	magic     [4]byte   frameMagic
//	version   uint8     outer frame version; currently frameVersion
//	length    uint32    number of payload bytes that follow the header
//	checksum  uint32    CRC-32 (IEEE) of the payload bytes only
//	payload   [length]byte
//
// The fixed header is frameHeaderSize bytes. Because length is uint32 no frame
// can hold more than 2^32-1 payload bytes; ReadFrame additionally enforces a
// caller-configured cap so a declared length cannot force an allocation before
// the bytes are available.
//
// The checksum detects accidental corruption of the payload. It is a CRC, not
// a message authentication code: it provides no integrity against an active
// adversary and must not be relied on to authenticate or sign a frame.
const (
	frameMagicLen    = 4
	frameVersionLen  = 1
	frameLengthLen   = 4
	frameChecksumLen = 4
	frameHeaderSize  = frameMagicLen + frameVersionLen + frameLengthLen + frameChecksumLen

	frameVersionPos  = frameMagicLen
	frameLengthPos   = frameVersionPos + frameVersionLen
	frameChecksumPos = frameLengthPos + frameLengthLen
)

var frameMagic = [frameMagicLen]byte{'H', 'L', 'L', 'F'}

// frameVersion is the outer framing version written by WriteFrame. It is
// independent of the inner encoding's version byte.
const frameVersion uint8 = 1

// DefaultMaxFramePayloadSize bounds the payload ReadFrame accepts unless the
// caller supplies a larger limit. The limit is a denial-of-service guard
// against a frame whose declared length is huge, not a property of the
// encoding: the largest dense Sketch (precision 18) needs 262152 payload
// bytes.
const DefaultMaxFramePayloadSize = 64 << 20

// ErrorBadMagic is returned, wrapped, when a frame does not start with the
// frame magic. It usually means the stream is not framed at all or is
// misaligned.
var ErrorBadMagic = errors.New("bad frame magic")

// ErrorFrameVersion is returned, wrapped, when the outer frame version is not
// supported. It is distinct from the inner encoding's ErrorInvalidVersion.
var ErrorFrameVersion = errors.New("unknown frame version")

// ErrorFrameTooShort is returned, wrapped, when the stream ends in the middle
// of a frame: the fixed header or the declared payload. Reaching the very end
// of a stream before a frame starts is reported as plain io.EOF instead.
var ErrorFrameTooShort = errors.New("truncated frame")

// ErrorFrameChecksum is returned, wrapped, when a complete frame was read but
// its payload's CRC-32 does not match the checksum in the header.
var ErrorFrameChecksum = errors.New("frame checksum mismatch")

// ErrorFrameTooLarge is returned, wrapped, when a frame declares more payload
// bytes than the read limit allows.
var ErrorFrameTooLarge = errors.New("frame payload too large")

var crc32Table = crc32.MakeTable(crc32.IEEE)

// WriteFrame writes one framed record holding payload to w and returns the
// total number of bytes written. payload is normally the result of
// MarshalBinary, but WriteFrame does not interpret it.
//
// The frame's fixed header and its payload are written separately, so payload
// is streamed as-is rather than copied. A Writer that accepts fewer bytes than
// requested turns into io.ErrShortWrite; the returned count is always the
// number of bytes that reached w.
func WriteFrame(w io.Writer, payload []byte) (n int64, err error) {
	if len(payload) > int(^uint32(0)) {
		return 0, fmt.Errorf("hyperloglog: frame payload of %d bytes exceeds %d: %w", len(payload), ^uint32(0), ErrorFrameTooLarge)
	}

	var header [frameHeaderSize]byte
	copy(header[0:frameMagicLen], frameMagic[:])
	header[frameVersionPos] = frameVersion
	binary.BigEndian.PutUint32(header[frameLengthPos:frameLengthPos+frameLengthLen], uint32(len(payload)))
	binary.BigEndian.PutUint32(header[frameChecksumPos:frameChecksumPos+frameChecksumLen], crc32.Checksum(payload, crc32Table))

	nh, err := writeFull(w, header[:])
	n += nh
	if err != nil {
		return n, err
	}
	np, err := writeFull(w, payload)
	n += np
	return n, err
}

// ReadFrame reads one framed record from r and returns its payload. The
// payload is only returned after its declared length has been seen, the full
// payload has arrived, and its checksum has verified; a checksum failure
// returns a nil payload.
//
// maxPayloadSize bounds the declared payload length; pass 0 to use
// DefaultMaxFramePayloadSize, and a positive value to override it. A length
// above the limit returns ErrorFrameTooLarge without consuming the payload.
//
// Cleanly reaching the end of r before the first byte returns io.EOF unwrapped,
// so callers can loop on errors.Is(err, io.EOF) over a concatenated stream.
// Ending partway through the header or payload returns ErrorFrameTooShort. A
// bad magic, unknown frame version, or checksum mismatch return ErrorBadMagic,
// ErrorFrameVersion and ErrorFrameChecksum respectively. The returned count is
// the number of bytes consumed from r; on error it counts what was read, which
// a caller can use to report the stream offset.
func ReadFrame(r io.Reader, maxPayloadSize int) (payload []byte, n int64, err error) {
	if maxPayloadSize <= 0 {
		maxPayloadSize = DefaultMaxFramePayloadSize
	}

	var header [frameHeaderSize]byte
	nh, err := io.ReadFull(r, header[:])
	n += int64(nh)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, n, io.EOF
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, n, fmt.Errorf("hyperloglog: frame header needs %d bytes, got %d: %w", frameHeaderSize, nh, ErrorFrameTooShort)
		}
		return nil, n, err
	}

	if header[0] != frameMagic[0] || header[1] != frameMagic[1] || header[2] != frameMagic[2] || header[3] != frameMagic[3] {
		return nil, n, fmt.Errorf("hyperloglog: frame magic % x, want % x: %w", header[:frameMagicLen], frameMagic[:], ErrorBadMagic)
	}

	version := header[frameVersionPos]
	if version != frameVersion {
		return nil, n, fmt.Errorf("hyperloglog: frame version %d: %w", version, ErrorFrameVersion)
	}

	length := binary.BigEndian.Uint32(header[frameLengthPos : frameLengthPos+frameLengthLen])
	if uint64(length) > uint64(maxPayloadSize) {
		return nil, n, fmt.Errorf("hyperloglog: frame payload declares %d bytes, limit %d: %w", length, maxPayloadSize, ErrorFrameTooLarge)
	}

	payload = make([]byte, length)
	np, err := io.ReadFull(r, payload)
	n += int64(np)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			// io.ReadFull returns EOF when zero payload bytes were read and
			// ErrUnexpectedEOF when some but not all were read. Both mean the
			// stream ended before the declared payload, so both are a
			// truncated frame; only a genuinely zero-length payload is legal,
			// which takes the successful path because ReadFull returns nil.
			return nil, n, fmt.Errorf("hyperloglog: frame payload declares %d bytes, got %d: %w", length, np, ErrorFrameTooShort)
		}
		return nil, n, err
	}

	want := binary.BigEndian.Uint32(header[frameChecksumPos : frameChecksumPos+frameChecksumLen])
	if got := crc32.Checksum(payload, crc32Table); got != want {
		return nil, n, fmt.Errorf("hyperloglog: frame checksum %08x, want %08x: %w", got, want, ErrorFrameChecksum)
	}

	return payload, n, nil
}

// WriteTo encodes sk with MarshalBinary and writes it as one framed record. It
// implements io.WriterTo and returns the number of wire bytes written, header
// included. A Writer that reports a short write returns io.ErrShortWrite.
func (sk *Sketch) WriteTo(w io.Writer) (int64, error) {
	payload, err := sk.MarshalBinary()
	if err != nil {
		return 0, err
	}
	return WriteFrame(w, payload)
}

// ReadFrom reads one framed record from r and replaces sk with the sketch in
// its payload. It implements io.ReaderFrom and rejects payloads larger than
// DefaultMaxFramePayloadSize; use ReadFrame followed by UnmarshalBinary to
// apply a different limit.
//
// The receiver is only replaced after the frame's checksum has verified and
// the inner encoding has fully parsed into a fresh sketch, so a truncated
// frame, a checksum mismatch, or an invalid inner encoding all leave sk
// unchanged. The inner encoding is handed to UnmarshalBinary unchanged, which
// accepts historical inner versions, so framing does not change what sketches
// can be read.
//
// Reaching the end of r before a frame begins returns io.EOF.
func (sk *Sketch) ReadFrom(r io.Reader) (int64, error) {
	payload, n, err := ReadFrame(r, 0)
	if err != nil {
		return n, err
	}

	var decoded Sketch
	if err := decoded.UnmarshalBinary(payload); err != nil {
		return n, err
	}
	*sk = decoded
	return n, nil
}

// writeFull writes all of p to w. io.Writer permits Write to return
// n < len(p) with a nil error, in which case the remaining bytes are written
// again; a Write that makes no progress at all is io.ErrShortWrite.
func writeFull(w io.Writer, p []byte) (int64, error) {
	var n int64
	for len(p) > 0 {
		nw, err := w.Write(p)
		n += int64(nw)
		if err != nil {
			return n, err
		}
		if nw == 0 || nw > len(p) {
			return n, io.ErrShortWrite
		}
		p = p[nw:]
	}
	return n, nil
}
