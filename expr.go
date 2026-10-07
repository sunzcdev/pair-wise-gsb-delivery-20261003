package roaring

import "errors"

// An Expr is a node in a Boolean filter expression over bitmaps. The result
// of an expression is always interpreted relative to an explicit universe, so
// a Not node denotes the relative complement (universe \ child), never an
// unbounded set of integers.
//
// Expressions are consumed by CountExpr, which computes the cardinality of
// the matched set without materializing the intermediate bitmaps of the
// internal nodes. Construct nodes with Leaf, And, Or and Not.
//
// The same node may appear more than once in an expression (a shared
// subexpression), and the same bitmap may be referenced by multiple leaves.
type Expr interface {
	// kind seals the interface so only the node types defined in this package
	// can satisfy Expr.
	kind() exprKind
}

type exprKind uint8

const (
	kindLeaf exprKind = iota
	kindAnd
	kindOr
	kindNot
)

type leafNode struct {
	bitmap *Bitmap
}

func (n *leafNode) kind() exprKind { return kindLeaf }

type andNode struct {
	left  Expr
	right Expr
}

func (n *andNode) kind() exprKind { return kindAnd }

type orNode struct {
	left  Expr
	right Expr
}

func (n *orNode) kind() exprKind { return kindOr }

type notNode struct {
	child Expr
}

func (n *notNode) kind() exprKind { return kindNot }

// Leaf wraps an input bitmap as an expression node. The caller retains
// ownership of the bitmap: CountExpr does not modify it. A nil bitmap is
// treated as the empty set.
func Leaf(bm *Bitmap) Expr {
	return &leafNode{bitmap: bm}
}

// And returns an expression matching the intersection of left and right.
func AndExpr(left, right Expr) Expr {
	return &andNode{left: left, right: right}
}

// Or returns an expression matching the union of left and right.
func OrExpr(left, right Expr) Expr {
	return &orNode{left: left, right: right}
}

// Not returns an expression matching the relative complement of child within
// the universe supplied to CountExpr: the result is universe \ child.
func NotExpr(child Expr) Expr {
	return &notNode{child: child}
}

var (
	// ErrNilExpression is returned when an expression or one of its operands
	// is nil.
	ErrNilExpression = errors.New("roaring: expression contains a nil node")
	// ErrExpressionCycle is returned when an expression refers back to one of
	// its ancestors.
	ErrExpressionCycle = errors.New("roaring: expression contains a cycle")
	// ErrExpressionTooDeep is returned when an expression nests more than
	// MaxExpressionDepth levels.
	ErrExpressionTooDeep = errors.New("roaring: expression is too deeply nested")
)

// MaxExpressionDepth bounds the nesting depth accepted by CountExpr. It
// protects against runaway recursion and is comfortably above any practical
// filter.
const MaxExpressionDepth = 10000

type nodeColor uint8

const (
	colorWhite nodeColor = iota
	colorGray
	colorBlack
)

type planNode struct {
	kind  exprKind
	index int
	left  int
	right int
	leaf  *Bitmap
}

type exprPlan struct {
	nodes []planNode
	root  int
}

// compileExpr validates the expression before execution: it rejects nil nodes,
// cycles and trees that nest deeper than MaxExpressionDepth. Nodes shared by
// several parents (a DAG) are compiled once.
func compileExpr(root Expr) (*exprPlan, error) {
	colors := map[Expr]nodeColor{}
	depth := map[Expr]int{}
	ids := map[Expr]int{}
	plan := &exprPlan{}

	var build func(Expr, int) (int, int, error)
	build = func(e Expr, level int) (int, int, error) {
		if e == nil {
			return -1, 0, ErrNilExpression
		}
		if level > MaxExpressionDepth {
			return -1, 0, ErrExpressionTooDeep
		}
		switch colors[e] {
		case colorGray:
			return -1, 0, ErrExpressionCycle
		case colorBlack:
			return ids[e], depth[e], nil
		}
		colors[e] = colorGray

		var id, left, right, childDepth int
		var leaf *Bitmap
		switch n := e.(type) {
		case *leafNode:
			leaf = n.bitmap
		case *andNode:
			var err error
			if left, childDepth, err = build(n.left, level+1); err != nil {
				return -1, 0, err
			}
			if right, childDepth, err = build(n.right, level+1); err != nil {
				return -1, 0, err
			}
		case *orNode:
			var err error
			if left, childDepth, err = build(n.left, level+1); err != nil {
				return -1, 0, err
			}
			if right, childDepth, err = build(n.right, level+1); err != nil {
				return -1, 0, err
			}
		case *notNode:
			var err error
			if left, childDepth, err = build(n.child, level+1); err != nil {
				return -1, 0, err
			}
		default:
			return -1, 0, ErrNilExpression
		}

		nodeDepth := childDepth + 1

		id = len(plan.nodes)
		ids[e] = id
		depth[e] = nodeDepth
		colors[e] = colorBlack
		plan.nodes = append(plan.nodes, planNode{
			kind:  e.kind(),
			index: id,
			left:  left,
			right: right,
			leaf:  leaf,
		})
		return id, nodeDepth, nil
	}

	rootID, _, err := build(root, 1)
	if err != nil {
		return nil, err
	}
	plan.root = rootID
	return plan, nil
}

