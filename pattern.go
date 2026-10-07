package xpath

import (
	"errors"
	"fmt"
	"math"
)

// Pattern is a compiled XSLT 1.0 pattern. A pattern answers whether a given
// node belongs to a structural shape ("does this node match?") without
// re-executing an XPath selection against the document root for every node.
//
// A Pattern is safe for concurrent use by multiple goroutines.
type Pattern struct {
	s        string
	branches []*patternBranch
}

// patternBranch is one operand of the top-level union in a pattern.
type patternBranch struct {
	absolute bool
	rootOnly bool
	steps    []*patternStep
}

// patternStep is one location path step in document order.
type patternStep struct {
	// axis is one of "child" or "attribute".
	axis string
	// nodeTest is the compiled name/kind test shared with the XPath engine.
	nodeTest func(NodeNavigator) bool
	// predicates are the positional predicates attached to this step, in
	// source order.
	predicates []node
	// descendant is true when a "//" separator stands between this step and
	// the step directly inside it (the preceding step in document order), or
	// between the document root and the first step of an absolute pattern.
	descendant bool
}

// CompilePattern compiles an XSLT 1.0 pattern string.
//
// The supported pattern subset is:
//
//   - name tests ("book"), wildcards ("*") and prefixed names ("ns:book")
//   - the attribute axis ("@lang", "@*")
//   - child and descendant separators ("/", "//")
//   - top-level union ("a | b")
//   - positional predicates ("item[1]", "item[position()=last()-1]")
//
// Pattern predicates are restricted to positional expressions built from
// numbers, position(), last(), arithmetic, numeric comparison, and/or and
// grouped subexpressions. Entry forms that XSLT patterns may use but this
// subset does not implement (id(...), key(...), and any other function call
// used as a path entry), and non-positional predicates that test attribute
// values or the existence of other nodes, are rejected at compile time.
func CompilePattern(expr string) (*Pattern, error) {
	if expr == "" {
		return nil, errors.New("xpath: pattern expression is nil")
	}
	var (
		branches []*patternBranch
		err      error
	)
	func() {
		defer func() {
			if e := recover(); e != nil {
				switch x := e.(type) {
				case string:
					err = errors.New(x)
				case error:
					err = x
				default:
					err = errors.New("unknown panic")
				}
			}
		}()
		ast := parse(expr, nil)
		for _, branchAST := range collectPatternBranches(ast) {
			branch, buildErr := buildPatternBranch(branchAST)
			if buildErr != nil {
				err = buildErr
				return
			}
			branches = append(branches, branch)
		}
	}()
	if err != nil {
		return nil, err
	}
	if len(branches) == 0 {
		return nil, errors.New("xpath: pattern expression is nil")
	}
	return &Pattern{s: expr, branches: branches}, nil
}

// MustCompilePattern compiles a pattern and panics if compilation fails.
func MustCompilePattern(expr string) *Pattern {
	p, err := CompilePattern(expr)
	if err != nil {
		panic(err)
	}
	return p
}

// String returns the pattern expression string.
func (p *Pattern) String() string {
	return p.s
}

// Match reports whether the given node matches the pattern. The navigator is
// not modified: matching only inspects the node, its ancestors and the sibling
// collections required to evaluate positional predicates.
//
// Positional predicates are evaluated against the pattern-defined sibling
// collection (the similarly named element children of the parent, or the
// attributes of the parent element), so position() and last() never collapse
// to 1.
func (p *Pattern) Match(current NodeNavigator) bool {
	if current == nil {
		return false
	}
	for _, branch := range p.branches {
		if branch.match(current) {
			return true
		}
	}
	return false
}

// collectPatternBranches splits a top-level union into its operand ASTs.
func collectPatternBranches(n node) []node {
	if op, ok := n.(*operatorNode); ok && op.Op == "|" {
		return append(collectPatternBranches(op.Left), collectPatternBranches(op.Right)...)
	}
	return []node{n}
}

