// Copyright (c) Faye Amacker. All rights reserved.
// Licensed under the MIT License. See LICENSE in the project root for license information.

package cbor

import (
	"encoding/binary"
	"fmt"
	"math"
	"reflect"

	"github.com/x448/float16"
)

// This file implements built-in support for RFC 8746 typed arrays (CBOR tag
// numbers 64-87).  A typed array is a tag enclosing a byte string that holds
// a sequence of fixed-width numbers, so it maps directly to a Go slice of
// numbers without manually unpacking the byte string.
//
// Covered element widths: uint8/16/32/64, int8/16/32/64, and float16/32/64,
// each in both big-endian and little-endian form.  Tags 83 and 87 (float128)
// are not covered because Go has no native 128-bit floating-point type; they
// remain unrecognized tags.
//
// Decoding:
//   - Decoding to any decodes a recognized typed array tag to one of the
//     named slice types defined below (Uint8Array, Uint16Array, ...,
//     Float64Array).
//   - Decoding to a Go slice or array whose element type matches the tag
//     (e.g. []uint16 for tag 65 or 69, including user-defined types based on
//     []uint16) fills it directly from the byte string.
//
// Encoding: the named slice types defined below encode as a typed array tag
// (always the big-endian variant) followed by a byte string.  Plain Go slices
// such as []uint16 still encode as CBOR arrays; only the named types defined
// here produce typed array tags.
//
// Conflict rules: tag numbers 64-87 are reserved by this codec and cannot be
// registered in a TagSet, and the named slice types defined here cannot be
// used as TagSet content types.  TagSet.Add returns an error for both cases.

// Named slice types for RFC 8746 typed arrays.  Decoding a typed array tag to
// any produces one of these types, and encoding one of these types emits the
// corresponding typed array tag (big-endian form) followed by a byte string.
type (
	// Uint8Array is a slice of uint8 encoded as CBOR tag 64 (uint8 array).
	Uint8Array []uint8
	// Uint16Array is a slice of uint16 encoded as CBOR tag 65 (big-endian uint16 array).
	Uint16Array []uint16
	// Uint32Array is a slice of uint32 encoded as CBOR tag 66 (big-endian uint32 array).
	Uint32Array []uint32
	// Uint64Array is a slice of uint64 encoded as CBOR tag 67 (big-endian uint64 array).
	Uint64Array []uint64
	// Int8Array is a slice of int8 encoded as CBOR tag 72 (int8 array).
	Int8Array []int8
	// Int16Array is a slice of int16 encoded as CBOR tag 73 (big-endian int16 array).
	Int16Array []int16
	// Int32Array is a slice of int32 encoded as CBOR tag 74 (big-endian int32 array).
	Int32Array []int32
	// Int64Array is a slice of int64 encoded as CBOR tag 75 (big-endian int64 array).
	Int64Array []int64
	// Float16Array is a slice of float16 encoded as CBOR tag 80 (big-endian float16 array).
	Float16Array []float16.Float16
	// Float32Array is a slice of float32 encoded as CBOR tag 81 (big-endian float32 array).
	Float32Array []float32
	// Float64Array is a slice of float64 encoded as CBOR tag 82 (big-endian float64 array).
	Float64Array []float64
)

// RFC 8746 typed array tag numbers.
const (
	tagNumUint8Array      = 64
	tagNumUint16ArrayBE   = 65
	tagNumUint32ArrayBE   = 66
	tagNumUint64ArrayBE   = 67
	tagNumUint8ArrayLE    = 68
	tagNumUint16ArrayLE   = 69
	tagNumUint32ArrayLE   = 70
	tagNumUint64ArrayLE   = 71
	tagNumInt8Array       = 72
	tagNumInt16ArrayBE    = 73
	tagNumInt32ArrayBE    = 74
	tagNumInt64ArrayBE    = 75
	tagNumInt8ArrayLE     = 76
	tagNumInt16ArrayLE    = 77
	tagNumInt32ArrayLE    = 78
	tagNumInt64ArrayLE    = 79
	tagNumFloat16ArrayBE  = 80
	tagNumFloat32ArrayBE  = 81
	tagNumFloat64ArrayBE  = 82
	tagNumFloat128ArrayBE = 83 // not supported, treated as unrecognized tag
	tagNumFloat16ArrayLE  = 84
	tagNumFloat32ArrayLE  = 85
	tagNumFloat64ArrayLE  = 86
	tagNumFloat128ArrayLE = 87 // not supported, treated as unrecognized tag
)

