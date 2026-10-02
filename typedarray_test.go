// Copyright (c) Faye Amacker. All rights reserved.
// Licensed under the MIT License. See LICENSE in the project root for license information.

package cbor

import (
	"encoding/hex"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/x448/float16"
)

// hexToBytes decodes a hex string to bytes for hand-computed test vectors.
func hexToBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("invalid hex test vector %q: %v", s, err)
	}
	return b
}

// TestTypedArrayDecodeToAny decodes hand-computed RFC 8746 byte vectors to
// any and expects the named slice types defined in typedarray.go.
func TestTypedArrayDecodeToAny(t *testing.T) {
	testCases := []struct {
		name string
		hex  string
		want any
	}{
		// uint8 (tag 64): bytes 00 7f ff
		{"uint8", "d84043007fff", Uint8Array{0x00, 0x7f, 0xff}},
		// uint8 little-endian (tag 68) is identical to tag 64
		{"uint8 LE", "d84443007fff", Uint8Array{0x00, 0x7f, 0xff}},
		// uint16 big-endian (tag 65): 0102 0304
		{"uint16 BE", "d8414401020304", Uint16Array{0x0102, 0x0304}},
		// uint16 little-endian (tag 69): 0102 0304 -> 0201 0403
		{"uint16 LE", "d8454401020304", Uint16Array{0x0201, 0x0403}},
		// uint32 big-endian (tag 66): 00000001 ffffffff
		{"uint32 BE", "d8424800000001ffffffff", Uint32Array{1, 0xffffffff}},
		// uint32 little-endian (tag 70): 01000000 ffffffff
		{"uint32 LE", "d8464801000000ffffffff", Uint32Array{1, 0xffffffff}},
		// uint64 big-endian (tag 67): 0102030405060708
		{"uint64 BE", "d843480102030405060708", Uint64Array{0x0102030405060708}},
		// uint64 little-endian (tag 71): 0807060504030201
		{"uint64 LE", "d847480807060504030201", Uint64Array{0x0102030405060708}},
		// int8 (tag 72): 80 ff
		{"int8", "d8484280ff", Int8Array{-128, -1}},
		// int8 little-endian (tag 76) is identical to tag 72
		{"int8 LE", "d84c4280ff", Int8Array{-128, -1}},
		// int16 big-endian (tag 73): ffff 8000 7fff
		{"int16 BE", "d84946ffff80007fff", Int16Array{-1, -32768, 32767}},
		// int16 little-endian (tag 77): ffff 0080 ff7f
		{"int16 LE", "d84d46ffff0080ff7f", Int16Array{-1, -32768, 32767}},
		// int32 big-endian (tag 74): ffffffff 80000000
		{"int32 BE", "d84a48ffffffff80000000", Int32Array{-1, math.MinInt32}},
		// int32 little-endian (tag 78): ffffffff 00000080
		{"int32 LE", "d84e48ffffffff00000080", Int32Array{-1, math.MinInt32}},
		// int64 big-endian (tag 75): ffffffffffffffff
		{"int64 BE", "d84b48ffffffffffffffff", Int64Array{-1}},
		// int64 little-endian (tag 79): 0100000000000080 -> 0x8000000000000001
		{"int64 LE", "d84f480100000000000080", Int64Array{math.MinInt64 + 1}},
		// empty uint16 array (tag 65, empty byte string)
		{"uint16 empty", "d84140", Uint16Array{}},
		// indefinite-length byte string content (tag 65): 5f 420102 420304 ff
		{"uint16 BE indefinite byte string", "d8415f420102420304ff", Uint16Array{0x0102, 0x0304}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			data := hexToBytes(t, tc.hex)
			var v any
			if err := Unmarshal(data, &v); err != nil {
				t.Fatalf("Unmarshal(0x%x) returned error %v", data, err)
			}
			if !reflect.DeepEqual(v, tc.want) {
				t.Errorf("Unmarshal(0x%x) = %#v, want %#v", data, v, tc.want)
			}
		})
	}
}

