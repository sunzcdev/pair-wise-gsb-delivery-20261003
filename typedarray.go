// Copyright (c) Faye Amacker. All rights reserved.
// Licensed under the MIT License. See LICENSE in the project root for license information.

package cbor

import (
	"encoding/binary"
	"math"
	"reflect"
	"strconv"

	"github.com/x448/float16"
)

// This file implements CBOR typed arrays (RFC 8746), which encode homogeneous
// arrays of numbers as a tag (64-87) enclosing a byte string of packed values.
//
// Supported tag numbers and Go types:
//
//	64: Uint8Slice (uint8)                 68: Uint8Slice (uint8)
//	65: Uint16BigEndianSlice (uint16)      69: Uint16LittleEndianSlice (uint16)
//	66: Uint32BigEndianSlice (uint32)      70: Uint32LittleEndianSlice (uint32)
//	67: Uint64BigEndianSlice (uint64)      71: Uint64LittleEndianSlice (uint64)
//	72: Int8Slice (int8)                   76: Int8Slice (int8)
//	73: Int16BigEndianSlice (int16)        77: Int16LittleEndianSlice (int16)
//	74: Int32BigEndianSlice (int32)        78: Int32LittleEndianSlice (int32)
//	75: Int64BigEndianSlice (int64)        79: Int64LittleEndianSlice (int64)
//	80: Float16BigEndianSlice (float16)    84: Float16LittleEndianSlice (float16)
//	81: Float32BigEndianSlice (float32)    85: Float32LittleEndianSlice (float32)
//	82: Float64BigEndianSlice (float64)    86: Float64LittleEndianSlice (float64)
//
// Tags 83 and 87 (float128 arrays) are not supported because Go has no native
// 128-bit floating-point type.  They are treated as unrecognized tags (decoded
// to cbor.Tag) and may be registered in a TagSet for custom handling.
//
// Decoding a typed array tag to a value of type any produces the named slice
// type listed above.  Decoding to a Go slice (named or unnamed) succeeds when
// the slice element type matches the tag's element type; the tag number
// determines how the byte string content is interpreted, so decoding tag 69
// (little-endian) to []uint16 or Uint16BigEndianSlice produces the same values.
//
// Encoding a named slice type emits its corresponding tag number and a
// definite-length byte string.  Encoding a nil named slice emits CBOR null.
// Unnamed Go slices (e.g. []uint16) continue to encode as CBOR arrays.
//
// Tag numbers 64-82 and 84-86 are built-in and cannot be registered in a
// TagSet, same as tag numbers 0-3 and 55799.

// Uint8Slice is a slice of uint8 encoded as CBOR typed array (tag 64, RFC 8746).
// Tag 68 (uint8 little-endian) also decodes to Uint8Slice.
type Uint8Slice []uint8

// Uint16BigEndianSlice is a slice of uint16 encoded as CBOR typed array (tag 65, RFC 8746).
type Uint16BigEndianSlice []uint16

// Uint16LittleEndianSlice is a slice of uint16 encoded as CBOR typed array (tag 69, RFC 8746).
type Uint16LittleEndianSlice []uint16

// Uint32BigEndianSlice is a slice of uint32 encoded as CBOR typed array (tag 66, RFC 8746).
type Uint32BigEndianSlice []uint32

// Uint32LittleEndianSlice is a slice of uint32 encoded as CBOR typed array (tag 70, RFC 8746).
type Uint32LittleEndianSlice []uint32

// Uint64BigEndianSlice is a slice of uint64 encoded as CBOR typed array (tag 67, RFC 8746).
type Uint64BigEndianSlice []uint64

// Uint64LittleEndianSlice is a slice of uint64 encoded as CBOR typed array (tag 71, RFC 8746).
type Uint64LittleEndianSlice []uint64

// Int8Slice is a slice of int8 encoded as CBOR typed array (tag 72, RFC 8746).
// Tag 76 (sint8 little-endian) also decodes to Int8Slice.
type Int8Slice []int8

// Int16BigEndianSlice is a slice of int16 encoded as CBOR typed array (tag 73, RFC 8746).
type Int16BigEndianSlice []int16

// Int16LittleEndianSlice is a slice of int16 encoded as CBOR typed array (tag 77, RFC 8746).
type Int16LittleEndianSlice []int16

