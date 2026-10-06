// Copyright (c) Faye Amacker. All rights reserved.
// Licensed under the MIT License. See LICENSE in the project root for license information.

package cbor

import (
	"errors"
	"math"
	"math/big"
	"testing"
)

// decimalFractionVector is a fixed test vector for CBOR tag 4. Mantissa and
// exponent are preserved exactly as encoded; wantRat is the exact value.
type decimalFractionVector struct {
	name     string
	cborHex  string
	exponent string
	mantissa string
	wantRat  string // exact value as a fraction string accepted by big.Rat.SetString
}

var decimalFractionVectors = []decimalFractionVector{
	{
		name:     "RFC 8949 example 27315e-2",
		cborHex:  "c48221196ab3",
		exponent: "-2",
		mantissa: "27315",
		wantRat:  "27315/100",
	},
	{
		// mantissa = 2^64+1 (does not fit uint64), exponent = -10
		name:     "mantissa 2^64+1, exponent -10",
		cborHex:  "c48229c249010000000000000001",
		exponent: "-10",
		mantissa: "18446744073709551617",
		wantRat:  "18446744073709551617/10000000000",
	},
	{
		// mantissa = -(2^70)-3, exponent = 7
		name:     "negative mantissa beyond 64 bits, positive exponent",
		cborHex:  "c48207c349400000000000000002",
		exponent: "7",
		mantissa: "-1180591620717411303427",
		wantRat:  "-11805916207174113034270000000",
	},
	{
		// mantissa with 30 significant decimal digits, exponent = -25
		name:     "30-digit mantissa, exponent -25",
		cborHex:  "c4823818c24d018ee90ff6c373e0ee4e3f0ad2",
		exponent: "-25",
		mantissa: "123456789012345678901234567890",
		wantRat:  "123456789012345678901234567890/10000000000000000000000000",
	},
	{
		// non-normalized form: trailing zero in mantissa must be preserved
		name:     "non-normalized 273150e-3",
		cborHex:  "c482221a00042afe",
		exponent: "-3",
		mantissa: "273150",
		wantRat:  "27315/100",
	},
}

