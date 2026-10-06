// Copyright (c) Faye Amacker. All rights reserved.
// Licensed under the MIT License. See LICENSE in the project root for license information.

package cbor

import (
	"errors"
	"math/big"
	"testing"
)

// bigFloatVector is a fixed test vector for CBOR tag 5. Mantissa and
// exponent are preserved exactly as encoded; wantRat is the exact value.
type bigFloatVector struct {
	name     string
	cborHex  string
	exponent string
	mantissa string
	wantRat  string // exact value as a fraction string accepted by big.Rat.SetString
}

var bigFloatVectors = []bigFloatVector{
	{
		name:     "RFC 8949 example 3p-1",
		cborHex:  "c5822003",
		exponent: "-1",
		mantissa: "3",
		wantRat:  "3/2",
	},
	{
		// mantissa = 2^65-1 (does not fit uint64), exponent = -64
		name:     "mantissa 2^65-1, exponent -64",
		cborHex:  "c582383fc24901ffffffffffffffff",
		exponent: "-64",
		mantissa: "36893488147419103231",
		wantRat:  "36893488147419103231/18446744073709551616",
	},
	{
		// mantissa = -(2^64)-1, exponent = 100
		name:     "negative mantissa beyond 64 bits, exponent 100",
		cborHex:  "c5821864c349010000000000000000",
		exponent: "100",
		mantissa: "-18446744073709551617",
		wantRat:  "-23384026197294446692526607923688757715991623892992",
	},
	{
		// non-normalized form: trailing zero bits in mantissa must be preserved
		name:     "non-normalized 24p-3",
		cborHex:  "c582221818",
		exponent: "-3",
		mantissa: "24",
		wantRat:  "3",
	},
}

func TestBigFloatDecodeVectors(t *testing.T) {
	for _, tc := range bigFloatVectors {
		t.Run(tc.name, func(t *testing.T) {
			data := mustHexDecode(tc.cborHex)

			var bf BigFloat
			if err := Unmarshal(data, &bf); err != nil {
				t.Fatalf("Unmarshal(0x%x) returned error %v", data, err)
			}

			wantExp := mustBigInt(tc.exponent)
			wantMant := mustBigInt(tc.mantissa)
			if bf.Exponent.Cmp(&wantExp) != 0 || bf.Mantissa.Cmp(&wantMant) != 0 {
				t.Errorf("Unmarshal(0x%x) = %v, want exponent %s mantissa %s", data, bf, tc.exponent, tc.mantissa)
			}

			r, err := bf.BigRat()
			if err != nil {
				t.Fatalf("BigRat() returned error %v", err)
			}
			wantRat, ok := new(big.Rat).SetString(tc.wantRat)
			if !ok {
				t.Fatalf("bad test vector wantRat %q", tc.wantRat)
			}
			if r.Cmp(wantRat) != 0 {
				t.Errorf("BigRat() = %s, want %s", r.RatString(), wantRat.RatString())
			}
		})
	}
}

func TestBigFloatRoundTripVectors(t *testing.T) {
	for _, tc := range bigFloatVectors {
		t.Run(tc.name, func(t *testing.T) {
			data := mustHexDecode(tc.cborHex)

			var bf BigFloat
			if err := Unmarshal(data, &bf); err != nil {
				t.Fatalf("Unmarshal(0x%x) returned error %v", data, err)
			}

			// Re-encoding must reproduce the original bytes exactly,
			// preserving the encoded (possibly non-normalized) form.
			encoded, err := bf.MarshalCBOR()
			if err != nil {
				t.Fatalf("MarshalCBOR() returned error %v", err)
			}
			if string(encoded) != string(data) {
				t.Errorf("round-trip encoding = 0x%x, want 0x%x", encoded, data)
			}

			// Value conversion must round-trip without precision loss:
			// bf -> big.Rat -> BigFloat, compared in normalized form.
			r, err := bf.BigRat()
			if err != nil {
				t.Fatalf("BigRat() returned error %v", err)
			}
			bf2, err := BigFloatFromBigRat(r)
			if err != nil {
				t.Fatalf("BigFloatFromBigRat() returned error %v", err)
			}
			n1, n2 := bf.Normalized(), bf2.Normalized()
			if n1.Exponent.Cmp(n2.Exponent) != 0 || n1.Mantissa.Cmp(n2.Mantissa) != 0 {
				t.Errorf("value round-trip = %v, want %v", n2, n1)
			}
		})
	}
}

func TestBigFloatEncodeVector(t *testing.T) {
	bf := NewBigFloat(big.NewInt(3), -1)
	data, err := bf.MarshalCBOR()
	if err != nil {
		t.Fatalf("MarshalCBOR() returned error %v", err)
	}
	want := mustHexDecode("c5822003")
	if string(data) != string(want) {
		t.Errorf("MarshalCBOR() = 0x%x, want 0x%x", data, want)
	}
}

func TestBigFloatNormalized(t *testing.T) {
	bf := BigFloat{
		Exponent: big.NewInt(-3),
		Mantissa: big.NewInt(24),
	}
	n := bf.Normalized()
	if n.Exponent.Cmp(big.NewInt(0)) != 0 || n.Mantissa.Cmp(big.NewInt(3)) != 0 {
		t.Errorf("Normalized() = %v, want exponent 0 mantissa 3", n)
	}
	// Original must be unmodified.
	if bf.Exponent.Cmp(big.NewInt(-3)) != 0 || bf.Mantissa.Cmp(big.NewInt(24)) != 0 {
		t.Errorf("Normalized() modified receiver: %v", bf)
	}
}