// Int32BigEndianSlice is a slice of int32 encoded as CBOR typed array (tag 74, RFC 8746).
type Int32BigEndianSlice []int32

// Int32LittleEndianSlice is a slice of int32 encoded as CBOR typed array (tag 78, RFC 8746).
type Int32LittleEndianSlice []int32

// Int64BigEndianSlice is a slice of int64 encoded as CBOR typed array (tag 75, RFC 8746).
type Int64BigEndianSlice []int64

// Int64LittleEndianSlice is a slice of int64 encoded as CBOR typed array (tag 79, RFC 8746).
type Int64LittleEndianSlice []int64

// Float16BigEndianSlice is a slice of float16 encoded as CBOR typed array (tag 80, RFC 8746).
type Float16BigEndianSlice []float16.Float16

// Float16LittleEndianSlice is a slice of float16 encoded as CBOR typed array (tag 84, RFC 8746).
type Float16LittleEndianSlice []float16.Float16

// Float32BigEndianSlice is a slice of float32 encoded as CBOR typed array (tag 81, RFC 8746).
type Float32BigEndianSlice []float32

// Float32LittleEndianSlice is a slice of float32 encoded as CBOR typed array (tag 85, RFC 8746).
type Float32LittleEndianSlice []float32

// Float64BigEndianSlice is a slice of float64 encoded as CBOR typed array (tag 82, RFC 8746).
type Float64BigEndianSlice []float64

// Float64LittleEndianSlice is a slice of float64 encoded as CBOR typed array (tag 86, RFC 8746).
type Float64LittleEndianSlice []float64

var (
	typeInt8Slice   = reflect.TypeFor[[]int8]()
	typeUint16Slice = reflect.TypeFor[[]uint16]()
	typeInt16Slice  = reflect.TypeFor[[]int16]()
	typeUint32Slice = reflect.TypeFor[[]uint32]()
	typeInt32Slice  = reflect.TypeFor[[]int32]()
	typeUint64Slice = reflect.TypeFor[[]uint64]()
	typeInt64Slice  = reflect.TypeFor[[]int64]()
	typeOfFloat16   = reflect.TypeFor[float16.Float16]()
	typeFloat16Sli  = reflect.TypeFor[[]float16.Float16]()
	typeFloat32Sli  = reflect.TypeFor[[]float32]()
	typeFloat64Sli  = reflect.TypeFor[[]float64]()

	typeOfUint8   = reflect.TypeFor[uint8]()
	typeOfInt8    = reflect.TypeFor[int8]()
	typeOfUint16  = reflect.TypeFor[uint16]()
	typeOfInt16   = reflect.TypeFor[int16]()
	typeOfUint32  = reflect.TypeFor[uint32]()
	typeOfInt32   = reflect.TypeFor[int32]()
	typeOfUint64  = reflect.TypeFor[uint64]()
	typeOfInt64   = reflect.TypeFor[int64]()
	typeOfFloat32 = reflect.TypeFor[float32]()
	typeOfFloat64 = reflect.TypeFor[float64]()
)

// typedArrayInfo describes an RFC 8746 typed array tag.
type typedArrayInfo struct {
	tagNum       uint64       // tag number used when encoding typ
	typ          reflect.Type // named slice type produced when decoding to any
	elemType     reflect.Type // element type of typ
	elemSize     int          // encoded size of each element in bytes
	littleEndian bool
}

