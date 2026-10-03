package bitset

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Codec serializes and deserializes BitSets with a fixed binary
// byte order and an optional limit on the number of logical bits.
//
// Unlike the package-level functions LittleEndian() and BigEndian(),
// which mutate global state shared by WriteTo/ReadFrom and the
// Marshal/Unmarshal helpers, a Codec is self-contained: creating or
// using one never changes the global configuration, and distinct
// Codecs (e.g. one big-endian and one little-endian) may be used
// concurrently without interfering with each other.
//
// The wire format is identical to the one used by BitSet.WriteTo:
// 1. uint64 length (number of logical bits)
// 2. []uint64 set, encoded with the Codec's byte order
//
// A Codec is safe for concurrent use by multiple goroutines.
type Codec struct {
	order   binary.ByteOrder
	maxBits uint
}

// NewCodec returns a Codec that encodes and decodes BitSets using
// the given byte order. maxBits is the maximum number of logical
// bits accepted when decoding: a stream declaring more bits is
// rejected before any payload is read. A maxBits of 0 disables
// the limit.
func NewCodec(order binary.ByteOrder, maxBits uint) *Codec {
	if order == nil {
		order = binary.BigEndian
	}
	return &Codec{order: order, maxBits: maxBits}
}

// Order returns the byte order the Codec was created with.
func (c *Codec) Order() binary.ByteOrder { return c.order }

// MaxBits returns the maximum number of logical bits accepted when
// decoding, or 0 if no limit was configured.
func (c *Codec) MaxBits() uint { return c.maxBits }

// checkLength validates a declared length against the Codec limits
// and returns the number of words needed to store it.
func (c *Codec) checkLength(length uint64) (uint, int, error) {
	newlength := uint(length)
	if uint64(newlength) != length {
		return 0, 0, errors.New("unmarshalling error: type mismatch")
	}
	if c.maxBits > 0 && newlength > c.maxBits {
		return 0, 0, fmt.Errorf("unmarshalling error: declared length %d exceeds codec limit of %d bits", newlength, c.maxBits)
	}
	return newlength, wordsNeeded(newlength), nil
}

// cleanTail reports whether the words hold no set bits beyond the
// declared length.
func cleanTail(words []uint64, length uint) bool {
	if rem := length & wordMask; rem != 0 && len(words) > 0 {
		return words[len(words)-1]&(allBits<<rem) == 0
	}
	return true
}

// Encode writes the BitSet to the stream using the Codec's byte
// order. The format matches BitSet.WriteTo, so data written by a
// Codec with the default order can be read by BitSet.ReadFrom and
// vice versa. Upon success, the number of bytes written is returned.
func (c *Codec) Encode(stream io.Writer, b *BitSet) (int64, error) {
	length := uint64(b.length)
	err := binary.Write(stream, c.order, &length)
	if err != nil {
		return 0, err
	}
	err = writeUint64ArrayOrder(stream, b.set[:b.wordCount()], c.order)
	if err != nil {
		return wordBytes, err
	}
	return int64(b.BinaryStorageSize()), nil
}

// Decode reads exactly one BitSet from the stream, as written by
// Encode, and stores it in b. Bytes following the BitSet in the
// stream are left for the caller: the Codec never reads past the
// end of the encoded value, so concatenated streams are supported.
//
// The declared length is validated against the Codec's bit limit
// before the payload is read. Truncated input, a length exceeding
// the limit, or dirty bits set beyond the declared length fail the
// whole decode: an error is returned and b is left unchanged.
// Upon success, the number of bytes read is returned.
func (c *Codec) Decode(stream io.Reader, b *BitSet) (int64, error) {
	var length uint64
	err := binary.Read(stream, c.order, &length)
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, err
	}
	newlength, nWords, err := c.checkLength(length)
	if err != nil {
		return 0, err
	}
	// Decode into a temporary buffer so that any failure leaves
	// the target BitSet untouched.
	words := make([]uint64, nWords)
	err = readUint64ArrayOrder(stream, words, c.order)
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, err
	}
	if !cleanTail(words, newlength) {
		return 0, errors.New("unmarshalling error: bits set beyond declared length")
	}
	b.set = words
	b.length = newlength
	return int64(wordBytes + wordBytes*nWords), nil
}

// Marshal encodes the BitSet into a new byte slice using the
// Codec's byte order. The result is identical to the one produced
// by WriteTo.
func (c *Codec) Marshal(b *BitSet) []byte {
	nWords := b.wordCount()
	buf := make([]byte, wordBytes+wordBytes*nWords)
	c.order.PutUint64(buf, uint64(b.length))
	for i, word := range b.set[:nWords] {
		c.order.PutUint64(buf[wordBytes*(i+1):], word)
	}
	return buf
}

// Unmarshal decodes one BitSet from data, as produced by Marshal,
// and stores it in b. It returns the number of bytes consumed;
// any trailing bytes in data are left to the caller, so several
// encoded BitSets may be concatenated in a single slice.
//
// The declared length and the required byte count are checked
// against the slice before decoding. Truncated data, a length
// exceeding the Codec's bit limit, or dirty bits set beyond the
// declared length fail the whole decode: an error is returned and
// b is left unchanged.
func (c *Codec) Unmarshal(data []byte, b *BitSet) (int, error) {
	if len(data) < wordBytes {
		return 0, io.ErrUnexpectedEOF
	}
	newlength, nWords, err := c.checkLength(c.order.Uint64(data))
	if err != nil {
		return 0, err
	}
	size := wordBytes + wordBytes*nWords
	if len(data) < size {
		return 0, io.ErrUnexpectedEOF
	}
	words := make([]uint64, nWords)
	for i := range words {
		words[i] = c.order.Uint64(data[wordBytes*(i+1):])
	}
	if !cleanTail(words, newlength) {
		return 0, errors.New("unmarshalling error: bits set beyond declared length")
	}
	b.set = words
	b.length = newlength
	return size, nil
}
