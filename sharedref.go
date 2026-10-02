// Copyright (c) Faye Amacker. All rights reserved.
// Licensed under the MIT License. See LICENSE in the project root for license information.

package cbor

import (
	"errors"
	"reflect"
	"strconv"
)

// SharedRefMode specifies whether Go pointer identity is preserved during
// encoding and decoding by using CBOR tag 28 (shareable) and tag 29 (sharedref).
type SharedRefMode int

const (
	// SharedRefNone disables shared reference support. This is the default.
	// CBOR tags 28 and 29 are treated as regular (unrecognized) tags.
	SharedRefNone SharedRefMode = iota

	// SharedRefAuto enables shared reference support.
	//
	// Encoding: non-nil Go pointers to struct, array, slice, and map values
	// participate in shared reference encoding. A pointer that is encoded
	// more than once in the same top-level data item (a shared node or a
	// cycle) is encoded as CBOR tag 28 (shareable) at its first occurrence,
	// and each subsequent occurrence of the same pointer is encoded as CBOR
	// tag 29 (sharedref) enclosing the reference number assigned to the
	// first occurrence. Pointers encoded only once are encoded as the value
	// pointed to, without any tag. Distinct pointers are never merged, even
	// if the values they point to are equal. Reference numbers always start
	// at 0 for each top-level data item.
	//
	// Decoding: CBOR tag 28 (shareable) decodes its content and registers
	// the decoded value's address under the next reference number. CBOR tag
	// 29 (sharedref) decodes to the previously decoded value registered
	// under the enclosed reference number, restoring pointer identity for
	// shared nodes and cycles. The destination of a sharedref must be a
	// pointer (or any) of a type the registered value is assignable to.
	// The number of shared references per top-level data item is limited by
	// DecOptions.MaxSharedRefs, and nesting depth is limited by
	// DecOptions.MaxNestedLevels. Reference numbers are reset for each
	// top-level data item.
	SharedRefAuto

	maxSharedRefMode
)

func (srm SharedRefMode) valid() bool {
	return srm >= 0 && srm < maxSharedRefMode
}

// encModeForValue returns the encMode to use for encoding one top-level data
// item. If shared reference encoding is enabled and v contains a pointer
// that is encoded more than once, it returns a shallow copy of em carrying
// per-item shared reference state. Otherwise it returns em unchanged.
func (em *encMode) encModeForValue(v reflect.Value) *encMode {
	if em.sharedRefs == SharedRefNone || !v.IsValid() {
		return em
	}
	state := newSharedRefEncState(v)
	if state == nil {
		return em
	}
	emCopy := *em
	emCopy.sharedRefState = state
	return &emCopy
}

// MaxSharedRefsError is returned when the number of shared references in a
// CBOR data item exceeds the limit specified by DecOptions.MaxSharedRefs.
type MaxSharedRefsError struct {
	maxSharedRefs int
}

func (e *MaxSharedRefsError) Error() string {
	return "cbor: exceeded max shared references " + strconv.Itoa(e.maxSharedRefs)
}

// InvalidSharedRefError is returned when CBOR tag 29 (sharedref) encloses a
// reference number that doesn't identify a previously decoded shareable value
// in the same top-level data item.
type InvalidSharedRefError struct {
	RefNum uint64
}

func (e *InvalidSharedRefError) Error() string {
	return "cbor: invalid shared reference " + strconv.FormatUint(e.RefNum, 10) +
		": no preceding shareable value with this reference number"
}

// isShareablePtr reports whether v is a pointer value of a type that
// participates in shared reference encoding: non-nil pointers to struct,
// array, slice, or map values.
func isShareablePtr(v reflect.Value) bool {
	switch v.Type().Elem().Kind() {
	case reflect.Struct, reflect.Array, reflect.Slice, reflect.Map:
		return true
	default:
		return false
	}
}

// sharedRefEncState holds per-top-level-item encoding state for shared
// reference encoding. A new state is created for each top-level data item,
// so reference numbers never leak into the next top-level data item.
type sharedRefEncState struct {
	// shared contains addresses of pointers that are encoded more than once.
	shared map[uintptr]struct{}
	// refs maps pointer address to its assigned reference number.
	refs map[uintptr]uint64
	// nextRef is the next unassigned reference number.
	nextRef uint64
}

