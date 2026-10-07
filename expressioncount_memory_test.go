package roaring

import (
	"math/rand"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A fully allocated 16-bit block (bitmapContainer) is 8 KiB. If Count ever
// materialized per-level intermediates over the whole universe, a single
// operation on numBlocks blocks would allocate on the order of
// numBlocks*8KiB. The budgets below are many orders of magnitude below that.
const blockBytes = 8 * 1024

// TestCountSparseMemoryNoFullUniverseAllocation runs a deeply nested expression
// over a very large but highly sparse universe. Intersections become empty in
// the first block container and short-circuit, so total heap churn must stay a
// small constant independent of the universe size; a full-universe allocation
// would allocate millions of bytes.
func TestCountSparseMemoryNoFullUniverseAllocation(t *testing.T) {
	const numBlocks = 2000
	rng := rand.New(rand.NewSource(11))

	universe := NewBitmap()
	set := make([]uint32, numBlocks)
	for i := range set {
		set[i] = uint32(i)*65536 + uint32(rng.Intn(10))
		universe.Add(set[i])
	}

	leaves := make([]Expr, 6)
	for i := range leaves {
		bm := NewBitmap()
		for _, base := range set {
			if rng.Intn(2) == 0 {
				bm.Add(base + uint32(rng.Intn(5)))
			}
		}
		leaves[i] = Leaf(bm)
	}

	// Build a deep DAG mixing NOT/AND/OR; shared sub-expression in the middle.
	shared := AndOf(leaves[0], Not(leaves[1]), OrOf(leaves[2], leaves[3]))
	var expr Expr = shared
	for i := 0; i < 60; i++ {
		if i%2 == 0 {
			expr = AndOf(expr, leaves[i%len(leaves)], Not(shared))
		} else {
			expr = OrOf(expr, AndOf(leaves[(i+1)%len(leaves)], Not(leaves[i%len(leaves)])))
		}
	}

	expected := materialize(expr, universe)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, err := Count(expr, universe)
	require.NoError(t, err)
	runtime.ReadMemStats(&after)

	assert.Equal(t, expected.GetCardinality(), got)
	// A materialized per-level approach would allocate a whole block (8 KiB)
	// per populated block at each level; with 60 levels that would be hundreds
	// of MiB. The streaming counter stays well under one block of scratch per
	// populated block in total, regardless of nesting depth.
	assert.Less(t, after.TotalAlloc-before.TotalAlloc,
		uint64(numBlocks)*uint64(blockBytes)/4,
		"sparse count must not allocate on the order of the universe size")
}

// TestCountNearlyFullMemoryNoFullUniverseAllocation exercises the hard case
// where nearly every block is dense: each OR level fills blocks and
// short-circuits at the first operand, and NOT flips without materializing a
// universe-sized result.
func TestCountNearlyFullMemoryNoFullUniverseAllocation(t *testing.T) {
	const numBlocks = 1000
	span := uint64(numBlocks) * 65536
	universe := Flip(NewBitmap(), 0, span)
	// Punch sparse holes so it is "nearly full" rather than exactly full.
	for key := 0; key < numBlocks; key++ {
		universe.Remove(uint32(key)*65536 + uint32(key%64))
	}

	// a is a fully dense leaf (every block full) so OR short-circuits at the
	// first operand; b misses a disjoint sparse value per block.
	a := Leaf(Flip(NewBitmap(), 0, span))
	b := Flip(NewBitmap(), 0, span)
	for key := 0; key < numBlocks; key++ {
		b.Remove(uint32(key)*65536 + uint32((key+31)%64))
	}
	bLeaf := Leaf(b)

	var expr Expr = OrOf(a, bLeaf)
	for i := 0; i < 40; i++ {
		if i%2 == 0 {
			expr = Not(AndOf(expr, Not(a), bLeaf))
		} else {
			expr = OrOf(expr, bLeaf, a)
		}
	}

	expected := materialize(expr, universe)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, err := Count(expr, universe)
	require.NoError(t, err)
	runtime.ReadMemStats(&after)

	assert.Equal(t, expected.GetCardinality(), got)
	assert.Less(t, after.TotalAlloc-before.TotalAlloc,
		uint64(numBlocks/2*blockBytes),
		"nearly-full count must not allocate a full universe of intermediates")
}

// TestCountNoResidentIntermediatesAfterCount forces a garbage collection after
// Count and verifies live heap barely grows: no per-level intermediate bitmap is
// retained by the time the call returns.
func TestCountNoResidentIntermediatesAfterCount(t *testing.T) {
	const numBlocks = 500
	rng := rand.New(rand.NewSource(99))
	universe := randomBitmap(rng, numBlocks*65536, 0.95)

	leafExprs := make([]Expr, 5)
	for i := range leafExprs {
		leafExprs[i] = Leaf(randomBitmap(rng, numBlocks*65536, []float64{0.9, 0.05, 0.99, 0.5, 0.001}[i]))
	}

	shared := OrOf(leafExprs[0], AndOf(leafExprs[1], Not(leafExprs[2])))
	expr := AndOf(Not(shared), OrOf(leafExprs[3], Not(leafExprs[4])), shared)

	_, err := Count(expr, universe)
	require.NoError(t, err)

	var baseline, afterCount runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&baseline)
	for i := 0; i < 10; i++ {
		_, err := Count(expr, universe)
		require.NoError(t, err)
	}
	runtime.GC()
	runtime.ReadMemStats(&afterCount)

	growth := int64(afterCount.HeapAlloc) - int64(baseline.HeapAlloc)
	// A handful of KiB of noise is acceptable; a full-universe residue would be
	// numBlocks*blockBytes (~4 MiB).
	assert.Less(t, growth, int64(256*1024), "no intermediates should remain live")
}