// buildPatternBranch walks a single location path from its innermost node
// outwards and records its steps in document order.
func buildPatternBranch(ast node) (*patternBranch, error) {
	branch := &patternBranch{}
	var (
		steps       []*patternStep
		pendingPred []node
	)
	for current := ast; current != nil; {
		switch n := current.(type) {
		case *filterNode:
			pendingPred = append(pendingPred, n.Condition)
			current = n.Input
		case *axisNode:
			if n.AxisType != "child" && n.AxisType != "attribute" {
				return nil, patternErrorf("unsupported axis %q in pattern (only child and attribute axes are supported)", n.AxisType)
			}
			if n.Prop != "" {
				return nil, patternErrorf("unsupported node test %s() in pattern (only name tests and wildcards are supported)", n.Prop)
			}
			if input, ok := n.Input.(*axisNode); ok && input.AxisType == "attribute" {
				return nil, patternErrorf("attribute step must be the last step in a pattern")
			}
			preds := make([]node, len(pendingPred))
			for i := range pendingPred {
				reversed := pendingPred[len(pendingPred)-1-i]
				preds[i] = reversed
				if err := validatePatternPredicate(reversed); err != nil {
					return nil, err
				}
			}
			step := &patternStep{
				axis:       n.AxisType,
				nodeTest:   axisPredicate(n),
				predicates: preds,
			}
			steps = append(steps, step)
			pendingPred = nil

			switch input := n.Input.(type) {
			case nil:
				current = nil
			case *rootNode:
				branch.absolute = true
				if n.AxisType == "attribute" && input.slash == "/" && !step.descendant {
					return nil, patternErrorf("absolute pattern must not start directly with an attribute step")
				}
				current = nil
			case *axisNode:
				if input.AxisType == "descendant-or-self" {
					step.descendant = true
					current = input.Input
				} else {
					current = input
				}
			case *filterNode:
				current = input
			default:
				return nil, patternErrorf("unsupported path entry in pattern (only name tests, wildcards and attributes are supported)")
			}
		case *rootNode:
			branch.absolute = true
			branch.rootOnly = n.slash == "/" && len(steps) == 0
			current = nil
		case *functionNode:
			return nil, patternEntryError(n)
		case *groupNode:
			return nil, patternErrorf("unsupported grouped path in pattern")
		case *variableNode:
			return nil, patternErrorf("unsupported variable reference in pattern")
		case *operatorNode:
			return nil, patternErrorf("unsupported operator %q in pattern path", n.Op)
		default:
			return nil, patternErrorf("unsupported construct in pattern")
		}
	}

	if len(steps) == 0 && !branch.rootOnly {
		return nil, patternErrorf("pattern must contain a location path")
	}
	for i, j := 0, len(steps)-1; i < j; i, j = i+1, j-1 {
		steps[i], steps[j] = steps[j], steps[i]
	}
	branch.steps = steps
	return branch, nil
}

// patternEntryError gives a precise diagnostic for function-call path
// entries, calling out id() and key() explicitly.
func patternEntryError(n *functionNode) error {
	switch n.FuncName {
	case "id", "key":
		return patternErrorf("unsupported pattern entry: %s() is not implemented in the XSLT 1.0 pattern subset", n.FuncName)
	default:
		return patternErrorf("unsupported pattern entry: %s() is not a valid path entry in the pattern subset", n.FuncName)
	}
}

// validatePatternPredicate restricts predicates to the positional subset.
func validatePatternPredicate(n node) error {
	switch v := n.(type) {
	case *operandNode:
		switch v.Val.(type) {
		case float64:
			return nil
		case string:
			return patternErrorf("unsupported string constant in pattern predicate (only positional number expressions are supported)")
		default:
			return patternErrorf("unsupported constant in pattern predicate (only numbers are supported)")
		}
	case *functionNode:
		switch v.FuncName {
		case "position", "last":
			if len(v.Args) != 0 {
				return patternErrorf("%s() takes no arguments in a pattern predicate", v.FuncName)
			}
			return nil
		default:
			return patternErrorf("unsupported function %s() in pattern predicate (only position() and last() are supported)", v.FuncName)
		}
	case *operatorNode:
		switch v.Op {
		case "+", "-", "*", "div", "mod", "=", "!=", "<", "<=", ">", ">=", "and", "or":
		default:
			return patternErrorf("unsupported operator %q in pattern predicate", v.Op)
		}
		if err := validatePatternPredicate(v.Left); err != nil {
			return err
		}
		return validatePatternPredicate(v.Right)
	case *groupNode:
		return validatePatternPredicate(v.Input)
	case *axisNode, *filterNode:
		return patternErrorf("unsupported node-set expression in pattern predicate")
	case *rootNode:
		return patternErrorf("unsupported absolute path in pattern predicate")
	case *variableNode:
		return patternErrorf("unsupported variable reference in pattern predicate")
	default:
		return patternErrorf("unsupported construct in pattern predicate")
	}
}

// match tests whether the candidate node satisfies this union branch.
func (b *patternBranch) match(current NodeNavigator) bool {
	if b.rootOnly {
		node := current.Copy()
		return !node.MoveToParent()
	}
	if len(b.steps) == 0 {
		return false
	}
	return b.matchFrom(len(b.steps)-1, current)
}

