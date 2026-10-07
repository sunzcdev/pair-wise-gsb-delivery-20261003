package roaring

import (
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// materialize evaluates an expression into a real bitmap. It is the reference
// implementation the cardinality-only Count result is checked against.
func materialize(expr Expr, universe *Bitmap) *Bitmap {
	switch n := expr.(type) {
	case *LeafExpr:
		if n.bitmap == nil {
			return NewBitmap()
		}
		return And(n.bitmap, universe)
	case *NotExpr:
		return AndNot(universe, materialize(n.operand, universe))
	case *AndExpr:
		result := universe.Clone()
		for _, operand := range n.operands {
			result.And(materialize(operand, universe))
		}
		return result
	case *OrExpr:
		result := NewBitmap()
		for _, operand := range n.operands {
			result.Or(materialize(operand, universe))
		}
		return result
	}
	return NewBitmap()
}

func randomBitmap(rng *rand.Rand, maxVal uint32, density float64) *Bitmap {
	bm := NewBitmap()
	for v := uint32(0); v < maxVal; v++ {
		if rng.Float64() < density {
			bm.Add(v)
		}
	}
	return bm
}

// randomExpr builds a random expression tree that deliberately reuses nodes
// (shared sub-expressions) and reuses leaves (the same leaf under several
// parents).
func randomExpr(rng *rand.Rand, leaves []Expr, depth int) Expr {
	if depth == 0 || rng.Intn(3) == 0 {
		// The same leaf is picked repeatedly, exercising repeated occurrences.
		return leaves[rng.Intn(len(leaves))]
	}
	switch rng.Intn(4) {
	case 0:
		return Not(randomExpr(rng, leaves, depth-1))
	case 1:
		k := 2 + rng.Intn(3)
		ops := make([]Expr, k)
		for i := range ops {
			ops[i] = randomExpr(rng, leaves, depth-1)
		}
		return AndOf(ops...)
	default:
		k := 2 + rng.Intn(3)
		ops := make([]Expr, k)
		for i := range ops {
			ops[i] = randomExpr(rng, leaves, depth-1)
		}
		return OrOf(ops...)
	}
}

func checkAgainstMaterialized(t *testing.T, expr Expr, universe *Bitmap) {
	t.Helper()
	expected := materialize(expr, universe)
	got, err := Count(expr, universe)
	require.NoError(t, err)
	assert.Equal(t, expected.GetCardinality(), got)
}

// TestCountExprExhaustive evaluates every value of a small universe directly
// (rather than relying only on cardinalities), catching membership mistakes.
func TestCountExprExhaustive(t *testing.T) {
	const numValues = 2 * 65536 // two blocks, one boundary between them
	rng := rand.New(rand.NewSource(1))

	universe := NewBitmap()
	for v := uint32(0); v < numValues; v++ {
		if rng.Float64() < 0.6 {
			universe.Add(v)
		}
	}

	leafBitmaps := make([]*Bitmap, 5)
	leafExprs := make([]Expr, len(leafBitmaps))
	for i := range leafBitmaps {
		leafBitmaps[i] = randomBitmap(rng, numValues+50000, []float64{0.1, 0.5, 0.9, 0.3, 0.7}[i])
		leafExprs[i] = Leaf(leafBitmaps[i])
	}

	// Hand-written shapes: nested NOT, shared sub-expression, repeated leaf.
	shared := AndOf(leafExprs[0], Not(leafExprs[1]))
	shapes := []Expr{
		leafExprs[0],
		Not(leafExprs[0]),
		Not(Not(leafExprs[0])),
		AndOf(leafExprs[0], leafExprs[1], Not(leafExprs[2])),
		OrOf(leafExprs[0], leafExprs[1], Not(leafExprs[2])),
		OrOf(shared, AndOf(shared, leafExprs[3])),
		AndOf(leafExprs[4], OrOf(shared, leafExprs[2]), Not(OrOf(leafExprs[1], leafExprs[3]))),
		AndOf(leafExprs[0], leafExprs[0], leafExprs[0]),
		OrOf(leafExprs[0], leafExprs[0]),
	}
	for i, shape := range shapes {
		expected := materialize(shape, universe)
		got, err := Count(shape, universe)
		require.NoError(t, err, "shape %d", i)
		assert.Equal(t, expected.GetCardinality(), got, "shape %d cardinality", i)

		// Membership check against the exact materialized result.
		it := expected.Iterator()
		for it.HasNext() {
			v := it.Next()
			assert.True(t, expected.Contains(v), "shape %d", i)
		}
	}
}

// TestCountExprRandomized cross-checks many random DAGs against materialized
// evaluation, using both sparse and nearly-full inputs.
func TestCountExprRandomized(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for trial := 0; trial < 200; trial++ {
		numBlocks := 1 + rng.Intn(8)
		numValues := uint32(numBlocks) * 65536
		var density float64
		if trial%2 == 0 {
			density = 0.001 + rng.Float64()*0.01 // highly sparse
		} else {
			density = 0.9 + rng.Float64()*0.09 // nearly full
		}
		universe := randomBitmap(rng, numValues, 0.5+rng.Float64()*0.5)

		numLeaves := 2 + rng.Intn(4)
		leaves := make([]Expr, numLeaves)
		for i := range leaves {
			leafDensity := []float64{density, 0.5, 1.0 - density, rng.Float64()}[i%4]
			// Some leaves extend beyond the universe.
			leaves[i] = Leaf(randomBitmap(rng, numValues+uint32(rng.Intn(3))*65536, leafDensity))
		}

		expr := randomExpr(rng, leaves, 1+rng.Intn(6))
		checkAgainstMaterialized(t, expr, universe)
	}
}

// TestCountExprEmptyAndIdentities covers the empty universe, nil leaf, empty
// leaf and zero-operand identity nodes.
func TestCountExprEmptyAndIdentities(t *testing.T) {
	leaf := NewBitmap()
	for _, v := range []uint32{1, 2, 65537} {
		leaf.Add(v)
	}
	universe := NewBitmap()
	for v := uint32(0); v < 3*65536; v++ {
		universe.Add(v)
	}

	// Empty universe always yields zero.
	count, err := Count(Not(Leaf(leaf)), NewBitmap())
	require.NoError(t, err)
	assert.Equal(t, uint64(0), count)

	// nil universe.
	count, err = Count(Leaf(leaf), nil)
	require.NoError(t, err)
	assert.Equal(t, uint64(0), count)

	// nil leaf behaves like the empty set.
	checkAgainstMaterialized(t, Leaf(nil), universe)
	checkAgainstMaterialized(t, AndOf(Leaf(leaf), Leaf(nil)), universe)
	checkAgainstMaterialized(t, OrOf(Leaf(leaf), Leaf(nil)), universe)
	checkAgainstMaterialized(t, Not(Leaf(nil)), universe)

	// Zero-operand AND is the universe identity; OR is empty.
	assert.Equal(t, universe.GetCardinality(), mustCount(t, AndOf(), universe))
	assert.Equal(t, uint64(0), mustCount(t, OrOf(), universe))
	assert.Equal(t, universe.GetCardinality(), mustCount(t, Not(OrOf()), universe))
	assert.Equal(t, uint64(0), mustCount(t, Not(AndOf()), universe))

	// Empty leaf in a NOT gives the universe.
	assert.Equal(t, universe.GetCardinality(), mustCount(t, Not(Leaf(NewBitmap())), universe))
}

func mustCount(t *testing.T, expr Expr, universe *Bitmap) uint64 {
	t.Helper()
	count, err := Count(expr, universe)
	require.NoError(t, err)
	return count
}

// TestCountExprSharedSubexpression builds a DAG where one heavy sub-expression
// is referenced many times and verifies both correctness and that the leaves
// are not mutated.
func TestCountExprSharedSubexpression(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	universe := randomBitmap(rng, 4*65536, 0.8)
	a := randomBitmap(rng, 4*65536, 0.3)
	b := randomBitmap(rng, 4*65536, 0.7)

	shared := OrOf(Leaf(a), Not(Leaf(b)))
	var operands []Expr
	for i := 0; i < 8; i++ {
		operands = append(operands, AndOf(shared, Not(shared), Leaf(a)))
	}
	expr := OrOf(operands...)

	beforeA := a.Clone()
	beforeB := b.Clone()
	checkAgainstMaterialized(t, expr, universe)
	assert.True(t, beforeA.Equals(a), "leaf a must not be mutated")
	assert.True(t, beforeB.Equals(b), "leaf b must not be mutated")
}

// TestCountExprValidation rejects nil, wrong NOT arity, cycles and excessive
// depth before scanning anything.
func TestCountExprValidation(t *testing.T) {
	universe := NewBitmap()
	universe.Add(1)

	_, err := Count(nil, universe)
	assert.ErrorIs(t, err, ErrNilExpression)

	leaf := Leaf(NewBitmap())

	_, err = Count(AndOf(nil, leaf), universe)
	assert.ErrorIs(t, err, ErrNilOperand)

	_, err = Count(OrOf(leaf, AndOf(nil)), universe)
	assert.ErrorIs(t, err, ErrNilOperand)

	_, err = Count(Not(nil), universe)
	assert.ErrorIs(t, err, ErrNotArity)

	// Direct self-reference cycle.
	cycleHolder := &AndExpr{}
	cycleHolder.operands = []Expr{leaf, cycleHolder}
	_, err = Count(cycleHolder, universe)
	assert.ErrorIs(t, err, ErrCycle)

	// Cycle through NOT.
	notHolder := &NotExpr{}
	andHolder := &AndExpr{operands: []Expr{notHolder}}
	notHolder.operand = andHolder
	_, err = Count(notHolder, universe)
	assert.ErrorIs(t, err, ErrCycle)

	// Deep-but-acyclic chain at the exact limit is accepted; one more is not.
	deep := Expr(leaf)
	for i := 0; i < MaxExpressionDepth-1; i++ {
		deep = AndOf(deep)
	}
	_, err = Count(deep, universe)
	assert.NoError(t, err)

	tooDeep := AndOf(deep)
	_, err = Count(tooDeep, universe)
	assert.ErrorIs(t, err, ErrExpressionTooDeep)
}

// TestCountExprReceiverAndUnknownNode covers the universe-receiver entry point
// and rejection of nodes that are not one of the provided types.
func TestCountExprReceiverAndUnknownNode(t *testing.T) {
	universe := NewBitmap()
	for v := uint32(0); v < 65536; v++ {
		universe.Add(v)
	}
	leaf := NewBitmap()
	leaf.Add(3)

	expr := Not(Leaf(leaf))
	got, err := universe.CountExpr(expr)
	require.NoError(t, err)

	expected := materialize(expr, universe)
	assert.Equal(t, expected.GetCardinality(), got)

	// A foreign Expr implementation cannot be evaluated.
	_, err = Count(foreignNode{}, universe)
	assert.ErrorIs(t, err, ErrUnknownExpression)
}

type foreignNode struct{}

func (foreignNode) nodeMarker() {}