// typedArrayInfosByTag maps supported RFC 8746 tag numbers (64-82, 84-86) to
// their typed array descriptions.  Unsupported tag numbers map to nil.
var typedArrayInfosByTag = func() (m [tagNumFloat128ArrayLittleEndian + 1]*typedArrayInfo) {
	infos := []typedArrayInfo{
		{tagNumUint8ArrayBigEndian, reflect.TypeFor[Uint8Slice](), typeOfUint8, 1, false},
		{tagNumUint16ArrayBigEndian, reflect.TypeFor[Uint16BigEndianSlice](), typeOfUint16, 2, false},
		{tagNumUint32ArrayBigEndian, reflect.TypeFor[Uint32BigEndianSlice](), typeOfUint32, 4, false},
		{tagNumUint64ArrayBigEndian, reflect.TypeFor[Uint64BigEndianSlice](), typeOfUint64, 8, false},
		{tagNumUint8ArrayLittleEndian, reflect.TypeFor[Uint8Slice](), typeOfUint8, 1, true},
		{tagNumUint16ArrayLittleEndian, reflect.TypeFor[Uint16LittleEndianSlice](), typeOfUint16, 2, true},
		{tagNumUint32ArrayLittleEndian, reflect.TypeFor[Uint32LittleEndianSlice](), typeOfUint32, 4, true},
		{tagNumUint64ArrayLittleEndian, reflect.TypeFor[Uint64LittleEndianSlice](), typeOfUint64, 8, true},
		{tagNumInt8ArrayBigEndian, reflect.TypeFor[Int8Slice](), typeOfInt8, 1, false},
		{tagNumInt16ArrayBigEndian, reflect.TypeFor[Int16BigEndianSlice](), typeOfInt16, 2, false},
		{tagNumInt32ArrayBigEndian, reflect.TypeFor[Int32BigEndianSlice](), typeOfInt32, 4, false},
		{tagNumInt64ArrayBigEndian, reflect.TypeFor[Int64BigEndianSlice](), typeOfInt64, 8, false},
		{tagNumInt8ArrayLittleEndian, reflect.TypeFor[Int8Slice](), typeOfInt8, 1, true},
		{tagNumInt16ArrayLittleEndian, reflect.TypeFor[Int16LittleEndianSlice](), typeOfInt16, 2, true},
		{tagNumInt32ArrayLittleEndian, reflect.TypeFor[Int32LittleEndianSlice](), typeOfInt32, 4, true},
		{tagNumInt64ArrayLittleEndian, reflect.TypeFor[Int64LittleEndianSlice](), typeOfInt64, 8, true},
		{tagNumFloat16ArrayBigEndian, reflect.TypeFor[Float16BigEndianSlice](), typeOfFloat16, 2, false},
		{tagNumFloat32ArrayBigEndian, reflect.TypeFor[Float32BigEndianSlice](), typeOfFloat32, 4, false},
		{tagNumFloat64ArrayBigEndian, reflect.TypeFor[Float64BigEndianSlice](), typeOfFloat64, 8, false},
		{tagNumFloat16ArrayLittleEndian, reflect.TypeFor[Float16LittleEndianSlice](), typeOfFloat16, 2, true},
		{tagNumFloat32ArrayLittleEndian, reflect.TypeFor[Float32LittleEndianSlice](), typeOfFloat32, 4, true},
		{tagNumFloat64ArrayLittleEndian, reflect.TypeFor[Float64LittleEndianSlice](), typeOfFloat64, 8, true},
	}
	for i := range infos {
		m[infos[i].tagNum] = &infos[i]
	}
	return m
}()

// typedArrayInfoByTagNum returns the typed array description for tagNum,
// or nil if tagNum is not a supported RFC 8746 typed array tag.
func typedArrayInfoByTagNum(tagNum uint64) *typedArrayInfo {
	if tagNum >= uint64(len(typedArrayInfosByTag)) {
		return nil
	}
	return typedArrayInfosByTag[tagNum]
}

// parseTypedArray decodes an RFC 8746 typed array to a value of type info.typ.
// It assumes the tag number was already consumed and the tag content is a
// well-formed byte string (enforced by validBuiltinTag).
func (d *decoder) parseTypedArray(info *typedArrayInfo) (reflect.Value, error) {
	b, _ := d.parseByteString()

	count, err := d.typedArrayElementCount(info, len(b))
	if err != nil {
		return reflect.Value{}, err
	}

	v := reflect.MakeSlice(info.typ, count, count)
	fillTypedArray(info, b, v)
	return v, nil
}

// parseTypedArrayToValue decodes an RFC 8746 typed array to v.
// It assumes the tag number was already consumed and the tag content is a
// well-formed byte string (enforced by validBuiltinTag).
func (d *decoder) parseTypedArrayToValue(info *typedArrayInfo, v reflect.Value, tInfo *typeInfo) error {
	tv, err := d.parseTypedArray(info)
	if err != nil {
		return err
	}

	if tInfo.nonPtrKind != reflect.Slice || tInfo.nonPtrType.Elem() != info.elemType {
		return &UnmarshalTypeError{CBORType: cborTypeTag.String(), GoType: tInfo.nonPtrType.String()}
	}

	v.Set(tv.Convert(tInfo.nonPtrType))
	return nil
}

