// Copyright (c) Faye Amacker. All rights reserved.
// Licensed under the MIT License. See LICENSE in the project root for license information.

package cbor

import (
	"errors"
	"reflect"
	"testing"
)

// lexicographicProfile is a custom profile used to demonstrate that map key
// ordering rules depend on the selected profile.
var lexicographicProfile = DeterministicProfile{
	ShortestHead:           true,
	DefiniteLengthOnly:     true,
	MapKeyOrder:            MapKeyOrderLexicographic,
	RejectDuplicateMapKeys: true,
}

// TestDeterministicProfileValidate checks that vectors with the same
// semantics but different encoded forms are classified as malformed,
// well-formed but non-deterministic, or conforming to the selected profile.
func TestDeterministicProfileValidate(t *testing.T) {
	core := CoreDeterministicProfile()
	canonical := CanonicalDeterministicProfile()

	tests := []struct {
		name       string
		hexData    string
		profile    DeterministicProfile
		malformed  bool              // expect a well-formedness error, not a *DeterministicError
		wantRule   DeterministicRule // empty means data conforms to the profile
		wantOffset int
	}{
		// Vector set A: map {1: 24, 1000: "x"} in different encoded forms.
		{name: "A deterministic", hexData: "a20118181903e86178", profile: core},
		{name: "A non-shortest value", hexData: "a2011900181903e86178", profile: core, wantRule: RuleShortestHead, wantOffset: 2},
		{name: "A non-shortest key", hexData: "a2180118181903e86178", profile: core, wantRule: RuleShortestHead, wantOffset: 1},
		{name: "A indefinite-length map", hexData: "bf0118181903e86178ff", profile: core, wantRule: RuleDefiniteLength, wantOffset: 0},
		{name: "A unsorted map keys", hexData: "a21903e86178011818", profile: core, wantRule: RuleMapKeyOrder, wantOffset: 6},
		{name: "A duplicate map key", hexData: "a2011818011818", profile: core, wantRule: RuleNoDuplicateMapKey, wantOffset: 4},

		// Malformed inputs never reach the deterministic checks.
		{name: "truncated map", hexData: "a20118181903", profile: core, malformed: true},
		{name: "invalid additional information", hexData: "1c", profile: core, malformed: true},
		{name: "extraneous data", hexData: "0102", profile: core, malformed: true},
		{name: "empty", hexData: "", profile: core, malformed: true},

		// Vector set B: float 1.5 in different encoded forms. Core
		// Deterministic Encoding requires the shortest float; Canonical CBOR
		// (RFC 7049) doesn't.
		{name: "B float16", hexData: "f93e00", profile: core},
		{name: "B float32 core", hexData: "fa3fc00000", profile: core, wantRule: RuleShortestFloat, wantOffset: 0},
		{name: "B float64 core", hexData: "fb3ff8000000000000", profile: core, wantRule: RuleShortestFloat, wantOffset: 0},
		{name: "B float32 canonical", hexData: "fa3fc00000", profile: canonical},
		{name: "B float64 canonical", hexData: "fb3ff8000000000000", profile: canonical},
		{name: "B NaN float16", hexData: "f97e00", profile: core},
		{name: "B NaN float64", hexData: "fb7ff8000000000000", profile: core, wantRule: RuleShortestFloat, wantOffset: 0},

		// Vector set C: map {h'ff': 0, "": 0}. Key order validity depends on
		// the selected profile's ordering rule.
		{name: "C length-first order", hexData: "a2600041ff00", profile: core},
		{name: "C lexicographic order violates length-first", hexData: "a241ff006000", profile: core, wantRule: RuleMapKeyOrder, wantOffset: 4},
		{name: "C lexicographic order", hexData: "a241ff006000", profile: lexicographicProfile},
		{name: "C length-first order violates lexicographic", hexData: "a2600041ff00", profile: lexicographicProfile, wantRule: RuleMapKeyOrder, wantOffset: 3},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := mustHexDecode(tc.hexData)
			err := tc.profile.Validate(data)

			if tc.malformed {
				if err == nil {
					t.Fatalf("Validate(0x%x) = nil, want well-formedness error", data)
				}
				var detErr *DeterministicError
				if errors.As(err, &detErr) {
					t.Fatalf("Validate(0x%x) = %v, want well-formedness error, not *DeterministicError", data, err)
				}
				return
			}

			if tc.wantRule == "" {
				if err != nil {
					t.Fatalf("Validate(0x%x) = %v, want nil", data, err)
				}
				return
			}

			var detErr *DeterministicError
			if !errors.As(err, &detErr) {
				t.Fatalf("Validate(0x%x) = %v, want *DeterministicError", data, err)
			}
			if detErr.Rule != tc.wantRule {
				t.Errorf("Validate(0x%x) rule = %q, want %q", data, detErr.Rule, tc.wantRule)
			}
			if detErr.Offset != tc.wantOffset {
				t.Errorf("Validate(0x%x) offset = %d, want %d", data, detErr.Offset, tc.wantOffset)
			}
		})
	}
}

// TestDeterministicVectorsSameSemantics verifies that the well-formed
// non-deterministic vectors decode to the same Go value as the deterministic
// vector, i.e. they differ only in encoded form.
func TestDeterministicVectorsSameSemantics(t *testing.T) {
	wantMap := map[any]any{uint64(1): uint64(24), uint64(1000): "x"}
	for _, hexData := range []string{
		"a20118181903e86178",   // deterministic
		"a2011900181903e86178", // non-shortest value
		"a2180118181903e86178", // non-shortest key
		"bf0118181903e86178ff", // indefinite-length map
		"a21903e86178011818",   // unsorted keys
	} {
		data := mustHexDecode(hexData)
		var v any
		if err := Unmarshal(data, &v); err != nil {
			t.Fatalf("Unmarshal(0x%x) = %v", data, err)
		}
		if !reflect.DeepEqual(v, wantMap) {
			t.Errorf("Unmarshal(0x%x) = %v, want %v", data, v, wantMap)
		}
	}

	for _, hexData := range []string{"f93e00", "fa3fc00000", "fb3ff8000000000000"} {
		data := mustHexDecode(hexData)
		var v any
		if err := Unmarshal(data, &v); err != nil {
			t.Fatalf("Unmarshal(0x%x) = %v", data, err)
		}
		if !reflect.DeepEqual(v, 1.5) {
			t.Errorf("Unmarshal(0x%x) = %v, want 1.5", data, v)
		}
	}
}
