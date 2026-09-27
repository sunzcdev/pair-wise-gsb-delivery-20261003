package msgpack

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// UnknownFieldsMap is an optional struct field type that captures MessagePack
// map entries which do not match any known struct field. It allows an old
// service to decode a message produced by a newer version, modify the fields
// it knows about, and re-encode the message without losing the fields it does
// not know about.
//
//	type Message struct {
//	    ID int64 `msgpack:"id"`
//
//	    Unknown msgpack.UnknownFieldsMap
//	}
//
// Values are stored as raw MessagePack-encoded bytes (RawMessage), so nested
// containers and extension types are preserved as-is and are never converted
// to JSON or any other intermediate representation.
//
// The following policies apply:
//
//   - Decoding: known struct fields take precedence. A map entry whose key
//     matches a known field is decoded into that field and is NOT stored in
//     the container. Only unmatched keys are captured.
//   - Duplicate keys in the input: the last entry wins, both for known fields
//     (existing behavior) and for captured unknown fields.
//   - Encoding: known fields are encoded from the struct first, then the
//     captured entries are appended in sorted key order. A container key that
//     conflicts with a known field name is rejected with an error instead of
//     silently dropping data or emitting a duplicate key.
//
// The container field must be an exported, non-embedded struct field of this
// exact type. It is ignored for array-encoded structs (as_array), where
// unknown fields can not be represented.
type UnknownFieldsMap map[string]RawMessage

var unknownFieldsMapType = reflect.TypeOf(UnknownFieldsMap(nil))

func (d *Decoder) decodeUnknownField(strct reflect.Value, f *field, name string) error {
	// name is a temporary string that is reused by the decoder.
	key := strings.Clone(name)

	raw, err := d.DecodeRaw()
	if err != nil {
		return err
	}

	v := fieldByIndexAlloc(strct, f.index)
	if v.IsNil() {
		v.Set(reflect.MakeMap(v.Type()))
	}
	v.SetMapIndex(reflect.ValueOf(key), reflect.ValueOf(raw))
	return nil
}

func unknownFieldsValue(strct reflect.Value, f *field) UnknownFieldsMap {
	v, ok := fieldByIndex(strct, f.index)
	if !ok || v.IsNil() {
		return nil
	}
	return v.Interface().(UnknownFieldsMap)
}

// unknownFieldsEntries returns the sorted keys of the unknown fields container,
// validating that they don't conflict with known struct fields.
func unknownFieldsEntries(strct reflect.Value, structFields *fields) ([]string, UnknownFieldsMap, error) {
	if structFields.unknownFields == nil {
		return nil, nil, nil
	}

	unknown := unknownFieldsValue(strct, structFields.unknownFields)
	if len(unknown) == 0 {
		return nil, nil, nil
	}

	keys := make([]string, 0, len(unknown))
	for k, raw := range unknown {
		if _, ok := structFields.Map[k]; ok {
			return nil, nil, fmt.Errorf("msgpack: UnknownFieldsMap key %q conflicts with a field of %s",
				k, strct.Type())
		}
		if len(raw) == 0 {
			return nil, nil, fmt.Errorf("msgpack: UnknownFieldsMap key %q holds an empty RawMessage", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	return keys, unknown, nil
}

func encodeUnknownFields(e *Encoder, keys []string, unknown UnknownFieldsMap) error {
	for _, k := range keys {
		if err := e.EncodeString(k); err != nil {
			return err
		}
		if err := e.write(unknown[k]); err != nil {
			return err
		}
	}
	return nil
}
