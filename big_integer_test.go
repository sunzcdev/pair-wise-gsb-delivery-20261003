package ber

import (
	"bytes"
	"math/big"
	"testing"
)

// bigPow2 returns 2^n without going through a decimal string.
func bigPow2(n uint) *big.Int {
	return new(big.Int).Lsh(big.NewInt(1), n)
}

// minIntLen returns the length of the minimal two's-complement DER content
// encoding of v, in octets.
func minIntLen(v *big.Int) int {
	switch v.Sign() {
	case 0:
		return 1
	case 1:
		// The value occupies ceil(n/8) octets, plus a leading 0x00 sign
		// octet when the most significant bit is set.
		n := v.BitLen()
		l := (n + 7) / 8
		if n%8 == 0 {
			l++
		}
		return l
	default:
		// v fits in l octets when v >= -2^(8l-1). A magnitude that is a
		// power of two reaches the bound directly; otherwise one more bit is
		// needed.
		m := new(big.Int).Neg(v)
		n := m.BitLen()
		// A magnitude that is a power of two satisfies m & (m - 1) == 0.
		if new(big.Int).And(m, new(big.Int).Sub(m, big.NewInt(1))).Sign() == 0 {
			return (n + 7) / 8
		}
		return (n + 8) / 8
	}
}

func TestNewBigIntegerErrNil(t *testing.T) {
	if _, err := NewBigIntegerErr(ClassUniversal, TypePrimitive, TagInteger, nil, ""); err == nil {
		t.Fatal("expected error for nil *big.Int")
	}

	defer func() {
		if recover() == nil {
			t.Fatal("NewBigInteger should panic on nil")
		}
	}()
	NewBigInteger(ClassUniversal, TypePrimitive, TagInteger, nil, "")
}

func TestBigIntegerCanonicalEncoding(t *testing.T) {
	data := []struct {
		name string
		v    *big.Int
		e    []byte // full packet encoding, tag + length + content
	}{
		{"zero", big.NewInt(0), []byte{0x02, 0x01, 0x00}},
		{"one", big.NewInt(1), []byte{0x02, 0x01, 0x01}},
		{"127", big.NewInt(127), []byte{0x02, 0x01, 0x7f}},
		{"128", big.NewInt(128), []byte{0x02, 0x02, 0x00, 0x80}},
		{"255", big.NewInt(255), []byte{0x02, 0x02, 0x00, 0xff}},
		{"256", big.NewInt(256), []byte{0x02, 0x02, 0x01, 0x00}},
		{"minus_one", big.NewInt(-1), []byte{0x02, 0x01, 0xff}},
		{"minus_128", big.NewInt(-128), []byte{0x02, 0x01, 0x80}},
		{"minus_129", big.NewInt(-129), []byte{0x02, 0x02, 0xff, 0x7f}},
		{"minus_256", big.NewInt(-256), []byte{0x02, 0x02, 0xff, 0x00}},
		{"2^63", bigPow2(63), append([]byte{0x02, 0x09, 0x00, 0x80}, bytes.Repeat([]byte{0x00}, 7)...)},
		{"2^64", bigPow2(64), append([]byte{0x02, 0x09, 0x01}, bytes.Repeat([]byte{0x00}, 8)...)},
		{"-2^63", new(big.Int).Neg(bigPow2(63)), append([]byte{0x02, 0x08, 0x80}, bytes.Repeat([]byte{0x00}, 7)...)},
		{"-2^64", new(big.Int).Neg(bigPow2(64)), append([]byte{0x02, 0x09, 0xff, 0x00}, bytes.Repeat([]byte{0x00}, 7)...)},
		{"2^127", bigPow2(127), append([]byte{0x02, 0x11, 0x00, 0x80}, bytes.Repeat([]byte{0x00}, 15)...)},
		{"2^128", bigPow2(128), append([]byte{0x02, 0x11, 0x01}, bytes.Repeat([]byte{0x00}, 16)...)},
		{"-2^127", new(big.Int).Neg(bigPow2(127)), append([]byte{0x02, 0x10, 0x80}, bytes.Repeat([]byte{0x00}, 15)...)},
		{"-2^128", new(big.Int).Neg(bigPow2(128)), append([]byte{0x02, 0x11, 0xff, 0x00}, bytes.Repeat([]byte{0x00}, 15)...)},
	}

	for _, d := range data {
		t.Run(d.name, func(t *testing.T) {
			if got := NewBigInteger(ClassUniversal, TypePrimitive, TagInteger, d.v, "").Bytes(); !bytes.Equal(got, d.e) {
				t.Fatalf("encoding %s: got % X, want % X", d.name, got, d.e)
			}
			if got := len(d.e) - 2; got != minIntLen(d.v) {
				t.Fatalf("minIntLen sanity for %s: got %d, want %d", d.name, got, minIntLen(d.v))
			}
		})
	}
}

