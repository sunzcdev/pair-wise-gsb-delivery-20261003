// Copyright (c) Faye Amacker. All rights reserved.
// Licensed under the MIT License. See LICENSE in the project root for license information.

package cbor

import (
	"errors"
	"io"
	"testing"
)

// TestValidateDeterministicMalformed verifies that malformed input is
// reported with a plain error (never *DeterministicError).
func TestValidateDeterministicMalformed(t *testing.T) {
	testCases := []struct {
		name    string
		hexData string
		wantErr error
	}{
		{"empty input", "", io.EOF},
		{"truncated head argument", "18", io.ErrUnexpectedEOF},
		{"invalid additional information 28", "1c", &SyntaxError{}},
		{"truncated byte string content", "41", io.ErrUnexpectedEOF},
		{"indefinite array missing break", "9f01", io.ErrUnexpectedEOF},
		{"truncated array element", "8201", io.ErrUnexpectedEOF},
		{"extraneous data after data item", "0102", &ExtraneousDataError{}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			data := mustHexDecode(tc.hexData)
			for _, profile := range []DeterministicProfile{ProfileCoreDeterministic, ProfileCanonical} {
				err := ValidateDeterministic(data, profile)
				if err == nil {
					t.Fatalf("ValidateDeterministic(%x) returned nil, want malformed error", data)
				}
				var detErr *DeterministicError
				if errors.As(err, &detErr) {
					t.Fatalf("ValidateDeterministic(%x) returned %T, want non-DeterministicError for malformed data", data, err)
				}
			}
		})
	}
}

// TestValidateDeterministicNonDeterministic verifies that well-formed but
// non-deterministic input is reported with *DeterministicError carrying the
// byte offset and rule name of the first violation.
func TestValidateDeterministicNonDeterministic(t *testing.T) {
	testCases := []struct {
		name       string
		hexData    string
		profile    DeterministicProfile
		wantRule   string
		wantOffset int
	}{
		// Shortest numeric representation.
		{"integer 10 with 1-byte argument", "180a", ProfileCoreDeterministic, RuleShortestNumericForm, 0},
		{"negative integer -24 with 1-byte argument", "3817", ProfileCoreDeterministic, RuleShortestNumericForm, 0},
		{"integer 65535 with 4-byte argument", "1a0000ffff", ProfileCoreDeterministic, RuleShortestNumericForm, 0},
		{"text string length with 1-byte argument", "780161", ProfileCoreDeterministic, RuleShortestNumericForm, 0},
		{"tag number 0 with 1-byte argument", "d80000", ProfileCoreDeterministic, RuleShortestNumericForm, 0},
		{"non-shortest integer nested in array", "81180a", ProfileCoreDeterministic, RuleShortestNumericForm, 1},
		{"non-shortest integer as map key", "a1180a01", ProfileCoreDeterministic, RuleShortestNumericForm, 1},
		{"non-shortest integer as map value", "a10119000a", ProfileCoreDeterministic, RuleShortestNumericForm, 2},

		// Container form.
		{"indefinite-length byte string", "5f4100ff", ProfileCoreDeterministic, RuleContainerForm, 0},
		{"indefinite-length text string", "7f6161ff", ProfileCoreDeterministic, RuleContainerForm, 0},
		{"indefinite-length array", "9f0102ff", ProfileCoreDeterministic, RuleContainerForm, 0},
		{"indefinite-length map", "bf0101ff", ProfileCoreDeterministic, RuleContainerForm, 0},
		{"indefinite-length array nested in map", "a1019fff", ProfileCoreDeterministic, RuleContainerForm, 2},

		// Preferred (shortest) floating-point form, core deterministic profile only.
		{"1.5 as float64", "fb3ff8000000000000", ProfileCoreDeterministic, RuleShortestNumericForm, 0},
		{"1.5 as float32", "fa3fc00000", ProfileCoreDeterministic, RuleShortestNumericForm, 0},
		{"0.0 as float64", "fb0000000000000000", ProfileCoreDeterministic, RuleShortestNumericForm, 0},
		{"infinity as float32", "fa7f800000", ProfileCoreDeterministic, RuleShortestNumericForm, 0},
		{"NaN as float64", "fb7ff8000000000000", ProfileCoreDeterministic, RuleShortestNumericForm, 0},
		{"NaN as float16 with payload", "f97e01", ProfileCoreDeterministic, RuleShortestNumericForm, 0},

		// Map key ordering.
		{"map keys 1, 0 out of order", "a201010002", ProfileCoreDeterministic, RuleMapKeyOrder, 3},
		{"duplicate map keys", "a200010002", ProfileCoreDeterministic, RuleMapKeyOrder, 3},
		{"nested map keys out of order", "a16161a201010002", ProfileCoreDeterministic, RuleMapKeyOrder, 6},

		// Map key ordering differs between profiles: bytewise lexicographic
		// order puts 1000 (0x1903e8) before h'00' (0x4100), while
		// length-first order puts the 2-byte key h'00' first.
		{"canonical length-first order violates core bytewise order", "a24100011903e802", ProfileCoreDeterministic, RuleMapKeyOrder, 4},
		{"core bytewise order violates canonical length-first order", "a21903e802410001", ProfileCanonical, RuleMapKeyOrder, 5},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			data := mustHexDecode(tc.hexData)
			err := ValidateDeterministic(data, tc.profile)
			if err == nil {
				t.Fatalf("ValidateDeterministic(%x) returned nil, want *DeterministicError", data)
			}
			var detErr *DeterministicError
			if !errors.As(err, &detErr) {
				t.Fatalf("ValidateDeterministic(%x) returned %T (%v), want *DeterministicError", data, err, err)
			}
			if detErr.Rule != tc.wantRule {
				t.Errorf("ValidateDeterministic(%x) returned rule %q, want %q", data, detErr.Rule, tc.wantRule)
			}
			if detErr.Offset != tc.wantOffset {
				t.Errorf("ValidateDeterministic(%x) returned offset %d, want %d", data, detErr.Offset, tc.wantOffset)
			}
		})
	}
}