// newSharedRefEncState returns encoding state for one top-level data item,
// or nil if v doesn't contain any pointer encoded more than once.
func newSharedRefEncState(v reflect.Value) *sharedRefEncState {
	counts := make(map[uintptr]int)
	visited := make(map[sharedRefVisit]struct{})
	countSharedRefs(v, counts, visited)

	var shared map[uintptr]struct{}
	for ptr, count := range counts {
		if count > 1 {
			if shared == nil {
				shared = make(map[uintptr]struct{})
			}
			shared[ptr] = struct{}{}
		}
	}
	if len(shared) == 0 {
		return nil
	}
	return &sharedRefEncState{shared: shared, refs: make(map[uintptr]uint64)}
}

// sharedRefVisit identifies a visited composite value during the pre-pass
// that counts pointer references. Tracking visited values keeps the pre-pass
// from recursing forever on cyclic object graphs.
type sharedRefVisit struct {
	typ reflect.Type
	ptr uintptr
}

// countSharedRefs counts how many times each shareable pointer occurs in the
// object graph rooted at v. Only occurrences are counted; equal but distinct
// values are never merged because counts are keyed by pointer address.
func countSharedRefs(v reflect.Value, counts map[uintptr]int, visited map[sharedRefVisit]struct{}) {
	// Dereference interface and pointer chains, counting shareable pointers.
	for {
		switch v.Kind() {
		case reflect.Interface:
			if v.IsNil() {
				return
			}
			v = v.Elem()

		case reflect.Pointer:
			if v.IsNil() {
				return
			}
			if isShareablePtr(v) {
				ptr := v.Pointer()
				counts[ptr]++
				visit := sharedRefVisit{typ: v.Type(), ptr: ptr}
				if _, ok := visited[visit]; ok {
					return
				}
				visited[visit] = struct{}{}
			}
			v = v.Elem()

		default:
			goto dereferenced
		}
	}

dereferenced:
	switch v.Kind() {
	case reflect.Struct:
		t := v.Type()
		for i := range v.NumField() {
			// Skip unexported fields because they are not encoded.
			if t.Field(i).PkgPath != "" {
				continue
			}
			countSharedRefs(v.Field(i), counts, visited)
		}

	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice {
			// Empty slices have no elements to traverse and may share a zero
			// pointer with other empty slices.
			if v.Len() == 0 {
				return
			}
			visit := sharedRefVisit{typ: v.Type(), ptr: v.Pointer()}
			if _, ok := visited[visit]; ok {
				return
			}
			visited[visit] = struct{}{}
		}
		for i := range v.Len() {
			countSharedRefs(v.Index(i), counts, visited)
		}

	case reflect.Map:
		if v.IsNil() {
			return
		}
		visit := sharedRefVisit{typ: v.Type(), ptr: uintptr(v.UnsafePointer())}
		if _, ok := visited[visit]; ok {
			return
		}
		visited[visit] = struct{}{}
		iter := v.MapRange()
		for iter.Next() {
			countSharedRefs(iter.Key(), counts, visited)
			countSharedRefs(iter.Value(), counts, visited)
		}
	}
}

// encodeSharedPtr encodes pointer v using CBOR tag 28 (shareable) or tag 29
// (sharedref) when v is encoded more than once in the same top-level data
// item. It returns done=true if the complete encoding of v was written
// (sharedref case). Otherwise it returns v's pointee and the caller continues
// encoding the pointee (shareable case, after tag 28 was written, or
// non-shared case, after nothing was written).
func (s *sharedRefEncState) encodeSharedPtr(dst []byte, v reflect.Value) (newDst []byte, pointee reflect.Value, done bool) {
	ptr := v.Pointer()
	if refNum, ok := s.refs[ptr]; ok {
		// This pointer was already encoded: emit sharedref to its reference number.
		dst = appendHead(dst, byte(cborTypeTag), tagNumSharedRef)
		dst = appendHead(dst, byte(cborTypePositiveInt), refNum)
		return dst, v, true
	}
	if _, ok := s.shared[ptr]; ok {
		// First occurrence of a pointer that occurs more than once:
		// assign a reference number and emit shareable.
		s.refs[ptr] = s.nextRef
		s.nextRef++
		dst = appendHead(dst, byte(cborTypeTag), tagNumShareable)
	}
	return dst, v.Elem(), false
}

