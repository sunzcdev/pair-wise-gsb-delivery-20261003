package roaring

import (
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// set32 is a plain map-based set used as an independent reference model.
type set32 map[uint32]struct{}

func newSet32(bm *Bitmap) set32 {
	s := set32{}
	if bm != nil {
		i := bm.Iterator()
		for i.HasNext() {
			s[i.Next()] = struct{}{}
		}
	}
	return s
}

// refExpr evaluates an expression element-wise on a universe.
func refExpr(universe set32, e Expr) int {
	var eval func(Expr, uint32) bool
	eval = func(node Expr, x uint32) bool {
		switch n := node.(type) {
		case *leafNode:
			return n.bitmap != nil && n.bitmap.Contains(x)
		case *andNode:
			return eval(n.left, x) && eval(n.right, x)
		case *orNode:
			return eval(n.left, x) || eval(n.right, x)
		case *notNode:
			return !eval(n.child, x)
		default:
			panic("unknown expression node")
		}
	}
	count := 0
	for x := range universe {
		if eval(e, x) {
			count++
		}
	}
	return count
}

// materializeExpr builds the full result bitmap using the regular public
// operations, including the universe clipping of leaves and Not nodes.
func materializeExpr(universe *Bitmap, e Expr) *Bitmap {
	var eval func(Expr) *Bitmap
	eval = func(node Expr) *Bitmap {
		switch n := node.(type) {
		case *leafNode:
			if n.bitmap == nil {
				return NewBitmap()
			}
			return And(universe, n.bitmap)
		case *andNode:
			return And(eval(n.left), eval(n.right))
		case *orNode:
			return Or(eval(n.left), eval(n.right))
		case *notNode:
			return AndNot(universe, eval(n.child))
		default:
			panic("unknown expression node")
		}
	}
	return eval(e)
}

func randomBitmap(rng *rand.Rand, maxValue uint32, density float64) *Bitmap {
	bm := NewBitmap()
	for x := uint32(0); x < maxValue; x++ {
		if rng.Float64() < density {
			bm.Add(x)
		}
	}
	return bm
}

// sparseBitmap draws n distinct values from [0, maxValue) without scanning the
// whole range, so it stays cheap even when maxValue approaches 2^32.
func sparseBitmap(rng *rand.Rand, maxValue uint32, n int) *Bitmap {
	bm := NewBitmap()
	for bm.GetCardinality() < uint64(n) {
		bm.Add(rng.Uint32N(maxValue))
	}
	return bm
}

func TestCountExprBasic(t *testing.T) {
	universe := BitmapOf(0, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	a := BitmapOf(1, 3, 5, 7, 9, 100) // odd plus something outside
	b := BitmapOf(0, 3, 6, 9)         // multiples of three
	c := NewBitmap()                  // empty leaf
	var nilBitmap *Bitmap             // nil leaf

	cases := []struct {
		name string
		expr Expr
	}{
		{"leaf", Leaf(a)},
		{"leaf empty", Leaf(c)},
		{"leaf nil", Leaf(nilBitmap)},
		{"and", AndExpr(Leaf(a), Leaf(b))},
		{"or", OrExpr(Leaf(a), Leaf(b))},
		{"not leaf", NotExpr(Leaf(a))},
		{"not empty", NotExpr(Leaf(c))},
		{"not not", NotExpr(NotExpr(Leaf(a)))},
		{"not and de morgan", NotExpr(AndExpr(Leaf(a), Leaf(b)))},
		{"and not", AndExpr(Leaf(a), NotExpr(Leaf(b)))},
		{"nested mix", OrExpr(
			AndExpr(Leaf(a), NotExpr(Leaf(b))),
			AndExpr(Leaf(b), NotExpr(Leaf(a))),
		)},
		{"and empty", AndExpr(Leaf(a), Leaf(c))},
		{"or empty", OrExpr(Leaf(a), Leaf(c))},
		{"or nil leaf", OrExpr(Leaf(a), Leaf(nilBitmap))},
	}

	uSet := newSet32(universe)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CountExpr(universe, tc.expr)
			require.NoError(t, err)

			materialized := materializeExpr(universe, tc.expr)
			assert.Equal(t, materialized.GetCardinality(), got, "matches materialized expression")
			assert.Equal(t, uint64(refExpr(uSet, tc.expr)), got, "matches element-wise reference")
		})
	}
}

