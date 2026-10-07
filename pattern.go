package xpath

import (
	"errors"
	"fmt"
)

// Pattern is a compiled XSLT 1.0 pattern.
//
// A pattern answers a different question from an XPath expression: does the
// current node belong to a given structure, rather than selecting nodes by
// evaluating a path from the document root. Match walks the current node's
// ancestor chain and its same-level siblings only, so it never searches the
// whole tree.
//
// The supported XSLT 1.0 pattern subset is:
//
//   - name tests and the '*' wildcard (including the 'prefix:*' form),
//   - attribute steps via '@name' or 'attribute::name',
//   - child steps separated by '/',
//   - descendant steps separated by '//',
//   - union alternatives separated by '|',
//   - predicates, including positional predicates such as '[1]',
//     '[position()=2]' and '[last()]'.
//
// Patterns that need an id()/key() entry form, or any other non-location-path
// primary expression, are rejected at compile time.
type Pattern struct {
	s     string
	paths []*pathPattern
}

// Match reports whether the current node of nav matches the pattern.
//
// The navigator is not moved; a copy is used internally. Predicate context is
// the candidate node, while position() and last() refer to the sibling
// collection required by the enclosing pattern step (the parent's children
// filtered by the step node test, or the owning element's attributes filtered
// by the attribute node test).
func (p *Pattern) Match(nav NodeNavigator) bool {
	if nav == nil || len(p.paths) == 0 {
		return false
	}
	cur := nav.Copy()
	for _, path := range p.paths {
		if matchPath(path, cur) {
			return true
		}
	}
	return false
}

// String returns the pattern source expression.
func (p *Pattern) String() string {
	return p.s
}

// CompilePattern compiles an XSLT 1.0 pattern expression.
func CompilePattern(expr string) (*Pattern, error) {
	return compilePattern(expr, nil)
}

// CompilePatternWithNS compiles an XSLT 1.0 pattern expression bound to the
// given prefix/namespace-URI map.
func CompilePatternWithNS(expr string, namespaces map[string]string) (*Pattern, error) {
	return compilePattern(expr, namespaces)
}

func compilePattern(expr string, namespaces map[string]string) (pat *Pattern, err error) {
	if expr == "" {
		return nil, errors.New("pattern expression is nil")
	}
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

	tree := parse(expr, namespaces)
	paths, err := collectPatternPaths(tree)
	if err != nil {
		return nil, err
	}
	return &Pattern{s: expr, paths: paths}, nil
}

// patternStep is one node test in a pattern with its predicates.
type patternStep struct {
	// axis holds the node test (AxisType, typeTest, LocalName/Prefix and
	// namespace information) reused via axisPredicate.
	axis *axisNode
	// descendant marks a '//' separator immediately before this step.
	descendant bool
	// attribute marks an attribute axis step.
	attribute bool
	preds     []*compiledPredicate
}

// pathPattern is one union alternative, in document order.
type pathPattern struct {
	absolute bool
	rootOnly bool
	steps    []*patternStep
}

