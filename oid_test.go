package ber

import (
	"bytes"
	"math/big"
	"strings"
	"testing"
)

// bigArcStrings are arc values spanning and exceeding the machine word size,
// up to the per-arc bit-length limit.
var bigArcStrings = []string{
	"4294967296",                              // 2^32
	"9223372036854775807",                     // math.MaxInt64
	"9223372036854775808",                     // 2^63
	"18446744073709551616",                    // 2^64
	"1267650600228229401496703205376",         // 2^100
	"170141183460469231731687303715884105727", // 2^127 - 1
	// 2^512 - 1, the largest arc allowed by maxOIDArcBits.
	"13407807929942597099574024998205846127479365820592393377723561443721764030073546976801874298166903427690031858186486050853753882811946569946433649006084095",
}

// TestEncodeOIDBigArcs verifies that arcs beyond the int64 range round-trip
// losslessly through the string API, both as trailing arcs and as the second
// arc (which is merged into the first subidentifier).
func TestEncodeOIDBigArcs(t *testing.T) {
	for _, arc := range bigArcStrings {
		for _, oid := range []string{"2.5." + arc, "2." + arc, "1.2." + arc + ".7"} {
			enc, err := encodeOID(oid)
			if err != nil {
				t.Errorf("error encoding %s: %v", oid, err)
				continue
			}
			arcs, err := parseOIDArcs(enc)
			if err != nil {
				t.Errorf("error parsing %s: %v", oid, err)
				continue
			}
			if got := oidArcsToString(arcs); got != oid {
				t.Errorf("OID %s round-tripped as %s", oid, got)
			}
		}
	}
}

// TestEncodeRelativeOIDBigArcs verifies that RELATIVE-OID arcs beyond the
// int64 range round-trip losslessly through the same base-128 logic.
func TestEncodeRelativeOIDBigArcs(t *testing.T) {
	for _, arc := range bigArcStrings {
		roid := "8571.3." + arc + ".2"
		enc, err := encodeRelativeOID(roid)
		if err != nil {
			t.Errorf("error encoding %s: %v", roid, err)
			continue
		}
		arcs, err := parseRelativeOIDArcs(enc)
		if err != nil {
			t.Errorf("error parsing %s: %v", roid, err)
			continue
		}
		if got := oidArcsToString(arcs); got != roid {
			t.Errorf("RELATIVE-OID %s round-tripped as %s", roid, got)
		}
	}
}

// TestEncodeOIDLegacyVectors verifies that inputs representable with the old
// int-based implementation produce exactly the same DER encoding as before.
func TestEncodeOIDLegacyVectors(t *testing.T) {
	vectors := []struct {
		oid string
		enc []byte
	}{
		{"1.2.840.113549", []byte{0x2a, 0x86, 0x48, 0x86, 0xf7, 0x0d}},
		{"2.5.4.3", []byte{0x55, 0x04, 0x03}},
		{"0.9.2342.19200300.100.1.1", []byte{0x09, 0x92, 0x26, 0x89, 0x93, 0xf2, 0x2c, 0x64, 0x01, 0x01}},
		{"2.39.0", []byte{0x77, 0x00}},
		{"0.0.0", []byte{0x00, 0x00}},
	}
	for _, v := range vectors {
		enc, err := encodeOID(v.oid)
		if err != nil {
			t.Errorf("error encoding %s: %v", v.oid, err)
			continue
		}
		if !bytes.Equal(enc, v.enc) {
			t.Errorf("OID %s encoded as %x, want %x", v.oid, enc, v.enc)
		}
		parsed, err := parseObjectIdentifier(enc)
		if err != nil {
			t.Errorf("error parsing %s: %v", v.oid, err)
			continue
		}
		if got := OIDToString(parsed); got != v.oid {
			t.Errorf("OID %s parsed as %s", v.oid, got)
		}
	}
}