// CountExpr returns the exact cardinality of the set matched by expr within
// universe. The relative complement in Not is taken against universe, so a
// match is only ever counted on an element that universe contains.
//
// Neither universe nor any leaf bitmap is modified, and no intermediate
// bitmap is materialized at an internal node: only per-container scratch
// space is used, so the working set never grows with the size of the full
// universe. Deep or cyclic expressions are rejected before any data is read.
//
// A nil universe is treated as the empty set and yields a cardinality of 0.
func CountExpr(universe *Bitmap, expr Expr) (uint64, error) {
	plan, err := compileExpr(expr)
	if err != nil {
		return 0, err
	}
	return countCompiled(universe, plan), nil
}

const (
	csEmpty uint8 = iota + 1
	csPartial
	csFull
)

type exprEvaluator struct {
	universe   *roaringArray
	plan       *exprPlan
	seen       []bool
	states     []uint8
	cards      []int
	containers []container
	// currentKey is the 16-bit high word of the universe key being evaluated.
	currentKey uint16
	// uCard is the cardinality of the universe container for the active key.
	uCard int
	// uContainer is the universe container for the active key.
	uContainer container
	// leafLookups counts leaf-container reads; it backs the package tests.
	leafLookups int
	// peakScratchBytes is the largest live scratch size reached while
	// evaluating a single universe key; it backs the package tests.
	peakScratchBytes int
}

func newExprEvaluator(plan *exprPlan) *exprEvaluator {
	n := len(plan.nodes)
	return &exprEvaluator{
		plan:       plan,
		seen:       make([]bool, n),
		states:     make([]uint8, n),
		cards:      make([]int, n),
		containers: make([]container, n),
	}
}

// beginKey resets the evaluator for one 16-bit universe key.
func (ev *exprEvaluator) beginKey(u container) {
	for i := range ev.states {
		ev.seen[i] = false
		ev.states[i] = csEmpty
		ev.containers[i] = nil
	}
	ev.uContainer = u
	ev.uCard = u.getCardinality()
}

// scratchBytes reports the memory currently held by cached per-key containers.
func (ev *exprEvaluator) scratchBytes() int {
	total := 0
	for i := range ev.states {
		if ev.containers[i] != nil {
			total += ev.containers[i].getSizeInBytes()
		}
	}
	return total
}

// leafContainer resolves the bitmap of a leaf for the active key, clipped to
// the universe container. A missing container means the empty set.
func (ev *exprEvaluator) leafContainer(bm *Bitmap, materialize bool) (uint8, int, container) {
	var leaf container
	if bm != nil {
		leaf = bm.highlowcontainer.getContainer(ev.currentKey)
		ev.leafLookups++
	}
	if leaf == nil {
		return csEmpty, 0, nil
	}
	if ev.uCard == 1<<16 {
		card := leaf.getCardinality()
		if card == 1<<16 {
			return csFull, card, nil
		}
		return csPartial, card, leaf
	}
	card := leaf.andCardinality(ev.uContainer)
	if card == 0 {
		return csEmpty, 0, nil
	}
	if card == ev.uCard {
		return csFull, card, nil
	}
	if materialize {
		return csPartial, card, leaf.and(ev.uContainer)
	}
	return csPartial, card, nil
}

// count returns the cardinality of node id for the active key, short
// circuiting operands that can no longer change the result.
func (ev *exprEvaluator) count(id int) int {
	if ev.seen[id] {
		return ev.cards[id]
	}
	ev.seen[id] = true
	n := &ev.plan.nodes[id]
	var state uint8
	var card int

	switch n.kind {
	case kindLeaf:
		state, card, _ = ev.leafContainer(n.leaf, false)

	case kindNot:
		card = ev.uCard - ev.count(n.left)
		if card == 0 {
			state = csEmpty
		} else if card == ev.uCard {
			state = csFull
		} else {
			state = csPartial
		}

	case kindAnd:
		leftCard := ev.count(n.left)
		if leftCard == 0 {
			state = csEmpty
			break
		}
		rightCard := ev.count(n.right)
		if rightCard == 0 {
			state = csEmpty
			card = 0
			break
		}
		if ev.states[n.left] == csFull {
			state = ev.states[n.right]
			card = rightCard
			break
		}
		if ev.states[n.right] == csFull {
			state = ev.states[n.left]
			card = leftCard
			break
		}
		c := ev.cont(n.left).and(ev.cont(n.right))
		card = c.getCardinality()
		if card == 0 {
			state = csEmpty
		} else {
			state = csPartial
		}

	case kindOr:
		leftCard := ev.count(n.left)
		if leftCard == ev.uCard {
			state = csFull
			card = ev.uCard
			break
		}
		rightCard := ev.count(n.right)
		if rightCard == ev.uCard {
			state = csFull
			card = ev.uCard
			break
		}
		inter := 0
		if leftCard > 0 && rightCard > 0 {
			inter = ev.cont(n.left).andCardinality(ev.cont(n.right))
		}
		card = leftCard + rightCard - inter
		if card == 0 {
			state = csEmpty
		} else {
			state = csPartial
		}
	}

	ev.states[id] = state
	ev.cards[id] = card
	return card
}