// TestTypedArrayDecodeFloatToAny verifies special floating-point bit patterns
// (negative zero, infinities, NaN) from hand-computed byte vectors.
func TestTypedArrayDecodeFloatToAny(t *testing.T) {
	t.Run("float16 BE", func(t *testing.T) {
		// tag 80: 3c00 (1.0), 7e00 (quiet NaN)
		var v any
		if err := Unmarshal(hexToBytes(t, "d850443c007e00"), &v); err != nil {
			t.Fatal(err)
		}
		got, ok := v.(Float16Array)
		if !ok {
			t.Fatalf("expected Float16Array, got %T", v)
		}
		if got[0].Float32() != 1.0 {
			t.Errorf("got[0] = %v, want 1.0", got[0].Float32())
		}
		if !got[1].IsNaN() || got[1].Bits() != 0x7e00 {
			t.Errorf("got[1] = bits %#04x, want NaN with bits 0x7e00", got[1].Bits())
		}
	})

	t.Run("float16 LE", func(t *testing.T) {
		// tag 84: 003c (1.0), 007e (quiet NaN)
		var v any
		if err := Unmarshal(hexToBytes(t, "d85444003c007e"), &v); err != nil {
			t.Fatal(err)
		}
		got, ok := v.(Float16Array)
		if !ok {
			t.Fatalf("expected Float16Array, got %T", v)
		}
		if got[0].Float32() != 1.0 {
			t.Errorf("got[0] = %v, want 1.0", got[0].Float32())
		}
		if !got[1].IsNaN() || got[1].Bits() != 0x7e00 {
			t.Errorf("got[1] = bits %#04x, want NaN with bits 0x7e00", got[1].Bits())
		}
	})

	t.Run("float32 BE special values", func(t *testing.T) {
		// tag 81: 7fc00000 (NaN), 7f800000 (+Inf), ff800000 (-Inf), 80000000 (-0.0)
		var v any
		if err := Unmarshal(hexToBytes(t, "d851507fc000007f800000ff80000080000000"), &v); err != nil {
			t.Fatal(err)
		}
		got, ok := v.(Float32Array)
		if !ok {
			t.Fatalf("expected Float32Array, got %T", v)
		}
		if math.Float32bits(got[0]) != 0x7fc00000 {
			t.Errorf("got[0] bits = %#08x, want NaN bits 0x7fc00000", math.Float32bits(got[0]))
		}
		if !math.IsInf(float64(got[1]), 1) {
			t.Errorf("got[1] = %v, want +Inf", got[1])
		}
		if !math.IsInf(float64(got[2]), -1) {
			t.Errorf("got[2] = %v, want -Inf", got[2])
		}
		if got[3] != 0 || !math.Signbit(float64(got[3])) {
			t.Errorf("got[3] = %v, want negative zero", got[3])
		}
	})

	t.Run("float32 LE special values", func(t *testing.T) {
		// tag 85: 0000c07f (NaN), 0000807f (+Inf), 000080ff (-Inf), 00000080 (-0.0)
		var v any
		if err := Unmarshal(hexToBytes(t, "d855500000c07f0000807f000080ff00000080"), &v); err != nil {
			t.Fatal(err)
		}
		got, ok := v.(Float32Array)
		if !ok {
			t.Fatalf("expected Float32Array, got %T", v)
		}
		if math.Float32bits(got[0]) != 0x7fc00000 {
			t.Errorf("got[0] bits = %#08x, want NaN bits 0x7fc00000", math.Float32bits(got[0]))
		}
		if !math.IsInf(float64(got[1]), 1) {
			t.Errorf("got[1] = %v, want +Inf", got[1])
		}
		if !math.IsInf(float64(got[2]), -1) {
			t.Errorf("got[2] = %v, want -Inf", got[2])
		}
		if got[3] != 0 || !math.Signbit(float64(got[3])) {
			t.Errorf("got[3] = %v, want negative zero", got[3])
		}
	})

	t.Run("float64 BE special values", func(t *testing.T) {
		// tag 82: 7ff8000000000000 (NaN), 7ff0000000000000 (+Inf), fff0000000000000 (-Inf), 8000000000000000 (-0.0)
		var v any
		if err := Unmarshal(hexToBytes(t, "d85258207ff80000000000007ff0000000000000fff00000000000008000000000000000"), &v); err != nil {
			t.Fatal(err)
		}
		got, ok := v.(Float64Array)
		if !ok {
			t.Fatalf("expected Float64Array, got %T", v)
		}
		if math.Float64bits(got[0]) != 0x7ff8000000000000 {
			t.Errorf("got[0] bits = %#016x, want NaN bits 0x7ff8000000000000", math.Float64bits(got[0]))
		}
		if !math.IsInf(got[1], 1) {
			t.Errorf("got[1] = %v, want +Inf", got[1])
		}
		if !math.IsInf(got[2], -1) {
			t.Errorf("got[2] = %v, want -Inf", got[2])
		}
		if got[3] != 0 || !math.Signbit(got[3]) {
			t.Errorf("got[3] = %v, want negative zero", got[3])
		}
	})

	t.Run("float64 LE special values", func(t *testing.T) {
		// tag 86: 000000000000f87f (NaN), 000000000000f07f (+Inf), 000000000000f0ff (-Inf), 0000000000000080 (-0.0)
		var v any
		if err := Unmarshal(hexToBytes(t, "d8565820000000000000f87f000000000000f07f000000000000f0ff0000000000000080"), &v); err != nil {
			t.Fatal(err)
		}
		got, ok := v.(Float64Array)
		if !ok {
			t.Fatalf("expected Float64Array, got %T", v)
		}
		if math.Float64bits(got[0]) != 0x7ff8000000000000 {
			t.Errorf("got[0] bits = %#016x, want NaN bits 0x7ff8000000000000", math.Float64bits(got[0]))
		}
		if !math.IsInf(got[1], 1) {
			t.Errorf("got[1] = %v, want +Inf", got[1])
		}
		if !math.IsInf(got[2], -1) {
			t.Errorf("got[2] = %v, want -Inf", got[2])
		}
		if got[3] != 0 || !math.Signbit(got[3]) {
			t.Errorf("got[3] = %v, want negative zero", got[3])
		}
	})
}

