package cbor

import (
	"errors"
	"math/big"
	"testing"
)

func decBigPtr(t *testing.T, s string) *big.Int {
	t.Helper()
	bi, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("bad big.Int literal %q", s)
	}
	return bi
}

// TestResourceLimits ensures hostile exponents/coefficients cannot force
// large allocations and that expansion limits are enforced.
func TestResourceLimits(t *testing.T) {
	// Exponent +2^63: rejected from the integer head alone.
	hugePos := mustHexDecode("c4821b800000000000000001")
	if _, err := DecodeDecimal(hugePos, nil); !errors.Is(err, ErrExponentTooLarge) {
		t.Fatalf("e=2^63 err = %v, want ErrExponentTooLarge", err)
	}
	// Exponent -2^63.
	hugeNeg := mustHexDecode("c5823b7fffffffffffffff01")
	if _, err := DecodeBigFloat(hugeNeg, nil); !errors.Is(err, ErrExponentTooLarge) {
		t.Fatalf("e=-2^63 err = %v, want ErrExponentTooLarge", err)
	}

	// Large but well-formed exponent past a tight custom limit is rejected
	// without materializing 10**100000.
	encoded, _ := NewDecimal(100000, big.NewInt(1)).MarshalCBOR()
	tight := NumericLimits{MaxExponent: 100}
	d, err := DecodeDecimal(encoded, &tight)
	if err != nil {
		t.Fatalf("decode with tight exponent limit should accept stored form: %v", err)
	}
	if _, err := d.Rat(&tight); !errors.Is(err, ErrExponentTooLarge) {
		t.Fatalf("Rat e=100000 err = %v, want ErrExponentTooLarge", err)
	}

	// Expanded value guard: 1*10^50000 fits MaxExponent (100k) but needs
	// ~200k bits, far over a 1024-bit expansion cap. Rat must fail while the
	// stored value still decodes and re-encodes.
	expLimits := NumericLimits{
		MaxStoredExponent: 1 << 40,
		MaxExponent:       100_000,
		MaxMantissaBits:   1_000_000,
		MaxExpandedBits:   1024,
	}
	bigExp, _ := NewDecimal(50_000, big.NewInt(1)).MarshalCBOR()
	d2, err := DecodeDecimal(bigExp, &expLimits)
	if err != nil {
		t.Fatalf("decode stored 10^4000000: %v", err)
	}
	if _, err := d2.Rat(&expLimits); !errors.Is(err, ErrExpandedValueTooLarge) {
		t.Fatalf("expanded Rat err = %v, want ErrExpandedValueTooLarge", err)
	}
	// The compact representation remains safe to render.
	if got := d2.String(); got != "1*10^50000" {
		t.Fatalf("compact String = %q, want 1*10^50000", got)
	}

	// Huge coefficient advertised by a bignum must be capped before allocation.
	// c2 5a <4-byte len> <len bytes> claims 1MB; a 128-bit mantissa cap fails.
	bigMant := make([]byte, 100_000)
	for i := range bigMant {
		bigMant[i] = 0xff
	}
	bigMantEnc := encodeTag4(t, 0, tagNumUnsignedBignum, bigMant)
	mantLimit := NumericLimits{MaxExponent: 100, MaxMantissaBits: 128, MaxExpandedBits: 1 << 20}
	if _, err := DecodeDecimal(bigMantEnc, &mantLimit); !errors.Is(err, ErrMantissaTooLarge) {
		t.Fatalf("large mantissa err = %v, want ErrMantissaTooLarge", err)
	}

	// An untrusted bignum length head advertising 4GB must be rejected from the
	// length alone, before any byte string slice is taken.
	advertised := []byte{
		0xc4, 0x82, 0x01, // tag4 [1,
		0xc2, 0x5a, 0xff, 0xff, 0xff, 0xff, // tag2, bstr len 2^32-1
		0x00, // a single payload byte (real buffer is tiny)
	}
	if _, err := DecodeDecimal(advertised, nil); !errors.Is(err, ErrMantissaTooLarge) {
		t.Fatalf("advertised 4GB bignum err = %v, want ErrMantissaTooLarge", err)
	}
}