func TestBigIntegerRoundTripBoundaries(t *testing.T) {
	// Exponents straddling the 63-, 64- and 127-bit content boundaries.
	// Values are built with shifts/complements, never parsed from decimal.
	makeValues := func(n uint) []*big.Int {
		pos := bigPow2(n)
		neg := new(big.Int).Neg(pos)
		return []*big.Int{
			new(big.Int).Sub(pos, big.NewInt(1)), // 2^n - 1
			pos,                                  // 2^n
			new(big.Int).Add(pos, big.NewInt(1)), // 2^n + 1
			new(big.Int).Add(neg, big.NewInt(1)), // -2^n + 1
			neg,                                  // -2^n
			new(big.Int).Sub(neg, big.NewInt(1)), // -2^n - 1
		}
	}

	seen := make(map[uint]bool)
	for _, n := range []uint{62, 63, 64, 65, 126, 127, 128, 200} {
		if seen[n] {
			continue
		}
		seen[n] = true
		for _, v := range makeValues(n) {
			packet := NewBigInteger(ClassUniversal, TypePrimitive, TagInteger, v, "")
			encoded := packet.Bytes()

			// Content must be minimal two's complement.
			if got := len(encoded) - 2; got != minIntLen(v) {
				t.Fatalf("2^%d boundary value %s: content length %d, want minimal %d", n, v.Text(2), got, minIntLen(v))
			}

			for _, mode := range []IntegerMode{IntegerModeBig, IntegerModeAuto} {
				decoded, err := DecodePacketErrWithOptions(encoded, WithIntegerMode(mode))
				if err != nil {
					t.Fatalf("decode (mode %d) of boundary value near 2^%d failed: %v", mode, n, err)
				}

				var got *big.Int
				switch dv := decoded.Value.(type) {
				case *big.Int:
					got = dv
				case int64:
					// Auto mode keeps int64-range values as int64.
					if mode != IntegerModeAuto {
						t.Fatalf("boundary value near 2^%d decoded as int64 in big mode", n)
					}
					got = big.NewInt(dv)
				default:
					t.Fatalf("boundary value near 2^%d decoded as unexpected type %T", n, decoded.Value)
				}

				if got.Cmp(v) != 0 {
					t.Fatalf("round trip (mode %d) near 2^%d: got %s, want %s", mode, n, got.Text(2), v.Text(2))
				}
				if mode == IntegerModeBig {
					if _, ok := decoded.Value.(*big.Int); !ok {
						t.Fatalf("big mode near 2^%d decoded as %T, want *big.Int", n, decoded.Value)
					}
				}
			}
		}
	}
}

