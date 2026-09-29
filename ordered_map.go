package msgpack

import (
	"fmt"
	"math/big"
	"reflect"
	"time"
)

// MapItem is an ordered key-value pair in an OrderedMap.
type MapItem struct {
	Key   interface{}
	Value interface{}
}

// OrderedMap represents a MessagePack map without converting it to a Go map.
// It preserves both item order and duplicate keys.
type OrderedMap []MapItem

var (
	_ CustomEncoder = OrderedMap(nil)
	_ CustomDecoder = (*OrderedMap)(nil)
)

// EncodeMsgpack implements CustomEncoder.
func (m OrderedMap) EncodeMsgpack(e *Encoder) error {
	if m == nil {
		return e.EncodeNil()
	}

	if err := e.EncodeMapLen(len(m)); err != nil {
		return err
	}

	for _, item := range m {
		if err := e.Encode(item.Key); err != nil {
			return err
		}
		if err := e.Encode(item.Value); err != nil {
			return err
		}
	}

	return nil
}

// DecodeMsgpack implements CustomDecoder.
func (m *OrderedMap) DecodeMsgpack(d *Decoder) error {
	decoded, err := d.DecodeOrderedMap()
	if err != nil {
		return err
	}
	*m = decoded
	return nil
}

// Get returns the first value associated with key.
//
// Key comparison follows the MessagePack data model: nil, bool, string,
// binary, integer, float, array, and map values are only compared with the
// same kind. Integers are compared mathematically regardless of their Go
// width, and float32/float64 values are compared as floating-point values.
// Arrays and OrderedMaps are compared element-by-element in order.
// Consequently, the integer 1 and the string "1" are distinct keys.
func (m OrderedMap) Get(key interface{}) (interface{}, bool) {
	for _, item := range m {
		if equalMessagePackValues(item.Key, key) {
			return item.Value, true
		}
	}
	return nil, false
}

// GetAll returns all values associated with key in item order.
func (m OrderedMap) GetAll(key interface{}) []interface{} {
	var values []interface{}
	for _, item := range m {
		if equalMessagePackValues(item.Key, key) {
			values = append(values, item.Value)
		}
	}
	return values
}

// DecodeOrderedMap decodes a MessagePack map as an OrderedMap. Nested maps
// contained in keys and values are also decoded as OrderedMap values.
func (d *Decoder) DecodeOrderedMap() (OrderedMap, error) {
	return d.decodeOrderedMap(d.flags&rejectDuplicateMapKeysFlag != 0)
}

func (d *Decoder) decodeOrderedMap(rejectDuplicates bool) (OrderedMap, error) {
	n, err := d.DecodeMapLen()
	if err != nil {
		return nil, err
	}
	if n == -1 {
		return nil, nil
	}
	if n < 0 {
		return nil, fmt.Errorf("msgpack: invalid map length %d", n)
	}

	oldMapDecoder := d.mapDecoder
	d.mapDecoder = func(d *Decoder) (interface{}, error) {
		return d.decodeOrderedMap(rejectDuplicates)
	}
	defer func() {
		d.mapDecoder = oldMapDecoder
	}()

	capacity := n
	if d.flags&disableAllocLimitFlag == 0 && capacity > maxMapSize {
		capacity = maxMapSize
	}

	m := make(OrderedMap, 0, capacity)
	for i := 0; i < n; i++ {
		key, err := d.decodeInterfaceCond()
		if err != nil {
			return nil, err
		}
		value, err := d.decodeInterfaceCond()
		if err != nil {
			return nil, err
		}

		if rejectDuplicates {
			for _, item := range m {
				if equalMessagePackValues(item.Key, key) {
					return nil, fmt.Errorf("msgpack: duplicate map key: %v", key)
				}
			}
		}

		m = append(m, MapItem{Key: key, Value: value})
	}

	return m, nil
}