// matchFrom matches steps[idx] (and its prefix) against candidate.
func (b *patternBranch) matchFrom(idx int, candidate NodeNavigator) bool {
	step := b.steps[idx]
	if !step.nodeTest(candidate) || !step.satisfiesPredicates(candidate) {
		return false
	}

	if idx == 0 {
		if !b.absolute {
			return true
		}
		// A leading "//" allows the first step at any depth.
		if step.descendant {
			return true
		}
		// The only legal anchored first step is a child of the document
		// root (single-step absolute attribute patterns are rejected at
		// compile time).
		parent := candidate.Copy()
		return parent.MoveToParent() && !parent.Copy().MoveToParent()
	}

	// The inner step is matched against the parent of the current step's
	// matched node. MoveToParent from an attribute lands directly on the
	// owning element, which is the correct inner context.
	context := candidate.Copy()
	if !context.MoveToParent() {
		return false
	}

	if step.descendant {
		node := context.Copy()
		for {
			if b.matchFrom(idx-1, node) {
				return true
			}
			if !node.MoveToParent() {
				return false
			}
		}
	}
	return b.matchFrom(idx-1, context)
}

// satisfiesPredicates evaluates the predicates of a step against the
// pattern-defined sibling collection.
func (s *patternStep) satisfiesPredicates(candidate NodeNavigator) bool {
	if len(s.predicates) == 0 {
		return true
	}
	position, last, ok := s.siblingPositionAndLast(candidate, s.predicatesNeedLast())
	if !ok {
		return false
	}
	ctx := &predicateContext{position: position, last: last}
	for _, pred := range s.predicates {
		evaluator := &patternPredicateEvaluator{ctx: ctx}
		value, err := evaluator.eval(pred)
		if err != nil {
			return false
		}
		switch v := value.(type) {
		case float64:
			if v != float64(position) {
				return false
			}
		case bool:
			if !v {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// predicatesNeedLast reports whether any predicate of the step calls last().
// When it does not, matching never has to walk the siblings that follow the
// candidate node.
func (s *patternStep) predicatesNeedLast() bool {
	for _, pred := range s.predicates {
		if predicateReferencesFunction(pred, "last") {
			return true
		}
	}
	return false
}

func predicateReferencesFunction(n node, name string) bool {
	switch v := n.(type) {
	case *functionNode:
		if v.FuncName == name {
			return true
		}
		for _, arg := range v.Args {
			if predicateReferencesFunction(arg, name) {
				return true
			}
		}
	case *operatorNode:
		if predicateReferencesFunction(v.Left, name) || predicateReferencesFunction(v.Right, name) {
			return true
		}
	case *filterNode:
		if predicateReferencesFunction(v.Input, name) || predicateReferencesFunction(v.Condition, name) {
			return true
		}
	case *groupNode:
		return predicateReferencesFunction(v.Input, name)
	}
	return false
}

// siblingPositionAndLast computes the XSLT pattern context for a candidate
// node: its 1-based position within, and optionally the total size of, the
// collection defined by the step. For a child step this counts the similarly
// named siblings around the candidate directly (no identity comparison and no
// descent into other subtrees); for an attribute step it scans the owning
// element's attributes.
func (s *patternStep) siblingPositionAndLast(candidate NodeNavigator, needLast bool) (int, int, bool) {
	if s.axis == "attribute" {
		return attributePositionAndLast(candidate, s.nodeTest, needLast)
	}

	position := 1
	preceding := candidate.Copy()
	for preceding.MoveToPrevious() {
		if s.nodeTest(preceding) {
			position++
		}
	}
	last := position
	if needLast {
		following := candidate.Copy()
		for following.MoveToNext() {
			if s.nodeTest(following) {
				last++
			}
		}
	}
	return position, last, true
}

// attributePositionAndLast computes the position of a candidate attribute
// within the matching attribute set of its owning element.
func attributePositionAndLast(candidate NodeNavigator, test func(NodeNavigator) bool, needLast bool) (int, int, bool) {
	if candidate.NodeType() != AttributeNode {
		return 0, 0, false
	}
	owner := candidate.Copy()
	if !owner.MoveToParent() {
		return 0, 0, false
	}
	position, last := 0, 0
	attr := owner.Copy()
	for attr.MoveToNextAttribute() {
		if test(attr) {
			last++
			if position == 0 && sameNode(attr, candidate) {
				position = last
				if !needLast {
					break
				}
			}
		}
	}
	if position == 0 {
		return 0, 0, false
	}
	return position, last, true
}

// nodeComparer is an optional NodeNavigator capability for O(1) node
// identity. Navigators that do not implement it fall back to a structural
// hash comparison.
type nodeComparer interface {
	IsSameNode(NodeNavigator) bool
}

func sameNode(a, b NodeNavigator) bool {
	if a.NodeType() != b.NodeType() {
		return false
	}
	if cmp, ok := a.(nodeComparer); ok {
		return cmp.IsSameNode(b)
	}
	return getHashCode(a.Copy()) == getHashCode(b.Copy())
}

func patternErrorf(format string, args ...interface{}) error {
	return errors.New("xpath: " + fmt.Sprintf(format, args...))
}

// predicateContext carries the XSLT pattern context size/position used by
// position() and last() inside predicates.
type predicateContext struct {
	position int
	last     int
}

// patternPredicateEvaluator evaluates the restricted predicate subset.
type patternPredicateEvaluator struct {
	ctx *predicateContext
}

// eval evaluates a validated predicate AST node against the pattern context.
func (e *patternPredicateEvaluator) eval(n node) (interface{}, error) {
	switch v := n.(type) {
	case *operandNode:
		if number, ok := v.Val.(float64); ok {
			return number, nil
		}
		return nil, errors.New("xpath: unsupported constant in pattern predicate")
	case *functionNode:
		switch v.FuncName {
		case "position":
			return float64(e.ctx.position), nil
		case "last":
			return float64(e.ctx.last), nil
		}
		return nil, patternErrorf("unsupported function %s() in pattern predicate", v.FuncName)
	case *groupNode:
		return e.eval(v.Input)
	case *operatorNode:
		return e.evalOperator(v)
	}
	return nil, errors.New("xpath: unsupported construct in pattern predicate")
}

func (e *patternPredicateEvaluator) evalOperator(op *operatorNode) (interface{}, error) {
	switch op.Op {
	case "and", "or":
		left, err := e.eval(op.Left)
		if err != nil {
			return nil, err
		}
		leftBool := predicateBool(left)
		if op.Op == "and" && !leftBool {
			return false, nil
		}
		if op.Op == "or" && leftBool {
			return true, nil
		}
		right, err := e.eval(op.Right)
		if err != nil {
			return nil, err
		}
		return predicateBool(right), nil
	case "+", "-", "*", "div", "mod":
		left, err := e.number(op.Left)
		if err != nil {
			return nil, err
		}
		right, err := e.number(op.Right)
		if err != nil {
			return nil, err
		}
		switch op.Op {
		case "+":
			return left + right, nil
		case "-":
			return left - right, nil
		case "*":
			return left * right, nil
		case "div":
			return left / right, nil
		default:
			return modulo(left, right), nil
		}
	case "=", "!=", "<", "<=", ">", ">=":
		left, err := e.eval(op.Left)
		if err != nil {
			return nil, err
		}
		right, err := e.eval(op.Right)
		if err != nil {
			return nil, err
		}
		var result bool
		switch op.Op {
		case "=":
			result = predicateEqual(left, right)
		case "!=":
			result = !predicateEqual(left, right)
		case "<":
			result = predicateLess(left, right)
		case "<=":
			result = predicateLess(left, right) || predicateEqual(left, right)
		case ">":
			result = predicateLess(right, left)
		default:
			result = predicateLess(right, left) || predicateEqual(left, right)
		}
		return result, nil
	}
	return nil, patternErrorf("unsupported operator %q in pattern predicate", op.Op)
}

func (e *patternPredicateEvaluator) number(n node) (float64, error) {
	value, err := e.eval(n)
	if err != nil {
		return 0, err
	}
	number, ok := value.(float64)
	if !ok {
		return 0, errors.New("xpath: expected a number in pattern predicate")
	}
	return number, nil
}

// predicateBool applies the XPath boolean coercion to a predicate value.
func predicateBool(value interface{}) bool {
	switch v := value.(type) {
	case bool:
		return v
	case float64:
		return v != 0 && !math.IsNaN(v)
	case string:
		return v != ""
	}
	return false
}

// predicateEqual reports XPath "=" equality for two predicate values.
func predicateEqual(left, right interface{}) bool {
	ln, lok := left.(float64)
	rn, rok := right.(float64)
	if lok && rok {
		return ln == rn
	}
	ls, lsok := left.(string)
	rs, rsok := right.(string)
	if lsok && rsok {
		return ls == rs
	}
	lb, lbok := left.(bool)
	rb, rbok := right.(bool)
	if lbok && rbok {
		return lb == rb
	}
	return false
}

// predicateLess reports XPath "<" ordering for two predicate values.
func predicateLess(left, right interface{}) bool {
	ln, lok := left.(float64)
	rn, rok := right.(float64)
	if lok && rok {
		return ln < rn
	}
	ls, lsok := left.(string)
	rs, rsok := right.(string)
	if lsok && rsok {
		return ls < rs
	}
	return false
}

// modulo implements XPath's "mod" using IEEE 754 remainder semantics.
func modulo(left, right float64) float64 {
	if right == 0 || math.IsNaN(left) || math.IsNaN(right) {
		return math.NaN()
	}
	return math.Mod(left, right)
}
