package ber

import (
	"fmt"
	"math/big"
)

// DefaultMaxIntegerBytes is the default maximum number of content octets
// accepted for a decoded INTEGER (or ENUMERATED) value. A BER encoder is
// allowed to emit arbitrarily many redundant sign-extension octets, so the
// limit bounds memory use when decoding untrusted input. Override it per
// decode with [WithMaxIntegerBytes].
const DefaultMaxIntegerBytes = 4096

// IntegerMode selects how a decoded INTEGER (or ENUMERATED) value is
// represented in [Packet.Value].
type IntegerMode int

const (
	// IntegerModeInt64 is the default: every integer is parsed into an
	// int64 and values wider than 8 content octets are rejected. This
	// preserves the historical behavior of [DecodePacketErr] and
	// [ReadPacket].
	IntegerModeInt64 IntegerMode = iota
	// IntegerModeBig represents every integer as *big.Int, including values
	// that fit in an int64.
	IntegerModeBig
	// IntegerModeAuto represents values that fit in an int64 as int64 and
	// wider values as *big.Int. "Fits in an int64" means the signed value is
	// within [-2^63, 2^63-1]; a 9-octet content encoding whose value is in
	// range (e.g. an extra leading sign byte) is still an int64.
	IntegerModeAuto
)

// DecodeOption configures decoding through [DecodePacketErrWithOptions] and
// [ReadPacketWithOptions].
type DecodeOption func(*decodeConfig)

type decodeConfig struct {
	integerMode     IntegerMode
	maxIntegerBytes int
}

func defaultDecodeConfig() decodeConfig {
	return decodeConfig{
		integerMode:     IntegerModeInt64,
		maxIntegerBytes: DefaultMaxIntegerBytes,
	}
}

// WithIntegerMode selects the Go type used for decoded INTEGER (and
// ENUMERATED) values. See [IntegerMode] for the available modes.
func WithIntegerMode(mode IntegerMode) DecodeOption {
	return func(c *decodeConfig) {
		c.integerMode = mode
	}
}

// WithMaxIntegerBytes bounds the number of content octets accepted for a
// decoded INTEGER (or ENUMERATED) value, including redundant sign-extension
// octets that BER permits. A value of 0 disables the limit.
func WithMaxIntegerBytes(n int) DecodeOption {
	return func(c *decodeConfig) {
		c.maxIntegerBytes = n
	}
}

// decodeIntegerContent parses INTEGER/ENUMERATED content under the configured
// mode and integer-byte budget.
func decodeIntegerContent(content []byte, cfg decodeConfig) (interface{}, error) {
	if cfg.maxIntegerBytes > 0 && len(content) > cfg.maxIntegerBytes {
		return nil, fmt.Errorf("integer length %d exceeds maximum %d bytes", len(content), cfg.maxIntegerBytes)
	}

	switch cfg.integerMode {
	case IntegerModeBig, IntegerModeAuto:
		v := parseBigInteger(content)
		if cfg.integerMode == IntegerModeAuto && v.IsInt64() {
			return v.Int64(), nil
		}
		return v, nil
	default:
		return ParseInt64(content)
	}
}

// parseBigInteger interprets content as a two's-complement signed integer and
// returns it as *big.Int. The result never aliases content: big.Int.SetBytes
// copies, and negative values use an intermediate copy before negating.
func parseBigInteger(content []byte) *big.Int {
	if len(content) == 0 {
		return new(big.Int)
	}

	neg := content[0]&0x80 != 0
	if !neg {
		return new(big.Int).SetBytes(content)
	}

	magnitude := make([]byte, len(content))
	copy(magnitude, content)
	for i := range magnitude {
		magnitude[i] = ^magnitude[i]
	}
	v := new(big.Int).SetBytes(magnitude)
	v.Add(v, big.NewInt(1))
	return v.Neg(v)
}

// NewBigIntegerErr builds an INTEGER packet from a *big.Int and returns an
// error if value is nil. The content is the minimal two's-complement DER
// encoding: negative values are encoded in two's complement, a leading 0x00
// octet is added to a positive value whose top bit is set, and zero is a
// single 0x00 octet. The packet does not share mutable storage with value:
// value is copied on construction and never read again, so mutating value
// afterwards leaves the packet unchanged. Use [NewBigInteger] for a panicking
// variant.
func NewBigIntegerErr(classType Class, tagType Type, tag Tag, value *big.Int, description string) (*Packet, error) {
	if value == nil {
		return nil, fmt.Errorf("invalid value <nil>, expected *big.Int")
	}

	p := Encode(classType, tagType, tag, nil, description)
	p.Value = new(big.Int).Set(value)
	p.Data.Write(encodeBigInteger(value))
	return p, nil
}

// NewBigInteger builds an INTEGER packet from a *big.Int. It panics if value
// is nil; use [NewBigIntegerErr] to receive an error instead. The packet does
// not share mutable storage with value.
func NewBigInteger(classType Class, tagType Type, tag Tag, value *big.Int, description string) *Packet {
	p, err := NewBigIntegerErr(classType, tagType, tag, value, description)
	if err != nil {
		panic(err.Error())
	}
	return p
}

// encodeBigInteger returns the minimal two's-complement big-endian encoding of
// i. The returned slice is freshly allocated and never aliases i's storage.
func encodeBigInteger(i *big.Int) []byte {
	switch i.Sign() {
	case 0:
		return []byte{0}
	case 1:
		b := i.Bytes()
		if b[0]&0x80 != 0 {
			out := make([]byte, len(b)+1)
			copy(out[1:], b)
			return out
		}
		out := make([]byte, len(b))
		copy(out, b)
		return out
	default:
		// Two's complement is ^(|i| - 1), computed on a copy so the caller's
		// value is untouched. A leading 0xff sign byte is added when the most
		// significant bit of the magnitude is clear.
		m := new(big.Int).Neg(i)
		m.Sub(m, big.NewInt(1))
		b := m.Bytes()
		if len(b) == 0 {
			return []byte{0xff}
		}
		for j := range b {
			b[j] = ^b[j]
		}
		if b[0]&0x80 == 0 {
			b = append([]byte{0xff}, b...)
		}
		return b
	}
}
