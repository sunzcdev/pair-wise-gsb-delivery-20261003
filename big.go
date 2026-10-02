package ber

import (
	"errors"
	"fmt"
	"math/big"
)

// MaxIntegerBits bounds the size, in bits, of INTEGER content accepted by
// ParseBigInt and by the IntegerDecodeBig and IntegerDecodeAuto decode modes.
// The limit applies to the raw content octets, so a value padded with a long
// run of sign-extension bytes (0x00 or 0xFF prefixes) is rejected even when
// the integer it represents is small. Set to 0 for no limit.
var MaxIntegerBits = 65536

// IntegerDecodeMode selects how the decoder represents INTEGER values in
// Packet.Value.
type IntegerDecodeMode int

const (
	// IntegerDecodeInt64 decodes INTEGER values as int64. This is the
	// default and the historical behavior; content longer than 8 bytes is
	// rejected.
	IntegerDecodeInt64 IntegerDecodeMode = iota
	// IntegerDecodeBig decodes every INTEGER value as *big.Int.
	IntegerDecodeBig
	// IntegerDecodeAuto decodes an INTEGER value as int64 when it fits in
	// one and as *big.Int otherwise. The choice depends only on the decoded
	// value, not on the encoding length: a sign-extended encoding of a
	// small value still yields an int64.
	IntegerDecodeAuto
)

// IntegerDecoding controls how INTEGER values are decoded by ReadPacket,
// DecodePacket, and DecodePacketErr. The default is IntegerDecodeInt64.
var IntegerDecoding = IntegerDecodeInt64

var bigOne = big.NewInt(1)

// ParseBigInt decodes the two's-complement content octets of an ASN.1 INTEGER
// into a big.Int. The result never shares storage with b. Content whose
// length exceeds MaxIntegerBits is rejected.
func ParseBigInt(b []byte) (*big.Int, error) {
	if MaxIntegerBits > 0 && len(b)*8 > MaxIntegerBits {
		return nil, fmt.Errorf("integer content of %d bits exceeds maximum of %d bits", len(b)*8, MaxIntegerBits)
	}

	// SetBytes copies b, so the result is independent of the caller's slice.
	ret := new(big.Int).SetBytes(b)
	if len(b) > 0 && b[0]&0x80 != 0 {
		// Negative value: subtract 2^(8*len(b)) to undo the two's-complement
		// offset.
		ret.Sub(ret, new(big.Int).Lsh(bigOne, uint(len(b)*8)))
	}
	return ret, nil
}

// encodeBigInteger returns the minimal two's-complement content octets for i:
// negative values are complement-encoded, positive values with the high bit
// set get a 0x00 sign byte, and zero is a single 0x00 byte.
func encodeBigInteger(i *big.Int) []byte {
	if i.Sign() >= 0 {
		// Bytes returns a fresh slice, empty for zero.
		b := i.Bytes()
		if len(b) == 0 {
			return []byte{0x00}
		}
		if b[0]&0x80 != 0 {
			return append([]byte{0x00}, b...)
		}
		return b
	}

	// Allocate one more byte than the magnitude needs, encode i + 2^(8*n),
	// then strip redundant 0xFF sign-extension bytes.
	n := i.BitLen()/8 + 1
	twoComplement := new(big.Int).Add(new(big.Int).Lsh(bigOne, uint(8*n)), i)
	b := twoComplement.Bytes()

	out := make([]byte, n)
	copy(out[n-len(b):], b)
	for len(out) > 1 && out[0] == 0xFF && out[1]&0x80 != 0 {
		out = out[1:]
	}
	return out
}

// decodeInteger decodes INTEGER content octets according to IntegerDecoding.
func decodeInteger(content []byte) (interface{}, error) {
	switch IntegerDecoding {
	case IntegerDecodeBig:
		return ParseBigInt(content)
	case IntegerDecodeAuto:
		v, err := ParseBigInt(content)
		if err != nil {
			return nil, err
		}
		if v.IsInt64() {
			return v.Int64(), nil
		}
		return v, nil
	default:
		return ParseInt64(content)
	}
}

// NewBigIntegerErr behaves like [NewBigInteger] but returns an error instead
// of panicking when value is nil.
func NewBigIntegerErr(classType Class, tagType Type, tag Tag, value *big.Int, description string) (*Packet, error) {
	if value == nil {
		return nil, errors.New("invalid nil *big.Int")
	}
	return NewIntegerErr(classType, tagType, tag, value, description)
}

// NewBigInteger builds an INTEGER packet from a big.Int of arbitrary
// magnitude. The packet does not share storage with value: the encoded
// content and the stored Value are independent copies. It panics if value is
// nil; use [NewBigIntegerErr] to receive an error instead.
func NewBigInteger(classType Class, tagType Type, tag Tag, value *big.Int, description string) *Packet {
	p, err := NewBigIntegerErr(classType, tagType, tag, value, description)
	if err != nil {
		panic(err.Error())
	}
	return p
}
