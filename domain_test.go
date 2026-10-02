package hyperloglog

import (
	"errors"
	"fmt"
	"testing"

	metro "github.com/dgryski/go-metro"
	"github.com/stretchr/testify/require"
)

func metroConfig(seed uint64, domain string) HashConfig {
	return HashConfig{
		Algorithm: "metro64",
		Seed:      seed,
		Domain:    domain,
		Hash:      func(e []byte) uint64 { return metro.Hash64(e, seed) },
	}
}

func TestDomainSketch_MergeRejectsDifferentSeeds(t *testing.T) {
	a, err := NewDomainSketch(metroConfig(1, ""))
	require.NoError(t, err)
	b, err := NewDomainSketch(metroConfig(2, ""))
	require.NoError(t, err)

	for i := 0; i < 1000; i++ {
		e := []byte(fmt.Sprintf("value-%d", i))
		a.Insert(e)
		b.Insert(e)
	}

	err = a.Merge(b)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrorDomainMismatch), "got %v", err)
}

func TestDomainSketch_MergeRejectsDifferentDomainLabels(t *testing.T) {
	a, err := NewDomainSketch(metroConfig(1, "users"))
	require.NoError(t, err)
	b, err := NewDomainSketch(metroConfig(1, "sessions"))
	require.NoError(t, err)

	a.Insert([]byte("alice"))
	b.Insert([]byte("alice"))

	require.ErrorIs(t, a.Merge(b), ErrorDomainMismatch)
}

func TestDomainSketch_SerializeMergeSameDomain(t *testing.T) {
	config := metroConfig(42, "events")

	// "Process" A: insert and serialize.
	a, err := NewDomainSketch(config)
	require.NoError(t, err)
	for i := 0; i < 10000; i++ {
		a.Insert([]byte(fmt.Sprintf("a-%d", i)))
	}
	data, err := a.MarshalBinary()
	require.NoError(t, err)

	// "Process" B: decode with the matching config, insert, merge back.
	b := &DomainSketch{}
	require.NoError(t, b.UnmarshalBinaryWithConfig(data, config))
	for i := 0; i < 10000; i++ {
		b.Insert([]byte(fmt.Sprintf("b-%d", i)))
	}

	merged, err := NewDomainSketch(config)
	require.NoError(t, err)
	require.NoError(t, merged.Merge(a))
	require.NoError(t, merged.Merge(b))

	exact := uint64(20000)
	est := merged.Estimate()
	require.LessOrEqual(t, 100*estimateError(est, exact), 2.0, "Exact %d, got %d", exact, est)
}

func TestDomainSketch_UnmarshalRejectsMismatchedConfig(t *testing.T) {
	a, err := NewDomainSketch(metroConfig(1, ""))
	require.NoError(t, err)
	a.Insert([]byte("alice"))
	data, err := a.MarshalBinary()
	require.NoError(t, err)

	configs := []HashConfig{
		metroConfig(2, ""),      // different seed
		metroConfig(1, "other"), // different domain label
		{Algorithm: "xxhash64", Seed: 1, Hash: metroConfig(1, "").Hash},
	}
	for _, config := range configs {
		d := &DomainSketch{}
		err := d.UnmarshalBinaryWithConfig(data, config)
		require.ErrorIs(t, err, ErrorDomainMismatch, "config %+v", config)
	}
}

func TestDomainSketch_LegacyDomainUnspecified(t *testing.T) {
	// A bare Sketch binary carries no domain identifier.
	sk := New()
	for i := 0; i < 1000; i++ {
		sk.Insert([]byte(fmt.Sprintf("legacy-%d", i)))
	}
	data, err := sk.MarshalBinary()
	require.NoError(t, err)

	legacy := &DomainSketch{}
	require.NoError(t, legacy.UnmarshalLegacyBinary(data))
	require.Equal(t, HashConfig{}, legacy.Config())

	// The unspecified domain must not silently match a named one.
	named, err := NewDomainSketch(metroConfig(1337, ""))
	require.NoError(t, err)
	require.ErrorIs(t, legacy.Merge(named), ErrorDomainMismatch)
	require.ErrorIs(t, named.Merge(legacy), ErrorDomainMismatch)

	// Two legacy sketches still merge with each other.
	other := &DomainSketch{}
	require.NoError(t, other.UnmarshalLegacyBinary(data))
	require.NoError(t, legacy.Merge(other))
	require.Equal(t, other.Estimate(), legacy.Estimate())

	// The two binary formats are never confused for one another.
	require.ErrorIs(t, (&DomainSketch{}).UnmarshalBinaryWithConfig(data, metroConfig(1337, "")), ErrorInvalidVersion)
	tagged, err := named.MarshalBinary()
	require.NoError(t, err)
	require.ErrorIs(t, (&DomainSketch{}).UnmarshalLegacyBinary(tagged), ErrorInvalidVersion)

	// A legacy sketch has no hash function to insert with.
	require.Panics(t, func() { legacy.Insert([]byte("x")) })
}

func TestDomainSketch_InsertHashDeclaresDomain(t *testing.T) {
	config := metroConfig(7, "external")
	a, err := NewDomainSketch(config)
	require.NoError(t, err)
	b, err := NewDomainSketch(config)
	require.NoError(t, err)

	// Hashes computed outside the sketch are added under the domain the
	// DomainSketch declares.
	for i := uint64(0); i < 5000; i++ {
		a.InsertHash(config.Hash(toByte(i)))
		b.InsertHash(config.Hash(toByte(i + 5000)))
	}
	require.NoError(t, a.Merge(b))

	exact := uint64(10000)
	est := a.Estimate()
	require.LessOrEqual(t, 100*estimateError(est, exact), 2.0, "Exact %d, got %d", exact, est)
}

func TestDomainSketch_InvalidHashConfig(t *testing.T) {
	_, err := NewDomainSketch(HashConfig{Seed: 1, Hash: metroConfig(1, "").Hash})
	require.ErrorIs(t, err, ErrorInvalidHashConfig)

	_, err = NewDomainSketch(HashConfig{Algorithm: "metro64", Seed: 1})
	require.ErrorIs(t, err, ErrorInvalidHashConfig)

	d := &DomainSketch{}
	require.ErrorIs(t, d.UnmarshalBinaryWithConfig(nil, HashConfig{}), ErrorInvalidHashConfig)
}
