package bitset

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// maxInt is the maximum value of the platform's int type.
const maxInt = int(^uint(0) >> 1)

var errLengthTooLarge = errors.New("bitset: declared length is too large")

// Codec encodes and decodes BitSets using an explicitly fixed binary byte
// order and an optional upper bound on logical length (in bits).
//
// Unlike the package-level LittleEndian()/BigEndian() functions, a Codec
// never touches the global configuration: the same program can safely use
// independent big-endian and little-endian Codecs concurrently. A Codec
// is safe for concurrent use after it has been created.
//
// The wire format is the same as the one used by WriteTo/ReadFrom:
//  1. uint64 length (number of bits)
//  2. ceil(length/64) uint64 words
//
// Decoding is strict: a truncated payload, a declared length larger than
// the configured maximum, or a non-zero padding (dirty) bit in the last
// word makes the whole decode fail and leaves the target BitSet unchanged.
type Codec struct {
	order   binary.ByteOrder
	maxBits uint
}

// NewCodec returns a Codec using the given byte order and maximum logical
// length in bits.
//
// order must be binary.BigEndian or binary.LittleEndian; a nil order
// defaults to binary.BigEndian to match the package default.
//
// maxBits is the largest bit length that the Codec accepts when reading or
// writing. A maxBits of 0 disables the limit.
func NewCodec(order binary.ByteOrder, maxBits uint) *Codec {
	if order == nil {
		order = binary.BigEndian
	}
	return &Codec{order: order, maxBits: maxBits}
}

// Order returns the byte order used by the Codec.
func (c *Codec) Order() binary.ByteOrder {
	return c.order
}

// MaxBits returns the maximum logical length (in bits) accepted by the
// Codec. A value of 0 means the limit is disabled.
func (c *Codec) MaxBits() uint {
	return c.maxBits
}

// Write writes the BitSet to a stream using the Codec's byte order.
//
// If the Codec has a maximum length and the BitSet is longer than it, an
// error is returned and nothing is written. Upon success, the number of
// bytes written is returned.
func (c *Codec) Write(stream io.Writer, b *BitSet) (int64, error) {
	if c.maxBits != 0 && b.length > c.maxBits {
		return 0, fmt.Errorf("bitset: length %d exceeds codec maximum %d", b.length, c.maxBits)
	}
	length := uint64(b.length)
	if err := binary.Write(stream, c.order, &length); err != nil {
		return 0, err
	}
	if err := writeUint64Array(stream, c.order, b.set[:b.wordCount()]); err != nil {
		// Upon failure, we do not guarantee the number of bytes written.
		return int64(wordBytes), err
	}
	return int64(b.BinaryStorageSize()), nil
}

// Read reads one BitSet from a stream using the Codec's byte order.
//
// Only the bytes belonging to the BitSet are consumed; any trailing bytes
// remain in the stream and are left for the caller. This makes it possible
// to read a single BitSet from a concatenated data stream.
//
// The decode is atomic: if the input is truncated, the declared length
// exceeds the Codec maximum, or the last word contains non-zero padding
// bits, the target BitSet is left unchanged and an error is returned.
// Upon success, the number of bytes read is returned.
func (c *Codec) Read(stream io.Reader, b *BitSet) (int64, error) {
	var header [wordBytes]byte
	if _, err := io.ReadFull(stream, header[:]); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, err
	}

	newlength, err := c.checkLength(c.order.Uint64(header[:]))
	if err != nil {
		return 0, err
	}

	nWords := wordsNeeded(newlength)
	required, ok := payloadBytesNeeded(nWords)
	if !ok {
		return 0, errLengthTooLarge
	}

	set := make([]uint64, nWords)
	if err := readUint64Array(stream, c.order, set); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, err
	}

	if err := checkPaddingBits(newlength, set); err != nil {
		return 0, err
	}

	b.set = set
	b.length = newlength
	return int64(wordBytes + required), nil
}

// Marshal encodes a BitSet into a byte slice using the Codec's byte order.
func (c *Codec) Marshal(b *BitSet) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := c.Write(&buf, b); err != nil {
		return []byte{}, err
	}
	return buf.Bytes(), nil
}

// Unmarshal decodes exactly one BitSet from a byte slice using the Codec's
// byte order.
//
// The slice must contain precisely one serialized BitSet: a truncated
// payload or extra trailing bytes make the decode fail, just like an
// over-long declared length or non-zero padding bits. On error the target
// BitSet is left unchanged.
func (c *Codec) Unmarshal(data []byte, b *BitSet) error {
	if len(data) < wordBytes {
		return io.ErrUnexpectedEOF
	}

	newlength, err := c.checkLength(c.order.Uint64(data[:wordBytes]))
	if err != nil {
		return err
	}

	nWords := wordsNeeded(newlength)
	payload, ok := payloadBytesNeeded(nWords)
	if !ok {
		return errLengthTooLarge
	}
	required := wordBytes + payload
	if len(data) != required {
		return fmt.Errorf("bitset: unmarshalling error: expected %d bytes, got %d", required, len(data))
	}

	set := make([]uint64, nWords)
	body := data[wordBytes:]
	for i := range set {
		set[i] = c.order.Uint64(body[wordBytes*i:])
	}

	if err := checkPaddingBits(newlength, set); err != nil {
		return err
	}

	b.set = set
	b.length = newlength
	return nil
}

// checkLength validates the declared logical length read from the header.
func (c *Codec) checkLength(length uint64) (uint, error) {
	newlength := uint(length)
	if uint64(newlength) != length {
		return 0, errors.New("bitset: unmarshalling error: type mismatch")
	}
	if c.maxBits != 0 && newlength > c.maxBits {
		return 0, fmt.Errorf("bitset: declared length %d exceeds codec maximum %d", newlength, c.maxBits)
	}
	return newlength, nil
}

// payloadBytesNeeded returns the number of payload (word) bytes for nWords
// words, or ok == false if the requirement would overflow the platform's
// addressable int range.
func payloadBytesNeeded(nWords int) (int, bool) {
	if nWords > (maxInt-wordBytes)/wordBytes {
		return 0, false
	}
	return nWords * wordBytes, true
}

// checkPaddingBits verifies that the bits beyond the logical length in the
// last word are all zero.
func checkPaddingBits(length uint, set []uint64) error {
	if length%wordSize != 0 && len(set) > 0 {
		validBits := length & wordMask
		if set[len(set)-1]&(allBits<<validBits) != 0 {
			return errors.New("bitset: unmarshalling error: non-zero padding bits")
		}
	}
	return nil
}
