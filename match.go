package xpath

import (
	"fmt"
	"reflect"
)

// contextKind identifies which context value a contextNumberNode carries.
type contextKind int

const (
	contextPosition contextKind = iota
	contextLast
)

// contextNumberNode is a synthetic AST leaf standing in for position() or
// last() inside a pattern predicate. Unlike a normal function node it reads
// the value from the matcher's live matchContext instead of deriving it from
// an input query, so positional predicates evaluate against the sibling
// collection mandated by the pattern step.
type contextNumberNode struct {
	nodeType
	kind contextKind
	ctx  *matchContext
}

func (c *contextNumberNode) String() string {
	if c.kind == contextPosition {
		return "position()"
	}
	return "last()"
}

// matchContext is the positional context shared by the predicates of one
// Match evaluation.
type matchContext struct {
	position int
	last     int
}

// contextNumberQuery evaluates to the live position()/last() value.
type contextNumberQuery struct {
	ctx  *matchContext
	kind contextKind
}

func (c *contextNumberQuery) Select(iterator) NodeNavigator { return nil }

func (c *contextNumberQuery) Evaluate(iterator) interface{} {
	if c.kind == contextPosition {
		return float64(c.ctx.position)
	}
	return float64(c.ctx.last)
}

func (c *contextNumberQuery) Clone() query {
	return &contextNumberQuery{ctx: c.ctx, kind: c.kind}
}

func (c *contextNumberQuery) ValueType() resultType { return xpathResultType.Number }

func (c *contextNumberQuery) Properties() queryProp { return queryProps.Merge }

// compiledPredicate is a pattern predicate with position()/last() rewritten to
// contextNumberNodes.
type compiledPredicate struct {
	q   query
	ctx *matchContext
}

// compilePredicate rewrites positional functions and builds the predicate.
func compilePredicate(cond node) (*compiledPredicate, error) {
	ctx := &matchContext{}
	rewritten := rewriteContextFunctions(cond, ctx)
	b := &builder{}
	props := builderProps.None
	q, err := b.processNode(rewritten, flagsEnum.None, &props)
	if err != nil {
		return nil, err
	}
	if q == nil {
		return nil, fmt.Errorf("invalid pattern predicate: %s", cond)
	}
	return &compiledPredicate{q: q, ctx: ctx}, nil
}

// rewriteContextFunctions replaces position()/last() zero-argument calls with
// contextNumberNodes bound to ctx.
func rewriteContextFunctions(n node, ctx *matchContext) node {
	switch v := n.(type) {
	case *functionNode:
		switch v.FuncName {
		case "position", "last":
			if len(v.Args) == 0 {
				kind := contextPosition
				if v.FuncName == "last" {
					kind = contextLast
				}
				return &contextNumberNode{nodeType: nodeContextNumber, kind: kind, ctx: ctx}
			}
		}
		args := make([]node, len(v.Args))
		for i, a := range v.Args {
			args[i] = rewriteContextFunctions(a, ctx)
		}
		return newFunctionNode(v.FuncName, v.Prefix, args)
	case *operatorNode:
		return newOperatorNode(v.Op,
			rewriteContextFunctions(v.Left, ctx),
			rewriteContextFunctions(v.Right, ctx))
	case *filterNode:
		// A relative path used inside a predicate (including the predicates
		// hanging off its steps) has its own positional context; leave it for
		// the regular engine to evaluate.
		return v
	case *groupNode:
		return newGroupNode(rewriteContextFunctions(v.Input, ctx))
	case *axisNode:
		// A relative path used inside a predicate has its own positional
		// context; do not rewrite the position()/last() calls of such a
		// nested path. They are evaluated by the regular engine.
		return v
	default:
		return n
	}
}

// matchIterator exposes the mutable current node used while evaluating
// predicates.
type matchIterator struct {
	current NodeNavigator
}

func (m *matchIterator) Current() NodeNavigator { return m.current }

// matchPath matches one union alternative.
func matchPath(path *pathPattern, n NodeNavigator) bool {
	if path.rootOnly {
		return n.NodeType() == RootNode
	}
	last := len(path.steps) - 1
	return matchStep(path, last, n)
}

// matchStep reports whether node matches steps[i], recursively satisfying the
// preceding steps along the ancestor chain.
func matchStep(path *pathPattern, i int, node NodeNavigator) bool {
	step := path.steps[i]
	if !nodeTestMatches(step, node) {
		return false
	}
	if !evalStepPredicates(step, node) {
		return false
	}
	if i == 0 {
		if path.absolute {
			parent := node.Copy()
			if !parent.MoveToParent() {
				return false
			}
			return parent.NodeType() == RootNode
		}
		return true
	}

	parent := node.Copy()
	if !parent.MoveToParent() {
		return false
	}
	if path.steps[i].descendant {
		for ancestor := parent; ; {
			if ancestor.NodeType() == ElementNode && matchStep(path, i-1, ancestor) {
				return true
			}
			if !ancestor.MoveToParent() {
				return false
			}
			if ancestor.NodeType() == RootNode {
				if path.absolute {
					return matchStep(path, i-1, ancestor)
				}
				return false
			}
		}
	}
	return parent.NodeType() == ElementNode && matchStep(path, i-1, parent)
}

