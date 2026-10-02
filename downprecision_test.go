package hyperloglog

import (
	"math/rand"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func randHashes(t *testing.T, n int) []uint64 {
	t.Helper()
	rng := rand.New(rand.NewSource(int64(n)))
	hashes := make([]uint64, n)
	for i := range hashes {
		hashes[i] = rng.Uint64()
	}
	return hashes
}

func sortedSparseKeys(sk *Sketch) []uint32 {
	keys := make([]uint32, 0, sk.tmpSet.Len()+sk.sparseList.Len())
	sk.tmpSet.ForEach(func(k uint32) {
		keys = append(keys, k)
	})
	for iter := sk.sparseList.Iter(); iter.HasNext(); {
		keys = append(keys, iter.Next())
	}
	slices.Sort(keys)
	return keys
}

func insertHashes(sk *Sketch, hashes []uint64) {
	for _, h := range hashes {
		sk.InsertHash(h)
	}
}

func TestDownPrecision_DiscardedBitsFeedRho(t *testing.T) {
	sk, err := NewSketch(8, false)
	require.NoError(t, err)

	// Index 0x14 at p=8: the discarded low nibble 0x4 has its first set
	// bit two positions in, so the folded rho is 2 no matter how large the
	// old rho was. A plain maximum over old registers would keep 57.
	sk.InsertHash(0x1400000000000000)
	require.EqualValues(t, 57, sk.regs[0x14])

	folded, err := sk.DownPrecision(4)
	require.NoError(t, err)
	require.EqualValues(t, 2, folded.regs[0x1])

	// Discarded bits all zero: the folded rho grows by the number of
	// discarded bits, here 4 + 57 = 61 = maxRho(4).
	sk2, err := NewSketch(8, false)
	require.NoError(t, err)
	sk2.InsertHash(0x1000000000000000)
	require.EqualValues(t, 57, sk2.regs[0x10])

	folded2, err := sk2.DownPrecision(4)
	require.NoError(t, err)
	require.EqualValues(t, 61, folded2.regs[0x1])
}

func TestDownPrecision_DenseMatchesDirectBuild(t *testing.T) {
	hashes := randHashes(t, 50000)

	direct, err := NewSketch(10, false)
	require.NoError(t, err)
	insertHashes(direct, hashes)

	high, err := NewSketch(14, false)
	require.NoError(t, err)
	insertHashes(high, hashes)

	folded, err := high.DownPrecision(10)
	require.NoError(t, err)

	require.False(t, folded.sparse())
	require.Equal(t, direct.regs, folded.regs)
	require.Equal(t, direct.Estimate(), folded.Estimate())
}

func TestDownPrecision_SparseMatchesDirectBuild(t *testing.T) {
	hashes := randHashes(t, 100)

	direct := NewTestSketch(10)
	insertHashes(direct, hashes)
	require.True(t, direct.sparse())

	high := NewTestSketch(14)
	insertHashes(high, hashes)
	require.True(t, high.sparse())

	folded, err := high.DownPrecision(10)
	require.NoError(t, err)

	require.True(t, folded.sparse())
	require.Equal(t, sortedSparseKeys(direct), sortedSparseKeys(folded))
	require.Equal(t, direct.Estimate(), folded.Estimate())
}

func TestDownPrecision_SparseSourceMatchesDirectDenseBuild(t *testing.T) {
	// Enough hashes that a p=10 sketch promotes to dense while the p=14
	// sketch stays sparse: folding the sparse source must still reproduce
	// the direct dense registers exactly.
	hashes := randHashes(t, 5000)

	direct := NewTestSketch(10)
	insertHashes(direct, hashes)
	require.False(t, direct.sparse())

	high := NewTestSketch(14)
	insertHashes(high, hashes)
	require.True(t, high.sparse())

	folded, err := high.DownPrecision(10)
	require.NoError(t, err)

	require.False(t, folded.sparse())
	require.Equal(t, direct.regs, folded.regs)
}

func TestDownPrecision_ZeroValue(t *testing.T) {
	var sk Sketch

	folded, err := sk.DownPrecision(10)
	require.NoError(t, err)
	require.EqualValues(t, 10, folded.p)
	require.True(t, folded.sparse())
	require.Zero(t, folded.Estimate())

	folded.InsertHash(1)
	require.EqualValues(t, 1, folded.Estimate())

	// The zero value stays a zero value.
	require.Zero(t, sk.Estimate())
	require.Zero(t, sk.p)
}

func TestDownPrecision_Errors(t *testing.T) {
	sk := NewTestSketch(10)
	sk.InsertHash(1)

	_, err := sk.DownPrecision(11)
	require.ErrorIs(t, err, ErrorInvalidPrecision)
	require.Contains(t, err.Error(), "cannot raise precision")

	_, err = sk.DownPrecision(18)
	require.ErrorIs(t, err, ErrorInvalidPrecision)

	_, err = sk.DownPrecision(3)
	require.ErrorIs(t, err, ErrorInvalidPrecision)

	_, err = sk.DownPrecision(19)
	require.ErrorIs(t, err, ErrorInvalidPrecision)

	var zero Sketch
	_, err = zero.DownPrecision(3)
	require.ErrorIs(t, err, ErrorInvalidPrecision)
}

func TestDownPrecision_EqualPrecisionClones(t *testing.T) {
	sk := NewTestSketch(10)
	insertHashes(sk, randHashes(t, 100))

	folded, err := sk.DownPrecision(10)
	require.NoError(t, err)
	require.True(t, isSketchEqual(sk, folded))

	folded.InsertHash(0xdeadbeef)
	require.NotEqual(t, sortedSparseKeys(sk), sortedSparseKeys(folded))
}

func TestDownPrecision_LeavesSourceUnchanged(t *testing.T) {
	for _, sparse := range []bool{true, false} {
		sk, err := NewSketch(12, sparse)
		require.NoError(t, err)
		insertHashes(sk, randHashes(t, 1000))

		before, err := sk.AppendBinary(nil)
		require.NoError(t, err)

		_, err = sk.DownPrecision(8)
		require.NoError(t, err)

		after, err := sk.AppendBinary(nil)
		require.NoError(t, err)
		require.Equal(t, before, after, "sparse=%v", sparse)
	}
}

func TestMergeLowerPrecision(t *testing.T) {
	hashesA := randHashes(t, 30000)
	hashesB := randHashes(t, 30001)

	build := func(p uint8, hashes ...[]uint64) *Sketch {
		sk, err := NewSketch(p, false)
		require.NoError(t, err)
		for _, hs := range hashes {
			insertHashes(sk, hs)
		}
		return sk
	}

	// Receiver keeps its lower precision, other is folded down.
	direct := build(10, hashesA, hashesB)
	skA, skB := build(10, hashesA), build(14, hashesB)
	require.NoError(t, skA.MergeLowerPrecision(skB))
	require.EqualValues(t, 10, skA.p)
	require.Equal(t, direct.regs, skA.regs)

	// Receiver is folded down to other's lower precision.
	direct8 := build(8, hashesA, hashesB)
	skC, skD := build(8, hashesA), build(16, hashesB)
	require.NoError(t, skD.MergeLowerPrecision(skC))
	require.EqualValues(t, 8, skD.p)
	require.Equal(t, direct8.regs, skD.regs)

	// Sources are unchanged.
	require.Equal(t, build(14, hashesB).regs, skB.regs)
	require.Equal(t, build(8, hashesA).regs, skC.regs)

	// Equal precisions behave like Merge.
	skE, skF := build(12, hashesA), build(12, hashesB)
	require.NoError(t, skE.MergeLowerPrecision(skF))
	require.Equal(t, build(12, hashesA, hashesB).regs, skE.regs)
}

func TestMergeLowerPrecision_Sparse(t *testing.T) {
	hashesA := randHashes(t, 100)
	hashesB := randHashes(t, 101)

	direct := NewTestSketch(10)
	insertHashes(direct, hashesA)
	insertHashes(direct, hashesB)

	skA := NewTestSketch(10)
	insertHashes(skA, hashesA)
	skB := NewTestSketch(14)
	insertHashes(skB, hashesB)

	require.NoError(t, skA.MergeLowerPrecision(skB))
	require.EqualValues(t, 10, skA.p)
	require.True(t, skA.sparse())
	require.Equal(t, sortedSparseKeys(direct), sortedSparseKeys(skA))
	require.Equal(t, direct.Estimate(), skA.Estimate())
}

func TestMergeLowerPrecision_ZeroAndNil(t *testing.T) {
	var sk Sketch
	require.NoError(t, sk.MergeLowerPrecision(nil))
	require.NoError(t, sk.MergeLowerPrecision(&Sketch{}))
	require.Zero(t, sk.p)

	require.NoError(t, sk.MergeLowerPrecision(New16NoSparse()))
	require.EqualValues(t, 16, sk.p)
	require.False(t, sk.sparse())

	before, err := sk.AppendBinary(nil)
	require.NoError(t, err)
	require.NoError(t, sk.MergeLowerPrecision(nil))
	require.NoError(t, sk.MergeLowerPrecision(&Sketch{}))
	after, err := sk.AppendBinary(nil)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestMerge_StillRejectsPrecisionMismatch(t *testing.T) {
	sk := NewTestSketch(16)
	sk2 := NewTestSketch(10)
	require.ErrorIs(t, sk.Merge(sk2), ErrorPrecisionMismatch)
	require.ErrorIs(t, sk2.Merge(sk), ErrorPrecisionMismatch)
}