// cont returns a container representing node id for the active key. The
// container is bounded by the universe and is only requested for operands
// known to matter after short circuiting.
func (ev *exprEvaluator) cont(id int) container {
	if c := ev.containers[id]; c != nil {
		return c
	}
	ev.count(id)
	n := &ev.plan.nodes[id]
	var c container

	switch n.kind {
	case kindLeaf:
		switch ev.states[id] {
		case csFull:
			c = ev.uContainer
		case csEmpty:
		default:
			_, _, c = ev.leafContainer(n.leaf, true)
		}

	case kindNot:
		switch ev.states[id] {
		case csEmpty:
		case csFull:
			c = ev.uContainer
		default:
			child := ev.cont(n.left)
			if child == nil {
				c = ev.uContainer
			} else {
				c = ev.uContainer.andNot(child)
			}
		}

	case kindAnd:
		switch {
		case ev.states[id] == csFull:
			c = ev.uContainer
		case ev.states[id] == csEmpty:
		case ev.states[n.left] == csEmpty || ev.states[n.right] == csEmpty:
		case ev.states[n.left] == csFull:
			c = ev.cont(n.right)
		case ev.states[n.right] == csFull:
			c = ev.cont(n.left)
		default:
			l := ev.cont(n.left)
			r := ev.cont(n.right)
			if l != nil && r != nil {
				c = l.and(r)
			}
		}

	case kindOr:
		switch {
		case ev.states[id] == csFull:
			c = ev.uContainer
		case ev.states[id] == csEmpty:
		case ev.states[n.left] == csEmpty:
			c = ev.cont(n.right)
		case ev.states[n.right] == csEmpty:
			c = ev.cont(n.left)
		default:
			l := ev.cont(n.left)
			r := ev.cont(n.right)
			switch {
			case l == nil:
				c = r
			case r == nil:
				c = l
			default:
				c = l.or(r)
			}
		}
	}

	if c != nil && c.getCardinality() == 0 {
		c = nil
	}
	ev.containers[id] = c
	if size := ev.scratchBytes(); size > ev.peakScratchBytes {
		ev.peakScratchBytes = size
	}
	return c
}

// countCompiled iterates only over the universe keys. Leaves and sub
// expressions outside the universe are never visited.
func countCompiled(universe *Bitmap, plan *exprPlan) uint64 {
	if universe == nil {
		return 0
	}
	ra := &universe.highlowcontainer
	ev := newExprEvaluator(plan)

	total := uint64(0)
	for i := 0; i < ra.size(); i++ {
		u := ra.getContainerAtIndex(i)
		ev.beginKey(u)
		ev.universe = ra
		ev.currentKey = ra.getKeyAtIndex(i)
		total += uint64(ev.count(plan.root))
	}
	return total
}

// exprDebugStats holds internal counters used by the package tests.
type exprDebugStats struct {
	leafLookups      int
	peakScratchBytes int
}

// countExprDebug runs CountExpr while tracking leaf reads and the largest live
// per-key scratch size. It is used only by the package's own tests.
func countExprDebug(universe *Bitmap, expr Expr) (uint64, exprDebugStats, error) {
	plan, err := compileExpr(expr)
	if err != nil {
		return 0, exprDebugStats{}, err
	}
	if universe == nil {
		return 0, exprDebugStats{}, nil
	}
	ra := &universe.highlowcontainer
	ev := newExprEvaluator(plan)
	total := uint64(0)
	for i := 0; i < ra.size(); i++ {
		u := ra.getContainerAtIndex(i)
		ev.beginKey(u)
		ev.universe = ra
		ev.currentKey = ra.getKeyAtIndex(i)
		total += uint64(ev.count(plan.root))
		ev.cont(plan.root)
	}
	return total, exprDebugStats{
		leafLookups:      ev.leafLookups,
		peakScratchBytes: ev.peakScratchBytes,
	}, nil
}