// TestValidateDeterministicConformant verifies that data conforming to the
// selected profile returns nil.
func TestValidateDeterministicConformant(t *testing.T) {
	testCases := []struct {
		name    string
		hexData string
		profile DeterministicProfile
	}{
		{"integer 10", "0a", ProfileCoreDeterministic},
		{"integer 24 in shortest form", "1818", ProfileCoreDeterministic},
		{"integer 65535 in shortest form", "19ffff", ProfileCoreDeterministic},
		{"negative integer -1", "20", ProfileCoreDeterministic},
		{"1.5 as float16", "f93e00", ProfileCoreDeterministic},
		{"NaN in preferred form", "f97e00", ProfileCoreDeterministic},
		{"non-shortest-representable float64", "fb7e37e43c8800759c", ProfileCoreDeterministic},
		{"empty array", "80", ProfileCoreDeterministic},
		{"definite array", "820102", ProfileCoreDeterministic},
		{"simple map with sorted keys", "a20a61781903e881f93e00", ProfileCoreDeterministic},
		{"tagged item", "c074323031332d30332d32315432303a30343a30305a", ProfileCoreDeterministic},

		// Same encodings under the canonical (RFC 7049) profile.
		{"integer 10 canonical", "0a", ProfileCanonical},
		{"definite array canonical", "820102", ProfileCanonical},
		{"simple map with sorted keys canonical", "a20a61781903e881f93e00", ProfileCanonical},

		// RFC 7049 canonical CBOR does not constrain float widths.
		{"1.5 as float64 canonical", "fb3ff8000000000000", ProfileCanonical},
		{"1.5 as float32 canonical", "fa3fc00000", ProfileCanonical},

		// Map key orderings that conform to exactly one profile.
		{"bytewise key order conforms to core", "a21903e802410001", ProfileCoreDeterministic},
		{"length-first key order conforms to canonical", "a24100011903e802", ProfileCanonical},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			data := mustHexDecode(tc.hexData)
			if err := ValidateDeterministic(data, tc.profile); err != nil {
				t.Errorf("ValidateDeterministic(%x) returned %v, want nil", data, err)
			}
		})
	}
}