func TestCountExprEmptyUniverse(t *testing.T) {
	// Not over an empty universe must be 0, never an infinite set.
	e := NotExpr(Leaf(NewBitmap()))
	got, err := CountExpr(NewBitmap(), e)
	require.NoError(t, err)
	assert.Equal(t, uint64(0), got)

	got, err = CountExpr(nil, NotExpr(Leaf(BitmapOf(1, 2, 3))))
	require.NoError(t, err)
	assert.Equal(t, uint64(0), got)
}

func TestCountExprRepeatedAndSharedLeaves(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 7))
	universe := randomBitmap(rng, 300000, 0.4)
	a := randomBitmap(rng, 300000, 0.3)
	b := randomBitmap(rng, 300000, 0.25)

	la := Leaf(a)
	lb := Leaf(b)

	// Same leaf appears multiple times.
	selfAnd := AndExpr(la, la)
	selfOr := OrExpr(lb, lb)

	// A genuine shared subexpression referenced by both branches.
	shared := AndExpr(la, NotExpr(lb))
	e := OrExpr(
		AndExpr(shared, lb),
		OrExpr(shared, selfAnd),
	)

	got, err := CountExpr(universe, e)
	require.NoError(t, err)
	assert.Equal(t, materializeExpr(universe, e).GetCardinality(), got)

	gotSelf, err := CountExpr(universe, OrExpr(selfAnd, selfOr))
	require.NoError(t, err)
	assert.Equal(t, materializeExpr(universe, OrExpr(selfAnd, selfOr)).GetCardinality(), gotSelf)
}

func TestCountExprRandomized(t *testing.T) {
	rng := rand.New(rand.NewPCG(1234, 99))

	for iter := 0; iter < 60; iter++ {
		// Spread values over many 16-bit keys to exercise all containers.
		maxValue := uint32(1 + rng.IntN(120000))
		universe := randomBitmap(rng, maxValue, 0.1+rng.Float64()*0.8)
		leaves := make([]Expr, 5)
		for i := range leaves {
			leaf := randomBitmap(rng, maxValue+20000, rng.Float64())
			leaves[i] = Leaf(leaf)
		}

		var gen func(depth int) Expr
		gen = func(depth int) Expr {
			if depth == 0 || rng.IntN(4) == 0 {
				return leaves[rng.IntN(len(leaves))]
			}
			switch rng.IntN(3) {
			case 0:
				return AndExpr(gen(depth-1), gen(depth-1))
			case 1:
				return OrExpr(gen(depth-1), gen(depth-1))
			default:
				return NotExpr(gen(depth - 1))
			}
		}
		e := gen(6)

		got, err := CountExpr(universe, e)
		require.NoError(t, err)

		want := uint64(refExpr(newSet32(universe), e))
		assert.Equal(t, want, got, "iteration %d", iter)
		if t.Failed() {
			return
		}
	}
}

func TestCountExprShortCircuit(t *testing.T) {
	// Single 16-bit key keeps the debug counters easy to reason about.
	universe := NewBitmap()
	for x := uint32(0); x < 1<<16; x++ {
		universe.Add(x)
	}
	empty := NewBitmap()
	a := BitmapOf(1, 2, 3)
	b := BitmapOf(3, 4, 5)

	// AND with an empty left operand must never read the right leaf.
	andExpr := AndExpr(Leaf(empty), Leaf(b))
	got, stats, err := countExprDebug(universe, andExpr)
	require.NoError(t, err)
	assert.Equal(t, uint64(0), got)
	assert.Equal(t, 1, stats.leafLookups, "right operand of AND not scanned")

	// OR with a full left operand must never read the right leaf.
	orExpr := OrExpr(Leaf(universe), Leaf(a))
	got, stats, err = countExprDebug(universe, orExpr)
	require.NoError(t, err)
	assert.Equal(t, uint64(1<<16), got)
	assert.Equal(t, 1, stats.leafLookups, "right operand of OR not scanned")
}