// typedArrayElementCount returns the number of elements in a typed array byte
// string of dataLen bytes.  It rejects input before any large allocation if
// the byte string length is not a multiple of the element size, or if the
// element count exceeds the max array elements limit.
func (d *decoder) typedArrayElementCount(info *typedArrayInfo, dataLen int) (int, error) {
	if dataLen%info.elemSize != 0 {
		return 0, &UnmarshalTypeError{
			CBORType: cborTypeByteString.String(),
			GoType:   info.typ.String(),
			errorMsg: "byte string length " + strconv.Itoa(dataLen) +
				" is not a multiple of typed array element size " + strconv.Itoa(info.elemSize),
		}
	}

	// dataLen is bounded by the input length (an int), so count cannot overflow.
	count := dataLen / info.elemSize
	if count > d.dm.maxArrayElements {
		return 0, &MaxArrayElementsError{d.dm.maxArrayElements}
	}
	return count, nil
}

// fillTypedArray decodes count elements from b into v, a slice of type info.typ.
// It assumes len(b) is a multiple of info.elemSize.
func fillTypedArray(info *typedArrayInfo, b []byte, v reflect.Value) {
	var bo binary.ByteOrder = binary.BigEndian
	if info.littleEndian {
		bo = binary.LittleEndian
	}

	switch info.elemType {
	case typeOfUint8:
		copy(v.Convert(typeByteSlice).Bytes(), b)

	case typeOfInt8:
		s := v.Convert(typeInt8Slice).Interface().([]int8)
		for i := range s {
			s[i] = int8(b[i])
		}

	case typeOfUint16:
		s := v.Convert(typeUint16Slice).Interface().([]uint16)
		for i := range s {
			s[i] = bo.Uint16(b[i*2:])
		}

	case typeOfInt16:
		s := v.Convert(typeInt16Slice).Interface().([]int16)
		for i := range s {
			s[i] = int16(bo.Uint16(b[i*2:]))
		}

	case typeOfUint32:
		s := v.Convert(typeUint32Slice).Interface().([]uint32)
		for i := range s {
			s[i] = bo.Uint32(b[i*4:])
		}

	case typeOfInt32:
		s := v.Convert(typeInt32Slice).Interface().([]int32)
		for i := range s {
			s[i] = int32(bo.Uint32(b[i*4:]))
		}

	case typeOfUint64:
		s := v.Convert(typeUint64Slice).Interface().([]uint64)
		for i := range s {
			s[i] = bo.Uint64(b[i*8:])
		}

	case typeOfInt64:
		s := v.Convert(typeInt64Slice).Interface().([]int64)
		for i := range s {
			s[i] = int64(bo.Uint64(b[i*8:]))
		}

	case typeOfFloat16:
		s := v.Convert(typeFloat16Sli).Interface().([]float16.Float16)
		for i := range s {
			s[i] = float16.Frombits(bo.Uint16(b[i*2:]))
		}

	case typeOfFloat32:
		s := v.Convert(typeFloat32Sli).Interface().([]float32)
		for i := range s {
			s[i] = math.Float32frombits(bo.Uint32(b[i*4:]))
		}

	case typeOfFloat64:
		s := v.Convert(typeFloat64Sli).Interface().([]float64)
		for i := range s {
			s[i] = math.Float64frombits(bo.Uint64(b[i*8:]))
		}
	}
}

// appendTypedArrayHead appends a typed array tag number and a byte string head
// for payloadLen bytes to dst.
func appendTypedArrayHead(dst []byte, tagNum uint64, payloadLen int) []byte {
	dst = appendHead(dst, byte(cborTypeTag), tagNum)
	return appendHead(dst, byte(cborTypeByteString), uint64(payloadLen))
}

// MarshalCBOR encodes s as CBOR typed array (tag 64, RFC 8746).
// A nil slice is encoded as CBOR null.
func (s Uint8Slice) MarshalCBOR() ([]byte, error) {
	if s == nil {
		return []byte{cborNil}, nil
	}
	buf := appendTypedArrayHead(make([]byte, 0, encodedHeadLength(tagNumUint8ArrayBigEndian)+encodedHeadLength(uint64(len(s)))+len(s)), tagNumUint8ArrayBigEndian, len(s))
	return append(buf, s...), nil
}

