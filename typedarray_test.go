// Copyright (c) Faye Amacker. All rights reserved.
// Licensed under the MIT License. See LICENSE in the project root for license information.

package cbor

import (
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/x448/float16"
)

// typedArrayTestVector is a known CBOR encoding (computed by hand from
// RFC 8746 tag number, byte string head, and packed element bytes) and the
// Go value it must decode to.  Vectors are not produced by this package's
// own encoder.
type typedArrayTestVector struct {
	name  string
	hex   string // known CBOR encoding
	value any    // expected value when decoding to any
}

var typedArrayTestVectors = []typedArrayTestVector{
	// tag 64: uint8 array, big-endian
	{"uint8", "d84043010203", Uint8Slice{1, 2, 3}},
	// tag 65: uint16 array, big-endian
	{"uint16 BE", "d84148000000010100ffff", Uint16BigEndianSlice{0, 1, 256, 65535}},
	// tag 66: uint32 array, big-endian
	{"uint32 BE", "d8424401020304", Uint32BigEndianSlice{0x01020304}},
	// tag 67: uint64 array, big-endian
	{"uint64 BE", "d843480102030405060708", Uint64BigEndianSlice{0x0102030405060708}},
	// tag 68: uint8 array, little-endian (identical to tag 64)
	{"uint8 LE tag", "d84443010203", Uint8Slice{1, 2, 3}},
	// tag 69: uint16 array, little-endian
	{"uint16 LE", "d8454601000001ffff", Uint16LittleEndianSlice{1, 256, 65535}},
	// tag 70: uint32 array, little-endian
	{"uint32 LE", "d8464404030201", Uint32LittleEndianSlice{0x01020304}},
	// tag 71: uint64 array, little-endian
	{"uint64 LE", "d847480807060504030201", Uint64LittleEndianSlice{0x0102030405060708}},
	// tag 72: sint8 array, big-endian (negative values are two's complement)
	{"int8", "d84845ff0001807f", Int8Slice{-1, 0, 1, -128, 127}},
	// tag 73: sint16 array, big-endian
	{"int16 BE", "d8494cfffeffff0000000180007fff", Int16BigEndianSlice{-2, -1, 0, 1, -32768, 32767}},
	// tag 74: sint32 array, big-endian
	{"int32 BE", "d84a48ffffffff80000000", Int32BigEndianSlice{-1, math.MinInt32}},
	// tag 75: sint64 array, big-endian
	{"int64 BE", "d84b50ffffffffffffffff8000000000000000", Int64BigEndianSlice{-1, math.MinInt64}},
	// tag 76: sint8 array, little-endian (identical to tag 72)
	{"int8 LE tag", "d84c42ff01", Int8Slice{-1, 1}},
	// tag 77: sint16 array, little-endian
	{"int16 LE", "d84d44feffff7f", Int16LittleEndianSlice{-2, 32767}},
	// tag 78: sint32 array, little-endian
	{"int32 LE", "d84e44ffffffff", Int32LittleEndianSlice{-1}},
	// tag 79: sint64 array, little-endian
	{"int64 LE", "d84f48feffffffffffffff", Int64LittleEndianSlice{-2}},
	// tag 80: float16 array, big-endian: 1.0, -0.0, +Inf, -Inf, NaN
	{"float16 BE", "d8504a3c0080007c00fc007e00", Float16BigEndianSlice{
		float16.Frombits(0x3c00), float16.Frombits(0x8000), float16.Frombits(0x7c00), float16.Frombits(0xfc00), float16.Frombits(0x7e00),
	}},
	// tag 81: float32 array, big-endian: -0.0, +Inf, -Inf, NaN, 1.5
	{"float32 BE", "d85154800000007f800000ff8000007fc000003fc00000", Float32BigEndianSlice{
		math.Float32frombits(0x80000000), math.Float32frombits(0x7f800000), math.Float32frombits(0xff800000), math.Float32frombits(0x7fc00000), math.Float32frombits(0x3fc00000),
	}},
	// tag 82: float64 array, big-endian: -0.0, NaN, +Inf
	{"float64 BE", "d852581880000000000000007ff80000000000007ff0000000000000", Float64BigEndianSlice{
		math.Float64frombits(0x8000000000000000), math.Float64frombits(0x7ff8000000000000), math.Float64frombits(0x7ff0000000000000),
	}},
	// tag 84: float16 array, little-endian: 1.0, -0.0
	{"float16 LE", "d85444003c0080", Float16LittleEndianSlice{float16.Frombits(0x3c00), float16.Frombits(0x8000)}},
	// tag 85: float32 array, little-endian: 1.5, -0.0
	{"float32 LE", "d855480000c03f00000080", Float32LittleEndianSlice{math.Float32frombits(0x3fc00000), math.Float32frombits(0x80000000)}},
	// tag 86: float64 array, little-endian: 1.0
	{"float64 LE", "d85648000000000000f03f", Float64LittleEndianSlice{math.Float64frombits(0x3ff0000000000000)}},
}

