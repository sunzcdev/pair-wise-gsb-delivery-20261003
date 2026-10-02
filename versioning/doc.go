// Package versioning adds an optional, explicit schema-versioning layer on top
// of MessagePack.
//
// # Why
//
// Plain MessagePack struct encoding identifies fields by their Go name. When a
// struct field is renamed, split, or given a new type, the field on the wire no
// longer matches the historical bytes, so existing records become unreadable.
//
// This package keeps the wire identity of a field independent of its Go name:
// every versioned field carries a stable integer ID in a struct tag. The schema
// version number is embedded in the encoded data itself (inside the MessagePack
// envelope), never inferred from deployment configuration.
//
// # Opt-in
//
// The mechanism is opt-in per Go type. Values of types registered with a
// Registry are encoded as a versioned envelope. Values of types that are not
// registered are passed straight through to the underlying msgpack encoder, so
// existing Marshal/Unmarshal output is unchanged.
//
// # Explicit migrations
//
// Converting between schema versions is performed only by functions the caller
// registers explicitly with Registry.Conversion. Fields are matched by stable
// numeric ID, never by name similarity: renaming a Go field requires no
// conversion, but the package will not guess that, for example, "name" should be
// split into "first_name"/"last_name".
package versioning