// TestTypedArrayDecodeToTypedSlice decodes RFC 8746 byte vectors directly to
// Go slices and arrays with matching element types.
func TestTypedArrayDecodeToTypedSlice(t *testing.T) {
	t.Run("uint16 BE and LE", func(t *testing.T) {
		var be, le []uint16
		if err := Unmarshal(hexToBytes(t, "d8414401020304"), &be); err != nil {
			t.Fatal(err)
		}
		if err := Unmarshal(hexToBytes(t, "d8454401020304"), &le); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(be, []uint16{0x0102, 0x0304}) {
			t.Errorf("BE = %v, want [258 772]", be)
		}
		if !reflect.DeepEqual(le, []uint16{0x0201, 0x0403}) {
			t.Errorf("LE = %v, want [513 1027]", le)
		}
	})

	t.Run("user-defined slice type", func(t *testing.T) {
		type temperatureReadings []int16
		var v temperatureReadings
		if err := Unmarshal(hexToBytes(t, "d84946ffff80007fff"), &v); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(v, temperatureReadings{-1, -32768, 32767}) {
			t.Errorf("got %v, want [-1 -32768 32767]", v)
		}
	})

	t.Run("named typed array type", func(t *testing.T) {
		var v Uint16Array
		if err := Unmarshal(hexToBytes(t, "d8454401020304"), &v); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(v, Uint16Array{0x0201, 0x0403}) {
			t.Errorf("got %v, want [513 1027]", v)
		}
	})

	t.Run("go array exact length", func(t *testing.T) {
		var v [3]int16
		if err := Unmarshal(hexToBytes(t, "d84946ffff80007fff"), &v); err != nil {
			t.Fatal(err)
		}
		if v != [3]int16{-1, -32768, 32767} {
			t.Errorf("got %v, want [-1 -32768 32767]", v)
		}
	})

	t.Run("go array longer than data zeroes rest", func(t *testing.T) {
		var v [4]uint16
		if err := Unmarshal(hexToBytes(t, "d8414401020304"), &v); err != nil {
			t.Fatal(err)
		}
		if v != [4]uint16{0x0102, 0x0304, 0, 0} {
			t.Errorf("got %v, want [258 772 0 0]", v)
		}
	})

	t.Run("float64 slice", func(t *testing.T) {
		var v []float64
		if err := Unmarshal(hexToBytes(t, "d852488000000000000000"), &v); err != nil {
			t.Fatal(err)
		}
		if len(v) != 1 || v[0] != 0 || !math.Signbit(v[0]) {
			t.Errorf("got %v, want [-0.0]", v)
		}
	})

	t.Run("float16 slice", func(t *testing.T) {
		var v []float16.Float16
		if err := Unmarshal(hexToBytes(t, "d850443c007e00"), &v); err != nil {
			t.Fatal(err)
		}
		if len(v) != 2 || v[0].Bits() != 0x3c00 || v[1].Bits() != 0x7e00 {
			t.Errorf("got %v, want bits [0x3c00 0x7e00]", v)
		}
	})

	t.Run("struct field", func(t *testing.T) {
		type reading struct {
			Samples []uint16 `cbor:"s"`
		}
		var v reading
		// {"s": tag(65) h'01020304'}
		if err := Unmarshal(hexToBytes(t, "a16173d8414401020304"), &v); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(v.Samples, []uint16{0x0102, 0x0304}) {
			t.Errorf("got %v, want [258 772]", v.Samples)
		}
	})
}