// typedArrayValuesEqual compares two decoded typed array values, treating
// NaN as equal to NaN and distinguishing negative zero from zero.
func typedArrayValuesEqual(a, b any) bool {
	av, bv := reflect.ValueOf(a), reflect.ValueOf(b)
	if av.Type() != bv.Type() || av.Kind() != reflect.Slice || av.Len() != bv.Len() {
		return false
	}
	for i := 0; i < av.Len(); i++ {
		ae, be := av.Index(i), bv.Index(i)
		if ae.Kind() != reflect.Float32 && ae.Kind() != reflect.Float64 {
			if !reflect.DeepEqual(ae.Interface(), be.Interface()) {
				return false
			}
			continue
		}
		af, bf := ae.Float(), be.Float()
		if af != bf {
			if !(math.IsNaN(af) && math.IsNaN(bf)) {
				return false
			}
		} else if af == 0 && math.Signbit(af) != math.Signbit(bf) {
			return false
		}
	}
	return true
}

func TestTypedArrayDecodeToAny(t *testing.T) {
	for _, tc := range typedArrayTestVectors {
		t.Run(tc.name, func(t *testing.T) {
			data := mustHexDecode(tc.hex)

			var v any
			if err := Unmarshal(data, &v); err != nil {
				t.Fatalf("Unmarshal(0x%x) returned error %v", data, err)
			}

			if !typedArrayValuesEqual(v, tc.value) {
				t.Errorf("Unmarshal(0x%x) = %v, want %v", data, v, tc.value)
			}
		})
	}
}

func TestTypedArrayEncode(t *testing.T) {
	for _, tc := range typedArrayTestVectors {
		t.Run(tc.name, func(t *testing.T) {
			// Skip vectors with non-canonical tag numbers (68 and 76 decode
			// to the same named types as tags 64 and 72).
			if tc.name == "uint8 LE tag" || tc.name == "int8 LE tag" {
				return
			}

			data, err := Marshal(tc.value)
			if err != nil {
				t.Fatalf("Marshal(%v) returned error %v", tc.value, err)
			}

			if want := mustHexDecode(tc.hex); !reflect.DeepEqual(data, want) {
				t.Errorf("Marshal(%v) = 0x%x, want 0x%x", tc.value, data, want)
			}
		})
	}
}

func TestTypedArrayRoundTrip(t *testing.T) {
	for _, tc := range typedArrayTestVectors {
		t.Run(tc.name, func(t *testing.T) {
			var v any
			if err := Unmarshal(mustHexDecode(tc.hex), &v); err != nil {
				t.Fatalf("Unmarshal returned error %v", err)
			}

			data, err := Marshal(v)
			if err != nil {
				t.Fatalf("Marshal returned error %v", err)
			}

			var v2 any
			if err := Unmarshal(data, &v2); err != nil {
				t.Fatalf("Unmarshal of round-tripped data returned error %v", err)
			}
			if !typedArrayValuesEqual(v2, tc.value) {
				t.Errorf("round trip = %v (%T), want %v (%T)", v2, v2, tc.value, tc.value)
			}
		})
	}
}