// encodeTag4 builds tag 4 [e, tag(bignum, bytes)] for limit tests.
func encodeTag4(t *testing.T, e int64, bignumTag uint64, raw []byte) []byte {
	t.Helper()
	d := NewDecimal(e, new(big.Int).SetBytes(raw))
	_ = d
	// Manually assemble to control the coefficient bytes exactly.
	out := []byte{0xc4, 0x82}
	if e >= 0 {
		out = appendHead(out, byte(cborTypePositiveInt), uint64(e))
	} else {
		out = appendHead(out, byte(cborTypeNegativeInt), uint64(-e-1))
	}
	out = appendHead(out, byte(cborTypeTag), bignumTag)
	out = appendHead(out, byte(cborTypeByteString), uint64(len(raw)))
	out = append(out, raw...)
	return out
}

// TestMalformedScaledItems covers structural rejection paths.
func TestMalformedScaledItems(t *testing.T) {
	cases := []struct {
		name string
		hex  string
	}{
		{"wrong tag number", "c58221196ab3"},             // tag 5 where decimal expected
		{"not an array", "c401"},                         // tag 4 wrapping an integer
		{"wrong array length 1", "c48101"},               // one element
		{"wrong array length 3", "c483010203"},           // three elements
		{"indefinite array", "c49f0102ff"},               // indefinite [1,2]
		{"float exponent", "c482f93c0001"},               // exponent half 1.0
		{"string exponent", "c482616101"},                // exponent text "a"
		{"string mantissa", "c482016161"},                // mantissa text
		{"truncated", "c48221"},                          // missing args
		{"trailing data", "c482010203"},                  // extra byte
		{"bignum non-bytestring", "c48201c201"},          // tag2 wrapping integer
		{"bignum indefinite bytestring", "c48201c25fff"}, // tag2 wrapping indef bstr
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := DecodeDecimal(mustHexDecode(c.hex), nil); err == nil {
				t.Fatalf("expected error decoding %s", c.name)
			}
		})
	}

	// A tag 4 value decoded as BigFloat (and vice versa) is rejected.
	if _, err := DecodeBigFloat(mustHexDecode("c4820102"), nil); err == nil {
		t.Fatal("tag 4 accepted as tag 5")
	}
	if _, err := DecodeDecimal(mustHexDecode("c5820102"), nil); err == nil {
		t.Fatal("tag 5 accepted as tag 4")
	}
}

// TestParseDecimal covers scale preservation, scientific notation and zeros.
func TestParseDecimal(t *testing.T) {
	cases := []struct {
		in   string
		e    int64
		m    string
		text string
	}{
		{"1", 0, "1", "1"},
		{"-1.25", -2, "-125", "-1.25"},
		{"1.10", -2, "110", "1.10"},
		{"0.001", -3, "1", "0.001"},
		{"0", 0, "0", "0"},
		{"0.00", -2, "0", "0.00"},
		{"-0", 0, "0", "0"},
		{".5", -1, "5", "0.5"},
		{"1.5e3", 2, "15", "1500"},
		{"1.5e-2", -3, "15", "0.015"},
		{"150", 0, "150", "150"},
		{"+2.5", -1, "25", "2.5"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			d, err := ParseDecimal(c.in, nil)
			if err != nil {
				t.Fatalf("ParseDecimal(%q): %v", c.in, err)
			}
			if d.Exponent != c.e || d.Mantissa.String() != c.m {
				t.Fatalf("ParseDecimal(%q) = e=%d m=%s, want e=%d m=%s", c.in, d.Exponent, d.Mantissa, c.e, c.m)
			}
			if got := d.String(); got != c.text {
				t.Fatalf("String = %q, want %q", got, c.text)
			}
		})
	}

	for _, bad := range []string{"", "abc", "1.2.3", "1e", "e5", "1ex", "."} {
		if _, err := ParseDecimal(bad, nil); err == nil {
			t.Fatalf("ParseDecimal(%q) unexpectedly succeeded", bad)
		}
	}

	// "1." has an empty fractional part; that is a valid scale of zero here,
	// but "." above must fail. Confirm the loop does not accept bare dot.
	if _, err := ParseDecimal(".", nil); err == nil {
		t.Fatal(`ParseDecimal(".") should fail`)
	}
}