var typeFloat16 = reflect.TypeFor[float16.Float16]()

// typedArrayTag describes one RFC 8746 typed array tag.
type typedArrayTag struct {
	beTagNum     uint64       // big-endian (canonical) tag number, used when encoding
	leTagNum     uint64       // little-endian tag number
	elemSize     int          // element width in bytes
	elemKind     reflect.Kind // element kind matched when decoding to a typed Go slice or array
	elemType     reflect.Type // non-nil only for float16, which is matched by type instead of kind
	sliceType    reflect.Type // named slice type produced when decoding to any
	littleEndian bool         // true if this tag number is the little-endian variant
}

var (
	typeUint8Array   = reflect.TypeFor[Uint8Array]()
	typeUint16Array  = reflect.TypeFor[Uint16Array]()
	typeUint32Array  = reflect.TypeFor[Uint32Array]()
	typeUint64Array  = reflect.TypeFor[Uint64Array]()
	typeInt8Array    = reflect.TypeFor[Int8Array]()
	typeInt16Array   = reflect.TypeFor[Int16Array]()
	typeInt32Array   = reflect.TypeFor[Int32Array]()
	typeInt64Array   = reflect.TypeFor[Int64Array]()
	typeFloat16Array = reflect.TypeFor[Float16Array]()
	typeFloat32Array = reflect.TypeFor[Float32Array]()
	typeFloat64Array = reflect.TypeFor[Float64Array]()
)

// typedArrayTags maps both big-endian and little-endian tag numbers to their
// typed array description.  Tags 83 and 87 (float128) are intentionally absent.
var typedArrayTags = map[uint64]typedArrayTag{}

// typedArrayTagBySliceType maps each named slice type to the big-endian typed
// array tag used to encode it.
var typedArrayTagBySliceType = map[reflect.Type]typedArrayTag{}

func init() {
	for _, ta := range []typedArrayTag{
		{beTagNum: tagNumUint8Array, leTagNum: tagNumUint8ArrayLE, elemSize: 1, elemKind: reflect.Uint8, sliceType: typeUint8Array},
		{beTagNum: tagNumUint16ArrayBE, leTagNum: tagNumUint16ArrayLE, elemSize: 2, elemKind: reflect.Uint16, sliceType: typeUint16Array},
		{beTagNum: tagNumUint32ArrayBE, leTagNum: tagNumUint32ArrayLE, elemSize: 4, elemKind: reflect.Uint32, sliceType: typeUint32Array},
		{beTagNum: tagNumUint64ArrayBE, leTagNum: tagNumUint64ArrayLE, elemSize: 8, elemKind: reflect.Uint64, sliceType: typeUint64Array},
		{beTagNum: tagNumInt8Array, leTagNum: tagNumInt8ArrayLE, elemSize: 1, elemKind: reflect.Int8, sliceType: typeInt8Array},
		{beTagNum: tagNumInt16ArrayBE, leTagNum: tagNumInt16ArrayLE, elemSize: 2, elemKind: reflect.Int16, sliceType: typeInt16Array},
		{beTagNum: tagNumInt32ArrayBE, leTagNum: tagNumInt32ArrayLE, elemSize: 4, elemKind: reflect.Int32, sliceType: typeInt32Array},
		{beTagNum: tagNumInt64ArrayBE, leTagNum: tagNumInt64ArrayLE, elemSize: 8, elemKind: reflect.Int64, sliceType: typeInt64Array},
		{beTagNum: tagNumFloat16ArrayBE, leTagNum: tagNumFloat16ArrayLE, elemSize: 2, elemKind: reflect.Uint16, elemType: typeFloat16, sliceType: typeFloat16Array},
		{beTagNum: tagNumFloat32ArrayBE, leTagNum: tagNumFloat32ArrayLE, elemSize: 4, elemKind: reflect.Float32, sliceType: typeFloat32Array},
		{beTagNum: tagNumFloat64ArrayBE, leTagNum: tagNumFloat64ArrayLE, elemSize: 8, elemKind: reflect.Float64, sliceType: typeFloat64Array},
	} {
		typedArrayTags[ta.beTagNum] = ta
		ta.littleEndian = true
		typedArrayTags[ta.leTagNum] = ta
		ta.littleEndian = false
		typedArrayTagBySliceType[ta.sliceType] = ta
	}
}