func TestCountExprScratchNotFullUniverse(t *testing.T) {
	// A dense bitmap container is 8192 bytes. Per-key scratch is bounded by the
	// number of (small, fixed-size) expression nodes and not by the width of
	// the universe, so it must stay a small constant multiple of one container.
	const perContainerBytes = 8192

	denseCase := func(maxValue uint32, density float64, rng *rand.Rand) (*Bitmap, Expr) {
		universe := randomBitmap(rng, maxValue, density)
		leaves := make([]Expr, 4)
		for i := range leaves {
			leaves[i] = Leaf(randomBitmap(rng, maxValue, 0.2+rng.Float64()*0.6))
		}
		e := OrExpr(
			AndExpr(leaves[0], NotExpr(leaves[1])),
			AndExpr(leaves[2], NotExpr(leaves[3])),
		)
		return universe, e
	}

	// Highly sparse universe spanning the full 32-bit range. Leaves are also
	// sparse and may sit outside the universe.
	sparseRng := rand.New(rand.NewPCG(1, 2))
	maxValue := uint32(1<<32 - 1)
	sparseUniverse := sparseBitmap(sparseRng, maxValue, 5000)
	sparseLeaves := make([]Expr, 4)
	for i := range sparseLeaves {
		sparseLeaves[i] = Leaf(sparseBitmap(sparseRng, maxValue, 5000))
	}
	sparseExpr := OrExpr(
		AndExpr(sparseLeaves[0], NotExpr(sparseLeaves[1])),
		AndExpr(sparseLeaves[2], NotExpr(sparseLeaves[3])),
	)
	_, sparseStats, err := countExprDebug(sparseUniverse, sparseExpr)
	require.NoError(t, err)
	sparseCount, err := CountExpr(sparseUniverse, sparseExpr)
	require.NoError(t, err)
	assert.Equal(t, materializeExpr(sparseUniverse, sparseExpr).GetCardinality(), sparseCount)
	assert.Equal(t, uint64(refExpr(newSet32(sparseUniverse), sparseExpr)), sparseCount)
	assert.Less(t, sparseStats.peakScratchBytes, 16*perContainerBytes,
		"sparse scratch independent of universe span")

	// Nearly full universe across many keys.
	denseRng := rand.New(rand.NewPCG(3, 4))
	denseUniverse, denseExpr := denseCase(5_000_000, 0.999, denseRng)
	_, denseStats, err := countExprDebug(denseUniverse, denseExpr)
	require.NoError(t, err)
	denseCount, err := CountExpr(denseUniverse, denseExpr)
	require.NoError(t, err)
	assert.Equal(t, materializeExpr(denseUniverse, denseExpr).GetCardinality(), denseCount)
	assert.Equal(t, uint64(refExpr(newSet32(denseUniverse), denseExpr)), denseCount)
	assert.Less(t, denseStats.peakScratchBytes, 16*perContainerBytes,
		"dense scratch independent of universe size")
}

func TestCountExprValidation(t *testing.T) {
	a := BitmapOf(1)

	_, err := CountExpr(a, nil)
	assert.ErrorIs(t, err, ErrNilExpression)

	_, err = CountExpr(a, AndExpr(Leaf(a), nil))
	assert.ErrorIs(t, err, ErrNilExpression)

	_, err = CountExpr(a, OrExpr(nil, Leaf(a)))
	assert.ErrorIs(t, err, ErrNilExpression)

	_, err = CountExpr(a, NotExpr(nil))
	assert.ErrorIs(t, err, ErrNilExpression)

	// Build a cyclic graph by sewing nodes together after construction.
	cyclic := &andNode{left: Leaf(a)}
	cyclic.right = cyclic
	_, err = CountExpr(a, cyclic)
	assert.ErrorIs(t, err, ErrExpressionCycle)

	// A deeper cycle.
	x := &notNode{}
	y := &orNode{left: Leaf(a), right: x}
	x.child = y
	_, err = CountExpr(a, x)
	assert.ErrorIs(t, err, ErrExpressionCycle)

	// Depth limit rejected before reading data.
	var deep Expr = Leaf(a)
	for i := 0; i <= MaxExpressionDepth; i++ {
		deep = NotExpr(deep)
	}
	_, err = CountExpr(a, deep)
	assert.ErrorIs(t, err, ErrExpressionTooDeep)
}

func TestCountExprDoesNotMutateInputs(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 8))
	universe := randomBitmap(rng, 200000, 0.5)
	leaves := []*Bitmap{
		randomBitmap(rng, 200000, 0.3),
		randomBitmap(rng, 200000, 0.4),
	}
	snapshots := []string{universe.String(), leaves[0].String(), leaves[1].String()}

	e := OrExpr(
		AndExpr(Leaf(leaves[0]), NotExpr(Leaf(leaves[1]))),
		NotExpr(AndExpr(Leaf(leaves[0]), Leaf(leaves[1]))),
	)
	_, err := CountExpr(universe, e)
	require.NoError(t, err)

	assert.Equal(t, snapshots[0], universe.String())
	assert.Equal(t, snapshots[1], leaves[0].String())
	assert.Equal(t, snapshots[2], leaves[1].String())
}
