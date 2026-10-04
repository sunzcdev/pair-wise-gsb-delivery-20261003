package hyperloglog

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func denseHistogram(sk *Sketch) []uint32 {
	clone := sk.Clone()
	clone.toNormal()
	hist := make([]uint32, maxRho(clone.p)+1)
	for _, r := range clone.regs {
		hist[r]++
	}
	return hist
}

func TestDiagnostics_ZeroValue(t *testing.T) {
	var sk Sketch
	d := sk.Diagnostics()
	require.EqualValues(t, 0, d.Precision)
	require.Equal(t, Uninitialized, d.Representation)
	require.Equal(t, "uninitialized", d.Representation.String())
	require.Equal(t, 0, d.Pending)
	require.EqualValues(t, 0, d.ZeroRegisters)
	require.Nil(t, d.RhoHistogram)
	require.EqualValues(t, 0, d.StandardError)
}

func TestDiagnostics_Dense(t *testing.T) {
	sk := New16NoSparse()
	for i := uint64(0); i < 1000; i++ {
		sk.InsertHash(i*0x9e3779b97f4a7c15 + 1)
	}

	d := sk.Diagnostics()
	require.EqualValues(t, 16, d.Precision)
	require.Equal(t, Dense, d.Representation)
	require.Equal(t, "dense", d.Representation.String())
	require.Equal(t, 0, d.Pending)
	require.Len(t, d.RhoHistogram, int(maxRho(16))+1)

	var total uint32
	var zeros uint32
	for _, r := range sk.regs {
		if r == 0 {
			zeros++
		}
	}
	for _, n := range d.RhoHistogram {
		total += n
	}
	require.EqualValues(t, sk.m, total)
	require.Equal(t, zeros, d.ZeroRegisters)
	require.Equal(t, zeros, d.RhoHistogram[0])
	require.InDelta(t, 1.04/math.Sqrt(float64(sk.m)), d.StandardError, 1e-12)
}

func TestDiagnostics_SparseMatchesDense(t *testing.T) {
	sk, err := NewSketch(14, true)
	require.NoError(t, err)
	for i := uint64(0); i < 100; i++ {
		sk.InsertHash(i*0x9e3779b97f4a7c15 + 1)
	}
	require.True(t, sk.sparse())

	d := sk.Diagnostics()
	require.EqualValues(t, 14, d.Precision)
	require.Equal(t, Sparse, d.Representation)
	require.Equal(t, "sparse", d.Representation.String())
	require.Equal(t, 100, d.Pending)
	require.Equal(t, denseHistogram(sk), d.RhoHistogram)
	require.Equal(t, d.RhoHistogram[0], d.ZeroRegisters)
	require.EqualValues(t, sk.m-100, d.ZeroRegisters)
}

func TestDiagnostics_SparseDuplicatesCountedOnce(t *testing.T) {
	sk, err := NewSketch(14, true)
	require.NoError(t, err)

	// Land the same key in both the compressed list and the tmp set: insert,
	// compact via Estimate, then insert the same hashes again.
	hashes := make([]uint64, 50)
	for i := range hashes {
		hashes[i] = uint64(i)*0x9e3779b97f4a7c15 + 1
		sk.InsertHash(hashes[i])
	}
	sk.Estimate()
	require.Equal(t, 0, sk.tmpSet.Len())
	for _, h := range hashes {
		sk.InsertHash(h)
	}
	require.Equal(t, len(hashes), sk.tmpSet.Len())

	d := sk.Diagnostics()
	require.Equal(t, len(hashes), d.Pending)
	// Every tmp set key duplicates a compressed list key, so the effective
	// set still holds only len(hashes) registers.
	require.EqualValues(t, sk.m-uint32(len(hashes)), d.ZeroRegisters)
	require.Equal(t, denseHistogram(sk), d.RhoHistogram)
}

func TestDiagnostics_IsReadOnly(t *testing.T) {
	sk, err := NewSketch(14, true)
	require.NoError(t, err)
	for i := uint64(0); i < 100; i++ {
		sk.InsertHash(i*0x9e3779b97f4a7c15 + 1)
	}
	require.True(t, sk.sparse())
	pending := sk.tmpSet.Len()
	require.Positive(t, pending)

	before, err := sk.MarshalBinary()
	require.NoError(t, err)
	listBefore := sk.sparseList

	sk.Diagnostics()

	after, err := sk.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.True(t, sk.sparse())
	require.Equal(t, pending, sk.tmpSet.Len())
	require.Same(t, listBefore, sk.sparseList)

	// A dense sketch is likewise untouched.
	dsk := NewNoSparse()
	for i := uint64(0); i < 100; i++ {
		dsk.InsertHash(i*0x9e3779b97f4a7c15 + 1)
	}
	before, err = dsk.MarshalBinary()
	require.NoError(t, err)
	dsk.Diagnostics()
	after, err = dsk.MarshalBinary()
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.False(t, dsk.sparse())
}
