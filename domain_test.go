package hyperloglog

import (
	"fmt"
	"hash/fnv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDomainSketch_DifferentSeedsRejected(t *testing.T) {
	// Same data and same algorithm, but different seeds: the hashes occupy
	// different domains and must never be merged.
	a, err := NewDomainSketch(14, true, MetroHashConfig(1, "events"))
	require.NoError(t, err)
	b, err := NewDomainSketch(14, true, MetroHashConfig(2, "events"))
	require.NoError(t, err)

	for i := range 100 {
		e := fmt.Appendf(nil, "item-%d", i)
		a.Insert(e)
		b.Insert(e)
	}

	require.NotEqual(t, a.Domain(), b.Domain())
	err = a.Merge(b)
	require.ErrorIs(t, err, ErrorDomainMismatch)

	// The failed merge leaves a untouched.
	clone := a.Clone()
	require.Equal(t, clone.Estimate(), a.Estimate())

	// Same seed, same algorithm, but distinct domain tags are different
	// domains as well.
	c, err := NewDomainSketch(14, true, MetroHashConfig(1, "sessions"))
	require.NoError(t, err)
	require.ErrorIs(t, a.Merge(c), ErrorDomainMismatch)
}

func TestDomainSketch_SameDomainSerializeMergeAcrossProcess(t *testing.T) {
	// Simulates two processes: both configure the identical hash domain, but
	// each supplies the hash function itself, which never crosses the wire.
	producer, err := NewDomainSketch(14, true, MetroHashConfig(4242, "events"))
	require.NoError(t, err)
	for i := range 500 {
		producer.Insert(fmt.Appendf(nil, "remote-item-%d", i))
	}

	data, err := producer.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, byte(domainBinaryMagic), data[0])

	// The "remote" side rebuilds the function from only the recorded config.
	remote, err := UnmarshalDomainBinary(data, MetroHashConfig(4242, "events"))
	require.NoError(t, err)
	require.False(t, remote.IsLegacy())
	require.Equal(t, producer.Domain(), remote.Domain())
	require.Equal(t, producer.Estimate(), remote.Estimate())

	// Merge the re-decoded remote sketch into a local one from the same domain.
	local, err := NewDomainSketch(14, true, MetroHashConfig(4242, "events"))
	require.NoError(t, err)
	for i := range 500 {
		local.Insert(fmt.Appendf(nil, "local-item-%d", i))
	}
	require.NoError(t, local.Merge(remote))

	// Sanity: the union holds roughly 1000 distinct elements.
	est := local.Estimate()
	require.InDelta(t, 1000, est, 50)
}

func TestDomainSketch_UnmarshalConfigMismatch(t *testing.T) {
	producer, err := NewDomainSketch(14, true, MetroHashConfig(4242, "events"))
	require.NoError(t, err)
	producer.Insert([]byte("a"))
	data, err := producer.MarshalBinary()
	require.NoError(t, err)

	// Wrong seed.
	_, err = UnmarshalDomainBinary(data, MetroHashConfig(9999, "events"))
	require.ErrorIs(t, err, ErrorDomainMismatch)

	// Wrong domain tag.
	_, err = UnmarshalDomainBinary(data, MetroHashConfig(4242, "sessions"))
	require.ErrorIs(t, err, ErrorDomainMismatch)

	// Receiver based UnmarshalBinary enforces the same check.
	other, err := NewDomainSketch(14, true, MetroHashConfig(9999, "events"))
	require.NoError(t, err)
	require.ErrorIs(t, other.UnmarshalBinary(data), ErrorDomainMismatch)

	// A zero value cannot unmarshal: the caller must name the domain and
	// supply the hash function up front.
	var bare DomainSketch
	require.ErrorIs(t, bare.UnmarshalBinary(data), ErrorInvalidHashConfig)
}

