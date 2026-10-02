package ber

import (
	"bytes"
	"math/big"
	"testing"
)

func pow2(n uint) *big.Int {
	return new(big.Int).Lsh(big.NewInt(1), n)
}

func TestEncodeBigInteger(t *testing.T) {
	tests := []struct {
		value *big.Int
		want  []byte
	}{
		{big.NewInt(0), []byte{0x00}},
		{big.NewInt(1), []byte{0x01}},
		{big.NewInt(127), []byte{0x7F}},
		{big.NewInt(128), []byte{0x00, 0x80}},
		{big.NewInt(255), []byte{0x00, 0xFF}},
		{big.NewInt(256), []byte{0x01, 0x00}},
		{big.NewInt(-1), []byte{0xFF}},
		{big.NewInt(-127), []byte{0x81}},
		{big.NewInt(-128), []byte{0x80}},
		{big.NewInt(-129), []byte{0xFF, 0x7F}},
		{big.NewInt(-256), []byte{0xFF, 0x00}},
		{big.NewInt(-32768), []byte{0x80, 0x00}},
		{big.NewInt(-32769), []byte{0xFF, 0x7F, 0xFF}},
		// 2^63: high bit of the 8-byte magnitude is set, so a sign byte is added.
		{pow2(63), []byte{0x00, 0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}},
		// -2^63 fits exactly in 8 bytes of two's complement.
		{new(big.Int).Neg(pow2(63)), []byte{0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}},
		// -(2^63)-1 needs a 0xFF sign-extension byte.
		{new(big.Int).Sub(new(big.Int).Neg(pow2(63)), big.NewInt(1)),
			[]byte{0xFF, 0x7F, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}},
		// 2^127: 16 magnitude bytes plus a sign byte.
		{pow2(127), append([]byte{0x00, 0x80}, make([]byte, 15)...)},
		// -2^127 fits exactly in 16 bytes.
		{new(big.Int).Neg(pow2(127)), append([]byte{0x80}, make([]byte, 15)...)},
	}

	for _, tc := range tests {
		got := encodeBigInteger(tc.value)
		if !bytes.Equal(got, tc.want) {
			t.Errorf("encodeBigInteger(%v) = %x, want %x", tc.value, got, tc.want)
			continue
		}
		// The encoding must round-trip back to the same value.
		back, err := ParseBigInt(got)
		if err != nil {
			t.Errorf("ParseBigInt(%x): %v", got, err)
			continue
		}
		if back.Cmp(tc.value) != 0 {
			t.Errorf("ParseBigInt(encodeBigInteger(%v)) = %v", tc.value, back)
		}
	}
}

func TestBigIntegerRoundTrip(t *testing.T) {
	defer func(m IntegerDecodeMode) { IntegerDecoding = m }(IntegerDecoding)
	IntegerDecoding = IntegerDecodeBig

	one := big.NewInt(1)
	var values []*big.Int
	// Cross the 63/64-bit (int64) and 127/128-bit boundaries in both
	// directions.
	for _, bits := range []uint{1, 7, 8, 62, 63, 64, 65, 126, 127, 128, 129} {
		p := pow2(bits)
		values = append(values,
			new(big.Int).Set(p),                        // 2^bits
			new(big.Int).Sub(p, one),                   // 2^bits - 1
			new(big.Int).Add(p, one),                   // 2^bits + 1
			new(big.Int).Neg(p),                        // -2^bits
			new(big.Int).Neg(new(big.Int).Sub(p, one)), // -(2^bits - 1)
			new(big.Int).Neg(new(big.Int).Add(p, one)), // -(2^bits + 1)
		)
	}
	values = append(values, big.NewInt(0))

	for _, v := range values {
		pkt := NewBigInteger(ClassUniversal, TypePrimitive, TagInteger, v, "big")
		encoded := pkt.Bytes()

		decoded, err := DecodePacketErr(encoded)
		if err != nil {
			t.Fatalf("DecodePacketErr(%x): %v", encoded, err)
		}
		got, ok := decoded.Value.(*big.Int)
		if !ok {
			t.Fatalf("value %v: decoded Value is %T, want *big.Int", v, decoded.Value)
		}
		if got.Cmp(v) != 0 {
			t.Errorf("round trip of %v gave %v", v, got)
		}
		// Re-encoding must reproduce the identical (minimal) encoding.
		if out := decoded.Bytes(); !bytes.Equal(out, encoded) {
			t.Errorf("value %v: re-encode = %x, want %x", v, out, encoded)
		}
	}
}

func TestBigIntegerNoSharedStorage(t *testing.T) {
	original := pow2(100)
	pkt := NewBigInteger(ClassUniversal, TypePrimitive, TagInteger, original, "big")
	encoded := pkt.Bytes()

	// Mutating the caller's big.Int must not affect the packet.
	original.SetInt64(0)
	if out := pkt.Bytes(); !bytes.Equal(out, encoded) {
		t.Errorf("packet bytes changed after mutating input: %x, want %x", out, encoded)
	}
	if v := pkt.Value.(*big.Int); v.Cmp(pow2(100)) != 0 {
		t.Errorf("packet Value changed after mutating input: %v", v)
	}

	// Mutating the packet's Value must not affect the caller's big.Int.
	pkt.Value.(*big.Int).SetInt64(7)
	if original.Cmp(big.NewInt(0)) != 0 {
		t.Errorf("input changed after mutating packet Value: %v", original)
	}

	// Decode side: the decoded big.Int must not share storage with the
	// packet's content buffer.
	defer func(m IntegerDecodeMode) { IntegerDecoding = m }(IntegerDecoding)
	IntegerDecoding = IntegerDecodeBig

	decoded, err := DecodePacketErr(encoded)
	if err != nil {
		t.Fatalf("DecodePacketErr: %v", err)
	}
	decoded.Value.(*big.Int).SetInt64(0)
	if out := decoded.Bytes(); !bytes.Equal(out, encoded) {
		t.Errorf("decoded packet bytes changed after mutating Value: %x, want %x", out, encoded)
	}
}

