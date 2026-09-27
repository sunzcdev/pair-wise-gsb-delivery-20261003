package msgpack

import "reflect"

// UnknownFields is an opt-in container that preserves MessagePack map entries
// which do not match any field of the destination struct. It enables
// forward-compatible services: an old binary can decode a message produced by a
// newer version, modify the fields it knows, and re-encode the message
// without losing the fields it does not know.
//
// To enable the feature, add a field tagged with the "unknownfields" option:
//
//	type Message struct {
//	    Name  string               `msgpack:"name"`
//	    Extra msgpack.UnknownFields `msgpack:",unknownfields"`
//	}
//
// The field type must be UnknownFields or any named map type with a string
// key and a RawMessage value. Values are stored as raw MessagePack bytes, so
// nested maps, arrays, and extension types are preserved byte-for-byte and
// are never converted to JSON or any other intermediate representation.
//
// Semantics:
//   - Decoding: a key that matches a known struct field is always decoded
//     into that field and is never stored in UnknownFields. Unknown keys are
//     captured instead of being skipped. DisallowUnknownFields does not
//     report an error for keys captured by the container.
//   - Duplicate keys in the input: the last occurrence wins, both for known
//     fields (existing behaviour) and for entries stored in UnknownFields.
//   - Encoding: known fields are encoded first, then the UnknownFields
//     entries in sorted key order. An entry whose key collides with a known
//     field name is skipped, so the encoded map never contains duplicate
//     keys and the known field always wins.
//   - The container is ignored for structs encoded as arrays (as_array).
type UnknownFields map[string]RawMessage

var (
	unknownFieldsType = reflect.TypeOf(UnknownFields(nil))
	rawMessageType    = unknownFieldsType.Elem()
)

// isUnknownFieldsType reports whether typ can hold unknown fields, i.e. it is
// a map with string keys and RawMessage values (named types included).
func isUnknownFieldsType(typ reflect.Type) bool {
	return typ.Kind() == reflect.Map &&
		typ.Key().Kind() == reflect.String &&
		typ.Elem() == rawMessageType
}