// TestScaledIntAndSelfDescribe checks Int extraction and self-describe tag
// wrapping, plus rejection of a bare untagged array.
func TestScaledIntAndSelfDescribe(t *testing.T) {
	d := NewDecimal(3, big.NewInt(27)) // 27000
	i, err := d.Int(nil)
	if err != nil {
		t.Fatal(err)
	}
	if i.Int64() != 27000 {
		t.Fatalf("Int = %d, want 27000", i)
	}
	if _, err := NewDecimal(-1, big.NewInt(1)).Int(nil); !errors.Is(err, ErrNotExact) {
		t.Fatalf("0.1 Int err = %v, want ErrNotExact", err)
	}

	bf := NewBigFloat(2, big.NewInt(3)) // 12
	bi, err := bf.Int(nil)
	if err != nil {
		t.Fatal(err)
	}
	if bi.Int64() != 12 {
		t.Fatalf("BigFloat.Int = %d, want 12", bi)
	}

	// 55799 self-describe wrapper around tag 4.
	wrapped := append([]byte{0xd9, 0xd9, 0xf7}, mustHexDecode("c4820102")...)
	dd, err := DecodeDecimal(wrapped, nil)
	if err != nil {
		t.Fatalf("self-describe decode: %v", err)
	}
	if dd.Exponent != 1 || dd.Mantissa.Int64() != 2 {
		t.Fatalf("self-describe value = e=%d m=%d", dd.Exponent, dd.Mantissa)
	}

	// A bare array without the tag is rejected.
	if _, err := DecodeDecimal(mustHexDecode("820102"), nil); err == nil {
		t.Fatal("bare array accepted as Decimal")
	}
}