// MarshalCBOR encodes s as CBOR typed array (tag 65, RFC 8746).
// A nil slice is encoded as CBOR null.
func (s Uint16BigEndianSlice) MarshalCBOR() ([]byte, error) {
	if s == nil {
		return []byte{cborNil}, nil
	}
	buf := appendTypedArrayHead(make([]byte, 0, encodedHeadLength(tagNumUint16ArrayBigEndian)+encodedHeadLength(uint64(len(s)*2))+len(s)*2), tagNumUint16ArrayBigEndian, len(s)*2)
	for _, e := range s {
		buf = binary.BigEndian.AppendUint16(buf, e)
	}
	return buf, nil
}

// MarshalCBOR encodes s as CBOR typed array (tag 69, RFC 8746).
// A nil slice is encoded as CBOR null.
func (s Uint16LittleEndianSlice) MarshalCBOR() ([]byte, error) {
	if s == nil {
		return []byte{cborNil}, nil
	}
	buf := appendTypedArrayHead(make([]byte, 0, encodedHeadLength(tagNumUint16ArrayLittleEndian)+encodedHeadLength(uint64(len(s)*2))+len(s)*2), tagNumUint16ArrayLittleEndian, len(s)*2)
	for _, e := range s {
		buf = binary.LittleEndian.AppendUint16(buf, e)
	}
	return buf, nil
}

// MarshalCBOR encodes s as CBOR typed array (tag 66, RFC 8746).
// A nil slice is encoded as CBOR null.
func (s Uint32BigEndianSlice) MarshalCBOR() ([]byte, error) {
	if s == nil {
		return []byte{cborNil}, nil
	}
	buf := appendTypedArrayHead(make([]byte, 0, encodedHeadLength(tagNumUint32ArrayBigEndian)+encodedHeadLength(uint64(len(s)*4))+len(s)*4), tagNumUint32ArrayBigEndian, len(s)*4)
	for _, e := range s {
		buf = binary.BigEndian.AppendUint32(buf, e)
	}
	return buf, nil
}

// MarshalCBOR encodes s as CBOR typed array (tag 70, RFC 8746).
// A nil slice is encoded as CBOR null.
func (s Uint32LittleEndianSlice) MarshalCBOR() ([]byte, error) {
	if s == nil {
		return []byte{cborNil}, nil
	}
	buf := appendTypedArrayHead(make([]byte, 0, encodedHeadLength(tagNumUint32ArrayLittleEndian)+encodedHeadLength(uint64(len(s)*4))+len(s)*4), tagNumUint32ArrayLittleEndian, len(s)*4)
	for _, e := range s {
		buf = binary.LittleEndian.AppendUint32(buf, e)
	}
	return buf, nil
}

// MarshalCBOR encodes s as CBOR typed array (tag 67, RFC 8746).
// A nil slice is encoded as CBOR null.
func (s Uint64BigEndianSlice) MarshalCBOR() ([]byte, error) {
	if s == nil {
		return []byte{cborNil}, nil
	}
	buf := appendTypedArrayHead(make([]byte, 0, encodedHeadLength(tagNumUint64ArrayBigEndian)+encodedHeadLength(uint64(len(s)*8))+len(s)*8), tagNumUint64ArrayBigEndian, len(s)*8)
	for _, e := range s {
		buf = binary.BigEndian.AppendUint64(buf, e)
	}
	return buf, nil
}

// MarshalCBOR encodes s as CBOR typed array (tag 71, RFC 8746).
// A nil slice is encoded as CBOR null.
func (s Uint64LittleEndianSlice) MarshalCBOR() ([]byte, error) {
	if s == nil {
		return []byte{cborNil}, nil
	}
	buf := appendTypedArrayHead(make([]byte, 0, encodedHeadLength(tagNumUint64ArrayLittleEndian)+encodedHeadLength(uint64(len(s)*8))+len(s)*8), tagNumUint64ArrayLittleEndian, len(s)*8)
	for _, e := range s {
		buf = binary.LittleEndian.AppendUint64(buf, e)
	}
	return buf, nil
}

