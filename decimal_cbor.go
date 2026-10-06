package cbor

import (
	"encoding/binary"
	"math"
	"math/big"
)

// MarshalCBOR encodes the Decimal as CBOR tag 4 wrapping [exponent,
// mantissa]. Small integers use compact CBOR integer heads; coefficients that
// do not fit are encoded as bignums (tag 2 or 3) per RFC 8949.
func (x Decimal) MarshalCBOR() ([]byte, error) {
	return marshalScaled(tagNumDecimalFraction, x.Exponent, x.Mantissa)
}

// MarshalCBOR encodes the BigFloat as CBOR tag 5 wrapping [exponent,
// mantissa].
func (x BigFloat) MarshalCBOR() ([]byte, error) {
	return marshalScaled(tagNumBigFloat, x.Exponent, x.Mantissa)
}

func marshalScaled(tag uint64, e int64, m *big.Int) ([]byte, error) {
	if m == nil {
		m = new(big.Int)
	}
	buf := make([]byte, 0, 16)
	buf = appendHead(buf, byte(cborTypeTag), tag)
	buf = appendHead(buf, byte(cborTypeArray), 2)
	buf = appendSignedInt(buf, e)
	buf = appendMantissa(buf, m)
	return buf, nil
}

func appendSignedInt(dst []byte, v int64) []byte {
	if v >= 0 {
		return appendHead(dst, byte(cborTypePositiveInt), uint64(v))
	}
	// CBOR negative int major type encodes -(n+1).
	return appendHead(dst, byte(cborTypeNegativeInt), uint64(-v-1))
}

func appendMantissa(dst []byte, m *big.Int) []byte {
	if m.IsInt64() {
		return appendSignedInt(dst, m.Int64())
	}
	// Coefficient outside int64 range: encode as a bignum byte string.
	if m.Sign() > 0 {
		dst = appendHead(dst, byte(cborTypeTag), tagNumUnsignedBignum)
	} else {
		dst = appendHead(dst, byte(cborTypeTag), tagNumNegativeBignum)
		// Tag 3 encodes -(1 + bytes).
		m = new(big.Int).Neg(m)
		m.Sub(m, big.NewInt(1))
	}
	b := m.Bytes()
	dst = appendHead(dst, byte(cborTypeByteString), uint64(len(b)))
	return append(dst, b...)
}

// UnmarshalCBOR decodes a CBOR tag 4 data item into x, preserving the
// exponent and coefficient exactly. DefaultLimits are enforced; to use
// custom limits call DecodeDecimal.
func (x *Decimal) UnmarshalCBOR(data []byte) error {
	d, err := DecodeDecimal(data, nil)
	if err != nil {
		return err
	}
	*x = d
	return nil
}

// UnmarshalCBOR decodes a CBOR tag 5 data item into x.
func (x *BigFloat) UnmarshalCBOR(data []byte) error {
	f, err := DecodeBigFloat(data, nil)
	if err != nil {
		return err
	}
	*x = f
	return nil
}

// DecodeDecimal parses a single CBOR tag 4 data item using the given limits.
// The data must be tag 4 followed by a definite two-element integer array;
// no trailing bytes are allowed. Tag 55799 (self-describe) wrappers are
// accepted.
func DecodeDecimal(data []byte, l *NumericLimits) (Decimal, error) {
	e, m, err := decodeScaled(data, tagNumDecimalFraction, l)
	if err != nil {
		return Decimal{}, err
	}
	return Decimal{Exponent: e, Mantissa: m}, nil
}

// DecodeBigFloat parses a single CBOR tag 5 data item using the given limits.
func DecodeBigFloat(data []byte, l *NumericLimits) (BigFloat, error) {
	e, m, err := decodeScaled(data, tagNumBigFloat, l)
	if err != nil {
		return BigFloat{}, err
	}
	return BigFloat{Exponent: e, Mantissa: m}, nil
}

type scaledDecoder struct {
	data []byte
	off  int
	lim  NumericLimits
}

func decodeScaled(data []byte, wantTag uint64, l *NumericLimits) (int64, *big.Int, error) {
	lim := DefaultLimits
	if l != nil {
		lim = l.orDefault()
	}
	d := &scaledDecoder{data: data, lim: lim}

	// Peel off and validate any self-describe CBOR (55799) wrappers.
	for {
		t, num, err := d.readTagHead()
		if err != nil {
			return 0, nil, err
		}
		if t != cborTypeTag {
			return 0, nil, &SyntaxError{msg: "cbor: expected tag " + itoa(wantTag)}
		}
		if num == tagNumSelfDescribedCBOR {
			continue
		}
		if num != wantTag {
			return 0, nil, &InadmissibleTagContentTypeError{
				tagNum:                 int(wantTag),
				expectedTagContentType: "tag " + itoa(wantTag),
				gotTagContentType:      "tag " + itoa(num),
			}
		}
		break
	}

	// Tag content must be a definite-length array of exactly two elements.
	t, ai, n, err := d.head()
	if err != nil {
		return 0, nil, err
	}
	if t != cborTypeArray {
		return 0, nil, &SyntaxError{msg: "cbor: tag " + itoa(wantTag) + " content must be an array"}
	}
	if ai == additionalInformationAsIndefiniteLengthFlag {
		return 0, nil, &SyntaxError{msg: "cbor: tag " + itoa(wantTag) + " array must use definite length"}
	}
	if n != 2 {
		return 0, nil, &SyntaxError{msg: "cbor: tag " + itoa(wantTag) + " array must have exactly 2 elements"}
	}

	e, err := d.readExponent()
	if err != nil {
		return 0, nil, err
	}
	if err := lim.checkStoredExponent(e); err != nil {
		return 0, nil, err
	}

	m, err := d.readMantissa()
	if err != nil {
		return 0, nil, err
	}
	if m.BitLen() > lim.MaxMantissaBits {
		return 0, nil, ErrMantissaTooLarge
	}

	if d.off != len(d.data) {
		return 0, nil, &SyntaxError{msg: "cbor: trailing data after tag " + itoa(wantTag) + " value"}
	}
	return e, m, nil
}