// TestRatIdentityAcrossBases chains conversions and checks the rational value
// is bit-for-bit identical after every step (no float64 anywhere).
func TestRatIdentityAcrossBases(t *testing.T) {
	// 314/100 = 3.14. Decimal -> BigFloat fails (factor 5), but the Rat is
	// stable through canonicalization and CBOR round trip.
	r := mustRat("314/100")
	d, err := RatToDecimal(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Exponent != -2 || d.Mantissa.Int64() != 314 {
		t.Fatalf("3.14 = e=%d m=%d", d.Exponent, d.Mantissa)
	}
	enc, _ := d.MarshalCBOR()
	var back Decimal
	if err := back.UnmarshalCBOR(enc); err != nil {
		t.Fatal(err)
	}
	r1, _ := d.Rat(nil)
	r2, _ := back.Rat(nil)
	r3, _ := d.Canonical().Rat(nil)
	if r1.Cmp(r) != 0 || r2.Cmp(r) != 0 || r3.Cmp(r) != 0 {
		t.Fatalf("rational identity lost: %s %s %s vs %s", r1, r2, r3, r)
	}

	// A purely binary value travels Decimal<->BigFloat with equal Rats.
	bf := NewBigFloat(-3, big.NewInt(7)) // 7/8
	dec, err := bf.ToDecimal(nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err := dec.ToBigFloat(nil)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := bf.Rat(nil)
	b, _ := dec.Rat(nil)
	c, _ := again.Rat(nil)
	if a.Cmp(b) != 0 || b.Cmp(c) != 0 {
		t.Fatalf("cross-base identity lost: %s %s %s", a, b, c)
	}
	if !again.IsCanonical() {
		t.Fatal("BigFloat after cross-base round trip not canonical")
	}
}

// TestNoFloat64RoundTrip verifies coefficients that float64 cannot distinguish
// remain distinct end to end, and that big.Float conversion is exact.
func TestNoFloat64RoundTrip(t *testing.T) {
	// Two values differing in the 55th significant decimal digit; float64
	// collapses them, the exact representation must not.
	a := NewDecimal(-54, decBigPtr(t, "1000000000000000000000000000000000000000000000000000001"))
	b := NewDecimal(-54, decBigPtr(t, "1000000000000000000000000000000000000000000000000000002"))
	ra, _ := a.Rat(nil)
	rb, _ := b.Rat(nil)
	if ra.Cmp(rb) == 0 {
		t.Fatal("distinct high-precision decimals collapsed")
	}
	enc, _ := a.MarshalCBOR()
	var back Decimal
	if err := back.UnmarshalCBOR(enc); err != nil {
		t.Fatal(err)
	}
	rback, _ := back.Rat(nil)
	if rback.Cmp(ra) != 0 {
		t.Fatal("55-digit value lost precision through CBOR")
	}

	// A non-power-of-two big.Float (0.1f64) round trips exactly through
	// BigFloat, revealing its binary error rather than hiding it.
	f := new(big.Float).SetPrec(53)
	if _, ok := f.SetString("0.1"); !ok {
		t.Fatal("SetString 0.1")
	}
	bf, err := FloatToBigFloat(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bf.IsCanonical() {
		t.Fatalf("FloatToBigFloat not canonical: e=%d m=%s", bf.Exponent, bf.Mantissa)
	}
	fback, err := bf.ExactFloat(nil)
	if err != nil {
		t.Fatal(err)
	}
	if fback.Cmp(f) != 0 {
		t.Fatalf("big.Float round trip changed value: %s vs %s", fback.Text('x', 20), f.Text('x', 20))
	}
}

// TestExactConversions checks that finite base conversions succeed exactly
// and values like 1/3 are reported as not representable instead of being
// rounded or computed forever.
func TestExactConversions(t *testing.T) {
	// 1/3 is representable in neither base 10 nor base 2.
	third := big.NewRat(1, 3)
	if _, err := RatToDecimal(third, nil); !errors.Is(err, ErrNotExact) {
		t.Fatalf("RatToDecimal(1/3) err = %v, want ErrNotExact", err)
	}
	if _, err := RatToBigFloat(third, nil); !errors.Is(err, ErrNotExact) {
		t.Fatalf("RatToBigFloat(1/3) err = %v, want ErrNotExact", err)
	}

	// 1/10 is exact in base 10 but not base 2.
	tenth := big.NewRat(1, 10)
	d10, err := RatToDecimal(tenth, nil)
	if err != nil {
		t.Fatalf("RatToDecimal(1/10): %v", err)
	}
	if d10.Exponent != -1 || d10.Mantissa.Cmp(big.NewInt(1)) != 0 {
		t.Fatalf("1/10 decimal = e=%d m=%s, want e=-1 m=1", d10.Exponent, d10.Mantissa)
	}
	if _, err := RatToBigFloat(tenth, nil); !errors.Is(err, ErrNotExact) {
		t.Fatalf("RatToBigFloat(1/10) err = %v, want ErrNotExact", err)
	}
	if _, err := d10.ExactFloat(nil); !errors.Is(err, ErrNotExact) {
		t.Fatalf("Decimal(0.1).ExactFloat err = %v, want ErrNotExact", err)
	}

	// 1/2 is exact in both bases; bigfloat -> decimal always exact.
	half := NewBigFloat(-1, big.NewInt(1))
	hd, err := half.ToDecimal(nil)
	if err != nil {
		t.Fatalf("BigFloat(0.5).ToDecimal: %v", err)
	}
	if hd.Exponent != -1 || hd.Mantissa.Cmp(big.NewInt(5)) != 0 {
		t.Fatalf("0.5 decimal = e=%d m=%s, want e=-1 m=5", hd.Exponent, hd.Mantissa)
	}
	hf, err := half.ExactFloat(nil)
	if err != nil {
		t.Fatalf("BigFloat(0.5).ExactFloat: %v", err)
	}
	if hf.Text('g', 5) != "0.5" {
		t.Fatalf("0.5 float text = %q", hf.Text('g', 5))
	}

	// 1/200 = 5*10^-3 canonical decimal, and has no finite base-2 form.
	r200 := big.NewRat(1, 200)
	d200, err := RatToDecimal(r200, nil)
	if err != nil {
		t.Fatalf("RatToDecimal(1/200): %v", err)
	}
	if d200.Exponent != -3 || d200.Mantissa.Cmp(big.NewInt(5)) != 0 {
		t.Fatalf("1/200 canonical = e=%d m=%s, want e=-3 m=5", d200.Exponent, d200.Mantissa)
	}
	if _, err := RatToBigFloat(r200, nil); !errors.Is(err, ErrNotExact) {
		t.Fatalf("RatToBigFloat(1/200) err = %v, want ErrNotExact", err)
	}

	// 3/8 is a finite base-2 fraction and base-10 fraction.
	d38 := big.NewRat(3, 8)
	bf38, err := RatToBigFloat(d38, nil)
	if err != nil {
		t.Fatalf("RatToBigFloat(3/8): %v", err)
	}
	if bf38.Exponent != -3 || bf38.Mantissa.Cmp(big.NewInt(3)) != 0 {
		t.Fatalf("3/8 bigfloat = e=%d m=%s, want e=-3 m=3", bf38.Exponent, bf38.Mantissa)
	}
	dec38, err := bf38.ToDecimal(nil)
	if err != nil {
		t.Fatalf("3/8 ToDecimal: %v", err)
	}
	if dec38.Rat(nil); err != nil {
		t.Fatal(err)
	}
	r38check, _ := dec38.Rat(nil)
	if r38check.Cmp(d38) != 0 {
		t.Fatalf("3/8 decimal round trip = %s", r38check)
	}
}

// TestPreserveVersusCanonical verifies that the original exponent form is
// retained through decode/marshal and that Canonical normalizes without
// changing the numeric value.
func TestPreserveVersusCanonical(t *testing.T) {
	raw := mustHexDecode("c482211864") // [-2, 100] = 1.00
	d, err := DecodeDecimal(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Exponent != -2 || d.Mantissa.Int64() != 100 {
		t.Fatalf("raw form = e=%d m=%d", d.Exponent, d.Mantissa)
	}
	if d.String() != "1.00" {
		t.Fatalf("raw String = %q, want 1.00", d.String())
	}
	if d.IsCanonical() {
		t.Fatal("1.00 should not be canonical")
	}
	enc, _ := d.MarshalCBOR()
	if string(enc) != string(raw) {
		t.Fatalf("preserving marshal changed bytes: %x", enc)
	}

	c := d.Canonical()
	if c.Exponent != 0 || c.Mantissa.Int64() != 1 || !c.IsCanonical() {
		t.Fatalf("canonical = e=%d m=%d", c.Exponent, c.Mantissa)
	}
	if c.String() != "1" {
		t.Fatalf("canonical String = %q, want 1", c.String())
	}
	// Numeric value is unchanged.
	rOrig, _ := d.Rat(nil)
	rCan, _ := c.Rat(nil)
	if rOrig.Cmp(rCan) != 0 {
		t.Fatal("canonicalization changed value")
	}

	// BigFloat canonical folds trailing powers of two.
	bf := NewBigFloat(-2, big.NewInt(12)) // 12*2^-2 = 3
	if bf.IsCanonical() {
		t.Fatal("12*2^-2 should not be canonical")
	}
	bfc := bf.Canonical()
	if bfc.Exponent != 0 || bfc.Mantissa.Int64() != 3 {
		t.Fatalf("bf canonical = e=%d m=%d, want e=0 m=3", bfc.Exponent, bfc.Mantissa)
	}

	// Zero canonicalizes to exponent 0 / coefficient 0.
	z := NewDecimal(7, big.NewInt(0)).Canonical()
	if z.Exponent != 0 || z.Mantissa.Sign() != 0 {
		t.Fatalf("zero canonical = e=%d m=%d", z.Exponent, z.Mantissa)
	}
}

func mustRat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic("bad rat " + s)
	}
	return r
}

// scaledRatBig builds m*base**e entirely with big.Int/Rat, never float64.
func scaledRatBig(t *testing.T, base int64, e int64, m string) *big.Rat {
	t.Helper()
	mi := decBigPtr(t, m)
	r := new(big.Rat).SetInt(mi)
	if e == 0 {
		return r
	}
	abs := e
	if abs < 0 {
		abs = -abs
	}
	scale := new(big.Int).Exp(big.NewInt(base), big.NewInt(abs), nil)
	if e > 0 {
		r.Mul(r, new(big.Rat).SetInt(scale))
	} else {
		r.Quo(r, new(big.Rat).SetInt(scale))
	}
	return r
}

// TestFixedVectorsBeyond64bit decodes hand-checked CBOR encodings whose
// coefficient and/or value exceed 64-bit range and compares every derived
// representation against math/big.
func TestFixedVectorsBeyond64bit(t *testing.T) {
	vectors := []struct {
		name string
		hex  string
		base int64
		tag  uint64
		exp  int64
		mant string
	}{
		{
			name: "decimal e-like 50 digits e=-49",
			hex:  "c4823830c255129967ba846982c774761258ab69769410eccf55a7",
			base: 10, tag: TagDecimalFraction, exp: -49,
			mant: "27182818284590452353602874713526624977572470936999",
		},
		{
			name: "bigfloat (2^100+13)*2^-20",
			hex:  "c58233c24d1000000000000000000000000d",
			base: 2, tag: TagBigFloat, exp: -20,
			mant: "1267650600228229401496703205389",
		},
		{
			name: "273.15",
			hex:  "c48221196ab3",
			base: 10, tag: TagDecimalFraction, exp: -2,
			mant: "27315",
		},
		{
			name: "negative 40-digit coefficient",
			hex:  "c48231c35103a0c92075c0dbf3b8acbc5f96ce3f0ad1",
			base: 10, tag: TagDecimalFraction, exp: -18,
			mant: "-1234567890123456789012345678901234567890",
		},
		{
			name: "uint64-overflow coefficient tag2, e=1000",
			hex:  "c4821903e8c248ffffffffffffffff",
			base: 10, tag: TagDecimalFraction, exp: 1000,
			mant: "18446744073709551615",
		},
		{
			name: "negative bignum coefficient tag3",
			hex:  "c582033b0000000100000002",
			base: 2, tag: TagBigFloat, exp: 3,
			mant: "-4294967299",
		},
	}

	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			data := mustHexDecode(v.hex)
			wantRat := scaledRatBig(t, v.base, v.exp, v.mant)

			switch v.tag {
			case TagDecimalFraction:
				d, err := DecodeDecimal(data, nil)
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				if d.Exponent != v.exp || d.Mantissa.String() != v.mant {
					t.Fatalf("got e=%d m=%s, want e=%d m=%s", d.Exponent, d.Mantissa, v.exp, v.mant)
				}
				// Rat step: exact comparison.
				gotRat, err := d.Rat(nil)
				if err != nil {
					t.Fatalf("Rat: %v", err)
				}
				if gotRat.Cmp(wantRat) != 0 {
					t.Fatalf("Rat mismatch\n got %s\nwant %s", gotRat, wantRat)
				}
				// Marshal round trip must reproduce bytes.
				enc, err := d.MarshalCBOR()
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				if string(enc) != string(data) {
					t.Fatalf("round trip bytes differ:\n got %x\nwant %x", enc, data)
				}
			case TagBigFloat:
				bf, err := DecodeBigFloat(data, nil)
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				if bf.Exponent != v.exp || bf.Mantissa.String() != v.mant {
					t.Fatalf("got e=%d m=%s, want e=%d m=%s", bf.Exponent, bf.Mantissa, v.exp, v.mant)
				}
				gotRat, err := bf.Rat(nil)
				if err != nil {
					t.Fatalf("Rat: %v", err)
				}
				if gotRat.Cmp(wantRat) != 0 {
					t.Fatalf("Rat mismatch\n got %s\nwant %s", gotRat, wantRat)
				}
				enc, err := bf.MarshalCBOR()
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				if string(enc) != string(data) {
					t.Fatalf("round trip bytes differ:\n got %x\nwant %x", enc, data)
				}
			}
		})
	}
}