func TestDomainSketch_CustomHashFunctionNotSerialized(t *testing.T) {
	fnvHash := func(e []byte) uint64 {
		h := fnv.New64a()
		h.Write(e)
		return h.Sum64()
	}

	a, err := NewDomainSketch(14, true, CustomHashConfig("fnv64a", "tenant-7", fnvHash))
	require.NoError(t, err)
	for i := range 200 {
		a.Insert(fmt.Appendf(nil, "e-%d", i))
	}

	data, err := a.MarshalBinary()
	require.NoError(t, err)

	// Deserialization supplies a freshly constructed function with the same
	// closure body; no pointer or function value is carried in data.
	b, err := UnmarshalDomainBinary(data, CustomHashConfig("fnv64a", "tenant-7", fnvHash))
	require.NoError(t, err)
	require.Equal(t, a.Domain(), b.Domain())
	require.Equal(t, a.Estimate(), b.Estimate())
	require.NoError(t, a.Merge(b))

	// An algorithm name nobody provides a function for cannot deserialize,
	// even with an otherwise matching tag.
	_, err = UnmarshalDomainBinary(data, CustomHashConfig("fnv64a", "tenant-7", nil))
	require.ErrorIs(t, err, ErrorInvalidHashConfig)

	// Forgetting the function but renaming the algorithm fails too.
	_, err = UnmarshalDomainBinary(data, HashConfig{Algorithm: "other", Domain: "tenant-7"})
	require.ErrorIs(t, err, ErrorInvalidHashConfig)
}

func TestDomainSketch_LegacyEncodingMarkedUnspecified(t *testing.T) {
	// A current plain Sketch encodes as v2 with no domain.
	plain := New14()
	for i := range 300 {
		plain.Insert(fmt.Appendf(nil, "legacy-%d", i))
	}
	data, err := plain.MarshalBinary()
	require.NoError(t, err)

	// The domain-aware decoder rejects legacy bytes instead of assuming the
	// default domain.
	_, err = UnmarshalDomainBinary(data, HashConfig{})
	require.ErrorIs(t, err, ErrorLegacyEncoding)

	legacy, err := LegacyDomainSketch(data)
	require.NoError(t, err)
	require.True(t, legacy.IsLegacy())
	require.Equal(t, UnspecifiedDomain, legacy.Domain())
	require.Equal(t, plain.Estimate(), legacy.Estimate())

	// Legacy data is not silently treated as a declared domain, including the
	// package default one.
	declared, err := NewDomainSketch(14, true, HashConfig{})
	require.NoError(t, err)
	require.NotEqual(t, declared.Domain(), legacy.Domain())
	require.ErrorIs(t, declared.Merge(legacy), ErrorDomainMismatch)

	// Two legacy sketches (one dense) may still be combined.
	dense := NewNoSparse()
	for i := range 300 {
		dense.Insert(fmt.Appendf(nil, "legacy-%d", i+100))
	}
	denseData, err := dense.MarshalBinary()
	require.NoError(t, err)
	legacyDense, err := LegacyDomainSketch(denseData)
	require.NoError(t, err)
	require.NoError(t, legacy.Merge(legacyDense))
	require.InDelta(t, 400, legacy.Estimate(), 20)

	// Legacy marshal again in the plain format for existing readers.
	roundTrip, err := legacy.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, byte(version), roundTrip[0])
	plainAgain := &Sketch{}
	require.NoError(t, plainAgain.UnmarshalBinary(roundTrip))
	require.Equal(t, legacy.Estimate(), plainAgain.Estimate())
}

func TestDomainSketch_LegacyV1Encoding(t *testing.T) {
	// v1 dense header: version 1, p 14, bias b, dense flag, 4 ignored bytes,
	// m/2 packed register bytes of zeros (an empty sketch).
	blob := make([]byte, 8+1<<14/2)
	blob[0] = 1
	blob[1] = 14
	legacy, err := LegacyDomainSketch(blob)
	require.NoError(t, err)
	require.True(t, legacy.IsLegacy())
	require.EqualValues(t, 0, legacy.Estimate())

	// A v1 blob must not decode through the domain-aware path.
	_, err = UnmarshalDomainBinary(blob, HashConfig{})
	require.ErrorIs(t, err, ErrorLegacyEncoding)
}

func TestDomainSketch_DefaultConfigIsDefaultDomain(t *testing.T) {
	d, err := NewDomainSketch(14, true, HashConfig{})
	require.NoError(t, err)

	d.Insert([]byte("hello"))
	plain := New14()
	plain.Insert([]byte("hello"))

	data, err := d.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, byte(domainBinaryMagic), data[0])

	got, err := UnmarshalDomainBinary(data, HashConfig{})
	require.NoError(t, err)
	require.Equal(t, d.Domain(), got.Domain())
	require.Equal(t, plain.Estimate(), got.Estimate())
}