func TestBigIntegerNoSharedStorage(t *testing.T) {
	original := bigPow2(130)
	snapshot := new(big.Int).Set(original)

	packet, err := NewBigIntegerErr(ClassUniversal, TypePrimitive, TagInteger, original, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	encoded := packet.Bytes()
	original.SetInt64(7)
	if !bytes.Equal(packet.Bytes(), encoded) {
		t.Fatal("mutating the source *big.Int changed the encoded packet")
	}
	if stored, ok := packet.Value.(*big.Int); !ok || stored.Cmp(snapshot) != 0 {
		t.Fatalf("packet.Value changed when the source was mutated: got %v, want %s", packet.Value, snapshot)
	}

	// A decoded value must not alias the packet's raw content either.
	decoded, err := DecodePacketErrWithOptions(encoded, WithIntegerMode(IntegerModeBig))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	decodedValue := decoded.Value.(*big.Int)
	decodedValue.SetInt64(0)
	if !bytes.Equal(decoded.ByteValue, encoded[2:]) {
		t.Fatal("mutating the decoded *big.Int changed the packet's raw content")
	}
}

func TestIntegerModeAutoTypeSelection(t *testing.T) {
	// int64 range, even with an extra sign-extension octet: always int64.
	small := NewInteger(ClassUniversal, TypePrimitive, TagInteger, int64(-1), "")
	decoded, err := DecodePacketErrWithOptions(small.Bytes(), WithIntegerMode(IntegerModeAuto))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if v, ok := decoded.Value.(int64); !ok || v != -1 {
		t.Fatalf("small value decoded as %T %v, want int64 -1", decoded.Value, decoded.Value)
	}

	// 2^63-1 (fits int64) carried in 8 octets.
	maxFitting := new(big.Int).Sub(bigPow2(63), big.NewInt(1))
	decoded, err = DecodePacketErrWithOptions(
		NewBigInteger(ClassUniversal, TypePrimitive, TagInteger, maxFitting, "").Bytes(),
		WithIntegerMode(IntegerModeAuto),
	)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if v, ok := decoded.Value.(int64); !ok || v != 1<<63-1 {
		t.Fatalf("2^63-1 decoded as %T %v, want int64", decoded.Value, decoded.Value)
	}

	// 2^63 needs *big.Int even though it occupies only 9 octets.
	tooBig := NewBigInteger(ClassUniversal, TypePrimitive, TagInteger, bigPow2(63), "")
	decoded, err = DecodePacketErrWithOptions(tooBig.Bytes(), WithIntegerMode(IntegerModeAuto))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	v, ok := decoded.Value.(*big.Int)
	if !ok || v.Cmp(bigPow2(63)) != 0 {
		t.Fatalf("2^63 decoded as %T %v, want *big.Int 2^63", decoded.Value, decoded.Value)
	}

	// Big mode always yields *big.Int, even for small values.
	decoded, err = DecodePacketErrWithOptions(small.Bytes(), WithIntegerMode(IntegerModeBig))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if _, ok := decoded.Value.(*big.Int); !ok {
		t.Fatalf("big mode decoded small value as %T, want *big.Int", decoded.Value)
	}

	// Default mode stays int64 and rejects a 9-octet integer.
	decoded, err = DecodePacketErr(tooBig.Bytes())
	if err == nil {
		t.Fatalf("default mode should reject a wide integer, got %v", decoded.Value)
	}
}

func TestIntegerByteBudget(t *testing.T) {
	// Build INTEGER content carrying 10 redundant leading 0xff sign octets
	// around the value -1, which canonically needs a single 0xff octet.
	content := bytes.Repeat([]byte{0xff}, 11)
	encoded := bytes.Join([][]byte{{0x02, byte(len(content))}, content}, nil)

	// A tight budget rejects the sign-extended form, but a wider one accepts it.
	if _, err := DecodePacketErrWithOptions(encoded,
		WithIntegerMode(IntegerModeBig),
		WithMaxIntegerBytes(10),
	); err == nil {
		t.Fatal("expected integer length to exceed a 10-byte budget")
	}

	packet, err := DecodePacketErrWithOptions(encoded,
		WithIntegerMode(IntegerModeBig),
		WithMaxIntegerBytes(11),
	)
	if err != nil {
		t.Fatalf("sign-extended value within budget should decode: %v", err)
	}
	if v := packet.Value.(*big.Int); v.Cmp(big.NewInt(-1)) != 0 {
		t.Fatalf("sign-extended value decoded as %v, want -1", v)
	}

	// A zero budget disables the limit.
	if _, err := DecodePacketErrWithOptions(encoded,
		WithIntegerMode(IntegerModeBig),
		WithMaxIntegerBytes(0),
	); err != nil {
		t.Fatalf("zero budget should be unlimited, got: %v", err)
	}

	// The budget also bounds the legacy int64 mode.
	if _, err := DecodePacketErrWithOptions(encoded, WithMaxIntegerBytes(10)); err == nil {
		t.Fatal("expected int64 mode to honor the byte budget")
	}
}

// TestBigIntegerPropertyRoundTrip exercises encode then decode over values
// built from bit patterns around every byte boundary up to 200 bits, for both
// signs. All values are constructed arithmetically, without decimal text.
func TestBigIntegerPropertyRoundTrip(t *testing.T) {
	check := func(t *testing.T, v *big.Int) {
		t.Helper()
		content := encodeBigInteger(v)
		if len(content) != minIntLen(v) {
			t.Fatalf("value %s: encoded length %d, want minimal %d", v.Text(2), len(content), minIntLen(v))
		}
		back := parseBigInteger(content)
		if back.Cmp(v) != 0 {
			t.Fatalf("round trip mismatch: got %s, want %s", back.Text(2), v.Text(2))
		}

		// In-range values must agree with the legacy int64 codec byte-for-byte.
		if v.IsInt64() {
			if got := encodeInteger(v.Int64()); !bytes.Equal(got, content) {
				t.Fatalf("int64/big encoding disagree for %d: % X vs % X", v.Int64(), got, content)
			}
		}
	}

	zero := new(big.Int)
	check(t, zero)
	for n := uint(0); n <= 200; n++ {
		base := bigPow2(n)
		for _, delta := range []int64{-1, 0, 1} {
			pos := new(big.Int).Add(base, big.NewInt(delta))
			neg := new(big.Int).Neg(pos)
			check(t, pos)
			if neg.Sign() != 0 {
				check(t, neg)
			}
		}
	}
}

func TestEnumeratedAndReadPacketWithOptions(t *testing.T) {
	value := bigPow2(100)
	packet, err := NewBigIntegerErr(ClassUniversal, TypePrimitive, TagEnumerated, value, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	decoded, err := ReadPacketWithOptions(bytes.NewReader(packet.Bytes()), WithIntegerMode(IntegerModeBig))
	if err != nil {
		t.Fatalf("ReadPacketWithOptions failed: %v", err)
	}
	got, ok := decoded.Value.(*big.Int)
	if !ok || got.Cmp(value) != 0 {
		t.Fatalf("enumerated decoded as %T %v, want *big.Int", decoded.Value, decoded.Value)
	}

	// ENUMERATED also respects the byte budget.
	content := bytes.Repeat([]byte{0x00}, 9)
	raw := bytes.Join([][]byte{{0x0a, byte(len(content))}, content}, nil)
	if _, err := ReadPacketWithOptions(bytes.NewReader(raw),
		WithIntegerMode(IntegerModeBig),
		WithMaxIntegerBytes(8),
	); err == nil {
		t.Fatal("expected ENUMERATED to honor the byte budget")
	}
}

func TestIntegerModePropagatesToChildren(t *testing.T) {
	seq := NewSequence("seq")
	seq.AppendChild(NewBigInteger(ClassUniversal, TypePrimitive, TagInteger, bigPow2(80), "big"))
	seq.AppendChild(NewInteger(ClassUniversal, TypePrimitive, TagInteger, int64(5), "small"))

	decoded, err := DecodePacketErrWithOptions(seq.Bytes(), WithIntegerMode(IntegerModeAuto))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if len(decoded.Children) != 2 {
		t.Fatalf("want 2 children, got %d", len(decoded.Children))
	}
	if big, ok := decoded.Children[0].Value.(*big.Int); !ok || big.Cmp(bigPow2(80)) != 0 {
		t.Fatalf("nested big integer decoded as %T %v", decoded.Children[0].Value, decoded.Children[0].Value)
	}
	if small, ok := decoded.Children[1].Value.(int64); !ok || small != 5 {
		t.Fatalf("nested small integer decoded as %T %v, want int64 5", decoded.Children[1].Value, decoded.Children[1].Value)
	}
}