func TestDecimalFractionDecodeVectors(t *testing.T) {
	for _, tc := range decimalFractionVectors {
		t.Run(tc.name, func(t *testing.T) {
			data := mustHexDecode(tc.cborHex)

			var df DecimalFraction
			if err := Unmarshal(data, &df); err != nil {
				t.Fatalf("Unmarshal(0x%x) returned error %v", data, err)
			}

			wantExp := mustBigInt(tc.exponent)
			wantMant := mustBigInt(tc.mantissa)
			if df.Exponent.Cmp(&wantExp) != 0 || df.Mantissa.Cmp(&wantMant) != 0 {
				t.Errorf("Unmarshal(0x%x) = %v, want exponent %s mantissa %s", data, df, tc.exponent, tc.mantissa)
			}

			r, err := df.BigRat()
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

func TestDecimalFractionRoundTripVectors(t *testing.T) {
	for _, tc := range decimalFractionVectors {
		t.Run(tc.name, func(t *testing.T) {
			data := mustHexDecode(tc.cborHex)

			var df DecimalFraction
			if err := Unmarshal(data, &df); err != nil {
				t.Fatalf("Unmarshal(0x%x) returned error %v", data, err)
			}

			// Re-encoding must reproduce the original bytes exactly,
			// preserving the encoded (possibly non-normalized) form.
			encoded, err := df.MarshalCBOR()
			if err != nil {
				t.Fatalf("MarshalCBOR() returned error %v", err)
			}
			if string(encoded) != string(data) {
				t.Errorf("round-trip encoding = 0x%x, want 0x%x", encoded, data)
			}

			// Value conversion must round-trip without precision loss:
			// df -> big.Rat -> DecimalFraction, compared in normalized form.
			r, err := df.BigRat()
			if err != nil {
				t.Fatalf("BigRat() returned error %v", err)
			}
			df2, err := DecimalFractionFromBigRat(r)
			if err != nil {
				t.Fatalf("DecimalFractionFromBigRat() returned error %v", err)
			}
			n1, n2 := df.Normalized(), df2.Normalized()
			if n1.Exponent.Cmp(n2.Exponent) != 0 || n1.Mantissa.Cmp(n2.Mantissa) != 0 {
				t.Errorf("value round-trip = %v, want %v", n2, n1)
			}
		})
	}
}

func TestDecimalFractionEncodeVector(t *testing.T) {
	df := NewDecimalFraction(big.NewInt(27315), -2)
	data, err := df.MarshalCBOR()
	if err != nil {
		t.Fatalf("MarshalCBOR() returned error %v", err)
	}
	want := mustHexDecode("c48221196ab3")
	if string(data) != string(want) {
		t.Errorf("MarshalCBOR() = 0x%x, want 0x%x", data, want)
	}
}

func TestDecimalFractionNormalized(t *testing.T) {
	df := DecimalFraction{
		Exponent: big.NewInt(-3),
		Mantissa: big.NewInt(273150),
	}
	n := df.Normalized()
	if n.Exponent.Cmp(big.NewInt(-2)) != 0 || n.Mantissa.Cmp(big.NewInt(27315)) != 0 {
		t.Errorf("Normalized() = %v, want exponent -2 mantissa 27315", n)
	}
	// Original must be unmodified.
	if df.Exponent.Cmp(big.NewInt(-3)) != 0 || df.Mantissa.Cmp(big.NewInt(273150)) != 0 {
		t.Errorf("Normalized() modified receiver: %v", df)
	}
}

func TestDecimalFractionFromBigRat(t *testing.T) {
	// 1/40 = 1/(2^3*5) = 25e-3
	df, err := DecimalFractionFromBigRat(big.NewRat(1, 40))
	if err != nil {
		t.Fatalf("DecimalFractionFromBigRat(1/40) returned error %v", err)
	}
	if df.Exponent.Cmp(big.NewInt(-3)) != 0 || df.Mantissa.Cmp(big.NewInt(25)) != 0 {
		t.Errorf("DecimalFractionFromBigRat(1/40) = %v, want exponent -3 mantissa 25", df)
	}

	// 1/3 has no finite decimal representation.
	if _, err := DecimalFractionFromBigRat(big.NewRat(1, 3)); !errors.Is(err, ErrNotExactlyRepresentable) {
		t.Errorf("DecimalFractionFromBigRat(1/3) returned %v, want ErrNotExactlyRepresentable", err)
	}

	// 22/7 has no finite decimal representation.
	if _, err := DecimalFractionFromBigRat(big.NewRat(22, 7)); !errors.Is(err, ErrNotExactlyRepresentable) {
		t.Errorf("DecimalFractionFromBigRat(22/7) returned %v, want ErrNotExactlyRepresentable", err)
	}
}

func TestDecimalFractionBigFloat(t *testing.T) {
	// 30-digit mantissa needs 97 bits; exact at 128 bits, inexact at 32.
	m := mustBigInt("123456789012345678901234567890")
	df := DecimalFraction{Exponent: big.NewInt(0), Mantissa: &m}

	f, err := df.BigFloat(128)
	if err != nil {
		t.Fatalf("BigFloat(128) returned error %v", err)
	}
	if got := f.Text('g', -1); got != "1.2345678901234567890123456789e+29" {
		t.Errorf("BigFloat(128) = %s, want 1.2345678901234567890123456789e+29", got)
	}

	if _, err := df.BigFloat(32); !errors.Is(err, ErrNotExactlyRepresentable) {
		t.Errorf("BigFloat(32) returned %v, want ErrNotExactlyRepresentable", err)
	}
}

func TestDecimalFractionExponentLimit(t *testing.T) {
	// Exponent = 2^65 (bignum on the wire): decoding stores it exactly, but
	// conversion to big.Rat must fail instead of allocating 10^(2^65).
	data := mustHexDecode("c482c24902000000000000000001")
	var df DecimalFraction
	if err := Unmarshal(data, &df); err != nil {
		t.Fatalf("Unmarshal(0x%x) returned error %v", data, err)
	}
	if _, err := df.BigRat(); !errors.Is(err, ErrExponentOutOfRange) {
		t.Errorf("BigRat() returned %v, want ErrExponentOutOfRange", err)
	}

	// Exponent = math.MaxInt64 fits int64 but exceeds MaxExactExponent.
	data = mustHexDecode("c4821b7fffffffffffffff01")
	if err := Unmarshal(data, &df); err != nil {
		t.Fatalf("Unmarshal(0x%x) returned error %v", data, err)
	}
	if _, err := df.BigRat(); !errors.Is(err, ErrExponentOutOfRange) {
		t.Errorf("BigRat() returned %v, want ErrExponentOutOfRange", err)
	}

	// Exponent at the limit is allowed.
	df = DecimalFraction{Exponent: big.NewInt(MaxExactExponent), Mantissa: big.NewInt(1)}
	r, err := df.BigRat()
	if err != nil {
		t.Fatalf("BigRat() with exponent %d returned error %v", MaxExactExponent, err)
	}
	want := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(MaxExactExponent), nil))
	if r.Cmp(want) != 0 {
		t.Errorf("BigRat() = %s, want %s", r.RatString(), want.RatString())
	}
}

func TestDecimalFractionUnmarshalErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cborHex string
	}{
		{"wrong tag number (tag 5)", "c5822003"},
		{"content not an array", "c401"},
		{"array of 3 elements", "c483010203"},
		{"non-integer exponent", "c482f93c0001"},
		{"non-integer mantissa", "c48201820203"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var df DecimalFraction
			if err := Unmarshal(mustHexDecode(tc.cborHex), &df); err == nil {
				t.Errorf("Unmarshal(0x%s) didn't return error", tc.cborHex)
			}
		})
	}
}

func TestDecimalFractionMarshalNil(t *testing.T) {
	if _, err := (DecimalFraction{}).MarshalCBOR(); err == nil {
		t.Error("MarshalCBOR() with nil fields didn't return error")
	}
	if _, err := (DecimalFraction{Exponent: big.NewInt(0)}).MarshalCBOR(); err == nil {
		t.Error("MarshalCBOR() with nil Mantissa didn't return error")
	}
}

func TestDecimalFractionString(t *testing.T) {
	df := NewDecimalFraction(big.NewInt(27315), -2)
	if got := df.String(); got != "27315e-2" {
		t.Errorf("String() = %q, want %q", got, "27315e-2")
	}
	df = NewDecimalFraction(big.NewInt(math.MaxInt64), 0)
	if got := df.String(); got != "9223372036854775807" {
		t.Errorf("String() = %q, want %q", got, "9223372036854775807")
	}
}
