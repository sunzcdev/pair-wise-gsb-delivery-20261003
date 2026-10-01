package msgpack_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"
)

type txShared struct {
	Name  string
	Extra int
}

// txConfig contains several fields that deliberately share the same
// maps, slices, and pointers.
type txConfig struct {
	Title    string
	Primary  *txShared
	Backup   *txShared // same pointer as Primary
	Labels   map[string]string
	LabelsRO map[string]string // same map as Labels
	Tags     []string
	TagsRO   []string // same slice as Tags
}

func newTxConfig() (*txConfig, *txShared, map[string]string, []string) {
	shared := &txShared{Name: "shared", Extra: 1}
	labels := map[string]string{"k": "v"}
	tags := []string{"a", "b"}
	cfg := &txConfig{
		Title:    "old",
		Primary:  shared,
		Backup:   shared,
		Labels:   labels,
		LabelsRO: labels,
		Tags:     tags,
		TagsRO:   tags,
	}
	return cfg, shared, labels, tags
}

// encodeTxUpdate writes an update for txConfig with an invalid value
// (a string where a []string is expected) for Tags at the very end.
func encodeTxUpdate(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	enc := msgpack.NewEncoder(buf)
	require.NoError(t, enc.EncodeMapLen(4))
	require.NoError(t, enc.EncodeString("Title"))
	require.NoError(t, enc.EncodeString("new"))
	require.NoError(t, enc.EncodeString("Primary"))
	require.NoError(t, enc.Encode(txShared{Name: "changed", Extra: 2}))
	require.NoError(t, enc.EncodeString("Labels"))
	require.NoError(t, enc.Encode(map[string]string{"k": "updated", "k2": "added"}))
	require.NoError(t, enc.EncodeString("Tags"))
	require.NoError(t, enc.EncodeString("not-a-slice"))
}

func TestDecodeTransactionalFailureKeepsSharedState(t *testing.T) {
	cfg, shared, labels, tags := newTxConfig()

	var buf bytes.Buffer
	encodeTxUpdate(t, &buf)

	err := msgpack.NewDecoder(&buf).DecodeTransactional(cfg)
	require.Error(t, err)

	// The target object is untouched, including fields that were
	// successfully decoded before the error.
	require.Equal(t, "old", cfg.Title)
	require.True(t, cfg.Primary == shared)
	require.True(t, cfg.Backup == shared)
	require.Equal(t, &txShared{Name: "shared", Extra: 1}, cfg.Primary)

	// References to the shared graph held outside the object are
	// untouched as well.
	require.Equal(t, map[string]string{"k": "v"}, labels)
	require.Equal(t, []string{"a", "b"}, tags)

	// The sharing topology is intact.
	require.True(t, cfg.Primary == cfg.Backup)
	cfg.Labels["probe"] = "1"
	require.Equal(t, "1", cfg.LabelsRO["probe"])
	delete(cfg.Labels, "probe")
	cfg.Tags[0] = "probe"
	require.Equal(t, "probe", cfg.TagsRO[0])
	cfg.Tags[0] = "a"
}

func TestDecodeTransactionalFailureContrastWithDecode(t *testing.T) {
	// Sanity check: the same input really does half-update the target
	// when the plain Decode is used.
	cfg, _, _, _ := newTxConfig()

	var buf bytes.Buffer
	encodeTxUpdate(t, &buf)

	err := msgpack.NewDecoder(&buf).Decode(cfg)
	require.Error(t, err)
	require.Equal(t, "new", cfg.Title)
	require.Equal(t, &txShared{Name: "changed", Extra: 2}, cfg.Primary)
}

func TestDecodeTransactionalSuccess(t *testing.T) {
	cfg, shared, labels, tags := newTxConfig()

	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	require.NoError(t, enc.EncodeMapLen(4))
	require.NoError(t, enc.EncodeString("Title"))
	require.NoError(t, enc.EncodeString("new"))
	require.NoError(t, enc.EncodeString("Primary"))
	require.NoError(t, enc.Encode(txShared{Name: "changed", Extra: 2}))
	require.NoError(t, enc.EncodeString("Labels"))
	require.NoError(t, enc.Encode(map[string]string{"k2": "added"}))
	require.NoError(t, enc.EncodeString("Tags"))
	require.NoError(t, enc.Encode([]string{"x", "y"}))

	require.NoError(t, msgpack.NewDecoder(&buf).DecodeTransactional(cfg))

	// New values are committed.
	require.Equal(t, "new", cfg.Title)
	require.Equal(t, &txShared{Name: "changed", Extra: 2}, cfg.Primary)
	require.Equal(t, map[string]string{"k": "v", "k2": "added"}, cfg.Labels)
	require.Equal(t, []string{"x", "y"}, cfg.Tags)

	// Sharing within the committed graph mirrors the original.
	require.True(t, cfg.Primary == cfg.Backup)
	cfg.Labels["probe"] = "1"
	require.Equal(t, "1", cfg.LabelsRO["probe"])
	cfg.Tags[0] = "probe"
	require.Equal(t, "probe", cfg.TagsRO[0])

	// The committed graph is fresh: references captured before the call
	// still point to the old, unmodified data.
	require.False(t, cfg.Primary == shared)
	require.Equal(t, &txShared{Name: "shared", Extra: 1}, shared)
	require.Equal(t, map[string]string{"k": "v"}, labels)
	require.Equal(t, []string{"a", "b"}, tags)
}

