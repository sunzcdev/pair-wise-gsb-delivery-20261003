package msgpack_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/vmihailenco/msgpack/v5"
)

// newVersionMsg is the message as known by the new version of the service.
type newVersionMsg struct {
	ID      int64                  `msgpack:"id"`
	Name    string                 `msgpack:"name"`
	Created time.Time              `msgpack:"created"` // encoded as a MessagePack ext type
	Meta    map[string]interface{} `msgpack:"meta"`    // nested containers
	Tags    []string               `msgpack:"tags"`
}

// oldVersionMsg is the same message as known by the old version of the
// service. It only understands id and name, but keeps everything else.
type oldVersionMsg struct {
	ID   int64  `msgpack:"id"`
	Name string `msgpack:"name"`

	Unknown msgpack.UnknownFieldsMap
}

// TestUnknownFieldsRoundTrip proves the "new version sends, old version
// modifies, new version receives" round trip does not swallow unknown fields,
// including nested containers and ext types.
func TestUnknownFieldsRoundTrip(t *testing.T) {
	// New version sends.
	sent := &newVersionMsg{
		ID:      42,
		Name:    "before",
		Created: time.Unix(1700000000, 123456789).UTC(),
		Meta: map[string]interface{}{
			"nested": map[string]interface{}{
				"list": []interface{}{int8(1), "two", true},
			},
			"blob": []byte{1, 2, 3},
		},
		Tags: []string{"a", "b"},
	}
	data, err := msgpack.Marshal(sent)
	require.NoError(t, err)

	// Old version receives, decodes the fields it knows, and modifies one.
	var relay oldVersionMsg
	require.NoError(t, msgpack.Unmarshal(data, &relay))
	require.Equal(t, int64(42), relay.ID)
	require.Equal(t, "before", relay.Name)

	// Unknown fields are captured; known fields are not duplicated there.
	require.Len(t, relay.Unknown, 3)
	require.Contains(t, relay.Unknown, "created")
	require.Contains(t, relay.Unknown, "meta")
	require.Contains(t, relay.Unknown, "tags")
	require.NotContains(t, relay.Unknown, "id")
	require.NotContains(t, relay.Unknown, "name")

	relay.Name = "after"

	// Old version forwards the message.
	forwarded, err := msgpack.Marshal(&relay)
	require.NoError(t, err)

	// New version receives and sees everything: the modified known field
	// and the untouched unknown fields, including the ext-typed timestamp
	// and the nested containers.
	var received newVersionMsg
	require.NoError(t, msgpack.Unmarshal(forwarded, &received))
	require.Equal(t, int64(42), received.ID)
	require.Equal(t, "after", received.Name)
	require.True(t, sent.Created.Equal(received.Created))
	require.Equal(t, sent.Meta, received.Meta)
	require.Equal(t, sent.Tags, received.Tags)
}

// TestUnknownFieldsDuplicateKeys documents that for duplicate keys in the
// input the last entry wins.
func TestUnknownFieldsDuplicateKeys(t *testing.T) {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	require.NoError(t, enc.EncodeMapLen(3))
	require.NoError(t, enc.EncodeString("id"))
	require.NoError(t, enc.EncodeInt64(7))
	require.NoError(t, enc.EncodeString("extra"))
	require.NoError(t, enc.EncodeString("first"))
	require.NoError(t, enc.EncodeString("extra"))
	require.NoError(t, enc.EncodeString("second"))

	var msg oldVersionMsg
	require.NoError(t, msgpack.Unmarshal(buf.Bytes(), &msg))
	require.Len(t, msg.Unknown, 1)

	forwarded, err := msgpack.Marshal(&msg)
	require.NoError(t, err)

	var decoded map[string]interface{}
	require.NoError(t, msgpack.Unmarshal(forwarded, &decoded))
	require.Equal(t, "second", decoded["extra"])
}

// TestUnknownFieldsConflictingKey documents that a container key conflicting
// with a known field name is rejected with an error instead of emitting a
// duplicate key or silently dropping data.
func TestUnknownFieldsConflictingKey(t *testing.T) {
	msg := &oldVersionMsg{
		ID:   1,
		Name: "known",
		Unknown: msgpack.UnknownFieldsMap{
			"name": msgpack.RawMessage{0xa4, 'o', 't', 'h', 'r'},
		},
	}
	_, err := msgpack.Marshal(msg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "conflicts with a field")
}

// TestUnknownFieldsRawPreserved verifies that unknown values are kept as raw
// MessagePack bytes, byte-for-byte, including ext types.
func TestUnknownFieldsRawPreserved(t *testing.T) {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	require.NoError(t, enc.EncodeMapLen(2))
	require.NoError(t, enc.EncodeString("id"))
	require.NoError(t, enc.EncodeInt64(1))
	require.NoError(t, enc.EncodeString("created"))
	tm := time.Unix(1700000000, 123456789).UTC()
	require.NoError(t, enc.EncodeTime(tm))

	// Raw ext bytes as they appear on the wire, after the "created" key.
	full := buf.Bytes()
	idx := bytes.Index(full, []byte("\xa7created"))
	require.NotEqual(t, -1, idx)
	extRaw := full[idx+len("\xa7created"):]

	var msg oldVersionMsg
	require.NoError(t, msgpack.Unmarshal(full, &msg))
	require.Equal(t, msgpack.RawMessage(extRaw), msg.Unknown["created"])
}
