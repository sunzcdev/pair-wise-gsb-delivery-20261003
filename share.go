// Copyright (c) Faye Amacker. All rights reserved.
// Licensed under the MIT License. See LICENSE in the project root for license information.

package cbor

import (
	"errors"
	"fmt"
	"reflect"
)

const (
	tagNumSharedValue = 28
	tagNumSharedRef   = 29
)

// SharedRefMode specifies whether CBOR shared value and shared reference tags are used.
type SharedRefMode int

const (
	// SharedRefNone disables CBOR tags 28 and 29.
	SharedRefNone SharedRefMode = iota

	// SharedRefAllowed uses tag 28 to mark the first encoding of an eligible pointer and
	// tag 29 to encode later occurrences of that same pointer. Eligible values are
	// non-named, non-nil single pointers to structs, arrays, slices, or maps which are
	// reached through concrete pointer fields or elements. Values reached through an
	// interface and library-special types such as time.Time are not shared.
	SharedRefAllowed

	maxSharedRefMode
)

func (m SharedRefMode) valid() bool {
	return m >= 0 && m < maxSharedRefMode
}

type sharedIdentity struct {
	typ reflect.Type
	ptr uintptr
}

type sharedEncodeState struct {
	values map[sharedIdentity]uint64
}

func (s *sharedEncodeState) get(v reflect.Value) (uint64, bool) {
	if s == nil || s.values == nil {
		return 0, false
	}
	id, ok := s.values[sharedPointerIdentity(v)]
	return id, ok
}

func (s *sharedEncodeState) add(v reflect.Value) uint64 {
	if s.values == nil {
		s.values = make(map[sharedIdentity]uint64)
	}
	id := uint64(len(s.values))
	s.values[sharedPointerIdentity(v)] = id
	return id
}

func sharedPointerIdentity(v reflect.Value) sharedIdentity {
	for v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}
	return sharedIdentity{typ: v.Type(), ptr: v.Addr().Pointer()}
}

func isShareablePointer(v reflect.Value) bool {
	if v.Kind() != reflect.Pointer || v.Type().Elem().Kind() == reflect.Pointer {
		return false
	}
	return isShareablePointerType(v.Type())
}

func isShareablePointerType(t reflect.Type) bool {
	if t.Kind() != reflect.Pointer {
		return false
	}
	if t.Name() != "" {
		return false
	}
	t = t.Elem()
	if t.Kind() == reflect.Pointer {
		return false
	}
	return isShareableKindType(t)
}

func isShareableKindType(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Struct, reflect.Array, reflect.Slice, reflect.Map:
	default:
		return false
	}
	if t == typeTime || t == typeBigInt || t == typeTag || t == typeRawTag ||
		t == typeRawMessage || t == typeByteString || t == typeSimpleValue {
		return false
	}
	return true
}

func (em *encMode) encodeShared(dst []byte, v reflect.Value, f encodeFunc) ([]byte, error) {
	if em.sharedRef != SharedRefAllowed || em.shared == nil || !isShareablePointer(v) {
		return f(dst, em, v)
	}
	if v.IsNil() {
		return f(dst, em, v)
	}

	if id, ok := em.shared.get(v); ok {
		dst = appendHead(dst, byte(cborTypeTag), tagNumSharedRef)
		dst = appendHead(dst, byte(cborTypePositiveInt), id)
		return dst, nil
	}

	id := em.shared.add(v)
	dst = appendHead(dst, byte(cborTypeTag), tagNumSharedValue)
	_ = id
	return f(dst, em.withoutRegisteredTagFor(v.Type()), v)
}

func (em *encMode) withoutRegisteredTagFor(t reflect.Type) *encMode {
	if em.tags == nil || em.tags.getTagItemFromType(t) == nil {
		return em
	}
	encCopy := *em
	encCopy.tags = nil
	return &encCopy
}

type sharedDecodeState struct {
	values     []reflect.Value
	refs       int
	valuesSeen int
	refsSeen   int
	maxValues  int
	maxRefs    int
}

func (s *sharedDecodeState) resetParseCounts() {
	s.values = nil
	s.refs = 0
	s.valuesSeen = 0
	s.refsSeen = 0
}

func (d *decoder) beginSharedDecoding() {
	if d.dm.sharedRef != SharedRefAllowed || d.shared != nil {
		return
	}
	d.shared = &sharedDecodeState{
		maxValues: d.dm.maxSharedValues,
		maxRefs:   d.dm.maxSharedRefs,
	}
}

func (d *decoder) resetSharedDecoding() {
	d.shared = nil
}

