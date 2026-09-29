package msgpack_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"
)

func TestOrderedMapRoundTripCompositeKeys(t *testing.T) {
	inner := msgpack.NewOrderedMap(
		msgpack.MapItem{Key: "x", Value: int64(1)},
		msgpack.MapItem{Key: 1, Value: "one"},
	)

	original := msgpack.NewOrderedMap(
		msgpack.MapItem{Key: "1", Value: "string one"},
		msgpack.MapItem{Key: int64(1), Value: "integer one"},
		msgpack.MapItem{Key: []interface{}{"k", int64(2)}, Value: "array key"},
		msgpack.MapItem{Key: *inner, Value: "map key"},
	)

	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	enc.UseCompactInts(true)
	require.NoError(t, enc.Encode(original))
	b := buf.Bytes()

	var decoded msgpack.OrderedMap
	decoded.KeepDuplicates = true
	require.NoError(t, msgpack.Unmarshal(b, &decoded))

	require.Equal(t, 4, decoded.Len())

	v, ok := decoded.Get(int(1))
	require.True(t, ok)
	require.Equal(t, "integer one", v)

	v, ok = decoded.Get("1")
	require.True(t, ok)
	require.Equal(t, "string one", v)

	v, ok = decoded.Get([]interface{}{"k", int(2)})
	require.True(t, ok)
	require.Equal(t, "array key", v)

	v, ok = decoded.Get(*inner)
	require.True(t, ok)
	require.Equal(t, "map key", v)

	require.False(t, decoded.Has([]interface{}{"k", int(3)}))

	// Order and bytes must survive the round trip exactly.
	var rebuf bytes.Buffer
	reenc := msgpack.NewEncoder(&rebuf)
	reenc.UseCompactInts(true)
	err := reenc.Encode(&decoded)
	require.NoError(t, err)
	require.True(t, bytes.Equal(b, rebuf.Bytes()))
}

func TestOrderedMapIntAndStringAreDifferentKeys(t *testing.T) {
	original := msgpack.NewOrderedMap(
		msgpack.MapItem{Key: 1, Value: "int"},
		msgpack.MapItem{Key: "1", Value: "string"},
	)

	b, err := msgpack.Marshal(original)
	require.NoError(t, err)

	var m msgpack.OrderedMap
	require.NoError(t, msgpack.Unmarshal(b, &m))

	require.Equal(t, 2, m.Len())

	v, ok := m.Get(int(1))
	require.True(t, ok)
	require.Equal(t, "int", v)

	// Integers of every width compare by numeric value.
	for _, key := range []interface{}{int8(1), int16(1), int32(1), int64(1), uint8(1), uint64(1)} {
		v, ok = m.Get(key)
		require.True(t, ok, "key %T should match integer 1", key)
		require.Equal(t, "int", v)
	}

	v, ok = m.Get("1")
	require.True(t, ok)
	require.Equal(t, "string", v)
}

func TestOrderedMapDuplicateKeysRejectedByDefault(t *testing.T) {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	require.NoError(t, enc.EncodeMapLen(2))
	require.NoError(t, enc.EncodeString("a"))
	require.NoError(t, enc.EncodeInt(1))
	require.NoError(t, enc.EncodeString("a"))
	require.NoError(t, enc.EncodeInt(2))

	var m msgpack.OrderedMap
	err := msgpack.Unmarshal(buf.Bytes(), &m)
	require.Error(t, err)
}

func TestOrderedMapDuplicateKeysKeptWhenAllowed(t *testing.T) {
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	require.NoError(t, enc.EncodeMapLen(2))
	require.NoError(t, enc.EncodeString("a"))
	require.NoError(t, enc.EncodeInt(1))
	require.NoError(t, enc.EncodeString("a"))
	require.NoError(t, enc.EncodeInt(2))

	var m msgpack.OrderedMap
	m.KeepDuplicates = true
	require.NoError(t, msgpack.Unmarshal(buf.Bytes(), &m))

	require.Equal(t, 2, m.Len())
	require.Equal(t, []interface{}{int8(1), int8(2)}, m.GetAll("a"))

	v, ok := m.Get("a")
	require.True(t, ok)
	require.Equal(t, int8(1), v)

	m.Set("a", 9)
	require.Equal(t, []interface{}{9, int8(2)}, m.GetAll("a"))

	require.True(t, m.Delete("a"))
	require.Equal(t, 0, m.Len())
}

func TestOrderedMapNestedDuplicateKeysRejected(t *testing.T) {
	inner := msgpack.NewOrderedMap()
	inner.KeepDuplicates = false
	inner.Add("dup", 1)
	inner.Add("dup", 2)

	b, err := msgpack.Marshal(inner)
	require.NoError(t, err)

	var m msgpack.OrderedMap
	err = msgpack.Unmarshal(b, &m)
	require.Error(t, err)

	m.KeepDuplicates = true
	require.NoError(t, msgpack.Unmarshal(b, &m))
	require.Equal(t, 2, m.Len())
}

func TestOrderedMapInvalidInputDoesNotPanic(t *testing.T) {
	invalid := [][]byte{
		nil,
		{},
		{0xc1},                         // code that is never valid in MessagePack
		{0x81},                         // fixmap with 1 pair, but no key/value
		{0x81, 0xa1},                   // fixmap with a truncated string key
		{0x81, 0xa1, 'k'},              // key without value
		{0xde, 0x00},                   // truncated map16 header
		{0xdf, 0xff, 0xff, 0xff, 0xff}, // map32 with huge length and no data
		{0x01},                         // integer where a map is expected
		{0x90},                         // array where a map is expected
	}

	for i, data := range invalid {
		var m msgpack.OrderedMap
		m.KeepDuplicates = true
		err := msgpack.Unmarshal(data, &m)
		require.Errorf(t, err, "input %d (%x) must be rejected", i, data)
	}
}

func TestOrderedMapSetAtDelete(t *testing.T) {
	m := msgpack.NewOrderedMap()
	m.Set("a", 1)
	m.Set("b", 2)
	m.Set("a", 3)

	require.Equal(t, 2, m.Len())
	require.Equal(t,
		[]msgpack.MapItem{
			{Key: "a", Value: 3},
			{Key: "b", Value: 2},
		},
		m.Items(),
	)

	m.Add("b", 4)
	require.Equal(t, []interface{}{2, 4}, m.GetAll("b"))

	key, value, ok := m.At(0)
	require.True(t, ok)
	require.Equal(t, "a", key)
	require.Equal(t, 3, value)

	_, _, ok = m.At(10)
	require.False(t, ok)
}
