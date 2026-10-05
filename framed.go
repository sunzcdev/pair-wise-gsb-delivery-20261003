package hyperloglog

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// Frame layout: a fixed 13 byte header followed by the payload.
//
//	bytes 0:4   magic "HLLF"
//	byte  4     frame format version, currently 1
//	bytes 5:9   payload length, uint32 big endian
//	bytes 9:13  CRC-32 (IEEE) checksum of the payload, uint32 big endian
//	bytes 13:   payload, a Sketch as produced by MarshalBinary
//
// Frames are self-delimiting, so any number of them can be concatenated in
// one file or stream and read back one Sketch at a time with ReadFrom.
const (
	frameMagic     = "HLLF"
	frameVersion   = 1
	frameHeaderLen = 4 + 1 + 4 + 4
)

// MaxFramePayloadSize bounds the payload length ReadFrom accepts. A frame
// declaring a larger payload is rejected with ErrorFrameTooLarge before any
// payload bytes are read, so a corrupt or hostile length prefix cannot cause
// a huge allocation. It is a variable so applications with tighter budgets
// can lower it; the largest legitimate payload (a dense precision 18 sketch)
// is well under 1 MiB.
var MaxFramePayloadSize = 64 << 20

// ErrorBadMagic is returned by ReadFrom when the stream does not start with
// the frame magic, meaning the stream is not a framed Sketch or the reader
// is misaligned.
var ErrorBadMagic = errors.New("bad frame magic")

// ErrorUnknownFrameVersion is returned by ReadFrom when the frame version
// byte is not one this package understands.
var ErrorUnknownFrameVersion = errors.New("unknown frame version")

// ErrorFrameTooLarge is returned by ReadFrom when the declared payload length
// exceeds MaxFramePayloadSize.
var ErrorFrameTooLarge = errors.New("frame payload too large")

// ErrorTruncatedFrame is returned by ReadFrom when the stream ends before
// the header or the declared payload is complete.
var ErrorTruncatedFrame = errors.New("truncated frame")

// ErrorChecksumMismatch is returned by ReadFrom when the payload's CRC-32
// does not match the header. The checksum detects accidental corruption
// (bit rot, torn writes, truncation that happens to land on a frame
// boundary); it is not a cryptographic authenticator and cannot detect
// deliberate tampering.
var ErrorChecksumMismatch = errors.New("frame checksum mismatch")

// WriteTo writes sk as one length-prefixed, checksummed frame to w and
// returns the number of bytes consumed from the stream, i.e. the full frame
// size (header plus payload). It implements io.WriterTo, and frames written
// this way can be concatenated and read back with ReadFrom.
//
// Short writes are retried until the whole frame is on the wire; WriteTo
// only returns before that if w reports an error or a zero-byte write.
func (sk *Sketch) WriteTo(w io.Writer) (int64, error) {
	payload, err := sk.MarshalBinary()
	if err != nil {
		return 0, err
	}

	frame := make([]byte, frameHeaderLen, frameHeaderLen+len(payload))
	copy(frame[0:4], frameMagic)
	frame[4] = frameVersion
	binary.BigEndian.PutUint32(frame[5:9], uint32(len(payload)))
	binary.BigEndian.PutUint32(frame[9:13], crc32.ChecksumIEEE(payload))
	frame = append(frame, payload...)

	var total int64
	for len(frame) > 0 {
		n, err := w.Write(frame)
		total += int64(n)
		frame = frame[n:]
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

// ReadFrom reads exactly one frame, as written by WriteTo, from r and
// replaces sk with its contents. It returns the number of bytes consumed
// from r. To read a stream of concatenated frames, call ReadFrom repeatedly
// until it returns io.EOF:
//
//	for {
//		var sk hyperloglog.Sketch
//		if _, err := sk.ReadFrom(r); err == io.EOF {
//			break
//		} else if err != nil {
//			return err
//		}
//		// use sk
//	}
//
// A clean end of stream before any header byte is read returns io.EOF
// unwrapped. A stream that ends mid-header or mid-payload returns an error
// wrapping ErrorTruncatedFrame; a bad magic, an unknown frame version, an
// oversized payload and a checksum mismatch are reported with ErrorBadMagic,
// ErrorUnknownFrameVersion, ErrorFrameTooLarge and ErrorChecksumMismatch
// respectively. The checksum is verified before the payload is decoded, and
// sk is only replaced once the whole frame has been validated; on any error
// sk is left unchanged.
//
// The payload is decoded with UnmarshalBinary, so every binary version
// UnmarshalBinary accepts is also accepted inside a frame.
func (sk *Sketch) ReadFrom(r io.Reader) (int64, error) {
	var header [frameHeaderLen]byte
	n, err := io.ReadFull(r, header[:])
	total := int64(n)
	if err != nil {
		if total == 0 && errors.Is(err, io.EOF) {
			return 0, io.EOF
		}
		return total, fmt.Errorf("hyperloglog: frame header needs %d bytes, have %d: %w", frameHeaderLen, total, ErrorTruncatedFrame)
	}

	if string(header[0:4]) != frameMagic {
		return total, fmt.Errorf("hyperloglog: magic %q, want %q: %w", header[0:4], frameMagic, ErrorBadMagic)
	}
	if v := header[4]; v != frameVersion {
		return total, fmt.Errorf("hyperloglog: frame version %d: %w", v, ErrorUnknownFrameVersion)
	}
	size := binary.BigEndian.Uint32(header[5:9])
	if size > uint32(MaxFramePayloadSize) {
		return total, fmt.Errorf("hyperloglog: payload of %d bytes exceeds limit of %d: %w", size, MaxFramePayloadSize, ErrorFrameTooLarge)
	}

	payload := make([]byte, size)
	n, err = io.ReadFull(r, payload)
	total += int64(n)
	if err != nil {
		return total, fmt.Errorf("hyperloglog: payload needs %d bytes, have %d: %w", size, n, ErrorTruncatedFrame)
	}

	if got, want := crc32.ChecksumIEEE(payload), binary.BigEndian.Uint32(header[9:13]); got != want {
		return total, fmt.Errorf("hyperloglog: checksum %#08x, want %#08x: %w", got, want, ErrorChecksumMismatch)
	}

	// UnmarshalBinary fully validates the payload before mutating sk, so the
	// receiver is still untouched if this fails.
	if err := sk.UnmarshalBinary(payload); err != nil {
		return total, err
	}
	return total, nil
}
