package hyperloglog

import (
	"math"
	"math/rand"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDiagnosticsZeroValue(t *testing.T) {
	var sk Sketch
	d := sk.Diagnostics()
	require.EqualValues(t, 0, d.Precision)
	require.False(t, d.Sparse)
	require.Zero(t, d.PendingMerges)
	require.Zero(t, d.ZeroRegisters)
	require.Nil(t, d.RhoHistogram)
	require.Zero(t, d.RelativeStdErr)

	// Diagnosing a zero value must not initialize it.
	require.EqualValues(t, 0, sk.p)
	require.Nil(t, sk.regs)
	require.Nil(t, sk.sparseList)
}

func TestDiagnosticsDense(t *testing.T) {
	sk := New16NoSparse()
	hashes := make([]uint64, 1000)
	for i := range hashes {
		hashes[i] = rand.Uint64()
		sk.InsertHash(hashes[i])
	}

	d := sk.Diagnostics()
	require.EqualValues(t, 16, d.Precision)
	require.False(t, d.Sparse)
	require.Zero(t, d.PendingMerges)
	require.InDelta(t, 1.04/math.Sqrt(1<<16), d.RelativeStdErr, 1e-12)

	wantHist := make([]uint64, maxRho(sk.p)+1)
	for _, r := range sk.regs {
		wantHist[r]++
	}
	require.Equal(t, wantHist, d.RhoHistogram)
	require.Equal(t, wantHist[0], d.ZeroRegisters)

	var sum uint64
	for _, n := range d.RhoHistogram {
		sum += n
	}
	require.EqualValues(t, sk.m, sum)
}

func TestDiagnosticsSparseReadOnly(t *testing.T) {
	sk := New14()
	for i := 0; i < 100; i++ {
		sk.InsertHash(rand.Uint64())
	}
	require.True(t, sk.sparse())
	require.Positive(t, sk.tmpSet.Len())

	encBefore, err := sk.MarshalBinary()
	require.NoError(t, err)
	tmpLenBefore := sk.tmpSet.Len()
	listBefore := sk.sparseList.Clone()

	d := sk.Diagnostics()
	d2 := sk.Diagnostics()
	require.Equal(t, d, d2)

	// The call must not compact the staging set, convert the
	// representation, or otherwise alter the encoding.
	require.True(t, sk.sparse())
	require.Equal(t, tmpLenBefore, sk.tmpSet.Len())
	require.Equal(t, listBefore, sk.sparseList)
	encAfter, err := sk.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, encBefore, encAfter)

	require.Equal(t, tmpLenBefore, d.PendingMerges)
}

func TestDiagnosticsSparseDuplicatesCountedByEffectiveSet(t *testing.T) {
	sk := New14()
	hashes := make([]uint64, 100)
	for i := range hashes {
		hashes[i] = rand.Uint64()
		sk.InsertHash(hashes[i])
	}
	// Compact the staging set into the compressed list, then stage the
	// same hashes again so every staged key duplicates a listed key.
	sk.Estimate()
	require.True(t, sk.sparse())
	require.Zero(t, sk.tmpSet.Len())
	for _, h := range hashes {
		sk.InsertHash(h)
	}
	require.Equal(t, len(hashes), sk.tmpSet.Len())

	d := sk.Diagnostics()
	require.Equal(t, len(hashes), d.PendingMerges)

	// The effective register state ignores the duplicates: it must match
	// the dense conversion of the same sketch.
	dense := sk.Clone()
	dense.toNormal()
	dd := dense.Diagnostics()
	require.Equal(t, dd.RhoHistogram, d.RhoHistogram)
	require.Equal(t, dd.ZeroRegisters, d.ZeroRegisters)

	// And it must match the registers the hashes themselves imply.
	wantHist := make([]uint64, maxRho(sk.p)+1)
	wantHist[0] = uint64(sk.m)
	regValue := make(map[uint32]uint8)
	for _, h := range hashes {
		idx, r := getPosVal(h, sk.p)
		if r > regValue[uint32(idx)] {
			regValue[uint32(idx)] = r
		}
	}
	for _, r := range regValue {
		wantHist[r]++
	}
	wantHist[0] -= uint64(len(regValue))
	require.Equal(t, wantHist, d.RhoHistogram)
	require.Equal(t, wantHist[0], d.ZeroRegisters)
}

func TestDiagnosticsSparseMatchesDenseConversion(t *testing.T) {
	sk := New14()
	for i := 0; i < 1000; i++ {
		sk.InsertHash(rand.Uint64())
	}
	require.True(t, sk.sparse())

	d := sk.Diagnostics()
	require.True(t, d.Sparse)

	dense := sk.Clone()
	dense.toNormal()
	dd := dense.Diagnostics()
	require.False(t, dd.Sparse)
	require.Equal(t, dd.RhoHistogram, d.RhoHistogram)
	require.Equal(t, dd.ZeroRegisters, d.ZeroRegisters)

	var sum uint64
	for _, n := range d.RhoHistogram {
		sum += n
	}
	require.EqualValues(t, sk.m, sum)
	require.Equal(t, d.RhoHistogram[0], d.ZeroRegisters)
}

func TestDiagnosticsHistogramLength(t *testing.T) {
	for p := uint8(4); p <= 18; p++ {
		sk, err := NewSketch(p, p%2 == 0)
		require.NoError(t, err)
		d := sk.Diagnostics()
		require.Len(t, d.RhoHistogram, int(maxRho(p))+1)
		require.EqualValues(t, uint64(1)<<p, d.ZeroRegisters)
		require.EqualValues(t, uint64(1)<<p, d.RhoHistogram[0])
		require.InDelta(t, 1.04/math.Sqrt(float64(uint64(1)<<p)), d.RelativeStdErr, 1e-12)
	}
}

func TestDiagnosticsDoesNotReplaceRegisters(t *testing.T) {
	sk := New16NoSparse()
	for i := 0; i < 100; i++ {
		sk.InsertHash(rand.Uint64())
	}
	regsBefore := slices.Clone(sk.regs)
	encBefore, err := sk.MarshalBinary()
	require.NoError(t, err)

	sk.Diagnostics()

	require.Equal(t, regsBefore, sk.regs)
	encAfter, err := sk.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, encBefore, encAfter)
}
