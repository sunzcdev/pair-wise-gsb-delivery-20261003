package msgpack_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/vmihailenco/msgpack/v5"
)

// messageV2 is the new version of the message. The old version only knows
// the "name" field.
type messageV2 struct {
	Name    string                 `msgpack:"name"`
	Version int                    `msgpack:"version"`
	Meta    map[string]interface{} `msgpack:"meta"` // nested container
	Tags    []string               `msgpack:"tags"` // nested container
	When    time.Time              `msgpack:"when"` // extension type
}

// messageV1 is the old version of the message. It opts in to preserving
// unknown fields so that a decode/modify/encode cycle is lossless.
type messageV1 struct {
	Name  string                `msgpack:"name"`
	Extra msgpack.UnknownFields `msgpack:",unknownfields"`
}

// TestUnknownFieldsRoundTrip proves the "new sends, old modifies, new
// receives" pipeline does not swallow unknown fields, including nested
// containers and extension types.
func TestUnknownFieldsRoundTrip(t *testing.T) {
	in := &messageV2{
		Name:    "original",
		Version: 2,
		Meta:    map[string]interface{}{"nested": map[string]interface{}{"deep": []interface{}{1, "two", 3.5}}},
		Tags:    []string{"a", "b"},
		When:    time.Unix(1700000000, 123456789).UTC(),
	}

	// New version sends.
	wire, err := msgpack.Marshal(in)
	require.NoError(t, err)

	// Reference: direct new-to-new round trip.
	var want messageV2
	require.NoError(t, msgpack.Unmarshal(wire, &want))

	// Old version receives, and every unknown value is preserved as raw
	// MessagePack bytes taken verbatim from the wire (no JSON conversion).
	var old messageV1
	require.NoError(t, msgpack.Unmarshal(wire, &old))
	require.Equal(t, "original", old.Name)
	require.Len(t, old.Extra, 4)
	for _, key := range []string{"version", "meta", "tags", "when"} {
		raw, ok := old.Extra[key]
		require.True(t, ok, "unknown field %q must be preserved", key)
		require.True(t, bytes.Contains(wire, []byte(raw)),
			"unknown field %q must be preserved byte-for-byte", key)
	}

	// Old version modifies a known field and forwards the message.
	old.Name = "modified"
	forwarded, err := msgpack.Marshal(&old)
	require.NoError(t, err)

	// New version receives the forwarded message; nothing was swallowed.
	want.Name = "modified"
	var got messageV2
	require.NoError(t, msgpack.Unmarshal(forwarded, &got))
	require.Equal(t, want, got)
}

// TestUnknownFieldsKnownFieldWins documents that a container entry whose key
// collides with a known field name is dropped on encode: the known field
// wins and the output never contains duplicate keys.
func TestUnknownFieldsKnownFieldWins(t *testing.T) {
	forged, err := msgpack.Marshal("forged")
	require.NoError(t, err)

	old := messageV1{
		Name: "known",
		Extra: msgpack.UnknownFields{
			"name":  msgpack.RawMessage(forged),
			"other": msgpack.RawMessage(forged),
		},
	}

	b, err := msgpack.Marshal(&old)
	require.NoError(t, err)

	dec := msgpack.NewDecoder(bytes.NewReader(b))
	n, err := dec.DecodeMapLen()
	require.NoError(t, err)
	require.Equal(t, 2, n, "colliding key must not produce a duplicate map entry")

	seen := map[string]interface{}{}
	for i := 0; i < n; i++ {
		k, err := dec.DecodeString()
		require.NoError(t, err)
		v, err := dec.DecodeInterface()
		require.NoError(t, err)
		seen[k] = v
	}
	require.Equal(t, map[string]interface{}{"name": "known", "other": "forged"}, seen)
}

// TestUnknownFieldsDuplicateKeys documents that for duplicate keys in the
// input the last occurrence wins, both for known fields and for preserved
// unknown fields.
func TestUnknownFieldsDuplicateKeys(t *testing.T) {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	require.NoError(t, enc.EncodeMapLen(4))
	require.NoError(t, enc.EncodeString("extra"))
	require.NoError(t, enc.EncodeInt(1))
	require.NoError(t, enc.EncodeString("extra"))
	require.NoError(t, enc.EncodeInt(2))
	require.NoError(t, enc.EncodeString("name"))
	require.NoError(t, enc.EncodeString("first"))
	require.NoError(t, enc.EncodeString("name"))
	require.NoError(t, enc.EncodeString("last"))

	var old messageV1
	require.NoError(t, msgpack.Unmarshal(buf.Bytes(), &old))
	require.Equal(t, "last", old.Name)

	require.Len(t, old.Extra, 1)
	var extra int
	require.NoError(t, msgpack.Unmarshal(old.Extra["extra"], &extra))
	require.Equal(t, 2, extra)
}

// TestUnknownFieldsDisallowUnknownFields documents that the container takes
// precedence over DisallowUnknownFields: captured keys are not an error.
func TestUnknownFieldsDisallowUnknownFields(t *testing.T) {
	wire, err := msgpack.Marshal(&messageV2{Name: "x", Version: 2})
	require.NoError(t, err)

	dec := msgpack.NewDecoder(bytes.NewReader(wire))
	dec.DisallowUnknownFields(true)

	var old messageV1
	require.NoError(t, dec.Decode(&old))
	require.Contains(t, old.Extra, "version")
}

// TestUnknownFieldsNamedType verifies that any named map type with string
// keys and RawMessage values can serve as the container.
func TestUnknownFieldsNamedType(t *testing.T) {
	type myUnknownFields map[string]msgpack.RawMessage
	type msg struct {
		ID    int             `msgpack:"id"`
		Extra myUnknownFields `msgpack:",unknownfields"`
	}

	wire, err := msgpack.Marshal(map[string]interface{}{"id": 1, "new": "field"})
	require.NoError(t, err)

	var m msg
	require.NoError(t, msgpack.Unmarshal(wire, &m))
	require.Equal(t, 1, m.ID)
	require.Contains(t, m.Extra, "new")

	out, err := msgpack.Marshal(&m)
	require.NoError(t, err)

	var decoded map[string]interface{}
	require.NoError(t, msgpack.Unmarshal(out, &decoded))
	require.Equal(t, map[string]interface{}{"id": int8(1), "new": "field"}, decoded)
}

// TestUnknownFieldsDeterministic verifies that re-encoding produces
// deterministic output (unknown keys are sorted).
func TestUnknownFieldsDeterministic(t *testing.T) {
	wire, err := msgpack.Marshal(map[string]interface{}{
		"name": "x", "z": 1, "a": 2, "m": 3,
	})
	require.NoError(t, err)

	var old messageV1
	require.NoError(t, msgpack.Unmarshal(wire, &old))

	first, err := msgpack.Marshal(&old)
	require.NoError(t, err)
	for i := 0; i < 10; i++ {
		next, err := msgpack.Marshal(&old)
		require.NoError(t, err)
		require.Equal(t, first, next)
	}
}

// TestUnknownFieldsEmpty verifies that an empty container does not change
// the encoding.
func TestUnknownFieldsEmpty(t *testing.T) {
	with, err := msgpack.Marshal(&messageV1{Name: "x"})
	require.NoError(t, err)

	without, err := msgpack.Marshal(&struct {
		Name string `msgpack:"name"`
	}{Name: "x"})
	require.NoError(t, err)

	require.Equal(t, without, with)
}