func TestIntegerDecodeModes(t *testing.T) {
	defer func(m IntegerDecodeMode) { IntegerDecoding = m }(IntegerDecoding)

	small := NewInteger(ClassUniversal, TypePrimitive, TagInteger, int64(42), "small").Bytes()
	large := NewBigInteger(ClassUniversal, TypePrimitive, TagInteger, pow2(63), "large").Bytes()
	// A 10-byte sign-extended encoding of -129.
	extended := []byte{0x02, 0x0A, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x7F}

	// Default int64 mode: unchanged behavior, oversized content is rejected.
	if _, err := DecodePacketErr(large); err == nil {
		t.Error("int64 mode: expected error for 9-byte INTEGER")
	}
	p, err := DecodePacketErr(small)
	if err != nil {
		t.Fatalf("int64 mode: %v", err)
	}
	if v, ok := p.Value.(int64); !ok || v != 42 {
		t.Errorf("int64 mode: Value = %v (%T), want int64(42)", p.Value, p.Value)
	}

	// Big mode: every INTEGER decodes as *big.Int.
	IntegerDecoding = IntegerDecodeBig
	p, err = DecodePacketErr(small)
	if err != nil {
		t.Fatalf("big mode: %v", err)
	}
	if v, ok := p.Value.(*big.Int); !ok || v.Cmp(big.NewInt(42)) != 0 {
		t.Errorf("big mode: Value = %v (%T), want *big.Int(42)", p.Value, p.Value)
	}
	p, err = DecodePacketErr(large)
	if err != nil {
		t.Fatalf("big mode: %v", err)
	}
	if v, ok := p.Value.(*big.Int); !ok || v.Cmp(pow2(63)) != 0 {
		t.Errorf("big mode: Value = %v (%T), want *big.Int(2^63)", p.Value, p.Value)
	}

	// Auto mode: int64 when the value fits, *big.Int otherwise, regardless of
	// encoding length.
	IntegerDecoding = IntegerDecodeAuto
	p, err = DecodePacketErr(small)
	if err != nil {
		t.Fatalf("auto mode: %v", err)
	}
	if v, ok := p.Value.(int64); !ok || v != 42 {
		t.Errorf("auto mode: Value = %v (%T), want int64(42)", p.Value, p.Value)
	}
	p, err = DecodePacketErr(large)
	if err != nil {
		t.Fatalf("auto mode: %v", err)
	}
	if v, ok := p.Value.(*big.Int); !ok || v.Cmp(pow2(63)) != 0 {
		t.Errorf("auto mode: Value = %v (%T), want *big.Int(2^63)", p.Value, p.Value)
	}
	// Sign-extended encoding of a small value still yields int64.
	p, err = DecodePacketErr(extended)
	if err != nil {
		t.Fatalf("auto mode extended: %v", err)
	}
	if v, ok := p.Value.(int64); !ok || v != -129 {
		t.Errorf("auto mode extended: Value = %v (%T), want int64(-129)", p.Value, p.Value)
	}
}

func TestParseBigIntBudget(t *testing.T) {
	defer func(n int) { MaxIntegerBits = n }(MaxIntegerBits)
	defer func(m IntegerDecodeMode) { IntegerDecoding = m }(IntegerDecoding)
	MaxIntegerBits = 64

	// A long run of sign-extension bytes exceeds the bit budget even though
	// the represented value (-1) is tiny.
	padded := make([]byte, 16)
	for i := range padded {
		padded[i] = 0xFF
	}
	if _, err := ParseBigInt(padded); err == nil {
		t.Error("expected error for content exceeding MaxIntegerBits")
	}
	if _, err := ParseBigInt([]byte{0x7F}); err != nil {
		t.Errorf("unexpected error for content within budget: %v", err)
	}

	IntegerDecoding = IntegerDecodeBig
	pkt := append([]byte{0x02, byte(len(padded))}, padded...)
	if _, err := DecodePacketErr(pkt); err == nil {
		t.Error("expected decode error for INTEGER exceeding MaxIntegerBits")
	}

	// A zero budget disables the limit.
	MaxIntegerBits = 0
	if _, err := ParseBigInt(padded); err != nil {
		t.Errorf("unexpected error with unlimited budget: %v", err)
	}
}

func TestNewBigIntegerNil(t *testing.T) {
	if _, err := NewBigIntegerErr(ClassUniversal, TypePrimitive, TagInteger, nil, "nil"); err == nil {
		t.Error("expected error for nil *big.Int")
	}
	if _, err := NewIntegerErr(ClassUniversal, TypePrimitive, TagInteger, (*big.Int)(nil), "nil"); err == nil {
		t.Error("expected error for nil *big.Int")
	}
	defer func() {
		if recover() == nil {
			t.Error("expected panic from NewBigInteger with nil value")
		}
	}()
	NewBigInteger(ClassUniversal, TypePrimitive, TagInteger, nil, "nil")
}