// MarshalCBOR encodes s as CBOR typed array (tag 72, RFC 8746).
// A nil slice is encoded as CBOR null.
func (s Int8Slice) MarshalCBOR() ([]byte, error) {
	if s == nil {
		return []byte{cborNil}, nil
	}
	buf := appendTypedArrayHead(make([]byte, 0, encodedHeadLength(tagNumInt8ArrayBigEndian)+encodedHeadLength(uint64(len(s)))+len(s)), tagNumInt8ArrayBigEndian, len(s))
	for _, e := range s {
		buf = append(buf, byte(e))
	}
	return buf, nil
}

// MarshalCBOR encodes s as CBOR typed array (tag 73, RFC 8746).
// A nil slice is encoded as CBOR null.
func (s Int16BigEndianSlice) MarshalCBOR() ([]byte, error) {
	if s == nil {
		return []byte{cborNil}, nil
	}
	buf := appendTypedArrayHead(make([]byte, 0, encodedHeadLength(tagNumInt16ArrayBigEndian)+encodedHeadLength(uint64(len(s)*2))+len(s)*2), tagNumInt16ArrayBigEndian, len(s)*2)
	for _, e := range s {
		buf = binary.BigEndian.AppendUint16(buf, uint16(e))
	}
	return buf, nil
}

// MarshalCBOR encodes s as CBOR typed array (tag 77, RFC 8746).
// A nil slice is encoded as CBOR null.
func (s Int16LittleEndianSlice) MarshalCBOR() ([]byte, error) {
	if s == nil {
		return []byte{cborNil}, nil
	}
	buf := appendTypedArrayHead(make([]byte, 0, encodedHeadLength(tagNumInt16ArrayLittleEndian)+encodedHeadLength(uint64(len(s)*2))+len(s)*2), tagNumInt16ArrayLittleEndian, len(s)*2)
	for _, e := range s {
		buf = binary.LittleEndian.AppendUint16(buf, uint16(e))
	}
	return buf, nil
}

// MarshalCBOR encodes s as CBOR typed array (tag 74, RFC 8746).
// A nil slice is encoded as CBOR null.
func (s Int32BigEndianSlice) MarshalCBOR() ([]byte, error) {
	if s == nil {
		return []byte{cborNil}, nil
	}
	buf := appendTypedArrayHead(make([]byte, 0, encodedHeadLength(tagNumInt32ArrayBigEndian)+encodedHeadLength(uint64(len(s)*4))+len(s)*4), tagNumInt32ArrayBigEndian, len(s)*4)
	for _, e := range s {
		buf = binary.BigEndian.AppendUint32(buf, uint32(e))
	}
	return buf, nil
}

// MarshalCBOR encodes s as CBOR typed array (tag 78, RFC 8746).
// A nil slice is encoded as CBOR null.
func (s Int32LittleEndianSlice) MarshalCBOR() ([]byte, error) {
	if s == nil {
		return []byte{cborNil}, nil
	}
	buf := appendTypedArrayHead(make([]byte, 0, encodedHeadLength(tagNumInt32ArrayLittleEndian)+encodedHeadLength(uint64(len(s)*4))+len(s)*4), tagNumInt32ArrayLittleEndian, len(s)*4)
	for _, e := range s {
		buf = binary.LittleEndian.AppendUint32(buf, uint32(e))
	}
	return buf, nil
}

// MarshalCBOR encodes s as CBOR typed array (tag 75, RFC 8746).
// A nil slice is encoded as CBOR null.
func (s Int64BigEndianSlice) MarshalCBOR() ([]byte, error) {
	if s == nil {
		return []byte{cborNil}, nil
	}
	buf := appendTypedArrayHead(make([]byte, 0, encodedHeadLength(tagNumInt64ArrayBigEndian)+encodedHeadLength(uint64(len(s)*8))+len(s)*8), tagNumInt64ArrayBigEndian, len(s)*8)
	for _, e := range s {
		buf = binary.BigEndian.AppendUint64(buf, uint64(e))
	}
	return buf, nil
}

// MarshalCBOR encodes s as CBOR typed array (tag 79, RFC 8746).
// A nil slice is encoded as CBOR null.
func (s Int64LittleEndianSlice) MarshalCBOR() ([]byte, error) {
	if s == nil {
		return []byte{cborNil}, nil
	}
	buf := appendTypedArrayHead(make([]byte, 0, encodedHeadLength(tagNumInt64ArrayLittleEndian)+encodedHeadLength(uint64(len(s)*8))+len(s)*8), tagNumInt64ArrayLittleEndian, len(s)*8)
	for _, e := range s {
		buf = binary.LittleEndian.AppendUint64(buf, uint64(e))
	}
	return buf, nil
}