// isTypedArrayTagNum reports whether tagNum is in the RFC 8746 typed array
// range (64-87), including the unsupported float128 tags 83 and 87.
func isTypedArrayTagNum(tagNum uint64) bool {
	return tagNum >= tagNumUint8Array && tagNum <= tagNumFloat128ArrayLE
}

// typedArrayElementCount returns the number of elements in a typed array whose
// byte string content is byteLen bytes long.  It rejects, before any slice is
// allocated, lengths that are not a multiple of the element size and element
// counts whose storage size computation would overflow int.
func typedArrayElementCount(byteLen, elemSize int) (int, error) {
	if byteLen%elemSize != 0 {
		return 0, fmt.Errorf("cbor: invalid typed array: byte string length %d is not a multiple of element size %d", byteLen, elemSize)
	}
	count := byteLen / elemSize
	if count > math.MaxInt/elemSize {
		// Unreachable with count derived from len([]byte), but guard against
		// overflow before count is used to size an allocation.
		return 0, fmt.Errorf("cbor: invalid typed array: element count %d with element size %d overflows int", count, elemSize)
	}
	return count, nil
}

// matchesType reports whether a Go slice or array described by tInfo can hold
// elements of this typed array.
func (ta typedArrayTag) matchesType(tInfo *typeInfo) bool {
	if tInfo.nonPtrKind != reflect.Slice && tInfo.nonPtrKind != reflect.Array {
		return false
	}
	elemTypeInfo := tInfo.elemTypeInfo
	if ta.elemType == typeFloat16 {
		// float16.Float16 has kind uint16, so it must be matched by type.
		return elemTypeInfo.typ == typeFloat16
	}
	if elemTypeInfo.typ == typeFloat16 {
		// Don't match an integer tag to a float16 element type.
		return false
	}
	return elemTypeInfo.kind == ta.elemKind
}

// decodeTypedArrayToAny decodes a typed array byte string to the named slice
// type registered for its tag number.
func decodeTypedArrayToAny(ta typedArrayTag, b []byte) (any, error) {
	count, err := typedArrayElementCount(len(b), ta.elemSize)
	if err != nil {
		return nil, err
	}
	rv := reflect.MakeSlice(ta.sliceType, count, count)
	fillTypedArray(rv, b, ta)
	return rv.Interface(), nil
}

// parseTypedArrayToValue decodes a typed array byte string to a Go slice or
// array.  It assumes the tag number matches tInfo (see matchesType).
func (d *decoder) parseTypedArrayToValue(v reflect.Value, tInfo *typeInfo, ta typedArrayTag) error {
	b, _ := d.parseByteString()
	count, err := typedArrayElementCount(len(b), ta.elemSize)
	if err != nil {
		return &UnmarshalTypeError{
			CBORType: cborTypeByteString.String(),
			GoType:   tInfo.nonPtrType.String(),
			errorMsg: err.Error(),
		}
	}
	if tInfo.nonPtrKind == reflect.Slice {
		if v.IsNil() || v.Cap() < count || count == 0 {
			v.Set(reflect.MakeSlice(tInfo.nonPtrType, count, count))
		}
		v.SetLen(count)
	}
	fillTypedArray(v, b, ta)
	return nil
}

