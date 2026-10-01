package msgpack

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
)

// UnmarshalTransactional is like Unmarshal, but decodes atomically:
// see Decoder.DecodeTransactional for details.
func UnmarshalTransactional(data []byte, v interface{}) error {
	dec := GetDecoder()
	dec.UsePreallocateValues(true)
	dec.Reset(bytes.NewReader(data))
	err := dec.DecodeTransactional(v)

	PutDecoder(dec)

	return err
}

// DecodeTransactional decodes the next MessagePack value into v atomically:
// on success *v is replaced with the decoded value, on failure *v is left
// untouched. It is intended for use cases like configuration hot-reload,
// where a failed decode must not leave the target object half-updated.
//
// To provide the guarantee, DecodeTransactional first deep-clones *v,
// decodes into the clone, and only then swaps the result into *v. The clone
// preserves the internal sharing topology: multiple fields that reference
// the same map, slice, or pointer keep referencing a single cloned object,
// so decoding behaves exactly as it would on the original value.
//
// Success path semantics: *v is replaced with a brand new value graph.
// References into the old graph held outside of v (for example a variable
// holding one of the old maps) are NOT updated and keep observing the old
// data. Always re-read sub-objects from v after a successful decode.
//
// Support boundary:
//   - The transactional guarantee covers only values reachable from v.
//     CustomDecoder/Unmarshaler implementations that mutate state outside
//     the decoded value (global variables, registries, other objects
//     captured via pointers) perform side effects the library cannot roll
//     back. Combining such decoders with DecodeTransactional is not
//     supported; custom decoders must confine mutations to the receiver.
//   - unsafe.Pointer fields are not supported: DecodeTransactional returns
//     an error before any decoding takes place.
//   - chan and func fields are copied as-is; the decoder never touches them.
func (d *Decoder) DecodeTransactional(v interface{}) error {
	vv := reflect.ValueOf(v)
	if !vv.IsValid() {
		return errors.New("msgpack: DecodeTransactional(nil)")
	}
	if vv.Kind() != reflect.Ptr {
		return fmt.Errorf("msgpack: DecodeTransactional(non-pointer %T)", v)
	}
	if vv.IsNil() {
		return fmt.Errorf("msgpack: DecodeTransactional(non-settable %T)", v)
	}

	clone, err := cloneForDecode(vv.Elem())
	if err != nil {
		return err
	}

	if err := d.DecodeValue(clone); err != nil {
		return err
	}

	vv.Elem().Set(clone)
	return nil
}

//------------------------------------------------------------------------------

// txVisit identifies a reference (pointer, map, or slice) that was already
// cloned, so that shared references map to a single clone.
type txVisit struct {
	typ reflect.Type
	ptr uintptr
	len int
	cap int
}

type txCloner struct {
	seen map[txVisit]reflect.Value
}

func cloneForDecode(v reflect.Value) (reflect.Value, error) {
	c := txCloner{seen: make(map[txVisit]reflect.Value)}
	return c.clone(v)
}

func (c *txCloner) clone(v reflect.Value) (reflect.Value, error) {
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			return reflect.Zero(v.Type()), nil
		}
		key := txVisit{typ: v.Type(), ptr: v.Pointer()}
		if prev, ok := c.seen[key]; ok {
			return prev, nil
		}
		clone := reflect.New(v.Type().Elem())
		c.seen[key] = clone
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
		if prev, ok := c.seen[key]; ok {
			return prev, nil
		}
		clone := reflect.MakeMapWithSize(v.Type(), v.Len())
		c.seen[key] = clone
		iter := v.MapRange()
		for iter.Next() {
			// Keys are immutable once inserted, so they are shared as-is.
			val, err := c.clone(iter.Value())
			if err != nil {
				return reflect.Value{}, err
			}
			clone.SetMapIndex(iter.Key(), val)
		}
		return clone, nil

	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type()), nil
		}
		key := txVisit{typ: v.Type(), ptr: v.Pointer(), len: v.Len(), cap: v.Cap()}
		if prev, ok := c.seen[key]; ok {
			return prev, nil
		}
		clone := reflect.MakeSlice(v.Type(), v.Len(), v.Cap())
		c.seen[key] = clone
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
		elem, err := c.clone(v.Elem())
		if err != nil {
			return reflect.Value{}, err
		}
		clone := reflect.New(v.Type()).Elem()
		clone.Set(elem)
		return clone, nil

	case reflect.Struct:
		// Shallow-copy the whole struct first so that unexported fields
		// (which the decoder can never reach) are preserved, then deep-clone
		// the exported fields.
		clone := reflect.New(v.Type()).Elem()
		clone.Set(v)
		typ := v.Type()
		for i := 0; i < v.NumField(); i++ {
			if typ.Field(i).PkgPath != "" {
				continue // unexported field
			}
			field, err := c.clone(v.Field(i))
			if err != nil {
				return reflect.Value{}, err
			}
			clone.Field(i).Set(field)
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

	case reflect.Chan, reflect.Func:
		// Opaque to the decoder; shared as-is.
		return v, nil

	case reflect.UnsafePointer:
		return reflect.Value{}, fmt.Errorf(
			"msgpack: DecodeTransactional does not support unsafe.Pointer (type %s)", v.Type())

	default:
		return v, nil
	}
}
