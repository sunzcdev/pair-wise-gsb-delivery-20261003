package msgpack_test

import (
	"bytes"
	"strings"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"
)

// txCustom is a custom decoder that confines mutations to the receiver,
// which is the supported combination with DecodeTransactional.
type txCustom string

func (s *txCustom) DecodeMsgpack(dec *msgpack.Decoder) error {
	v, err := dec.DecodeString()
	if err != nil {
		return err
	}
	*s = txCustom(strings.ToUpper(v))
	return nil
}

type txConfig struct {
	Name      string
	Primary   map[string]int // shares the map with Secondary
	Secondary map[string]int
	Labels    []string // shares the backing array with Aliases
	Aliases   []string
	Level     *int // shares the pointer with MaxLevel
	MaxLevel  *int
	Custom    txCustom
	Note      string // last field; the input error lands here
}

func newTxConfig() (*txConfig, map[string]int, []string, *int) {
	shared := map[string]int{"one": 1}
	labels := []string{"a", "b"}
	level := 3
	cfg := &txConfig{
		Name:      "old",
		Primary:   shared,
		Secondary: shared,
		Labels:    labels,
		Aliases:   labels,
		Level:     &level,
		MaxLevel:  &level,
		Custom:    "OLD",
		Note:      "keep",
	}
	return cfg, shared, labels, &level
}

// encodeTxConfig encodes the config as a msgpack array in field order.
// With corruptLast the final element is 0xc1, which is not a valid msgpack
// code, so decoding fails on the very last byte of the input.
func encodeTxConfig(t *testing.T, corruptLast bool) []byte {
	t.Helper()

	buf := new(bytes.Buffer)
	enc := msgpack.NewEncoder(buf)
	require.NoError(t, enc.EncodeArrayLen(9))
	require.NoError(t, enc.EncodeString("new"))
	require.NoError(t, enc.Encode(map[string]int{"two": 2}))
	require.NoError(t, enc.Encode(map[string]int{"three": 3}))
	require.NoError(t, enc.Encode([]string{"x", "y"}))
	require.NoError(t, enc.Encode([]string{"x", "y"}))
	require.NoError(t, enc.EncodeInt(9))
	require.NoError(t, enc.EncodeInt(9))
	require.NoError(t, enc.EncodeString("custom"))
	if corruptLast {
		require.NoError(t, buf.WriteByte(0xc1))
	} else {
		require.NoError(t, enc.EncodeString("updated"))
	}
	return buf.Bytes()
}

func sameStringMap(a, b map[string]int) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == 0 && len(b) == 0 && a != nil == (b != nil)
	}
	// Maps are reference types: mutate one, observe the other.
	a["__probe__"] = 1
	_, ok := b["__probe__"]
	delete(a, "__probe__")
	return ok
}

func TestDecodeTransactionalFailure(t *testing.T) {
	cfg, sharedMap, sharedSlice, level := newTxConfig()

	err := msgpack.UnmarshalTransactional(encodeTxConfig(t, true), cfg)
	require.Error(t, err)

	// The target object is fully unchanged, including the fields that were
	// decoded successfully before the error at the end of the input.
	require.Equal(t, "old", cfg.Name)
	require.Equal(t, map[string]int{"one": 1}, cfg.Primary)
	require.Equal(t, map[string]int{"one": 1}, cfg.Secondary)
	require.Equal(t, []string{"a", "b"}, cfg.Labels)
	require.Equal(t, []string{"a", "b"}, cfg.Aliases)
	require.Equal(t, 3, *cfg.Level)
	require.Equal(t, 3, *cfg.MaxLevel)
	require.Equal(t, txCustom("OLD"), cfg.Custom)
	require.Equal(t, "keep", cfg.Note)

	// The sharing topology inside the object is unchanged.
	require.True(t, sameStringMap(cfg.Primary, cfg.Secondary))
	require.True(t, &cfg.Labels[0] == &cfg.Aliases[0])
	require.True(t, cfg.Level == cfg.MaxLevel)

	// References into the object graph held outside of cfg still observe
	// the old data. A shallow copy of the top-level struct would have let
	// the decoder write "two"/"three" into the shared map.
	require.Equal(t, map[string]int{"one": 1}, sharedMap)
	require.True(t, sameStringMap(sharedMap, cfg.Primary))
	require.Equal(t, []string{"a", "b"}, sharedSlice)
	require.Equal(t, 3, *level)
}

func TestDecodeTransactionalSuccess(t *testing.T) {
	cfg, sharedMap, _, level := newTxConfig()

	require.NoError(t, msgpack.UnmarshalTransactional(encodeTxConfig(t, false), cfg))

	// Map decoding merges into the existing map. Because Primary and
	// Secondary shared a single map, both decodes land in the same clone,
	// exactly as they would with a plain Decode on the original object.
	require.Equal(t, map[string]int{"one": 1, "two": 2, "three": 3}, cfg.Primary)
	require.Equal(t, cfg.Primary, cfg.Secondary)
	require.True(t, sameStringMap(cfg.Primary, cfg.Secondary))

	require.Equal(t, "new", cfg.Name)
	require.Equal(t, []string{"x", "y"}, cfg.Labels)
	require.Equal(t, []string{"x", "y"}, cfg.Aliases)
	require.True(t, &cfg.Labels[0] == &cfg.Aliases[0])
	require.Equal(t, 9, *cfg.Level)
	require.Equal(t, 9, *cfg.MaxLevel)
	require.True(t, cfg.Level == cfg.MaxLevel)
	require.Equal(t, txCustom("CUSTOM"), cfg.Custom)
	require.Equal(t, "updated", cfg.Note)

	// Documented semantics: *cfg was swapped for a new value graph.
	// External references into the OLD graph are not updated and keep
	// observing the pre-decode state.
	require.Equal(t, map[string]int{"one": 1}, sharedMap)
	require.False(t, sameStringMap(sharedMap, cfg.Primary))
	require.Equal(t, 3, *level)
}

func TestDecodeTransactionalUnsupported(t *testing.T) {
	type withUnsafePtr struct {
		Name string
		Ptr  unsafe.Pointer
	}

	n := 1
	v := &withUnsafePtr{Name: "old", Ptr: unsafe.Pointer(&n)}

	buf := new(bytes.Buffer)
	enc := msgpack.NewEncoder(buf)
	require.NoError(t, enc.EncodeArrayLen(2))
	require.NoError(t, enc.EncodeString("new"))
	require.NoError(t, enc.EncodeInt(1))

	err := msgpack.UnmarshalTransactional(buf.Bytes(), v)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsafe.Pointer")

	// The error is returned before any decoding takes place.
	require.Equal(t, "old", v.Name)
}

func TestDecodeTransactionalInvalidArgs(t *testing.T) {
	require.Error(t, msgpack.UnmarshalTransactional(nil, nil))
	require.Error(t, msgpack.UnmarshalTransactional(nil, "not a pointer"))
	var cfg *txConfig
	require.Error(t, msgpack.UnmarshalTransactional(nil, cfg))
}

func TestDecodeTransactionalMatchesDecode(t *testing.T) {
	// Transactional decode must produce the same result as a plain decode
	// into the same starting value.
	data := encodeTxConfig(t, false)

	cfg1, _, _, _ := newTxConfig()
	require.NoError(t, msgpack.UnmarshalTransactional(data, cfg1))

	cfg2, _, _, _ := newTxConfig()
	require.NoError(t, msgpack.Unmarshal(data, cfg2))

	require.Equal(t, cfg2, cfg1)
}
