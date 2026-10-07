package roaring

import "errors"

// Errors returned when an expression cannot be counted.
var (
	// ErrNilExpression is returned when Count is called with a nil expression.
	ErrNilExpression = errors.New("roaring: expression is nil")
	// ErrNilOperand is returned when an AND, OR or NOT node contains a nil operand.
	ErrNilOperand = errors.New("roaring: expression contains a nil operand")
	// ErrCycle is returned when the expression graph contains a cycle.
	ErrCycle = errors.New("roaring: expression graph contains a cycle")
	// ErrExpressionTooDeep is returned when the longest root-to-leaf path of
	// the expression graph exceeds MaxExpressionDepth.
	ErrExpressionTooDeep = errors.New("roaring: expression is too deep")
	// ErrNotArity is returned when a NOT node does not contain exactly one operand.
	ErrNotArity = errors.New("roaring: NOT expression must have exactly one operand")
	// ErrUnknownExpression is returned when a node does not implement one of
	// the concrete expression types provided by this package.
	ErrUnknownExpression = errors.New("roaring: unknown expression node type")
)

// MaxExpressionDepth bounds the number of nodes on the longest root-to-leaf
// path that Count accepts. It keeps evaluation stack usage bounded and makes
// cyclic or pathologically deep graphs cheap to reject before any container is
// scanned.
const MaxExpressionDepth = 10000

// Expr is a boolean expression over 32-bit integer sets.
//
// Leaves reference a caller-owned Bitmap directly: the bitmap is borrowed,
// never copied or modified. Internal nodes combine operands with AND, OR or a
// NOT that is always taken relative to the explicit universe passed to Count.
// Sub-expressions may be shared (the same node can appear under several
// parents) and the same Bitmap may back several leaves.
//
// Expr only supports counting. Evaluation streams through the roaring
// containers block by block and never materializes an intermediate bitmap the
// size of the universe.
type Expr interface {
	nodeMarker()
}

// LeafExpr is an expression whose set of values is the content of the
// referenced Bitmap. A nil bitmap behaves like an empty leaf.
type LeafExpr struct {
	bitmap *Bitmap
}

// AndExpr is the intersection of its operands. With no operands it is the AND
// identity (the whole universe).
type AndExpr struct {
	operands []Expr
}

// OrExpr is the union of its operands. With no operands it is the OR identity
// (the empty set).
type OrExpr struct {
	operands []Expr
}

// NotExpr is the complement of its single operand within the universe passed
// to Count. Because the universe is finite and explicit, a NOT never denotes
// an infinite set.
type NotExpr struct {
	operand Expr
}

func (*LeafExpr) nodeMarker() {}
func (*AndExpr) nodeMarker()  {}
func (*OrExpr) nodeMarker()   {}
func (*NotExpr) nodeMarker()  {}

// Leaf returns an expression referencing bm. The bitmap is not copied and must
// not be modified while a Count call that uses the leaf is running.
func Leaf(bm *Bitmap) *LeafExpr {
	return &LeafExpr{bitmap: bm}
}

// AndOf returns an expression for the intersection of the given operands.
// Operands may be shared sub-expressions and may appear more than once.
func AndOf(operands ...Expr) *AndExpr {
	return &AndExpr{operands: operands}
}

// OrOf returns an expression for the union of the given operands.
// Operands may be shared sub-expressions and may appear more than once.
func OrOf(operands ...Expr) *OrExpr {
	return &OrExpr{operands: operands}
}

// Not returns universe \ operand, where universe is supplied to Count.
// Exactly one operand is required.
func Not(operand Expr) *NotExpr {
	return &NotExpr{operand: operand}
}

// Count returns the number of values in universe for which expr is true.
// universe is the explicit finite domain used to evaluate every NOT node and
// is not modified. Leaves may contain values outside universe; only their
// intersection with universe contributes.
//
// No intermediate bitmap is materialized. The expression is evaluated one
// 16-bit container block at a time, leaf containers are located by binary
// search (never iterated), and the only scratch space is a single block, so
// working memory is independent of the universe size. AND and OR short-circuit
// within a block: once an intersection becomes empty (or a union becomes
// full), the remaining operands are not evaluated for that block.
//
// The graph is validated before any container is touched: nil operands, bad
// NOT arity, cycles and paths longer than MaxExpressionDepth are rejected.
func Count(expr Expr, universe *Bitmap) (uint64, error) {
	if expr == nil {
		return 0, ErrNilExpression
	}
	refs, err := analyze(expr)
	if err != nil {
		return 0, err
	}
	if universe == nil || universe.IsEmpty() {
		return 0, nil
	}

	ra := &universe.highlowcontainer
	ev := evaluator{universe: ra, refs: refs, memo: make(map[Expr]memoEntry)}

	var total uint64
	for i := 0; i < ra.size(); i++ {
		key := ra.getKeyAtIndex(i)
		uniContainer := ra.getContainerAtIndex(i)
		raw := ev.block(expr, key, uniContainer)
		if raw != nil {
			total += uint64(raw.andCardinality(uniContainer))
		}
		ev.resetMemo()
	}
	return total, nil
}