// TestCountShortCircuitDoesNotTouchIrrelevantLeaves verifies conditional
// short-circuiting against the materialized result. With an empty first AND
// operand the intersection is zero regardless of the other operands, and with
// a full first OR operand the union is full regardless of the rest. The
// cardinality must match while the leaves stay unmodified.
func TestCountShortCircuitDoesNotTouchIrrelevantLeaves(t *testing.T) {
	const numBlocks = 100
	rng := rand.New(rand.NewSource(123))
	universe := randomBitmap(rng, numBlocks*65536, 0.7)

	emptyLeaf := Leaf(NewBitmap())
	fullLeaf := Leaf(Flip(NewBitmap(), 0, uint64(numBlocks)*65536))
	heavyA := Leaf(randomBitmap(rng, numBlocks*65536, 0.6))
	heavyB := Leaf(randomBitmap(rng, numBlocks*65536, 0.4))
	heavyC := Leaf(randomBitmap(rng, numBlocks*65536, 0.9))

	andExpr := AndOf(emptyLeaf, heavyA, Not(heavyB), OrOf(heavyC, heavyA))
	orExpr := OrOf(fullLeaf, heavyA, AndOf(Not(heavyB), heavyC))

	checkAgainstMaterialized(t, andExpr, universe)
	checkAgainstMaterialized(t, orExpr, universe)

	// AND with an empty operand must allocate essentially nothing: once the
	// first block container is known empty no later operand is folded.
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, err := Count(andExpr, universe)
	require.NoError(t, err)
	runtime.ReadMemStats(&after)
	assert.Equal(t, uint64(0), got)
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(8*blockBytes))

	// OR with a full operand short-circuits per block; compare allocation to a
	// full-universe materialization which would be numBlocks*blockBytes.
	runtime.ReadMemStats(&before)
	got, err = Count(orExpr, universe)
	require.NoError(t, err)
	runtime.ReadMemStats(&after)
	assert.Equal(t, universe.GetCardinality(), got)
	assert.Less(t, after.TotalAlloc-before.TotalAlloc,
		uint64(numBlocks*blockBytes)/4)
}