func TestUnmarshalTransactional(t *testing.T) {
	cfg, _, _, _ := newTxConfig()

	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	require.NoError(t, enc.EncodeMapLen(1))
	require.NoError(t, enc.EncodeString("Title"))
	require.NoError(t, enc.EncodeString("new"))

	require.NoError(t, msgpack.UnmarshalTransactional(buf.Bytes(), cfg))
	require.Equal(t, "new", cfg.Title)

	// Truncated input fails and leaves the target unchanged.
	err := msgpack.UnmarshalTransactional(buf.Bytes()[:len(buf.Bytes())-2], cfg)
	require.Error(t, err)
	require.Equal(t, "new", cfg.Title)
}

type txSideEffectDecoder struct {
	Value string
}

func (s *txSideEffectDecoder) DecodeMsgpack(dec *msgpack.Decoder) error {
	v, err := dec.DecodeString()
	if err != nil {
		return err
	}
	s.Value = v
	return nil
}

type txConfigWithCustom struct {
	Title  string
	Custom txSideEffectDecoder
}

func TestDecodeTransactionalRejectsCustomDecoder(t *testing.T) {
	cfg := &txConfigWithCustom{
		Title:  "old",
		Custom: txSideEffectDecoder{Value: "orig"},
	}

	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	require.NoError(t, enc.EncodeMapLen(2))
	require.NoError(t, enc.EncodeString("Title"))
	require.NoError(t, enc.EncodeString("new"))
	require.NoError(t, enc.EncodeString("Custom"))
	require.NoError(t, enc.EncodeString("changed"))

	dec := msgpack.NewDecoder(&buf)
	err := dec.DecodeTransactional(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "DecodeTransactional")
	require.Contains(t, err.Error(), "txSideEffectDecoder")

	// The target is unchanged and the input was not consumed.
	require.Equal(t, "old", cfg.Title)
	require.Equal(t, "orig", cfg.Custom.Value)

	var check txConfigWithCustom
	require.NoError(t, dec.Decode(&check))
	require.Equal(t, "new", check.Title)
	require.Equal(t, "changed", check.Custom.Value)
}

func TestDecodeTransactionalRejectsCustomDecoderInInterface(t *testing.T) {
	cfg := &struct {
		Any interface{}
	}{Any: &txSideEffectDecoder{Value: "orig"}}

	err := msgpack.UnmarshalTransactional([]byte{0x91, 0xa1, 0x78}, cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "DecodeTransactional")
	require.Equal(t, "orig", cfg.Any.(*txSideEffectDecoder).Value)
}

func TestDecodeTransactionalAllowsTime(t *testing.T) {
	type txWithTime struct {
		Name string
		At   time.Time
	}

	now := time.Now()
	cfg := &txWithTime{Name: "old", At: now}

	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	require.NoError(t, enc.EncodeMapLen(2))
	require.NoError(t, enc.EncodeString("Name"))
	require.NoError(t, enc.EncodeString("new"))
	require.NoError(t, enc.EncodeString("At"))
	require.NoError(t, enc.Encode(now.Add(time.Hour)))

	require.NoError(t, msgpack.NewDecoder(&buf).DecodeTransactional(cfg))
	require.Equal(t, "new", cfg.Name)
	require.True(t, cfg.At.Equal(now.Add(time.Hour)))

	// A failure after the time field leaves the original time untouched.
	var bad bytes.Buffer
	enc = msgpack.NewEncoder(&bad)
	require.NoError(t, enc.EncodeMapLen(2))
	require.NoError(t, enc.EncodeString("At"))
	require.NoError(t, enc.Encode(now.Add(2*time.Hour)))
	require.NoError(t, enc.EncodeString("Name"))
	require.NoError(t, enc.Encode(true)) // invalid: bool where string is expected

	err := msgpack.NewDecoder(&bad).DecodeTransactional(cfg)
	require.Error(t, err)
	require.Equal(t, "new", cfg.Name)
	require.True(t, cfg.At.Equal(now.Add(time.Hour)))
}

func TestDecodeTransactionalInvalidTarget(t *testing.T) {
	dec := msgpack.NewDecoder(bytes.NewReader([]byte{0x01}))

	require.Error(t, dec.DecodeTransactional(nil))

	var notPtr string
	require.Error(t, dec.DecodeTransactional(notPtr))

	var nilPtr *string
	require.Error(t, dec.DecodeTransactional(nilPtr))
}

func TestDecodeTransactionalNilAndCycles(t *testing.T) {
	type node struct {
		Name string
		Next *node
	}

	n1 := &node{Name: "n1"}
	n2 := &node{Name: "n2"}
	n1.Next = n2
	n2.Next = n1 // cycle

	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	require.NoError(t, enc.EncodeMapLen(2))
	require.NoError(t, enc.EncodeString("Name"))
	require.NoError(t, enc.EncodeString("n1-updated"))
	require.NoError(t, enc.EncodeString("Next"))
	require.NoError(t, enc.Encode(true)) // invalid: bool where node is expected

	err := msgpack.NewDecoder(&buf).DecodeTransactional(n1)
	require.Error(t, err)
	require.Equal(t, "n1", n1.Name)
	require.True(t, n1.Next == n2)
	require.True(t, n2.Next == n1)

	// Nil maps, slices, and pointers survive a failed decode.
	cfg := &txConfig{}
	err = msgpack.UnmarshalTransactional([]byte{0x81, 0xa5, 'T', 'i', 't', 'l', 'e'}, cfg)
	require.Error(t, err)
	require.Nil(t, cfg.Primary)
	require.Nil(t, cfg.Labels)
	require.Nil(t, cfg.Tags)
}
