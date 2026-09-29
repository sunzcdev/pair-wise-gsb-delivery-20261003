package msgpack_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"
)

func TestOrderedMapRoundTrip(t *testing.T) {
	in := msgpack.OrderedMap{
		{
			Key: []interface{}{
				int(1),
				"x",
				msgpack.OrderedMap{{Key: "nested", Value: true}},
			},
			Value: "composite",
		},
		{Key: int(1), Value: "one"},
		{Key: "1", Value: "string one"},
		{Key: int(1), Value: "duplicate one"},
	}

	b, err := msgpack.Marshal(in)
	require.NoError(t, err)

	dec := msgpack.NewDecoder(bytes.NewReader(b))
	out, err := dec.DecodeOrderedMap()
	require.NoError(t, err)
	require.Len(t, out, 4)
	expected := msgpack.OrderedMap{
		{
			Key: []interface{}{
				int8(1),
				"x",
				msgpack.OrderedMap{{Key: "nested", Value: true}},
			},
			Value: "composite",
		},
		{Key: int8(1), Value: "one"},
		{Key: "1", Value: "string one"},
		{Key: int8(1), Value: "duplicate one"},
	}
	require.Equal(t, expected, out)

	compositeValue, ok := out.Get([]interface{}{
		int8(1),
		"x",
		msgpack.OrderedMap{{Key: "nested", Value: true}},
	})
	require.True(t, ok)
	require.Equal(t, "composite", compositeValue)

	first, ok := out.Get(int8(1))
	require.True(t, ok)
	require.Equal(t, "one", first)
	require.Equal(t, []interface{}{"one", "duplicate one"}, out.GetAll(int64(1)))

	first, ok = out.Get("1")
	require.True(t, ok)
	require.Equal(t, "string one", first)

	_, ok = out.Get(uint8(2))
	require.False(t, ok)

	b2, err := msgpack.Marshal(out)
	require.NoError(t, err)

	var roundTrip msgpack.OrderedMap
	require.NoError(t, msgpack.Unmarshal(b2, &roundTrip))
	require.Equal(t, expected, roundTrip)
}

func TestOrderedMapUnmarshal(t *testing.T) {
	in := msgpack.OrderedMap{{Key: int8(1), Value: "one"}}
	b, err := msgpack.Marshal(in)
	require.NoError(t, err)

	var out msgpack.OrderedMap
	require.NoError(t, msgpack.Unmarshal(b, &out))
	require.Equal(t, in, out)
}

func TestOrderedMapRejectDuplicateKeys(t *testing.T) {
	in := msgpack.OrderedMap{
		{Key: int(1), Value: "one"},
		{Key: "1", Value: "string one"},
		{Key: int8(1), Value: "duplicate one"},
	}
	b, err := msgpack.Marshal(in)
	require.NoError(t, err)

	var out msgpack.OrderedMap
	dec := msgpack.NewDecoder(bytes.NewReader(b))
	dec.DisallowDuplicateMapKeys(true)
	err = dec.Decode(&out)
	require.EqualError(t, err, "msgpack: duplicate map key: 1")
	require.Nil(t, out)
}

func TestOrderedMapInvalidInput(t *testing.T) {
	tests := [][]byte{
		nil,
		{},
		{0x81},
		{0x81, 0x01},
		{0x81, 0xc1},
		{0x81, 0x81, 0xc1},
		{0x82, 0x01, 0xa1, 0x61, 0x01},
	}

	for _, rejectDuplicates := range []bool{false, true} {
		for _, b := range tests {
			dec := msgpack.NewDecoder(bytes.NewReader(b))
			dec.DisallowDuplicateMapKeys(rejectDuplicates)

			require.NotPanics(t, func() {
				_, err := dec.DecodeOrderedMap()
				require.Error(t, err)
			})

			var out msgpack.OrderedMap
			dec = msgpack.NewDecoder(bytes.NewReader(b))
			dec.DisallowDuplicateMapKeys(rejectDuplicates)

			require.NotPanics(t, func() {
				err := dec.Decode(&out)
				require.Error(t, err)
			})
		}
	}
}

func TestOrderedMapInvalidKeyEncoding(t *testing.T) {
	in := msgpack.OrderedMap{{Key: make(chan bool), Value: "value"}}
	_, err := msgpack.Marshal(in)
	require.Error(t, err)
}
