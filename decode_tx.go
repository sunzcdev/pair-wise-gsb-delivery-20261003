package msgpack

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
)

// UnmarshalTransactional is like Unmarshal, but decodes atomically:
// it either fully replaces the value pointed to by v or leaves it unchanged.
// See Decoder.DecodeTransactional for the exact semantics and limitations.
func UnmarshalTransactional(data []byte, v interface{}) error {
	dec := GetDecoder()
	dec.UsePreallocateValues(true)
	dec.Reset(bytes.NewReader(data))
	err := dec.DecodeTransactional(v)

	PutDecoder(dec)

	return err
}

// DecodeTransactional decodes the next MessagePack value into v atomically:
// on success the decoded value is committed to v, on failure v (and every
// map, slice, and pointer reachable from it) is left exactly as it was.
// It is intended for use cases like configuration hot-reloading, where a
// failed decode must not leave the target object half-updated.
//
// To provide the guarantee, DecodeTransactional first deep-copies *v,
// decodes into the copy, and only then assigns the copy back to *v. The
// copy preserves the sharing topology of the original graph: if several
// fields of *v point to the same map, slice, or pointer, the corresponding
// fields of the copy (and hence of the committed value) still share a
// single map, slice, or pointer.
//
// Reference semantics of a successful decode: the committed value is a
// fresh object graph. Unlike Decode, which mutates existing maps, slices,
// and pointers in place, DecodeTransactional replaces them. References to
// the old graph captured before the call (e.g. a sub-map stored in a
// separate variable) keep pointing to the old, unmodified data. Sharing
// *within* the new graph mirrors the old one as described above.
//
// Limitations and support boundaries:
//
//   - The rollback guarantee covers only the value graph reachable from v.
//     Side effects outside that graph (writes to globals, files, channels,
//     or objects captured by a decoder) are never rolled back.
//   - Types implementing CustomDecoder, Unmarshaler, encoding.BinaryUnmarshaler,
//     or encoding.TextUnmarshaler are rejected with an error before any
//     decoding happens, because the library cannot verify that such decoders
//     are free of external side effects. Decoders registered with Register
//     or RegisterExtDecoder (e.g. the built-in time.Time decoder) are
//     trusted to mutate only the value they are given.
//   - Unexported struct fields are copied shallowly. State reachable only
//     through unexported fields is shared with the original value and is
//     not rolled back. Built-in decoders never mutate such state in place.
//   - Slices that share a backing array with different offsets or lengths
//     are copied independently; their sharing is not preserved.
func (d *Decoder) DecodeTransactional(v interface{}) error {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return errors.New("msgpack: DecodeTransactional(nil)")
	}
	if rv.Kind() != reflect.Ptr {
		return fmt.Errorf("msgpack: DecodeTransactional(non-pointer %T)", v)
	}
	if rv.IsNil() {
		return fmt.Errorf("msgpack: DecodeTransactional(non-settable %T)", v)
	}

	target := rv.Elem()
	if err := checkTransactionalType(target.Type(), d.structTag, make(map[reflect.Type]bool)); err != nil {
		return err
	}

	cloner := newTxCloner()
	clone, err := cloner.clone(target)
	if err != nil {
		return err
	}

	if err := d.DecodeValue(clone); err != nil {
		return err
	}

	target.Set(clone)
	return nil
}

//------------------------------------------------------------------------------

// checkTransactionalType rejects types whose decoding hooks may have side
// effects that the library cannot roll back. It walks the same fields the
// decoder would visit, so ignored and unexported fields are not checked.
func checkTransactionalType(typ reflect.Type, structTag string, seen map[reflect.Type]bool) error {
	if seen[typ] {
		return nil
	}
	seen[typ] = true

	if err := checkCustomDecoderType(typ); err != nil {
		return err
	}

	switch typ.Kind() {
	case reflect.Ptr, reflect.Slice, reflect.Array:
		return checkTransactionalType(typ.Elem(), structTag, seen)
	case reflect.Map:
		if err := checkTransactionalType(typ.Key(), structTag, seen); err != nil {
			return err
		}
		return checkTransactionalType(typ.Elem(), structTag, seen)
	case reflect.Struct:
		fs := structs.Fields(typ, structTag)
		for _, f := range fs.List {
			fieldType := txFieldType(typ, f.index)
			if err := checkTransactionalType(fieldType, structTag, seen); err != nil {
				return err
			}
		}
	}
	return nil
}

func txFieldType(typ reflect.Type, index []int) reflect.Type {
	for _, i := range index {
		if typ.Kind() == reflect.Ptr {
			typ = typ.Elem()
		}
		typ = typ.Field(i).Type
	}
	return typ
}

func checkCustomDecoderType(typ reflect.Type) error {
	if isRegisteredExtType(typ) ||
		(typ.Kind() == reflect.Ptr && isRegisteredExtType(typ.Elem())) {
		// Types registered via RegisterExtDecoder (e.g. time.Time) are
		// trusted to mutate only the value they decode into.
		return nil
	}
	if implementsCustomDecoder(typ) {
		return fmt.Errorf(
			"msgpack: DecodeTransactional does not support %s: "+
				"custom decoders may have side effects that cannot be rolled back", typ)
	}
	return nil
}