// TestValidateDeterministicSameSemantics groups vectors that carry the same
// semantic value in different encoding forms and distinguishes the three
// outcomes: malformed, well-formed but non-deterministic, and conforming.
func TestValidateDeterministicSameSemantics(t *testing.T) {
	testCases := []struct {
		name    string
		hexData string
		profile DeterministicProfile
		want    string // "malformed", "non-deterministic", or "conforming"
	}{
		// Semantic value: integer 10.
		{"int 10: truncated head", "18", ProfileCoreDeterministic, "malformed"},
		{"int 10: 1-byte argument", "180a", ProfileCoreDeterministic, "non-deterministic"},
		{"int 10: shortest form", "0a", ProfileCoreDeterministic, "conforming"},

		// Semantic value: array [1, 2].
		{"array: truncated", "8201", ProfileCoreDeterministic, "malformed"},
		{"array: indefinite-length", "9f0102ff", ProfileCoreDeterministic, "non-deterministic"},
		{"array: definite-length", "820102", ProfileCoreDeterministic, "conforming"},

		// Semantic value: 1.5.
		{"1.5: truncated float64", "fb3ff8", ProfileCoreDeterministic, "malformed"},
		{"1.5: float64", "fb3ff8000000000000", ProfileCoreDeterministic, "non-deterministic"},
		{"1.5: float32", "fa3fc00000", ProfileCoreDeterministic, "non-deterministic"},
		{"1.5: float16", "f93e00", ProfileCoreDeterministic, "conforming"},
		{"1.5: float64 under canonical profile", "fb3ff8000000000000", ProfileCanonical, "conforming"},

		// Semantic value: map {h'00': 1, 1000: 2}.
		{"map: truncated", "a24100011903", ProfileCoreDeterministic, "malformed"},
		{"map: length-first order under core profile", "a24100011903e802", ProfileCoreDeterministic, "non-deterministic"},
		{"map: bytewise order under canonical profile", "a21903e802410001", ProfileCanonical, "non-deterministic"},
		{"map: bytewise order under core profile", "a21903e802410001", ProfileCoreDeterministic, "conforming"},
		{"map: length-first order under canonical profile", "a24100011903e802", ProfileCanonical, "conforming"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			data := mustHexDecode(tc.hexData)
			err := ValidateDeterministic(data, tc.profile)
			switch tc.want {
			case "malformed":
				if err == nil {
					t.Fatalf("ValidateDeterministic(%x) returned nil, want malformed error", data)
				}
				var detErr *DeterministicError
				if errors.As(err, &detErr) {
					t.Fatalf("ValidateDeterministic(%x) returned %T, want non-DeterministicError", data, err)
				}
			case "non-deterministic":
				var detErr *DeterministicError
				if !errors.As(err, &detErr) {
					t.Fatalf("ValidateDeterministic(%x) returned %v, want *DeterministicError", data, err)
				}
			case "conforming":
				if err != nil {
					t.Fatalf("ValidateDeterministic(%x) returned %v, want nil", data, err)
				}
			}
		})
	}
}

func TestValidateDeterministicInvalidProfile(t *testing.T) {
	if err := ValidateDeterministic(mustHexDecode("0a"), DeterministicProfile(-1)); err == nil {
		t.Error("ValidateDeterministic with invalid profile returned nil, want error")
	}
}

// TestValidateDeterministicDoesNotDecodeReencode demonstrates that inputs
// which decode and re-encode to identical bytes are still rejected when the
// received bytes themselves are not deterministic.
func TestValidateDeterministicDoesNotDecodeReencode(t *testing.T) {
	// 0x180a decodes to 10 and re-encodes deterministically as 0x0a; the
	// received bytes themselves are not deterministic and must be rejected.
	data := mustHexDecode("180a")
	var v any
	if err := Unmarshal(data, &v); err != nil {
		t.Fatalf("Unmarshal(%x) returned %v", data, err)
	}
	if _, err := Marshal(v); err != nil {
		t.Fatalf("Marshal(%v) returned %v", v, err)
	}
	var detErr *DeterministicError
	if err := ValidateDeterministic(data, ProfileCoreDeterministic); !errors.As(err, &detErr) {
		t.Fatalf("ValidateDeterministic(%x) returned %v, want *DeterministicError", data, err)
	}
}