// TestOIDPacketBigArcRoundTrip verifies end-to-end packet handling of OIDs
// and RELATIVE-OIDs with arcs beyond the int64 range.
func TestOIDPacketBigArcRoundTrip(t *testing.T) {
	oid := "2.999.1267650600228229401496703205376.18446744073709551616"
	p, err := NewOIDErr(ClassUniversal, TypePrimitive, TagObjectIdentifier, oid, "big OID")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	decoded, err := DecodePacketErr(p.Bytes())
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if decoded.Value != oid {
		t.Errorf("OID round-tripped as %v", decoded.Value)
	}

	roid := "8571.3.170141183460469231731687303715884105727"
	p, err = NewRelativeOIDErr(ClassUniversal, TypePrimitive, TagRelativeOID, roid, "big RELATIVE-OID")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	decoded, err = DecodePacketErr(p.Bytes())
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if decoded.Value != roid {
		t.Errorf("RELATIVE-OID round-tripped as %v", decoded.Value)
	}
}

// TestEncodeOIDInvalid verifies rejection of empty arcs, negative arcs,
// invalid first arcs, and arcs exceeding the bit-length and count limits.
func TestEncodeOIDInvalid(t *testing.T) {
	// 2^512, one bit over the per-arc limit.
	tooBig := "2.1." + new(big.Int).Lsh(big.NewInt(1), 512).String()
	// 257 arcs, one over the arc-count limit.
	tooMany := "1.1" + strings.Repeat(".1", 255)

	for _, oid := range []string{
		"", "1", "1..2", ".1.2", "1.2.", "1.-2", "-1.2", "3.1", "1.40", "0.40",
		"abc", "1.2.x", tooBig, tooMany,
	} {
		if enc, err := encodeOID(oid); err == nil {
			t.Errorf("expected error encoding %q, got %x", oid, enc)
		}
	}

	// Boundary values that must still be accepted.
	for _, oid := range []string{"1.39", "2.39", "2.40", "1.1" + strings.Repeat(".1", 254)} {
		if _, err := encodeOID(oid); err != nil {
			t.Errorf("unexpected error encoding %q: %v", oid, err)
		}
	}
}

// TestEncodeRelativeOIDInvalid verifies rejection of invalid RELATIVE-OID
// inputs.
func TestEncodeRelativeOIDInvalid(t *testing.T) {
	tooBig := new(big.Int).Lsh(big.NewInt(1), 512).String()
	tooMany := "1" + strings.Repeat(".1", 256)

	for _, roid := range []string{"", "1..2", "1.-2", "1." + tooBig, tooMany} {
		if enc, err := encodeRelativeOID(roid); err == nil {
			t.Errorf("expected error encoding %q, got %x", roid, enc)
		}
	}
}

// TestDecodeOIDInvalid verifies rejection of non-minimal base-128,
// unterminated arcs, and content exceeding the arc limits.
func TestDecodeOIDInvalid(t *testing.T) {
	oversizedArc := append(bytes.Repeat([]byte{0xff}, 75), 0x7f) // 532 bits
	tooManyArcs := bytes.Repeat([]byte{0x01}, 300)

	for _, enc := range [][]byte{
		{},
		{0x80, 0x2a}, // non-minimal base-128
		{0x86},       // unterminated arc
		{0x2a, 0x86}, // unterminated trailing arc
		oversizedArc,
		tooManyArcs,
	} {
		if _, err := parseOIDArcs(enc); err == nil {
			t.Errorf("expected error parsing %x", enc)
		}
		if _, err := parseRelativeOIDArcs(enc); err == nil {
			t.Errorf("expected error parsing RELATIVE-OID %x", enc)
		}
	}
}

// TestParseObjectIdentifierIntFallback verifies the int-based parse helpers
// still work for representable OIDs and fail for arcs beyond the int range.
func TestParseObjectIdentifierIntFallback(t *testing.T) {
	enc, err := encodeOID("1.2.840.113549")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	parsed, err := parseObjectIdentifier(enc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := OIDToString(parsed); got != "1.2.840.113549" {
		t.Errorf("parsed as %s", got)
	}

	enc, err = encodeOID("2.5.18446744073709551616")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err = parseObjectIdentifier(enc); err == nil {
		t.Error("expected error for arc beyond int range")
	}
}
