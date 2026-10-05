package ber

import (
	"bytes"
	"math/big"
	"strings"
	"testing"
)

// TestEncodeOIDKnownVectors confirms the big-arc encoder produces byte-for-byte
// identical output to the historical int-based encoder for legacy inputs.
func TestEncodeOIDKnownVectors(t *testing.T) {
	for _, tc := range []struct {
		oid     string
		content []byte
	}{
		{"1.2.840.113549", []byte{0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d}},
		{"2.5.4.3", []byte{0x55, 0x04, 0x03}},
		{"0.0", []byte{0x00}},
		{"1.39", []byte{0x4f}},
		{"2.0", []byte{0x50}},
		{"2.39", []byte{0x77}},
		{"2.40", []byte{0x78}},
		{"2.48", []byte{0x81, 0x00}},
	} {
		enc, err := encodeOID(tc.oid)
		if err != nil {
			t.Fatalf("encodeOID(%q): %v", tc.oid, err)
		}
		if !bytes.Equal(enc, tc.content) {
			t.Errorf("encodeOID(%q) = %x, want %x", tc.oid, enc, tc.content)
		}
		parsed, err := parseObjectIdentifier(enc)
		if err != nil {
			t.Fatalf("parseObjectIdentifier(%x): %v", enc, err)
		}
		if got := OIDToString(parsed); got != tc.oid {
			t.Errorf("round-trip of %q gave %q", tc.oid, got)
		}
	}
}

// TestOIDBigRoundTrip exercises arcs that exceed 32-bit, 64-bit, and uint64
// machine words, verifying lossless round-trips through both the string API
// and the arbitrary-precision API.
func TestOIDBigRoundTrip(t *testing.T) {
	oids := []string{
		// arc beyond int32 but within int64
		"2.5.3000000000",
		// arc beyond int64
		"2.999.18446744073709551616",
		// arc beyond uint64
		"1.2.340282366920938463463374607431768211455",
		// huge second arc merged with arc0=2 in the big domain
		"2.123456789012345678901234567890123456789012345678901234567890",
		// mixed small and huge arcs
		"0.10.5.1888.234.324234.170141183460469231731687303715884105727",
	}
	for _, oid := range oids {
		enc, err := encodeOID(oid)
		if err != nil {
			t.Fatalf("encodeOID(%q): %v", oid, err)
		}
		arcs, err := DecodeOIDBig(enc)
		if err != nil {
			t.Fatalf("DecodeOIDBig(encodeOID(%q)): %v", oid, err)
		}
		if got := OIDBigToString(arcs); got != oid {
			t.Errorf("big round-trip of %q gave %q", oid, got)
		}

		// Full packet round-trip through the string API.
		p, err := NewOIDErr(ClassUniversal, TypePrimitive, TagObjectIdentifier, oid, "")
		if err != nil {
			t.Fatalf("NewOIDErr(%q): %v", oid, err)
		}
		decoded := DecodePacket(p.Bytes())
		if decoded == nil {
			t.Fatalf("DecodePacket returned nil for %q", oid)
		}
		if decoded.Value != oid {
			t.Errorf("packet round-trip of %q gave %v", oid, decoded.Value)
		}
	}
}

// TestRelativeOIDBigRoundTrip confirms RELATIVE-OID shares the same big-arc
// base-128 logic, including arcs beyond machine word size.
func TestRelativeOIDBigRoundTrip(t *testing.T) {
	for _, oid := range []string{
		"8571.3.2",
		"1.18446744073709551616.2",
		"340282366920938463463374607431768211455.0.7",
	} {
		enc, err := encodeRelativeOID(oid)
		if err != nil {
			t.Fatalf("encodeRelativeOID(%q): %v", oid, err)
		}
		arcs, err := DecodeRelativeOIDBig(enc)
		if err != nil {
			t.Fatalf("DecodeRelativeOIDBig(encodeRelativeOID(%q)): %v", oid, err)
		}
		if got := OIDBigToString(arcs); got != oid {
			t.Errorf("big round-trip of %q gave %q", oid, got)
		}

		p, err := NewRelativeOIDErr(ClassUniversal, TypePrimitive, TagRelativeOID, oid, "")
		if err != nil {
			t.Fatalf("NewRelativeOIDErr(%q): %v", oid, err)
		}
		decoded := DecodePacket(p.Bytes())
		if decoded == nil {
			t.Fatalf("DecodePacket returned nil for %q", oid)
		}
		if decoded.Value != oid {
			t.Errorf("packet round-trip of %q gave %v", oid, decoded.Value)
		}
	}
}

