package msgpack

import (
	"bytes"
	"fmt"
	"reflect"
)

// MapItem is a single key-value pair in an OrderedMap.
type MapItem struct {
	Key   interface{}
	Value interface{}
}

// OrderedMap is an ordered list of MessagePack key-value pairs.
//
// Unlike map[interface{}]interface{}, OrderedMap can hold any valid
// MessagePack key, including arrays and maps, preserves the original
// order of the pairs, and never silently drops duplicate keys.
//
// Get, Has, Set, and Delete compare keys using the following rules:
//   - nil is equal to nil;
//   - bools and strings are equal when their values are equal;
//   - all integer types (signed and unsigned) are equal when they
//     represent the same numeric value, e.g. int8(1) == uint64(1);
//   - floats are equal to floats with the same value; a float is never
//     equal to an integer, because they are different MessagePack types;
//   - a string is never equal to a []byte, because str and bin are
//     different MessagePack types;
//   - slices are equal when they have the same length and pairwise
//     equal elements;
//   - maps and OrderedMaps are equal when they contain the same set of
//     key-value pairs, regardless of order;
//   - pointers and interfaces are unwrapped before comparing;
//   - values of any other type are compared with reflect.DeepEqual.
//
// In particular, integer 1 and string "1" are always different keys.
type OrderedMap struct {
	// KeepDuplicates controls how decoding handles duplicate keys.
	// When false (the default), decoding fails with an error on the
	// first duplicate key. When true, all pairs are kept in the
	// original order.
	KeepDuplicates bool

	items []MapItem
}

var (
	_ CustomEncoder = OrderedMap{}
	_ CustomDecoder = (*OrderedMap)(nil)
)

var orderedMapType = reflect.TypeOf(OrderedMap{})

// NewOrderedMap returns a new OrderedMap initialized with the given items.
func NewOrderedMap(items ...MapItem) *OrderedMap {
	m := new(OrderedMap)
	m.items = append(m.items, items...)
	return m
}

// Len returns the number of key-value pairs in the map.
func (m *OrderedMap) Len() int {
	if m == nil {
		return 0
	}
	return len(m.items)
}

// Items returns a copy of the key-value pairs in the map.
func (m *OrderedMap) Items() []MapItem {
	if m == nil {
		return nil
	}
	items := make([]MapItem, len(m.items))
	copy(items, m.items)
	return items
}

// At returns the key-value pair at index i.
func (m *OrderedMap) At(i int) (key, value interface{}, ok bool) {
	if m == nil || i < 0 || i >= len(m.items) {
		return nil, nil, false
	}
	item := m.items[i]
	return item.Key, item.Value, true
}

// Get returns the value of the first pair with a matching key.
func (m *OrderedMap) Get(key interface{}) (interface{}, bool) {
	if m == nil {
		return nil, false
	}
	for i := range m.items {
		if msgpackValueEqual(m.items[i].Key, key) {
			return m.items[i].Value, true
		}
	}
	return nil, false
}

// GetAll returns the values of all pairs with a matching key.
func (m *OrderedMap) GetAll(key interface{}) []interface{} {
	if m == nil {
		return nil
	}
	var values []interface{}
	for i := range m.items {
		if msgpackValueEqual(m.items[i].Key, key) {
			values = append(values, m.items[i].Value)
		}
	}
	return values
}

// Has reports whether the map contains a pair with a matching key.
func (m *OrderedMap) Has(key interface{}) bool {
	_, ok := m.Get(key)
	return ok
}

// Set replaces the value of the first pair with a matching key,
// or appends a new pair when there is no match.
func (m *OrderedMap) Set(key, value interface{}) {
	for i := range m.items {
		if msgpackValueEqual(m.items[i].Key, key) {
			m.items[i].Value = value
			return
		}
	}
	m.items = append(m.items, MapItem{Key: key, Value: value})
}

// Add appends a new key-value pair, even if the key already exists.
func (m *OrderedMap) Add(key, value interface{}) {
	m.items = append(m.items, MapItem{Key: key, Value: value})
}

// Delete removes all pairs with a matching key.
func (m *OrderedMap) Delete(key interface{}) bool {
	if m == nil {
		return false
	}
	items := m.items[:0]
	deleted := false
	for i := range m.items {
		if msgpackValueEqual(m.items[i].Key, key) {
			deleted = true
			continue
		}
		items = append(items, m.items[i])
	}
	m.items = items
	return deleted
}

// EncodeMsgpack implements CustomEncoder.
func (m OrderedMap) EncodeMsgpack(enc *Encoder) error {
	if err := enc.EncodeMapLen(len(m.items)); err != nil {
		return err
	}
	for i := range m.items {
		if err := enc.Encode(m.items[i].Key); err != nil {
			return err
		}
		if err := enc.Encode(m.items[i].Value); err != nil {
			return err
		}
	}
	return nil
}

// DecodeMsgpack implements CustomDecoder.
func (m *OrderedMap) DecodeMsgpack(dec *Decoder) error {
	prev := dec.mapDecoder
	dec.mapDecoder = m.decodeNestedMap
	defer func() {
		dec.mapDecoder = prev
	}()
	return m.decodeMap(dec)
}

