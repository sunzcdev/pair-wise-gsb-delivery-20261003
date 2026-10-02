package hyperloglog

import (
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"
)

func denseRegs(t *testing.T, sk *Sketch) []uint8 {
	t.Helper()
	if sk.sparse() {
		sk.toNormal()
	}
	return sk.regs
}

// The same InsertHash batch must produce register-identical sketches whether
// it is inserted at the low precision directly or inserted at a higher
// precision and then folded down.
func TestReducePrecision_MatchesDirectConstruction(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	hashes := make([]uint64, 2000)
	for i := range hashes {
		hashes[i] = rng.Uint64()
	}

	for _, sparse := range []bool{true, false} {
		for _, ph := range []uint8{18, 16, 14, 10} {
			for _, pl := range []uint8{4, 8, 12} {
				if pl >= ph {
					continue
				}
				direct, err := NewSketch(pl, sparse)
				require.NoError(t, err)
				high, err := NewSketch(ph, sparse)
				require.NoError(t, err)
				for _, x := range hashes {
					direct.InsertHash(x)
					high.InsertHash(x)
				}

				wasSparse := high.sparse()
				reduced, err := high.ReducePrecision(pl)
				require.NoError(t, err)
				require.EqualValues(t, pl, reduced.p)
				require.Equal(t, wasSparse, reduced.sparse(),
					"sparse=%v p%d->p%d: representation not preserved", sparse, ph, pl)

				require.Equal(t, denseRegs(t, direct), denseRegs(t, reduced),
					"sparse=%v p%d->p%d: registers differ", sparse, ph, pl)
			}
		}
	}
}

// The index bits the fold drops were the top bits of w, so they lead the new
// rho run: a set bit among them caps rho inside the dropped bits, and only
// all-zero dropped bits let the old rho follow them.
func TestReducePrecision_DiscardedBitsJoinRho(t *testing.T) {
	hashes := []uint64{
		// Index 0xA1 at p=8: dropped bits 0001 cap rho at 4 at p=4.
		0xA180000000000000,
		// Index 0xA0 at p=8: dropped bits 0000, old rho 8 follows them
		// for a folded rho of 4+8 = 12.
		0xA001000000000000,
		// Index 0xB5 at p=8: old rho 56, but dropped bits 0101 cap the
		// folded rho at 2. Taking a max of old registers would keep 56.
		0xB500000000000001,
	}

	high, err := NewSketch(8, false)
	require.NoError(t, err)
	for _, x := range hashes {
		high.InsertHash(x)
	}
	require.EqualValues(t, 1, high.regs[0xA1])
	require.EqualValues(t, 8, high.regs[0xA0])
	require.EqualValues(t, 56, high.regs[0xB5])

	reduced, err := high.ReducePrecision(4)
	require.NoError(t, err)
	require.EqualValues(t, 12, reduced.regs[0xA])
	require.EqualValues(t, 2, reduced.regs[0xB])

	low, err := NewSketch(4, false)
	require.NoError(t, err)
	for _, x := range hashes {
		low.InsertHash(x)
	}
	require.Equal(t, low.regs, reduced.regs)

	// Folding must not touch the original sketch.
	require.EqualValues(t, 8, high.p)
	require.EqualValues(t, 1, high.regs[0xA1])
	require.EqualValues(t, 8, high.regs[0xA0])
	require.EqualValues(t, 56, high.regs[0xB5])
}

func TestReducePrecision_Errors(t *testing.T) {
	sk := New14()

	_, err := sk.ReducePrecision(16)
	require.ErrorIs(t, err, ErrorPrecisionIncrease, "raising precision must fail")

	_, err = sk.ReducePrecision(3)
	require.ErrorIs(t, err, ErrorInvalidPrecision)

	_, err = sk.ReducePrecision(19)
	require.ErrorIs(t, err, ErrorInvalidPrecision)

	// Reducing to the current precision returns an independent copy.
	sk.InsertHash(0x00010fffffffffff)
	clone, err := sk.ReducePrecision(14)
	require.NoError(t, err)
	require.Equal(t, sk.Estimate(), clone.Estimate())
	clone.InsertHash(0x00020fffffffffff)
	require.EqualValues(t, 1, sk.Estimate(), "clone must not share state with the original")
}

func TestReducePrecision_ZeroValue(t *testing.T) {
	var sk Sketch

	// A zero value behaves like New(): an empty precision 14 sketch.
	reduced, err := sk.ReducePrecision(10)
	require.NoError(t, err)
	require.EqualValues(t, 10, reduced.p)
	require.True(t, reduced.sparse())
	require.Zero(t, reduced.Estimate())

	_, err = sk.ReducePrecision(16)
	require.ErrorIs(t, err, ErrorPrecisionIncrease)

	require.Zero(t, sk.Estimate(), "reducing must not initialize the zero value")
}

func TestReducePrecision_SparseStaysSparse(t *testing.T) {
	high := New16()
	direct, err := NewSketch(8, true)
	require.NoError(t, err)
	for i := uint64(0); i < 100; i++ {
		x := uint64(0x9e3779b97f4a7c15) * (i + 1)
		high.InsertHash(x)
		direct.InsertHash(x)
	}

	reduced, err := high.ReducePrecision(8)
	require.NoError(t, err)
	require.True(t, reduced.sparse(), "sparse sketch should fold to a sparse sketch")
	require.True(t, high.sparse(), "original sketch should be untouched")
	require.Equal(t, denseRegs(t, direct), denseRegs(t, reduced))
}

func TestMergeDown(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	hashes := make([]uint64, 5000)
	for i := range hashes {
		hashes[i] = rng.Uint64()
	}

	insertAll := func(sk *Sketch) {
		for _, x := range hashes {
			sk.InsertHash(x)
		}
	}

	// Expected result: everything inserted at the lower precision directly.
	want, err := NewSketch(10, false)
	require.NoError(t, err)
	insertAll(want)

	// Merging a higher precision other folds other down.
	sk, err := NewSketch(10, true)
	require.NoError(t, err)
	high, err := NewSketch(16, false)
	require.NoError(t, err)
	insertAll(high)
	require.NoError(t, sk.MergeDown(high))
	require.EqualValues(t, 10, sk.p)
	require.Equal(t, want.regs, denseRegs(t, sk))
	require.EqualValues(t, 16, high.p, "MergeDown must not modify its argument")

	// Merging a lower precision other folds sk down to it.
	sk2, err := NewSketch(16, true)
	require.NoError(t, err)
	insertAll(sk2)
	low, err := NewSketch(10, false)
	require.NoError(t, err)
	insertAll(low)
	require.NoError(t, sk2.MergeDown(low))
	require.EqualValues(t, 10, sk2.p)
	require.Equal(t, want.regs, denseRegs(t, sk2))

	// Equal precisions behave like Merge.
	sk3, err := NewSketch(10, false)
	require.NoError(t, err)
	require.NoError(t, sk3.MergeDown(low))
	require.Equal(t, want.regs, sk3.regs)

	// Nil and zero-value others are empty; a zero-value receiver adopts
	// the other sketch as with Merge.
	require.NoError(t, sk3.MergeDown(nil))
	require.NoError(t, sk3.MergeDown(&Sketch{}))
	var dst Sketch
	require.NoError(t, dst.MergeDown(high))
	require.EqualValues(t, 16, dst.p)

	// Merge itself still rejects a precision mismatch.
	require.ErrorIs(t, high.Merge(low), ErrorPrecisionMismatch)
}