// CountExpr is Count with the universe as the receiver. It returns the number
// of values of rb for which expr is true, without materializing intermediate
// bitmaps. rb is treated as the explicit finite universe for every NOT node.
func (rb *Bitmap) CountExpr(expr Expr) (uint64, error) {
	return Count(expr, rb)
}

// fullBlock is the complete 16-bit block, used as the per-block AND identity.
// It is only ever read (all folding operations are non-mutating), so a single
// shared instance is safe.
var fullBlock container = rangeOfOnes(0, MaxUint16)

// memoEntry records the raw (universe-unbounded, except NOT which is bounded)
// container of a shared node within one block.
type memoEntry struct {
	container container
	ok        bool
}

// evaluator counts one expression against one universe. It walks the universe
// blocks; memo is cleared between blocks and therefore only ever retains
// containers for a single block, regardless of the universe size.
type evaluator struct {
	universe *roaringArray
	// refs is the number of incoming edges of each node; nodes referenced
	// more than once are memoized within each block.
	refs map[Expr]int
	memo map[Expr]memoEntry
}

func (ev *evaluator) resetMemo() {
	for node := range ev.memo {
		delete(ev.memo, node)
	}
}

// block returns the raw container of node within the block identified by key.
// It returns nil when the node has no values in the block. The result is
// universe-unbounded for leaves/AND/OR; NOT results are already intersected
// with the universe block, which is harmless under the final intersection
// applied by Count. All folding uses non-mutating container operations, so
// cached containers and caller-owned leaf containers are never modified.
func (ev *evaluator) block(node Expr, key uint16, uniContainer container) container {
	if ev.refs[node] > 1 {
		if entry, cached := ev.memo[node]; cached {
			if !entry.ok {
				return nil
			}
			return entry.container
		}
		result := ev.computeBlock(node, key, uniContainer)
		ev.memo[node] = memoEntry{container: result, ok: result != nil}
		return result
	}
	return ev.computeBlock(node, key, uniContainer)
}

func (ev *evaluator) computeBlock(node Expr, key uint16, uniContainer container) container {
	switch n := node.(type) {
	case *LeafExpr:
		if n.bitmap == nil {
			return nil
		}
		return n.bitmap.highlowcontainer.getContainer(key)

	case *NotExpr:
		child := ev.block(n.operand, key, uniContainer)
		if child == nil {
			return uniContainer
		}
		return uniContainer.andNot(child)

	case *AndExpr:
		var acc container
		for _, operand := range n.operands {
			current := ev.block(operand, key, uniContainer)
			if current == nil {
				return nil
			}
			if acc == nil {
				acc = current
				continue
			}
			acc = acc.and(current)
			if acc.isEmpty() {
				return nil
			}
		}
		if acc == nil {
			return fullBlock
		}
		return acc

	case *OrExpr:
		var acc container
		for _, operand := range n.operands {
			current := ev.block(operand, key, uniContainer)
			if current == nil {
				continue
			}
			if acc == nil {
				acc = current
			} else {
				acc = acc.or(current)
			}
			if acc.isFull() {
				return acc
			}
		}
		return acc
	}
	return nil
}

// analysisFrame is an active DFS frame used by analyze.
type analysisFrame struct {
	node      Expr
	nextChild int
}

// analyze validates the expression graph reachable from root: rejects nil
// operands, NOT nodes without exactly one operand, cycles and paths longer than
// MaxExpressionDepth. It returns the number of incoming edges of every node,
// counting the root once.
func analyze(root Expr) (map[Expr]int, error) {
	const (
		white = 0
		gray  = 1
		black = 2
	)

	color := make(map[Expr]int)
	refs := make(map[Expr]int)

	color[root] = gray
	refs[root] = 1
	frames := []analysisFrame{{node: root}}

	for len(frames) > 0 {
		top := &frames[len(frames)-1]
		children, err := nodeChildren(top.node)
		if err != nil {
			return nil, err
		}

		if top.nextChild >= len(children) {
			color[top.node] = black
			frames = frames[:len(frames)-1]
			continue
		}

		child := children[top.nextChild]
		top.nextChild++
		if child == nil {
			return nil, ErrNilOperand
		}

		switch color[child] {
		case gray:
			return nil, ErrCycle
		case black:
			refs[child]++
			continue
		}

		if len(frames)+1 > MaxExpressionDepth {
			return nil, ErrExpressionTooDeep
		}
		refs[child]++
		color[child] = gray
		frames = append(frames, analysisFrame{node: child})
	}

	return refs, nil
}

// nodeChildren returns the direct operands of node and enforces NOT arity.
func nodeChildren(node Expr) ([]Expr, error) {
	switch n := node.(type) {
	case *LeafExpr:
		return nil, nil
	case *AndExpr:
		return n.operands, nil
	case *OrExpr:
		return n.operands, nil
	case *NotExpr:
		if n.operand == nil {
			return nil, ErrNotArity
		}
		return []Expr{n.operand}, nil
	default:
		return nil, ErrUnknownExpression
	}
}