// decodeNestedMap decodes nested maps as OrderedMap so that arbitrary
// keys are representable at any depth.
func (m *OrderedMap) decodeNestedMap(d *Decoder) (interface{}, error) {
	nested := OrderedMap{KeepDuplicates: m.KeepDuplicates}
	if err := nested.decodeMap(d); err != nil {
		return nil, err
	}
	return nested, nil
}

func (m *OrderedMap) decodeMap(d *Decoder) error {
	n, err := d.DecodeMapLen()
	if err != nil {
		return err
	}
	if n == -1 {
		m.items = nil
		return nil
	}

	ln := n
	if d.flags&disableAllocLimitFlag == 0 {
		ln = min(ln, maxMapSize)
	}
	items := make([]MapItem, 0, ln)

	for i := 0; i < n; i++ {
		key, err := d.decodeInterfaceCond()
		if err != nil {
			return err
		}

		value, err := d.decodeInterfaceCond()
		if err != nil {
			return err
		}

		if !m.KeepDuplicates {
			for j := range items {
				if msgpackValueEqual(items[j].Key, key) {
					return fmt.Errorf("msgpack: duplicate key %v decoding OrderedMap", key)
				}
			}
		}

		items = append(items, MapItem{Key: key, Value: value})
	}

	m.items = items
	return nil
}

//------------------------------------------------------------------------------

func msgpackValueEqual(a, b interface{}) bool {
	return valueEqual(reflect.ValueOf(a), reflect.ValueOf(b))
}

func valueEqual(a, b reflect.Value) bool {
	a = unwrapValue(a)
	b = unwrapValue(b)

	if !a.IsValid() || !b.IsValid() {
		return !a.IsValid() && !b.IsValid()
	}

	ka, kb := a.Kind(), b.Kind()

	switch {
	case isIntKind(ka) && isIntKind(kb):
		return a.Int() == b.Int()
	case isUintKind(ka) && isUintKind(kb):
		return a.Uint() == b.Uint()
	case isIntKind(ka) && isUintKind(kb):
		v := a.Int()
		return v >= 0 && uint64(v) == b.Uint()
	case isUintKind(ka) && isIntKind(kb):
		v := b.Int()
		return v >= 0 && a.Uint() == uint64(v)
	case isFloatKind(ka) && isFloatKind(kb):
		return a.Float() == b.Float()
	}

	if ka != kb {
		// Allow comparing an OrderedMap with a Go map.
		aItems, aOK := orderedMapItems(a)
		bItems, bOK := orderedMapItems(b)
		return aOK && bOK && mapItemsEqual(aItems, bItems)
	}

	switch ka {
	case reflect.Bool:
		return a.Bool() == b.Bool()
	case reflect.String:
		return a.String() == b.String()
	case reflect.Slice:
		if a.Type().Elem().Kind() == reflect.Uint8 &&
			b.Type().Elem().Kind() == reflect.Uint8 {
			return bytes.Equal(a.Bytes(), b.Bytes())
		}
		if a.Len() != b.Len() {
			return false
		}
		for i := 0; i < a.Len(); i++ {
			if !valueEqual(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Map, reflect.Struct:
		aItems, aOK := orderedMapItems(a)
		bItems, bOK := orderedMapItems(b)
		if aOK && bOK {
			return mapItemsEqual(aItems, bItems)
		}
	}

	if a.Type() != b.Type() {
		return false
	}
	return reflect.DeepEqual(a.Interface(), b.Interface())
}

func unwrapValue(v reflect.Value) reflect.Value {
	for {
		switch v.Kind() {
		case reflect.Interface, reflect.Ptr:
			if v.IsNil() {
				return reflect.Value{}
			}
			v = v.Elem()
		default:
			return v
		}
	}
}

func orderedMapItems(v reflect.Value) ([]MapItem, bool) {
	if v.Type() == orderedMapType {
		return v.Interface().(OrderedMap).items, true
	}
	if v.Kind() == reflect.Map {
		items := make([]MapItem, 0, v.Len())
		iter := v.MapRange()
		for iter.Next() {
			items = append(items, MapItem{
				Key:   iter.Key().Interface(),
				Value: iter.Value().Interface(),
			})
		}
		return items, true
	}
	return nil, false
}

// mapItemsEqual compares two lists of pairs as multisets of key-value
// pairs, ignoring order.
func mapItemsEqual(a, b []MapItem) bool {
	if len(a) != len(b) {
		return false
	}
	used := make([]bool, len(b))
outer:
	for i := range a {
		for j := range b {
			if used[j] {
				continue
			}
			if msgpackValueEqual(a[i].Key, b[j].Key) &&
				msgpackValueEqual(a[i].Value, b[j].Value) {
				used[j] = true
				continue outer
			}
		}
		return false
	}
	return true
}

func isIntKind(kind reflect.Kind) bool {
	switch kind {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return true
	}
	return false
}

func isUintKind(kind reflect.Kind) bool {
	switch kind {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	}
	return false
}

func isFloatKind(kind reflect.Kind) bool {
	switch kind {
	case reflect.Float32, reflect.Float64:
		return true
	}
	return false
}