// MarshalCBOR encodes s as CBOR typed array (tag 80, RFC 8746).
// A nil slice is encoded as CBOR null.
func (s Float16BigEndianSlice) MarshalCBOR() ([]byte, error) {
	if s == nil {
		return []byte{cborNil}, nil
	}
	buf := appendTypedArrayHead(make([]byte, 0, encodedHeadLength(tagNumFloat16ArrayBigEndian)+encodedHeadLength(uint64(len(s)*2))+len(s)*2), tagNumFloat16ArrayBigEndian, len(s)*2)
	for _, e := range s {
		buf = binary.BigEndian.AppendUint16(buf, e.Bits())
	}
	return buf, nil
}

// MarshalCBOR encodes s as CBOR typed array (tag 84, RFC 8746).
// A nil slice is encoded as CBOR null.
func (s Float16LittleEndianSlice) MarshalCBOR() ([]byte, error) {
	if s == nil {
		return []byte{cborNil}, nil
	}
	buf := appendTypedArrayHead(make([]byte, 0, encodedHeadLength(tagNumFloat16ArrayLittleEndian)+encodedHeadLength(uint64(len(s)*2))+len(s)*2), tagNumFloat16ArrayLittleEndian, len(s)*2)
	for _, e := range s {
		buf = binary.LittleEndian.AppendUint16(buf, e.Bits())
	}
	return buf, nil
}

// MarshalCBOR encodes s as CBOR typed array (tag 81, RFC 8746).
// A nil slice is encoded as CBOR null.
func (s Float32BigEndianSlice) MarshalCBOR() ([]byte, error) {
	if s == nil {
		return []byte{cborNil}, nil
	}
	buf := appendTypedArrayHead(make([]byte, 0, encodedHeadLength(tagNumFloat32ArrayBigEndian)+encodedHeadLength(uint64(len(s)*4))+len(s)*4), tagNumFloat32ArrayBigEndian, len(s)*4)
	for _, e := range s {
		buf = binary.BigEndian.AppendUint32(buf, math.Float32bits(e))
	}
	return buf, nil
}

// MarshalCBOR encodes s as CBOR typed array (tag 85, RFC 8746).
// A nil slice is encoded as CBOR null.
func (s Float32LittleEndianSlice) MarshalCBOR() ([]byte, error) {
	if s == nil {
		return []byte{cborNil}, nil
	}
	buf := appendTypedArrayHead(make([]byte, 0, encodedHeadLength(tagNumFloat32ArrayLittleEndian)+encodedHeadLength(uint64(len(s)*4))+len(s)*4), tagNumFloat32ArrayLittleEndian, len(s)*4)
	for _, e := range s {
		buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(e))
	}
	return buf, nil
}

// MarshalCBOR encodes s as CBOR typed array (tag 82, RFC 8746).
// A nil slice is encoded as CBOR null.
func (s Float64BigEndianSlice) MarshalCBOR() ([]byte, error) {
	if s == nil {
		return []byte{cborNil}, nil
	}
	buf := appendTypedArrayHead(make([]byte, 0, encodedHeadLength(tagNumFloat64ArrayBigEndian)+encodedHeadLength(uint64(len(s)*8))+len(s)*8), tagNumFloat64ArrayBigEndian, len(s)*8)
	for _, e := range s {
		buf = binary.BigEndian.AppendUint64(buf, math.Float64bits(e))
	}
	return buf, nil
}

// MarshalCBOR encodes s as CBOR typed array (tag 86, RFC 8746).
// A nil slice is encoded as CBOR null.
func (s Float64LittleEndianSlice) MarshalCBOR() ([]byte, error) {
	if s == nil {
		return []byte{cborNil}, nil
	}
	buf := appendTypedArrayHead(make([]byte, 0, encodedHeadLength(tagNumFloat64ArrayLittleEndian)+encodedHeadLength(uint64(len(s)*8))+len(s)*8), tagNumFloat64ArrayLittleEndian, len(s)*8)
	for _, e := range s {
		buf = binary.LittleEndian.AppendUint64(buf, math.Float64bits(e))
	}
	return buf, nil
}