// collectPatternPaths flattens a union expression and compiles each branch.
func collectPatternPaths(tree node) ([]*pathPattern, error) {
	var roots []node
	flattenUnion(tree, &roots)
	paths := make([]*pathPattern, 0, len(roots))
	for _, r := range roots {
		path, err := compilePathPattern(r)
		if err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func flattenUnion(n node, out *[]node) {
	if op, ok := n.(*operatorNode); ok && op.Op == "|" {
		flattenUnion(op.Left, out)
		flattenUnion(op.Right, out)
		return
	}
	*out = append(*out, n)
}

// compilePathPattern converts a single (non-union) location-path AST into a
// validated pathPattern.
func compilePathPattern(n node) (*pathPattern, error) {
	path := &pathPattern{}

	if r, ok := n.(*rootNode); ok {
		switch r.slash {
		case "/":
			// A bare "/" matches the document node. rootNode is terminal in
			// the AST (it carries no input).
			path.absolute = true
			path.rootOnly = true
			return path, nil
		default:
			return nil, fmt.Errorf("unsupported pattern: %q", r.slash)
		}
	}

	steps, anchored, err := compilePathSteps(n)
	if err != nil {
		return nil, err
	}
	path.absolute = anchored
	path.steps = steps

	// An attribute step can only be the final step of a pattern.
	for i, s := range path.steps {
		if s.attribute && i != len(path.steps)-1 {
			return nil, errors.New("unsupported pattern: an attribute step can only end a pattern")
		}
	}
	return path, nil
}

// compilePathSteps converts a right-nested location-path AST into document
// order steps. It returns the steps and whether the path is anchored to the
// document root (leading "/" rather than "//" or a relative start).
func compilePathSteps(n node) ([]*patternStep, bool, error) {
	// Strip predicate wrappers around the final step, collecting predicates
	// from innermost (nearest the step) to outermost.
	var preds []node
	for {
		f, ok := n.(*filterNode)
		if !ok {
			break
		}
		preds = append(preds, f.Condition)
		n = f.Input
	}

	axis, ok := n.(*axisNode)
	if !ok {
		return nil, false, patternEntryError(n)
	}
	step, err := compilePatternStep(axis)
	if err != nil {
		return nil, false, err
	}

	var (
		prefix   []*patternStep
		anchored bool
	)
	switch input := axis.Input.(type) {
	case nil:
		// A relative path (or the first step reached after recursion).
	case *rootNode:
		switch input.slash {
		case "/":
			anchored = true
		default:
			return nil, false, fmt.Errorf("unsupported pattern: %q", input.slash)
		}
	case *axisNode:
		switch input.AxisType {
		case "descendant-or-self":
			step.descendant = true
			if _, isRoot := input.Input.(*rootNode); isRoot {
				// Leading "//": document scoped with no root-parent constraint.
				anchored = false
			} else {
				p, a, err := compilePathSteps(input.Input)
				if err != nil {
					return nil, false, err
				}
				prefix = p
				anchored = a
			}
		case "child", "attribute":
			p, a, err := compilePathSteps(input)
			if err != nil {
				return nil, false, err
			}
			prefix = p
			anchored = a
		default:
			return nil, false, fmt.Errorf("unsupported pattern axis: %s", input.AxisType)
		}
	default:
		return nil, false, patternEntryError(axis.Input)
	}

	// Predicates were collected innermost-first; reverse to source order.
	for i, j := 0, len(preds)-1; i < j; i, j = i+1, j-1 {
		preds[i], preds[j] = preds[j], preds[i]
	}
	for _, cond := range preds {
		if err := attachPredicate(step, cond); err != nil {
			return nil, false, err
		}
	}

	return append(prefix, step), anchored, nil
}

func patternEntryError(n node) error {
	switch n.(type) {
	case *functionNode:
		return errors.New("unsupported pattern: id()/key() and other function entry forms are not allowed in an XSLT pattern")
	case *variableNode:
		return errors.New("unsupported pattern: variable references are not allowed in an XSLT pattern")
	case *operandNode:
		return errors.New("unsupported pattern: literal expressions are not allowed in an XSLT pattern")
	case *groupNode:
		return errors.New("unsupported pattern: parenthesized expressions are not allowed in an XSLT pattern")
	default:
		return errors.New("unsupported pattern: only location paths are allowed")
	}
}

func compilePatternStep(a *axisNode) (*patternStep, error) {
	switch a.AxisType {
	case "child", "attribute":
	default:
		return nil, fmt.Errorf("unsupported pattern axis: %s", a.AxisType)
	}
	switch a.typeTest {
	case ElementNode, AttributeNode:
	default:
		return nil, errors.New("unsupported pattern node test: only name tests and '*' are supported")
	}
	if a.Prop != "" {
		return nil, fmt.Errorf("unsupported pattern node test: %s()", a.Prop)
	}
	return &patternStep{axis: a, attribute: a.AxisType == "attribute"}, nil
}

// attachPredicate validates a predicate expression and compiles it.
func attachPredicate(step *patternStep, cond node) error {
	if err := checkPatternPredicate(cond); err != nil {
		return err
	}
	pred, err := compilePredicate(cond)
	if err != nil {
		return err
	}
	step.preds = append(step.preds, pred)
	return nil
}

// checkPatternPredicate rejects entry-form functions that the first version
// does not support anywhere in a pattern.
func checkPatternPredicate(n node) error {
	switch v := n.(type) {
	case *functionNode:
		switch v.FuncName {
		case "id", "key", "document":
			return fmt.Errorf("unsupported pattern: %s() is not allowed in an XSLT pattern", v.FuncName)
		}
		for _, arg := range v.Args {
			if err := checkPatternPredicate(arg); err != nil {
				return err
			}
		}
	case *operatorNode:
		if err := checkPatternPredicate(v.Left); err != nil {
			return err
		}
		if err := checkPatternPredicate(v.Right); err != nil {
			return err
		}
	case *filterNode:
		if err := checkPatternPredicate(v.Input); err != nil {
			return err
		}
		if err := checkPatternPredicate(v.Condition); err != nil {
			return err
		}
	case *groupNode:
		return checkPatternPredicate(v.Input)
	case *axisNode:
		if v.Input != nil {
			if err := checkPatternPredicate(v.Input); err != nil {
				return err
			}
		}
	}
	return nil
}