// readTagHead reads one tag initial byte and its number.
func (d *scaledDecoder) readTagHead() (cborType, uint64, error) {
	t, _, n, err := d.head()
	if err != nil {
		return 0, 0, err
	}
	return t, n, nil
}

func (d *scaledDecoder) head() (cborType, byte, uint64, error) {
	if d.off >= len(d.data) {
		return 0, 0, 0, &SyntaxError{msg: "cbor: unexpected end of CBOR data"}
	}
	b := d.data[d.off]
	d.off++
	t := cborType(b & 0xe0)
	ai := b & 0x1f
	if ai <= 23 {
		return t, ai, uint64(ai), nil
	}
	var n uint64
	switch ai {
	case 24:
		if d.off+1 > len(d.data) {
			return 0, 0, 0, &SyntaxError{msg: "cbor: truncated 1-byte argument"}
		}
		n = uint64(d.data[d.off])
		d.off++
	case 25:
		if d.off+2 > len(d.data) {
			return 0, 0, 0, &SyntaxError{msg: "cbor: truncated 2-byte argument"}
		}
		n = uint64(binary.BigEndian.Uint16(d.data[d.off:]))
		d.off += 2
	case 26:
		if d.off+4 > len(d.data) {
			return 0, 0, 0, &SyntaxError{msg: "cbor: truncated 4-byte argument"}
		}
		n = uint64(binary.BigEndian.Uint32(d.data[d.off:]))
		d.off += 4
	case 27:
		if d.off+8 > len(d.data) {
			return 0, 0, 0, &SyntaxError{msg: "cbor: truncated 8-byte argument"}
		}
		n = binary.BigEndian.Uint64(d.data[d.off:])
		d.off += 8
	case 31:
		return t, ai, 0, nil
	default:
		return 0, 0, 0, &SyntaxError{msg: "cbor: invalid additional information " + itoa(uint64(ai))}
	}
	return t, ai, n, nil
}

// readExponent reads an element that must be a CBOR integer (major type 0 or
// 1). Floats are rejected: an exponent such as 1e2 has no exact integer
// meaning and is inadmissible per RFC 8949.
func (d *scaledDecoder) readExponent() (int64, error) {
	t, _, n, err := d.head()
	if err != nil {
		return 0, err
	}
	switch t {
	case cborTypePositiveInt:
		if n > math.MaxInt64 {
			return 0, ErrExponentTooLarge
		}
		return int64(n), nil
	case cborTypeNegativeInt:
		v := int64(-1) - int64(n)
		if n > math.MaxInt64 {
			// Result is more negative than MinInt64.
			return 0, ErrExponentTooLarge
		}
		return v, nil
	default:
		return 0, &SyntaxError{msg: "cbor: exponent in tag 4 or 5 array must be an integer"}
	}
}

// readMantissa reads a CBOR integer or a bignum (tag 2 or 3). Other types
// are rejected.
func (d *scaledDecoder) readMantissa() (*big.Int, error) {
	t, ai, n, err := d.head()
	if err != nil {
		return nil, err
	}
	switch t {
	case cborTypePositiveInt:
		return new(big.Int).SetUint64(n), nil
	case cborTypeNegativeInt:
		bi := new(big.Int).SetUint64(n)
		bi.Add(bi, big.NewInt(1))
		bi.Neg(bi)
		return bi, nil
	case cborTypeTag:
		// n holds the tag number read by head.
		switch n {
		case tagNumUnsignedBignum, tagNumNegativeBignum:
		default:
			return nil, &SyntaxError{msg: "cbor: mantissa tag must be 2 or 3"}
		}
		bt, bai, blen, err := d.head()
		if err != nil {
			return nil, err
		}
		if bt != cborTypeByteString || bai == additionalInformationAsIndefiniteLengthFlag {
			return nil, &SyntaxError{msg: "cbor: bignum must wrap a definite byte string"}
		}
		if blen > uint64(d.lim.MaxMantissaBits/8) {
			return nil, ErrMantissaTooLarge
		}
		if d.off+int(blen) > len(d.data) {
			return nil, &SyntaxError{msg: "cbor: truncated bignum byte string"}
		}
		b := d.data[d.off : d.off+int(blen)]
		d.off += int(blen)
		bi := new(big.Int).SetBytes(b)
		if n == tagNumNegativeBignum {
			bi.Add(bi, big.NewInt(1))
			bi.Neg(bi)
		}
		return bi, nil
	default:
		_ = ai
		return nil, &SyntaxError{msg: "cbor: mantissa in tag 4 or 5 array must be an integer or bignum"}
	}
}

// itoa avoids importing strconv in this hot path; the numbers formatted are
// tiny tag numbers.
func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