// TestTypedArraySpecialFloatBitPatterns verifies endianness, negative zero,
// and special floating-point bit patterns against known byte strings.
func TestTypedArraySpecialFloatBitPatterns(t *testing.T) {
	// tag 80, float16 big-endian: 1.0 (0x3c00), -0.0 (0x8000), +Inf (0x7c00), -Inf (0xfc00), NaN (0x7e00)
	var f16 Float16BigEndianSlice
	if err := Unmarshal(mustHexDecode("d8504a3c0080007c00fc007e00"), &f16); err != nil {
		t.Fatal(err)
	}
	if f16[0].Float32() != 1.0 {
		t.Errorf("f16[0] = %v, want 1.0", f16[0].Float32())
	}
	if f16[1].Bits() != 0x8000 || !f16[1].Signbit() {
		t.Errorf("f16[1] bits = %#x, want 0x8000 (negative zero)", f16[1].Bits())
	}
	if !f16[2].IsInf(1) || !f16[3].IsInf(-1) {
		t.Errorf("f16[2], f16[3] = %v, %v, want +Inf, -Inf", f16[2], f16[3])
	}
	if !f16[4].IsNaN() {
		t.Errorf("f16[4] = %v, want NaN", f16[4])
	}

	// tag 85, float32 little-endian: 1.5 (0x3fc00000), -0.0 (0x80000000)
	var f32 []float32
	if err := Unmarshal(mustHexDecode("d855480000c03f00000080"), &f32); err != nil {
		t.Fatal(err)
	}
	if f32[0] != 1.5 {
		t.Errorf("f32[0] = %v, want 1.5", f32[0])
	}
	if f32[1] != 0 || !math.Signbit(float64(f32[1])) {
		t.Errorf("f32[1] = %v, want negative zero", f32[1])
	}

	// tag 81, float32 big-endian: NaN with payload bits 0x7fc00000
	var f32nan []float32
	if err := Unmarshal(mustHexDecode("d851447fc00000"), &f32nan); err != nil {
		t.Fatal(err)
	}
	if !math.IsNaN(float64(f32nan[0])) || math.Float32bits(f32nan[0]) != 0x7fc00000 {
		t.Errorf("f32nan[0] bits = %#x, want 0x7fc00000", math.Float32bits(f32nan[0]))
	}

	// tag 82, float64 big-endian: -0.0, NaN (0x7ff8000000000000), +Inf
	var f64 []float64
	if err := Unmarshal(mustHexDecode("d852581880000000000000007ff80000000000007ff0000000000000"), &f64); err != nil {
		t.Fatal(err)
	}
	if f64[0] != 0 || !math.Signbit(f64[0]) {
		t.Errorf("f64[0] = %v, want negative zero", f64[0])
	}
	if !math.IsNaN(f64[1]) || math.Float64bits(f64[1]) != 0x7ff8000000000000 {
		t.Errorf("f64[1] bits = %#x, want 0x7ff8000000000000", math.Float64bits(f64[1]))
	}
	if !math.IsInf(f64[2], 1) {
		t.Errorf("f64[2] = %v, want +Inf", f64[2])
	}

	// tag 86, float64 little-endian: -Inf (0xfff0000000000000)
	var f64inf []float64
	if err := Unmarshal(mustHexDecode("d85648000000000000f0ff"), &f64inf); err != nil {
		t.Fatal(err)
	}
	if !math.IsInf(f64inf[0], -1) {
		t.Errorf("f64inf[0] = %v, want -Inf", f64inf[0])
	}
}