func equalMessagePackValues(a, b interface{}) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}

	av := reflect.ValueOf(a)
	bv := reflect.ValueOf(b)

	for av.Kind() == reflect.Ptr || av.Kind() == reflect.Interface {
		if av.IsNil() {
			return false
		}
		av = av.Elem()
	}
	for bv.Kind() == reflect.Ptr || bv.Kind() == reflect.Interface {
		if bv.IsNil() {
			return false
		}
		bv = bv.Elem()
	}

	if av.Type() == orderedMapType || bv.Type() == orderedMapType {
		if av.Type() != bv.Type() {
			return false
		}
		return equalOrderedMaps(av.Interface().(OrderedMap), bv.Interface().(OrderedMap))
	}

	if at, ok := a.(time.Time); ok {
		bt, ok := b.(time.Time)
		return ok && at.Equal(bt)
	}

	switch av.Kind() {
	case reflect.Bool:
		return bv.Kind() == reflect.Bool && av.Bool() == bv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if !isIntegerKind(bv.Kind()) {
			return false
		}
		return reflectInteger(av).Cmp(reflectInteger(bv)) == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if !isIntegerKind(bv.Kind()) {
			return false
		}
		return reflectInteger(av).Cmp(reflectInteger(bv)) == 0
	case reflect.Float32, reflect.Float64:
		if bv.Kind() != reflect.Float32 && bv.Kind() != reflect.Float64 {
			return false
		}
		return av.Convert(float64Type).Float() == bv.Convert(float64Type).Float()
	case reflect.String:
		return bv.Kind() == reflect.String && av.String() == bv.String()
	case reflect.Slice, reflect.Array:
		if bv.Kind() != reflect.Slice && bv.Kind() != reflect.Array {
			return false
		}
		if av.Type().Elem().Kind() == reflect.Uint8 && bv.Type().Elem().Kind() == reflect.Uint8 {
			return equalByteSequences(av, bv)
		}
		return equalValueSequences(av, bv)
	case reflect.Map:
		return bv.Kind() == reflect.Map && equalReflectMaps(av, bv)
	default:
		return reflect.DeepEqual(av.Interface(), bv.Interface())
	}
}

var (
	orderedMapType = reflect.TypeOf(OrderedMap{})
	float64Type    = reflect.TypeOf(float64(0))
)

func isIntegerKind(kind reflect.Kind) bool {
	switch kind {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	default:
		return false
	}
}

func reflectInteger(v reflect.Value) *big.Int {
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return big.NewInt(v.Int())
	default:
		return new(big.Int).SetUint64(v.Uint())
	}
}

func equalByteSequences(a, b reflect.Value) bool {
	if a.Len() != b.Len() || (a.Kind() == reflect.Slice && a.IsNil() != b.IsNil()) {
		return false
	}
	for i := 0; i < a.Len(); i++ {
		if a.Index(i).Uint() != b.Index(i).Uint() {
			return false
		}
	}
	return true
}

func equalValueSequences(a, b reflect.Value) bool {
	if a.Len() != b.Len() || (a.Kind() == reflect.Slice && a.IsNil() != b.IsNil()) {
		return false
	}
	for i := 0; i < a.Len(); i++ {
		if !equalMessagePackValues(a.Index(i).Interface(), b.Index(i).Interface()) {
			return false
		}
	}
	return true
}

func equalReflectMaps(a, b reflect.Value) bool {
	if a.Len() != b.Len() || (a.IsNil() != b.IsNil()) {
		return false
	}

	iter := a.MapRange()
	for iter.Next() {
		key := iter.Key()
		value := iter.Value()
		matched := false

		other := b.MapRange()
		for other.Next() {
			if equalMessagePackValues(key.Interface(), other.Key().Interface()) &&
				equalMessagePackValues(value.Interface(), other.Value().Interface()) {
				matched = true
				break
			}
		}

		if !matched {
			return false
		}
	}

	return true
}

func equalOrderedMaps(a, b OrderedMap) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !equalMessagePackValues(a[i].Key, b[i].Key) ||
			!equalMessagePackValues(a[i].Value, b[i].Value) {
			return false
		}
	}
	return true
}