// TestEncodeOIDBigExactBytes pins the base-128 encoding of an arc wider than
// any machine word: 2^70 encodes as 0x81 followed by nine 0x80 and a 0x00.
func TestEncodeOIDBigExactBytes(t *testing.T) {
	arc := new(big.Int).Lsh(big.NewInt(1), 70)
	enc, err := EncodeRelativeOIDBig([]*big.Int{arc})
	if err != nil {
		t.Fatalf("EncodeRelativeOIDBig: %v", err)
	}
	want := append([]byte{0x81}, bytes.Repeat([]byte{0x80}, 9)...)
	want = append(want, 0x00)
	if !bytes.Equal(enc, want) {
		t.Fatalf("EncodeRelativeOIDBig(2^70) = %x, want %x", enc, want)
	}
	arcs, err := DecodeRelativeOIDBig(enc)
	if err != nil {
		t.Fatalf("DecodeRelativeOIDBig: %v", err)
	}
	if len(arcs) != 1 || arcs[0].Cmp(arc) != 0 {
		t.Fatalf("round-trip of 2^70 gave %v", arcs)
	}
}

// TestOIDBigFromStringAPI exercises the exported arbitrary-precision path
// end to end.
func TestOIDBigFromStringAPI(t *testing.T) {
	arcs, err := OIDBigFromString("2.999.123456789012345678901234567890")
	if err != nil {
		t.Fatalf("OIDBigFromString: %v", err)
	}
	enc, err := EncodeOIDBig(arcs)
	if err != nil {
		t.Fatalf("EncodeOIDBig: %v", err)
	}
	back, err := DecodeOIDBig(enc)
	if err != nil {
		t.Fatalf("DecodeOIDBig: %v", err)
	}
	if got := OIDBigToString(back); got != "2.999.123456789012345678901234567890" {
		t.Fatalf("round-trip gave %q", got)
	}
}

// TestOIDEncodeRejections covers invalid string input: empty arcs, negative
// arcs, non-numeric arcs, bad leading arcs, and oversized arcs.
func TestOIDEncodeRejections(t *testing.T) {
	for _, oid := range []string{
		"",         // no arcs at all
		"1",        // too few arcs
		"1..2",     // empty arc
		"1.2.",     // trailing empty arc
		".1.2",     // leading empty arc
		"1.-2",     // negative arc
		"-1.2",     // negative first arc
		"1.+2",     // explicit sign
		"1. 2",     // whitespace
		"1.2a",     // non-numeric
		"3.1",      // arc0 > 2
		"4.0",      // arc0 > 2
		"0.40",     // arc1 >= 40 with arc0 < 2
		"1.40",     // arc1 >= 40 with arc0 < 2
		"1.2.3.−4", // unicode minus is not a digit
	} {
		if _, err := encodeOID(oid); err == nil {
			t.Errorf("encodeOID(%q): expected error", oid)
		}
	}

	// An arc wider than maxOIDArcBits is rejected.
	huge := new(big.Int).Lsh(big.NewInt(1), maxOIDArcBits).String()
	if _, err := encodeOID("2.5." + huge); err == nil {
		t.Error("encodeOID with oversized arc: expected error")
	}
	// The merged first subidentifier counts against the same limit:
	// arc1 = 2^512 - 1 is a legal arc, but 40*2 + arc1 needs 513 bits.
	maxArc := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), maxOIDArcBits), big.NewInt(1))
	if _, err := EncodeOIDBig([]*big.Int{big.NewInt(2), maxArc}); err == nil {
		t.Error("EncodeOIDBig with oversized merged first arcs: expected error")
	}
	// Negative and nil arcs are rejected on the big path.
	if _, err := EncodeOIDBig([]*big.Int{big.NewInt(1), big.NewInt(2), big.NewInt(-1)}); err == nil {
		t.Error("EncodeOIDBig with negative arc: expected error")
	}
	if _, err := EncodeOIDBig([]*big.Int{big.NewInt(1), nil}); err == nil {
		t.Error("EncodeOIDBig with nil arc: expected error")
	}
	// Too many arcs.
	if _, err := encodeOID("1.2" + strings.Repeat(".3", maxOIDArcs)); err == nil {
		t.Error("encodeOID with too many arcs: expected error")
	}
	// Empty relative OID.
	if _, err := EncodeRelativeOIDBig(nil); err == nil {
		t.Error("EncodeRelativeOIDBig(nil): expected error")
	}
	if _, err := encodeRelativeOID(""); err == nil {
		t.Error("encodeRelativeOID(\"\"): expected error")
	}
}