// TestTypedArrayDecodeTypeMismatch verifies that a typed array tag does not
// decode to a Go type with a non-matching element type.
func TestTypedArrayDecodeTypeMismatch(t *testing.T) {
	testCases := []struct {
		name string
		hex  string
		v    any
	}{
		// tag 65 (uint16 array) cannot decode to []uint32
		{"uint16 tag to []uint32", "d8414401020304", &[]uint32{}},
		// tag 65 (uint16 array) cannot decode to []int16
		{"uint16 tag to []int16", "d8414401020304", &[]int16{}},
		// tag 80 (float16 array) cannot decode to []uint16
		{"float16 tag to []uint16", "d850443c007e00", &[]uint16{}},
		// tag 65 (uint16 array) cannot decode to []float16.Float16
		{"uint16 tag to []float16", "d8414401020304", &[]float16.Float16{}},
		// tag 82 (float64 array) cannot decode to []float32
		{"float64 tag to []float32", "d852488000000000000000", &[]float32{}},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			data := hexToBytes(t, tc.hex)
			if err := Unmarshal(data, tc.v); err == nil {
				t.Errorf("Unmarshal(0x%x) to %T didn't return error", data, tc.v)
			}
		})
	}
}

// TestTypedArrayInvalidLength verifies that byte strings whose length is not
// a multiple of the element size are rejected.
func TestTypedArrayInvalidLength(t *testing.T) {
	// tag 65 (uint16 array) with 3-byte content
	data := hexToBytes(t, "d84143010203")

	var a any
	if err := Unmarshal(data, &a); err == nil {
		t.Error("Unmarshal to any didn't return error for 3-byte uint16 array")
	} else if !strings.Contains(err.Error(), "not a multiple of element size") {
		t.Errorf("Unmarshal to any returned unexpected error: %v", err)
	}

	var s []uint16
	if err := Unmarshal(data, &s); err == nil {
		t.Error("Unmarshal to []uint16 didn't return error for 3-byte uint16 array")
	} else {
		var ute *UnmarshalTypeError
		if !errors.As(err, &ute) {
			t.Errorf("Unmarshal to []uint16 returned %T, want *UnmarshalTypeError", err)
		}
	}
}

// TestTypedArrayInvalidContentType verifies that typed array tags not followed
// by a byte string are rejected.
func TestTypedArrayInvalidContentType(t *testing.T) {
	// tag 65 (uint16 array) followed by text string "a"
	data := hexToBytes(t, "d8416161")

	var a any
	err := Unmarshal(data, &a)
	if err == nil {
		t.Fatal("Unmarshal didn't return error for typed array tag with text string content")
	}
	var icte *InadmissibleTagContentTypeError
	if !errors.As(err, &icte) {
		t.Errorf("Unmarshal returned %T (%v), want *InadmissibleTagContentTypeError", err, err)
	}
}

// TestTypedArrayElementCount tests the element count validation helper directly.
func TestTypedArrayElementCount(t *testing.T) {
	if _, err := typedArrayElementCount(3, 2); err == nil {
		t.Error("typedArrayElementCount(3, 2) didn't return error")
	}
	if _, err := typedArrayElementCount(7, 8); err == nil {
		t.Error("typedArrayElementCount(7, 8) didn't return error")
	}
	if n, err := typedArrayElementCount(0, 8); err != nil || n != 0 {
		t.Errorf("typedArrayElementCount(0, 8) = %d, %v; want 0, nil", n, err)
	}
	if n, err := typedArrayElementCount(16, 8); err != nil || n != 2 {
		t.Errorf("typedArrayElementCount(16, 8) = %d, %v; want 2, nil", n, err)
	}
}

// TestTypedArrayFloat128Unrecognized verifies that float128 tags (83 and 87)
// are treated as unrecognized tags.
func TestTypedArrayFloat128Unrecognized(t *testing.T) {
	// tag 83 with 1-byte content 00
	var v any
	if err := Unmarshal(hexToBytes(t, "d8534100"), &v); err != nil {
		t.Fatal(err)
	}
	tag, ok := v.(Tag)
	if !ok {
		t.Fatalf("expected cbor.Tag, got %T", v)
	}
	if tag.Number != tagNumFloat128ArrayBE || !reflect.DeepEqual(tag.Content, []byte{0x00}) {
		t.Errorf("got %+v, want Tag{83, [0]}", tag)
	}
}