func TestTypedArrayDecodeToTypedSlice(t *testing.T) {
	t.Run("uint16 BE to []uint16", func(t *testing.T) {
		var v []uint16
		if err := Unmarshal(mustHexDecode("d84146000100020003"), &v); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(v, []uint16{1, 2, 3}) {
			t.Errorf("got %v, want [1 2 3]", v)
		}
	})

	t.Run("uint16 LE to []uint16", func(t *testing.T) {
		var v []uint16
		if err := Unmarshal(mustHexDecode("d8454601000001ffff"), &v); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(v, []uint16{1, 256, 65535}) {
			t.Errorf("got %v, want [1 256 65535]", v)
		}
	})

	t.Run("uint16 LE to Uint16BigEndianSlice", func(t *testing.T) {
		// The tag number determines how the byte string is interpreted;
		// decoding succeeds for any destination with matching element type.
		var v Uint16BigEndianSlice
		if err := Unmarshal(mustHexDecode("d8454601000001ffff"), &v); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(v, Uint16BigEndianSlice{1, 256, 65535}) {
			t.Errorf("got %v, want [1 256 65535]", v)
		}
	})

	t.Run("uint8 to []byte", func(t *testing.T) {
		var v []byte
		if err := Unmarshal(mustHexDecode("d84043010203"), &v); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(v, []byte{1, 2, 3}) {
			t.Errorf("got %v, want [1 2 3]", v)
		}
	})

	t.Run("int64 BE to named type", func(t *testing.T) {
		var v Int64BigEndianSlice
		if err := Unmarshal(mustHexDecode("d84b50ffffffffffffffff8000000000000000"), &v); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(v, Int64BigEndianSlice{-1, math.MinInt64}) {
			t.Errorf("got %v, want [-1 %d]", v, int64(math.MinInt64))
		}
	})

	t.Run("float64 BE to []float64", func(t *testing.T) {
		var v []float64
		if err := Unmarshal(mustHexDecode("d852483ff0000000000000"), &v); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(v, []float64{1.0}) {
			t.Errorf("got %v, want [1]", v)
		}
	})

	t.Run("empty byte string", func(t *testing.T) {
		var v []uint32
		if err := Unmarshal(mustHexDecode("d84240"), &v); err != nil {
			t.Fatal(err)
		}
		if v == nil || len(v) != 0 {
			t.Errorf("got %v, want empty non-nil slice", v)
		}
	})

	t.Run("indefinite-length byte string content", func(t *testing.T) {
		var v []uint16
		if err := Unmarshal(mustHexDecode("d8415f420001420002ff"), &v); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(v, []uint16{1, 2}) {
			t.Errorf("got %v, want [1 2]", v)
		}
	})

	t.Run("null to named slice", func(t *testing.T) {
		v := Uint16BigEndianSlice{1}
		if err := Unmarshal(mustHexDecode("f6"), &v); err != nil {
			t.Fatal(err)
		}
		if v != nil {
			t.Errorf("got %v, want nil", v)
		}
	})

	t.Run("struct field", func(t *testing.T) {
		type reading struct {
			Samples []uint16 `cbor:"samples"`
		}
		var v reading
		// {"samples": 65(h'000100020003')}
		if err := Unmarshal(mustHexDecode("a16773616d706c6573d84146000100020003"), &v); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(v.Samples, []uint16{1, 2, 3}) {
			t.Errorf("got %v, want [1 2 3]", v.Samples)
		}
	})

	t.Run("to cbor.Tag keeps generic behavior", func(t *testing.T) {
		var v Tag
		if err := Unmarshal(mustHexDecode("d84146000100020003"), &v); err != nil {
			t.Fatal(err)
		}
		if v.Number != 65 {
			t.Errorf("got tag number %d, want 65", v.Number)
		}
		if !reflect.DeepEqual(v.Content, []byte{0, 1, 0, 2, 0, 3}) {
			t.Errorf("got content %v, want [0 1 0 2 0 3]", v.Content)
		}
	})

	t.Run("float128 tag 83 decodes to cbor.Tag", func(t *testing.T) {
		// Tag 83 (float128 array) is not supported and is treated as an
		// unrecognized tag.
		var v any
		if err := Unmarshal(mustHexDecode("d8535000000000000000000000000000000000"), &v); err != nil {
			t.Fatal(err)
		}
		tag, ok := v.(Tag)
		if !ok {
			t.Fatalf("got type %T, want cbor.Tag", v)
		}
		if tag.Number != 83 {
			t.Errorf("got tag number %d, want 83", tag.Number)
		}
		if !reflect.DeepEqual(tag.Content, make([]byte, 16)) {
			t.Errorf("got content %v, want 16 zero bytes", tag.Content)
		}
	})
}