// addSharedRef registers v under the next shared reference number, enforcing
// the limit on the number of shared references per top-level data item.
func (d *decoder) addSharedRef(v reflect.Value) error {
	if len(d.sharedRefs) >= d.dm.maxSharedRefs {
		return &MaxSharedRefsError{d.dm.maxSharedRefs}
	}
	d.sharedRefs = append(d.sharedRefs, v)
	return nil
}

// sharedRefValue reads the reference number enclosed in CBOR tag 29
// (sharedref) and returns the value registered under that number.
// The tag 29 head must already be consumed.
func (d *decoder) sharedRefValue() (reflect.Value, error) {
	if t := d.nextCBORType(); t != cborTypePositiveInt {
		d.skip()
		return reflect.Value{}, errors.New("cbor: invalid shared reference: tag 29 content must be an unsigned integer, got " + t.String())
	}
	_, _, refNum := d.readHead()
	if refNum >= uint64(len(d.sharedRefs)) { //nolint:gosec
		return reflect.Value{}, &InvalidSharedRefError{RefNum: refNum}
	}
	return d.sharedRefs[refNum], nil
}

// parseShareableToValue decodes a CBOR tag 28 (shareable) or tag 29
// (sharedref) data item into v. It reports whether the next data item was
// such a tagged item (and was therefore handled).
func (d *decoder) parseShareableToValue(v reflect.Value) (bool, error) {
	off := d.off
	_, _, tagNum := d.readHead()

	switch tagNum {
	case tagNumShareable:
		// Allocate destination pointers and register the destination address
		// before decoding the content, so that cyclic references enclosed in
		// the content resolve to the value being decoded.
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				if !v.CanSet() {
					d.skip()
					return true, errors.New("cbor: cannot set new value for " + v.Type().String())
				}
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		if !v.CanAddr() {
			d.skip()
			return true, errors.New("cbor: cannot decode shareable value to unaddressable " + v.Type().String())
		}
		if err := d.addSharedRef(v.Addr()); err != nil {
			d.skip()
			return true, err
		}
		return true, d.parseToValue(v, getTypeInfo(v.Type()))

	case tagNumSharedRef:
		sv, err := d.sharedRefValue()
		if err != nil {
			return true, err
		}
		if !sv.Type().AssignableTo(v.Type()) {
			return true, &UnmarshalTypeError{
				CBORType: cborTypeTag.String(),
				GoType:   v.Type().String(),
				errorMsg: "cannot assign shared reference of type " + sv.Type().String(),
			}
		}
		v.Set(sv)
		return true, nil

	default:
		d.off = off
		return false, nil
	}
}

// parseShareable decodes a CBOR tag 28 (shareable) or tag 29 (sharedref)
// data item to a value of type any. It reports whether the next data item
// was such a tagged item (and was therefore handled).
func (d *decoder) parseShareable() (any, bool, error) {
	off := d.off
	_, _, tagNum := d.readHead()

	switch tagNum {
	case tagNumShareable:
		iv, err := d.parse(false)
		if err != nil {
			return nil, true, err
		}
		// The decoded value is registered after its content is decoded, so
		// cyclic sharedrefs enclosed in the content cannot resolve to it.
		rv := reflect.ValueOf(iv)
		if !rv.IsValid() {
			// Shareable content decoded to nil (e.g. CBOR null). Register a
			// placeholder so subsequent reference numbers stay consistent.
			rv = reflect.ValueOf(new(any))
		}
		if err := d.addSharedRef(rv); err != nil {
			return nil, true, err
		}
		return iv, true, nil

	case tagNumSharedRef:
		sv, err := d.sharedRefValue()
		if err != nil {
			return nil, true, err
		}
		return sv.Interface(), true, nil

	default:
		d.off = off
		return nil, false, nil
	}
}