func TestBigFloatFromBigRat(t *testing.T) {
	// 1/1024 = 1p-10
	bf, err := BigFloatFromBigRat(big.NewRat(1, 1024))
	if err != nil {
		t.Fatalf("BigFloatFromBigRat(1/1024) returned error %v", err)
	}
	if bf.Exponent.Cmp(big.NewInt(-10)) != 0 || bf.Mantissa.Cmp(big.NewInt(1)) != 0 {
		t.Errorf("BigFloatFromBigRat(1/1024) = %v, want exponent -10 mantissa 1", bf)
	}

	// 1/3 and 1/10 have no finite binary representation.
	if _, err := BigFloatFromBigRat(big.NewRat(1, 3)); !errors.Is(err, ErrNotExactlyRepresentable) {
		t.Errorf("BigFloatFromBigRat(1/3) returned %v, want ErrNotExactlyRepresentable", err)
	}
	if _, err := BigFloatFromBigRat(big.NewRat(1, 10)); !errors.Is(err, ErrNotExactlyRepresentable) {
		t.Errorf("BigFloatFromBigRat(1/10) returned %v, want ErrNotExactlyRepresentable", err)
	}
}

func TestBigFloatFromBigFloat(t *testing.T) {
	f := big.NewFloat(1.5)
	bf, err := BigFloatFromBigFloat(f)
	if err != nil {
		t.Fatalf("BigFloatFromBigFloat(1.5) returned error %v", err)
	}
	if bf.Exponent.Cmp(big.NewInt(-1)) != 0 || bf.Mantissa.Cmp(big.NewInt(3)) != 0 {
		t.Errorf("BigFloatFromBigFloat(1.5) = %v, want exponent -1 mantissa 3", bf)
	}

	if _, err := BigFloatFromBigFloat(new(big.Float).SetInf(false)); err == nil {
		t.Error("BigFloatFromBigFloat(+Inf) didn't return error")
	}
}

func TestBigFloatBigFloat(t *testing.T) {
	// 2^65-1 needs 65 bits; exact at 65 bits, inexact at 64.
	m := mustBigInt("36893488147419103231")
	bf := BigFloat{Exponent: big.NewInt(0), Mantissa: &m}

	f, err := bf.BigFloat(65)
	if err != nil {
		t.Fatalf("BigFloat(65) returned error %v", err)
	}
	if got := f.Text('g', -1); got != "3.6893488147419103231e+19" {
		t.Errorf("BigFloat(65) = %s, want 3.6893488147419103231e+19", got)
	}

	if _, err := bf.BigFloat(64); !errors.Is(err, ErrNotExactlyRepresentable) {
		t.Errorf("BigFloat(64) returned %v, want ErrNotExactlyRepresentable", err)
	}
}

func TestBigFloatExponentLimit(t *testing.T) {
	// Exponent = 2^65 (bignum on the wire): decoding stores it exactly, but
	// conversion to big.Rat must fail instead of allocating 2^(2^65).
	data := mustHexDecode("c582c24902000000000000000001")
	var bf BigFloat
	if err := Unmarshal(data, &bf); err != nil {
		t.Fatalf("Unmarshal(0x%x) returned error %v", data, err)
	}
	if _, err := bf.BigRat(); !errors.Is(err, ErrExponentOutOfRange) {
		t.Errorf("BigRat() returned %v, want ErrExponentOutOfRange", err)
	}

	// Exponent = math.MinInt64 fits int64 but exceeds MaxExactExponent.
	data = mustHexDecode("c5823b7fffffffffffffff01")
	if err := Unmarshal(data, &bf); err != nil {
		t.Fatalf("Unmarshal(0x%x) returned error %v", data, err)
	}
	if _, err := bf.BigRat(); !errors.Is(err, ErrExponentOutOfRange) {
		t.Errorf("BigRat() returned %v, want ErrExponentOutOfRange", err)
	}

	// Exponent at the limit is allowed.
	bf = BigFloat{Exponent: big.NewInt(-MaxExactExponent), Mantissa: big.NewInt(1)}
	r, err := bf.BigRat()
	if err != nil {
		t.Fatalf("BigRat() with exponent %d returned error %v", -MaxExactExponent, err)
	}
	want := new(big.Rat).SetFrac(big.NewInt(1), new(big.Int).Lsh(big.NewInt(1), MaxExactExponent))
	if r.Cmp(want) != 0 {
		t.Errorf("BigRat() = %s, want %s", r.RatString(), want.RatString())
	}
}

func TestBigFloatUnmarshalErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cborHex string
	}{
		{"wrong tag number (tag 4)", "c4822003"},
		{"content not an array", "c501"},
		{"array of 1 element", "c58101"},
		{"non-integer exponent", "c582f93c0001"},
		{"non-integer mantissa", "c582016161"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var bf BigFloat
			if err := Unmarshal(mustHexDecode(tc.cborHex), &bf); err == nil {
				t.Errorf("Unmarshal(0x%s) didn't return error", tc.cborHex)
			}
		})
	}
}

func TestBigFloatMarshalNil(t *testing.T) {
	if _, err := (BigFloat{}).MarshalCBOR(); err == nil {
		t.Error("MarshalCBOR() with nil fields didn't return error")
	}
	if _, err := (BigFloat{Mantissa: big.NewInt(0)}).MarshalCBOR(); err == nil {
		t.Error("MarshalCBOR() with nil Exponent didn't return error")
	}
}

func TestBigFloatString(t *testing.T) {
	bf := NewBigFloat(big.NewInt(3), -1)
	if got := bf.String(); got != "3p-1" {
		t.Errorf("String() = %q, want %q", got, "3p-1")
	}
}