func TestTypedArrayDecodeErrors(t *testing.T) {
	t.Run("byte string length not multiple of element size", func(t *testing.T) {
		// tag 65 (uint16) with 3-byte content
		var v []uint16
		err := Unmarshal(mustHexDecode("d84143000102"), &v)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		var typeErr *UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			t.Fatalf("got error type %T, want *UnmarshalTypeError", err)
		}
		if !strings.Contains(err.Error(), "not a multiple") {
			t.Errorf("got error %q, want mention of not a multiple", err)
		}
	})

	t.Run("byte string length not multiple of element size to any", func(t *testing.T) {
		// tag 82 (float64) with 4-byte content
		var v any
		err := Unmarshal(mustHexDecode("d8524400000000"), &v)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "not a multiple") {
			t.Errorf("got error %q, want mention of not a multiple", err)
		}
	})

	t.Run("element count exceeds max array elements", func(t *testing.T) {
		dm, err := DecOptions{MaxArrayElements: 16}.DecMode()
		if err != nil {
			t.Fatal(err)
		}
		// tag 65 (uint16) with 17 elements (34-byte content)
		var v []uint16
		err = dm.Unmarshal(mustHexDecode("d8415822"+strings.Repeat("0001", 17)), &v)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		var maxErr *MaxArrayElementsError
		if !errors.As(err, &maxErr) {
			t.Fatalf("got error type %T (%v), want *MaxArrayElementsError", err, err)
		}
	})

	t.Run("tag content not byte string", func(t *testing.T) {
		// tag 65 followed by text string
		var v []uint16
		err := Unmarshal(mustHexDecode("d8416161"), &v)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		var tagContentErr *InadmissibleTagContentTypeError
		if !errors.As(err, &tagContentErr) {
			t.Fatalf("got error type %T (%v), want *InadmissibleTagContentTypeError", err, err)
		}
	})

	t.Run("mismatched element type", func(t *testing.T) {
		// tag 65 (uint16) to []uint32
		var v []uint32
		err := Unmarshal(mustHexDecode("d84146000100020003"), &v)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		var typeErr *UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			t.Fatalf("got error type %T, want *UnmarshalTypeError", err)
		}
	})

	t.Run("float16 tag to []uint16", func(t *testing.T) {
		// tag 80 (float16) to []uint16: element types must match exactly
		var v []uint16
		err := Unmarshal(mustHexDecode("d850423c00"), &v)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		var typeErr *UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			t.Fatalf("got error type %T, want *UnmarshalTypeError", err)
		}
	})

	t.Run("typed array tag to non-slice destination", func(t *testing.T) {
		var v string
		err := Unmarshal(mustHexDecode("d84043010203"), &v)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		var typeErr *UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			t.Fatalf("got error type %T, want *UnmarshalTypeError", err)
		}
	})
}

func TestTypedArrayTagSetConflict(t *testing.T) {
	type myUint16s []uint16

	// Tag numbers 64-82 and 84-86 are built-in and cannot be registered.
	for _, num := range []uint64{64, 65, 72, 80, 82, 84, 86} {
		t.Run("tag "+strconv.Itoa(int(num))+" rejected", func(t *testing.T) {
			ts := NewTagSet()
			err := ts.Add(
				TagOptions{DecTag: DecTagOptional, EncTag: EncTagRequired},
				reflect.TypeOf(myUint16s{}),
				num,
			)
			if err == nil {
				t.Fatalf("expected error registering tag %d, got nil", num)
			}
			if !strings.Contains(err.Error(), "built-in") {
				t.Errorf("got error %q, want mention of built-in", err)
			}
		})
	}

	// Tag numbers 83 and 87 (float128) are unsupported and may be registered.
	for _, num := range []uint64{83, 87} {
		t.Run("tag "+strconv.Itoa(int(num))+" allowed", func(t *testing.T) {
			ts := NewTagSet()
			if err := ts.Add(
				TagOptions{DecTag: DecTagOptional, EncTag: EncTagRequired},
				reflect.TypeOf(myUint16s{}),
				num,
			); err != nil {
				t.Fatalf("registering tag %d returned error %v", num, err)
			}
		})
	}
}

func TestTypedArrayEncodeNil(t *testing.T) {
	data, err := Marshal(Uint16BigEndianSlice(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(data, []byte{0xf6}) {
		t.Errorf("got 0x%x, want 0xf6 (CBOR null)", data)
	}
}