// TestTypedArrayTagSetConflict verifies that RFC 8746 tag numbers and the
// built-in named slice types cannot be registered in a TagSet.
func TestTypedArrayTagSetConflict(t *testing.T) {
	opts := TagOptions{EncTag: EncTagRequired, DecTag: DecTagRequired}

	for _, num := range []uint64{64, 65, 82, 86, 83, 87} {
		ts := NewTagSet()
		if err := ts.Add(opts, reflect.TypeOf([]byte{}), num); err == nil {
			t.Errorf("TagSet.Add(tag number %d) didn't return error", num)
		}
	}

	for _, typ := range []reflect.Type{
		reflect.TypeOf(Uint8Array{}),
		reflect.TypeOf(Uint16Array{}),
		reflect.TypeOf(Int16Array{}),
		reflect.TypeOf(Float16Array{}),
		reflect.TypeOf(Float64Array{}),
	} {
		ts := NewTagSet()
		if err := ts.Add(opts, typ, 1000); err == nil {
			t.Errorf("TagSet.Add(%s) didn't return error", typ)
		}
	}

	// Registering an unrelated type with an unrelated tag number still works.
	type myType struct{}
	ts := NewTagSet()
	if err := ts.Add(opts, reflect.TypeOf(myType{}), 1000); err != nil {
		t.Errorf("TagSet.Add(myType, 1000) returned error %v", err)
	}
}

// TestTypedArrayEncode encodes the named slice types and compares against
// hand-computed RFC 8746 byte vectors.
func TestTypedArrayEncode(t *testing.T) {
	testCases := []struct {
		name string
		v    any
		hex  string
	}{
		{"uint8", Uint8Array{0x00, 0x7f, 0xff}, "d84043007fff"},
		{"uint16", Uint16Array{0x0102, 0x0304}, "d8414401020304"},
		{"uint32", Uint32Array{1, 0xffffffff}, "d8424800000001ffffffff"},
		{"uint64", Uint64Array{0x0102030405060708}, "d843480102030405060708"},
		{"int8", Int8Array{-1}, "d84841ff"},
		{"int16", Int16Array{-2}, "d84942fffe"},
		{"int32", Int32Array{-3}, "d84a44fffffffd"},
		{"int64", Int64Array{-4}, "d84b48fffffffffffffffc"},
		{"float16", Float16Array{float16.Frombits(0x3c00), float16.Frombits(0x7e00)}, "d850443c007e00"},
		{"float32 negative zero", Float32Array{float32(math.Copysign(0, -1))}, "d8514480000000"},
		{"float64 +Inf", Float64Array{math.Inf(1)}, "d852487ff0000000000000"},
		{"empty uint16", Uint16Array{}, "d84140"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := Marshal(tc.v)
			if err != nil {
				t.Fatalf("Marshal(%#v) returned error %v", tc.v, err)
			}
			want := hexToBytes(t, tc.hex)
			if !reflect.DeepEqual(data, want) {
				t.Errorf("Marshal(%#v) = 0x%x, want 0x%x", tc.v, data, want)
			}

			// Decoding the encoding must reproduce the original value.
			var roundTrip any
			if err := Unmarshal(data, &roundTrip); err != nil {
				t.Fatalf("Unmarshal(0x%x) returned error %v", data, err)
			}
			if !reflect.DeepEqual(roundTrip, tc.v) {
				t.Errorf("round trip of %#v = %#v", tc.v, roundTrip)
			}
		})
	}
}

// TestTypedArrayDecodeKnownVectorsRoundTrip decodes hand-computed vectors
// (including little-endian forms) and re-encodes them to the canonical
// big-endian form.
func TestTypedArrayDecodeKnownVectorsRoundTrip(t *testing.T) {
	testCases := []struct {
		name    string
		hex     string
		wantHex string
	}{
		{"uint16 LE to BE", "d8454401020304", "d8414402010403"},
		{"int16 LE to BE", "d84d46ffff0080ff7f", "d84946ffff80007fff"},
		{"float64 LE to BE", "d856480000000000000080", "d852488000000000000000"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var v any
			if err := Unmarshal(hexToBytes(t, tc.hex), &v); err != nil {
				t.Fatal(err)
			}
			data, err := Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			if want := hexToBytes(t, tc.wantHex); !reflect.DeepEqual(data, want) {
				t.Errorf("re-encoded 0x%x, want 0x%x", data, want)
			}
		})
	}
}