func nodeTestMatches(step *patternStep, node NodeNavigator) bool {
	a := step.axis
	if a.typeTest != node.NodeType() && a.typeTest != allNode {
		return false
	}
	// Namespace-bound name test (CompilePatternWithNS): compare by namespace
	// URI and local name.
	if a.hasNamespaceURI {
		type namespaceURL interface {
			NamespaceURL() string
		}
		nsNav, ok := node.(namespaceURL)
		if !ok || a.namespaceURI != nsNav.NamespaceURL() {
			return false
		}
		return a.LocalName == "" || a.LocalName == node.LocalName()
	}
	if a.Prefix != "" && node.Prefix() != a.Prefix {
		return false
	}
	// Empty LocalName is the '*' wildcard. A "prefix:*" form also has an
	// empty local name; only the prefix needs to agree.
	return a.LocalName == "" || a.LocalName == node.LocalName()
}

// evalStepPredicates evaluates all predicates of a step against node. The
// positional context at every stage is the sibling collection produced by the
// step node test and narrowed by the preceding predicates.
func evalStepPredicates(step *patternStep, node NodeNavigator) bool {
	if len(step.preds) == 0 {
		return true
	}

	siblings := collectStepSiblings(step, node)
	index := -1
	for idx, sib := range siblings {
		if sameNode(sib, node) {
			index = idx
			break
		}
	}
	if index < 0 {
		return false
	}

	alive := append([]NodeNavigator(nil), siblings...)
	aliveIndex := index
	for _, pred := range step.preds {
		pred.ctx.position = 0
		pred.ctx.last = len(alive)
		next := make([]NodeNavigator, 0, len(alive))
		target := alive[aliveIndex]
		nextIndex := -1
		for pos, cand := range alive {
			pred.ctx.position = pos + 1
			keep := evalPredicate(pred, cand)
			if keep {
				next = append(next, cand)
				if sameNode(cand, target) {
					nextIndex = len(next) - 1
				}
			}
		}
		alive = next
		aliveIndex = nextIndex
		if aliveIndex < 0 {
			return false
		}
	}
	return true
}

// collectStepSiblings returns the same-axis nodes that pass the step node
// test, in document order. For a child step these are the parent's children;
// for an attribute step these are the owning element's attributes.
func collectStepSiblings(step *patternStep, node NodeNavigator) []NodeNavigator {
	parent := node.Copy()
	if !parent.MoveToParent() {
		return nil
	}

	var out []NodeNavigator
	if step.attribute {
		attr := parent.Copy()
		for attr.MoveToNextAttribute() {
			if nodeTestMatches(step, attr) {
				out = append(out, attr.Copy())
			}
		}
		return out
	}

	child := parent.Copy()
	if !child.MoveToChild() {
		return out
	}
	for {
		if nodeTestMatches(step, child) {
			out = append(out, child.Copy())
		}
		if !child.MoveToNext() {
			break
		}
	}
	return out
}

// evalPredicate applies one predicate to cand, honoring numeric-predicate
// semantics (a number means position() equals that number).
func evalPredicate(pred *compiledPredicate, cand NodeNavigator) bool {
	it := &matchIterator{current: cand.Copy()}
	val := pred.q.Clone().Evaluate(it)
	switch v := val.(type) {
	case bool:
		return v
	case float64:
		return int(v) == pred.ctx.position
	case string:
		return len(v) > 0
	case nil:
		return false
	default:
		rv := reflect.ValueOf(val)
		switch rv.Kind() {
		case reflect.Bool:
			return rv.Bool()
		case reflect.Float64:
			return int(rv.Float()) == pred.ctx.position
		case reflect.String:
			return rv.Len() > 0
		default:
			// A node-set/query result: non-empty is true.
			if q, ok := val.(query); ok {
				return q.Select(it) != nil
			}
			return true
		}
	}
}

// sameNode reports whether two navigators address the same node. It relies on
// structural navigation without depending on a concrete navigator type.
// Navigators may optionally implement NodeComparer for an exact check.
func sameNode(a, b NodeNavigator) bool {
	if cmp, ok := a.(NodeComparer); ok {
		return cmp.IsSameNode(b)
	}
	if a.NodeType() != b.NodeType() || a.LocalName() != b.LocalName() {
		return false
	}

	x, y := a.Copy(), b.Copy()
	// Walk both nodes up to the root, comparing each level by the node type,
	// local name and the number of preceding same-type-and-name siblings.
	for {
		px, py := siblingOrdinal(x), siblingOrdinal(y)
		if px != py || x.LocalName() != y.LocalName() || x.Prefix() != y.Prefix() {
			return false
		}
		upX, upY := x.MoveToParent(), y.MoveToParent()
		if upX != upY {
			return false
		}
		if !upX {
			return true
		}
	}
}

// siblingOrdinal returns the zero-based index of the current node among its
// parent's children (document order), ignoring node types.
func siblingOrdinal(n NodeNavigator) int {
	probe := n.Copy()
	idx := 0
	for probe.MoveToPrevious() {
		idx++
	}
	return idx
}