func implementsCustomDecoder(typ reflect.Type) bool {
	if typ.Implements(customDecoderType) ||
		typ.Implements(unmarshalerType) ||
		typ.Implements(binaryUnmarshalerType) ||
		typ.Implements(textUnmarshalerType) {
		return true
	}
	if typ.Kind() != reflect.Ptr {
		ptr := reflect.PtrTo(typ)
		return ptr.Implements(customDecoderType) ||
			ptr.Implements(unmarshalerType) ||
			ptr.Implements(binaryUnmarshalerType) ||
			ptr.Implements(textUnmarshalerType)
	}
	return false
}

func isRegisteredExtType(typ reflect.Type) bool {
	for _, info := range extTypes {
		if info.Type == typ {
			return true
		}
	}
	return false
}

//------------------------------------------------------------------------------

// txVisit identifies an already cloned pointer, map, or slice so that
// values shared between several fields are cloned exactly once and the
// clone keeps the sharing topology of the original.
type txVisit struct {
	typ      reflect.Type
	ptr      uintptr
	len, cap int // slices only: distinguish sub-slices of one backing array
}

type txCloner struct {
	visited map[txVisit]reflect.Value
}

func newTxCloner() *txCloner {
	return &txCloner{
		visited: make(map[txVisit]reflect.Value),
	}
}

//nolint:gocyclo
func (c *txCloner) clone(v reflect.Value) (reflect.Value, error) {
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			return reflect.Zero(v.Type()), nil
		}
		key := txVisit{typ: v.Type(), ptr: v.Pointer()}
		if prev, ok := c.visited[key]; ok {
			return prev, nil
		}
		clone := reflect.New(v.Type().Elem())
		c.visited[key] = clone
		elem, err := c.clone(v.Elem())
		if err != nil {
			return reflect.Value{}, err
		}
		clone.Elem().Set(elem)
		return clone, nil

	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type()), nil
		}
		key := txVisit{typ: v.Type(), ptr: v.Pointer()}
		if prev, ok := c.visited[key]; ok {
			return prev, nil
		}
		clone := reflect.MakeMapWithSize(v.Type(), v.Len())
		c.visited[key] = clone
		iter := v.MapRange()
		for iter.Next() {
			k, err := c.clone(iter.Key())
			if err != nil {
				return reflect.Value{}, err
			}
			val, err := c.clone(iter.Value())
			if err != nil {
				return reflect.Value{}, err
			}
			clone.SetMapIndex(k, val)
		}
		return clone, nil

	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type()), nil
		}
		key := txVisit{typ: v.Type(), ptr: v.Pointer(), len: v.Len(), cap: v.Cap()}
		if prev, ok := c.visited[key]; ok {
			return prev, nil
		}
		clone := reflect.MakeSlice(v.Type(), v.Len(), v.Cap())
		c.visited[key] = clone
		for i := 0; i < v.Len(); i++ {
			elem, err := c.clone(v.Index(i))
			if err != nil {
				return reflect.Value{}, err
			}
			clone.Index(i).Set(elem)
		}
		return clone, nil

	case reflect.Array:
		clone := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			elem, err := c.clone(v.Index(i))
			if err != nil {
				return reflect.Value{}, err
			}
			clone.Index(i).Set(elem)
		}
		return clone, nil

	case reflect.Interface:
		if v.IsNil() {
			return reflect.Zero(v.Type()), nil
		}
		elem := v.Elem()
		// The concrete type is only known at runtime, so the custom
		// decoder check is repeated here for values held in interfaces.
		if err := checkCustomDecoderType(elem.Type()); err != nil {
			return reflect.Value{}, err
		}
		cloned, err := c.clone(elem)
		if err != nil {
			return reflect.Value{}, err
		}
		clone := reflect.New(v.Type()).Elem()
		clone.Set(cloned)
		return clone, nil

	case reflect.Struct:
		// Shallow-copy the whole struct first: this also carries over
		// unexported fields, which are then shared with the original
		// (see the DecodeTransactional documentation).
		clone := reflect.New(v.Type()).Elem()
		clone.Set(v)
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).PkgPath != "" {
				continue // unexported field: shallow copy only
			}
			field, err := c.clone(v.Field(i))
			if err != nil {
				return reflect.Value{}, err
			}
			clone.Field(i).Set(field)
		}
		return clone, nil

	default:
		// Scalar kinds are copied by value. Kinds that cannot be
		// deep-copied (chan, func, unsafe.Pointer) are copied shallowly;
		// the decoder cannot mutate them in place, so the rollback
		// guarantee still holds.
		clone := reflect.New(v.Type()).Elem()
		clone.Set(v)
		return clone, nil
	}
}