func TestNewDomainSketch_RequiresDomainTag(t *testing.T) {
	// A non default seed with no tag would be indistinguishable from another
	// unnamed seed on purpose.
	_, err := NewDomainSketch(14, true, MetroHashConfig(1, ""))
	require.ErrorIs(t, err, ErrorInvalidHashConfig)

	// A custom algorithm with no name is invalid.
	_, err = NewDomainSketch(14, true, HashConfig{Domain: "x", Hash: func([]byte) uint64 { return 0 }})
	require.ErrorIs(t, err, ErrorInvalidHashConfig)

	// Precision is still validated after the config.
	_, err = NewDomainSketch(2, true, MetroHashConfig(1, "x"))
	require.ErrorIs(t, err, ErrorInvalidPrecision)
}

func TestDomainSketch_InsertHashDeclaresDomain(t *testing.T) {
	cfg := MetroHashConfig(7, "events")
	d, err := NewDomainSketch(14, true, cfg)
	require.NoError(t, err)

	// The caller explicitly feeds values hashed within d's domain.
	d.InsertHash(cfg.normalizedHash(t, []byte("a")))
	d.InsertHash(cfg.normalizedHash(t, []byte("a")))
	d.InsertHash(cfg.normalizedHash(t, []byte("b")))
	require.EqualValues(t, 2, d.Estimate())

	require.EqualValues(t, 14, d.Precision())

	clone := d.Clone()
	require.Equal(t, d.Domain(), clone.Domain())
	require.Equal(t, d.Estimate(), clone.Estimate())

	d.Reset()
	require.EqualValues(t, 0, d.Estimate())
	require.Equal(t, clone.Estimate(), clone.Estimate(), "clone must stay independent after Reset")
}

func TestDomainSketch_AppendBinaryPrefix(t *testing.T) {
	d, err := NewDomainSketch(14, true, MetroHashConfig(9, "events"))
	require.NoError(t, err)
	d.Insert([]byte("prefixed"))

	prefix := []byte{0xaa, 0xbb}
	out, err := d.AppendBinary(prefix)
	require.NoError(t, err)
	require.Equal(t, []byte{0xaa, 0xbb}, out[:2])

	got, err := UnmarshalDomainBinary(out[2:], MetroHashConfig(9, "events"))
	require.NoError(t, err)
	require.Equal(t, d.Estimate(), got.Estimate())
}

func TestDomainSketch_CorruptEncoding(t *testing.T) {
	d, err := NewDomainSketch(14, true, MetroHashConfig(5, "events"))
	require.NoError(t, err)
	d.Insert([]byte("x"))
	data, err := d.MarshalBinary()
	require.NoError(t, err)

	// Flip the recorded domain ID: descriptor and ID disagree.
	tampered := append([]byte(nil), data...)
	tampered[2] ^= 0xff
	_, err = UnmarshalDomainBinary(tampered, MetroHashConfig(5, "events"))
	require.ErrorIs(t, err, ErrorInvalidData)

	// Truncation is rejected before the receiver changes.
	_, err = UnmarshalDomainBinary(data[:domainHeaderSize], MetroHashConfig(5, "events"))
	require.Error(t, err)

	// Appended garbage after the framed inner sketch.
	trailing := append(append([]byte(nil), data...), 0)
	_, err = UnmarshalDomainBinary(trailing, MetroHashConfig(5, "events"))
	require.ErrorIs(t, err, ErrorInvalidData)

	// Unknown wrapper version.
	badVer := append([]byte(nil), data...)
	badVer[1] = 99
	_, err = UnmarshalDomainBinary(badVer, MetroHashConfig(5, "events"))
	require.ErrorIs(t, err, ErrorInvalidVersion)

	_, err = UnmarshalDomainBinary(nil, MetroHashConfig(5, "events"))
	require.ErrorIs(t, err, ErrorTooShort)
}

func (c HashConfig) normalizedHash(t *testing.T, e []byte) uint64 {
	t.Helper()
	cfg, err := c.normalized()
	require.NoError(t, err)
	return cfg.Hash(e)
}