// fillTypedArray fills a Go slice or array from a typed array byte string.
// If the destination is a fixed-size array longer than the element count, the
// remaining elements are zeroed; extra elements are dropped, matching how
// CBOR arrays are decoded to Go arrays.
func fillTypedArray(rv reflect.Value, b []byte, ta typedArrayTag) {
	n := rv.Len()
	count := len(b) / ta.elemSize
	if count > n {
		count = n
	}
	b = b[:count*ta.elemSize]

	switch {
	case ta.elemType == typeFloat16:
		// float16.Float16 has kind uint16; set the raw bits directly.
		for i := range count {
			rv.Index(i).SetUint(uint64(uint16FromBytes(b[i*2:], ta.littleEndian)))
		}

	case ta.elemKind == reflect.Float32:
		for i := range count {
			rv.Index(i).SetFloat(float64(math.Float32frombits(uint32FromBytes(b[i*4:], ta.littleEndian))))
		}

	case ta.elemKind == reflect.Float64:
		for i := range count {
			rv.Index(i).SetFloat(math.Float64frombits(uint64FromBytes(b[i*8:], ta.littleEndian)))
		}

	case ta.elemKind == reflect.Uint8:
		for i := range count {
			rv.Index(i).SetUint(uint64(b[i]))
		}

	case ta.elemKind == reflect.Int8:
		for i := range count {
			rv.Index(i).SetInt(int64(int8(b[i])))
		}

	case ta.elemKind == reflect.Uint16:
		for i := range count {
			rv.Index(i).SetUint(uint64(uint16FromBytes(b[i*2:], ta.littleEndian)))
		}

	case ta.elemKind == reflect.Int16:
		for i := range count {
			rv.Index(i).SetInt(int64(int16(uint16FromBytes(b[i*2:], ta.littleEndian))))
		}

	case ta.elemKind == reflect.Uint32:
		for i := range count {
			rv.Index(i).SetUint(uint64(uint32FromBytes(b[i*4:], ta.littleEndian)))
		}

	case ta.elemKind == reflect.Int32:
		for i := range count {
			rv.Index(i).SetInt(int64(int32(uint32FromBytes(b[i*4:], ta.littleEndian))))
		}

	case ta.elemKind == reflect.Uint64:
		for i := range count {
			rv.Index(i).SetUint(uint64FromBytes(b[i*8:], ta.littleEndian))
		}

	case ta.elemKind == reflect.Int64:
		for i := range count {
			rv.Index(i).SetInt(int64(uint64FromBytes(b[i*8:], ta.littleEndian)))
		}
	}

	// Set remaining Go array elements to zero values.
	for i := count; i < n; i++ {
		rv.Index(i).SetZero()
	}
}

func uint16FromBytes(b []byte, littleEndian bool) uint16 {
	if littleEndian {
		return binary.LittleEndian.Uint16(b)
	}
	return binary.BigEndian.Uint16(b)
}

func uint32FromBytes(b []byte, littleEndian bool) uint32 {
	if littleEndian {
		return binary.LittleEndian.Uint32(b)
	}
	return binary.BigEndian.Uint32(b)
}

func uint64FromBytes(b []byte, littleEndian bool) uint64 {
	if littleEndian {
		return binary.LittleEndian.Uint64(b)
	}
	return binary.BigEndian.Uint64(b)
}

// encodeTypedArray encodes a named typed array slice type as a big-endian
// RFC 8746 typed array tag followed by a byte string.
func encodeTypedArray(dst []byte, em *encMode, v reflect.Value) ([]byte, error) {
	if v.Kind() == reflect.Slice && v.IsNil() && em.nilContainers == NilContainerAsNull {
		return append(dst, cborNil), nil
	}
	ta := typedArrayTagBySliceType[v.Type()]
	dst = appendHead(dst, byte(cborTypeTag), ta.beTagNum)
	n := v.Len()
	dst = appendHead(dst, byte(cborTypeByteString), uint64(n*ta.elemSize)) //nolint:gosec
	if ta.elemKind == reflect.Uint8 {
		return append(dst, v.Bytes()...), nil
	}
	for i := range n {
		e := v.Index(i)
		switch ta.elemKind {
		case reflect.Int8:
			dst = append(dst, byte(e.Int()))
		case reflect.Uint16: // also covers float16.Float16 (raw bits)
			dst = binary.BigEndian.AppendUint16(dst, uint16(e.Uint()))
		case reflect.Int16:
			dst = binary.BigEndian.AppendUint16(dst, uint16(int16(e.Int())))
		case reflect.Uint32:
			dst = binary.BigEndian.AppendUint32(dst, uint32(e.Uint()))
		case reflect.Int32:
			dst = binary.BigEndian.AppendUint32(dst, uint32(int32(e.Int())))
		case reflect.Uint64:
			dst = binary.BigEndian.AppendUint64(dst, e.Uint())
		case reflect.Int64:
			dst = binary.BigEndian.AppendUint64(dst, uint64(e.Int()))
		case reflect.Float32:
			dst = binary.BigEndian.AppendUint32(dst, math.Float32bits(float32(e.Float())))
		case reflect.Float64:
			dst = binary.BigEndian.AppendUint64(dst, math.Float64bits(e.Float()))
		}
	}
	return dst, nil
}