func (d *decoder) beginSharedParse() {
	if d.shared != nil {
		d.shared.resetParseCounts()
	}
}

func (s *sharedDecodeState) countValidatedTag(tagNum uint64) error {
	switch tagNum {
	case tagNumSharedValue:
		s.valuesSeen++
		if s.valuesSeen > s.maxValues {
			return &MaxSharedValuesError{s.maxValues}
		}
	case tagNumSharedRef:
		s.refsSeen++
		if s.refsSeen > s.maxRefs {
			return &MaxSharedRefsError{s.maxRefs}
		}
	}
	return nil
}

type InvalidSharedRefError struct {
	id     uint64
	max    uint64
	reason string
}

func (e *InvalidSharedRefError) Error() string {
	if e.reason != "" {
		return "cbor: invalid shared reference: " + e.reason
	}
	return fmt.Sprintf("cbor: invalid shared reference %d (valid range is [0, %d])", e.id, e.max)
}

type MaxSharedValuesError struct {
	max int
}

func (e *MaxSharedValuesError) Error() string {
	return fmt.Sprintf("cbor: exceeded max number of shared values %d", e.max)
}

type MaxSharedRefsError struct {
	max int
}

func (e *MaxSharedRefsError) Error() string {
	return fmt.Sprintf("cbor: exceeded max number of shared references %d", e.max)
}

func (d *decoder) parseSharedValue(v reflect.Value) (bool, error) {
	if d.dm.sharedRef != SharedRefAllowed || d.nextCBORType() != cborTypeTag {
		return false, nil
	}

	off := d.off
	_, _, tagNum := d.readHead()
	switch tagNum {
	case tagNumSharedRef:
		if d.nextCBORType() != cborTypePositiveInt {
			d.skip()
			return true, &InvalidSharedRefError{reason: "tag 29 must be followed by an unsigned integer"}
		}
		_, _, id := d.readHead()
		if id > uint64(^uint(0)>>1) {
			return true, &InvalidSharedRefError{reason: "tag 29 reference number is too large"}
		}
		if int(id) >= len(d.shared.values) {
			return true, &InvalidSharedRefError{id: id, max: uint64(len(d.shared.values)) - 1}
		}
		d.shared.refs++
		if d.shared.refs > d.shared.maxRefs {
			return true, &MaxSharedRefsError{d.shared.maxRefs}
		}
		return true, d.setSharedRef(v, d.shared.values[id])

	case tagNumSharedValue:
		if v.Kind() != reflect.Pointer || v.Type().Elem().Kind() == reflect.Pointer {
			d.skip()
			return true, &UnmarshalTypeError{
				CBORType: cborTypeTag.String(),
				GoType:   v.Type().String(),
				errorMsg: "tag 28 requires a pointer to struct, array, slice, or map",
			}
		}
		if !v.CanSet() {
			d.skip()
			return true, errors.New("cbor: cannot set shared reference for " + v.Type().String())
		}

		if !isShareablePointer(v) {
			d.skip()
			return true, &UnmarshalTypeError{
				CBORType: cborTypeTag.String(),
				GoType:   v.Type().String(),
				errorMsg: "tag 28 can only be decoded into pointer to struct, array, slice, or map",
			}
		}

		if len(d.shared.values) >= d.shared.maxValues {
			return true, &MaxSharedValuesError{d.shared.maxValues}
		}
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		canonical := v.Elem()
		d.shared.values = append(d.shared.values, canonical.Addr())

		if err := d.parseToValue(canonical, getTypeInfo(canonical.Type())); err != nil {
			return true, err
		}
		return true, nil

	default:
		d.off = off
		return false, nil
	}
}

func (d *decoder) setSharedRef(v, canonical reflect.Value) error {
	if v.Kind() == reflect.Interface {
		if !v.IsNil() {
			v = v.Elem()
		} else if canonical.Type().Implements(v.Type()) {
			v.Set(canonical)
			return nil
		}
	}
	if v.Kind() != reflect.Pointer || !v.CanSet() {
		return &UnmarshalTypeError{
			CBORType: cborTypeTag.String(),
			GoType:   v.Type().String(),
			errorMsg: "tag 29 requires a settable pointer destination",
		}
	}
	if !canonical.Type().AssignableTo(v.Type()) {
		return &UnmarshalTypeError{
			CBORType: cborTypeTag.String(),
			GoType:   v.Type().String(),
			errorMsg: "cannot use shared reference of type " + canonical.Type().String(),
		}
	}
	v.Set(canonical)
	return nil
}