// TestOIDDecodeRejections covers malformed content octets: non-minimal
// base-128, unterminated arcs, oversized arcs, and too many arcs.
func TestOIDDecodeRejections(t *testing.T) {
	for _, content := range []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"non-minimal first arc", []byte{0x80, 0x2a}},
		{"non-minimal later arc", []byte{0x2a, 0x80, 0x01}},
		{"unterminated first arc", []byte{0xaa}},
		{"unterminated later arc", []byte{0x2a, 0x81}},
		{"oversized arc", append([]byte{0x2a}, bytes.Repeat([]byte{0xff}, 74)...)},
		{"too many arcs", append([]byte{0x2a}, bytes.Repeat([]byte{0x01}, maxOIDArcs)...)},
	} {
		if _, err := DecodeOIDBig(content.data); err == nil {
			t.Errorf("DecodeOIDBig(%s): expected error", content.name)
		}
	}

	if _, err := DecodeRelativeOIDBig([]byte{}); err == nil {
		t.Error("DecodeRelativeOIDBig(empty): expected error")
	}
	if _, err := DecodeRelativeOIDBig([]byte{0x80, 0x01}); err == nil {
		t.Error("DecodeRelativeOIDBig(non-minimal): expected error")
	}
	if _, err := DecodeRelativeOIDBig([]byte{0x81}); err == nil {
		t.Error("DecodeRelativeOIDBig(unterminated): expected error")
	}
}

// TestOIDDecodeArcLimits verifies the single-arc bit limit boundary: an arc
// of exactly maxOIDArcBits bits decodes, one bit more does not.
func TestOIDDecodeArcLimits(t *testing.T) {
	maxArc := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), maxOIDArcBits), big.NewInt(1))
	enc, err := EncodeRelativeOIDBig([]*big.Int{maxArc})
	if err != nil {
		t.Fatalf("EncodeRelativeOIDBig(max arc): %v", err)
	}
	arcs, err := DecodeRelativeOIDBig(enc)
	if err != nil {
		t.Fatalf("DecodeRelativeOIDBig(max arc): %v", err)
	}
	if len(arcs) != 1 || arcs[0].Cmp(maxArc) != 0 {
		t.Fatalf("round-trip of max arc gave %v", arcs)
	}

	tooBig := new(big.Int).Lsh(big.NewInt(1), maxOIDArcBits)
	if _, err := EncodeRelativeOIDBig([]*big.Int{tooBig}); err == nil {
		t.Fatal("EncodeRelativeOIDBig(oversized arc): expected error")
	}
}

// TestParseObjectIdentifierLegacyInt confirms the []int API still decodes
// arcs that fit in an int and rejects wider arcs, while the big path and the
// packet decoder retain them losslessly.
func TestParseObjectIdentifierLegacyInt(t *testing.T) {
	enc, err := encodeOID("2.5.3000000000")
	if err != nil {
		t.Fatalf("encodeOID: %v", err)
	}
	if _, err := parseObjectIdentifier(enc); err == nil {
		t.Fatal("parseObjectIdentifier with arc > MaxInt32: expected error")
	}
	if _, err := parseRelativeObjectIdentifier(enc); err == nil {
		t.Fatal("parseRelativeObjectIdentifier with arc > MaxInt32: expected error")
	}

	decoded := DecodePacket(append([]byte{byte(TagObjectIdentifier), byte(len(enc))}, enc...))
	if decoded == nil {
		t.Fatal("DecodePacket returned nil for big-arc OID")
	}
	if decoded.Value != "2.5.3000000000" {
		t.Fatalf("DecodePacket value = %v, want 2.5.3000000000", decoded.Value)
	}
}
