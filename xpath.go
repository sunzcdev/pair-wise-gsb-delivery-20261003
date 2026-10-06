package xpath

import (
	"errors"
	"fmt"
)

// NodeType represents a type of XPath node.
type NodeType int

const (
	// RootNode is a root node of the XML document or node tree.
	RootNode NodeType = iota

	// ElementNode is an element, such as <element>.
	ElementNode

	// AttributeNode is an attribute, such as id='123'.
	AttributeNode

	// TextNode is the text content of a node.
	TextNode

	// CommentNode is a comment node, such as <!-- my comment -->
	CommentNode

	// allNode is any types of node, used by xpath package only to predicate match.
	allNode
)

// NodeNavigator provides cursor model for navigating XML data.
type NodeNavigator interface {
	// NodeType returns the XPathNodeType of the current node.
	NodeType() NodeType

	// LocalName gets the Name of the current node.
	LocalName() string

	// Prefix returns namespace prefix associated with the current node.
	Prefix() string

	// Value gets the value of current node.
	Value() string

	// Copy does a deep copy of the NodeNavigator and all its components.
	Copy() NodeNavigator

	// MoveToRoot moves the NodeNavigator to the root node of the current node.
	MoveToRoot()

	// MoveToParent moves the NodeNavigator to the parent node of the current node.
	MoveToParent() bool

	// MoveToNextAttribute moves the NodeNavigator to the next attribute on current node.
	MoveToNextAttribute() bool

	// MoveToChild moves the NodeNavigator to the first child node of the current node.
	MoveToChild() bool

	// MoveToFirst moves the NodeNavigator to the first sibling node of the current node.
	MoveToFirst() bool

	// MoveToNext moves the NodeNavigator to the next sibling node of the current node.
	MoveToNext() bool

	// MoveToPrevious moves the NodeNavigator to the previous sibling node of the current node.
	MoveToPrevious() bool

	// MoveTo moves the NodeNavigator to the same position as the specified NodeNavigator.
	MoveTo(NodeNavigator) bool
}

// NodeIterator holds all matched Node object.
type NodeIterator struct {
	node   NodeNavigator
	query  query
	scalar bool

	// bindings holds the per-evaluation variable values. It is nil for the
	// legacy Select entry point (no variables can be declared there).
	bindings Bindings

	// cursors holds the per-evaluation node-set iteration cursors, keyed by
	// the variable's shared cluster id. Each NodeIterator owns its own map, so
	// consuming one selection never hands an exhausted cursor to another call.
	cursors map[int]*int
}

// Current returns current node which matched.
func (t *NodeIterator) Current() NodeNavigator {
	return t.node
}

// variableValue returns the value bound to name for this evaluation. It
// implements variableScope so queries nested under a Select can resolve
// variables from the iterator that threads through the query tree.
func (t *NodeIterator) variableValue(name VariableName) (interface{}, bool) {
	if t.bindings == nil {
		return nil, false
	}
	v, ok := t.bindings[name]
	return v, ok
}

// variableCursor returns the shared node-set iteration cursor for a variable
// cluster within this selection.
func (t *NodeIterator) variableCursor(cluster int) *int {
	if t.cursors == nil {
		t.cursors = make(map[int]*int)
	}
	cur, ok := t.cursors[cluster]
	if !ok {
		start := -1
		cur = &start
		t.cursors[cluster] = cur
	}
	return cur
}

// MoveNext moves Navigator to the next match node.
func (t *NodeIterator) MoveNext() bool {
	if t.scalar {
		return false
	}
	n := t.query.Select(t)
	if n == nil {
		return false
	}
	if !t.node.MoveTo(n) {
		t.node = n.Copy()
	}
	return true
}

// Select selects a node set using the specified XPath expression.
// This method is deprecated, recommend using Expr.Select() method instead.
func Select(root NodeNavigator, expr string) *NodeIterator {
	exp, err := Compile(expr)
	if err != nil {
		panic(err)
	}
	return exp.Select(root)
}

// Expr is an XPath expression for query.
type Expr struct {
	s    string
	q    query
	vars map[VariableName]struct{}
}

type iteratorFunc func() NodeNavigator

func (f iteratorFunc) Current() NodeNavigator {
	return f()
}

// Evaluate returns the result of the expression.
// The result type of the expression is one of the follow: bool,float64,string,NodeIterator).
func (expr *Expr) Evaluate(root NodeNavigator) interface{} {
	return expr.EvaluateWith(root, nil)
}

// EvaluateWith returns the result of the expression evaluated against root with
// the supplied variable bindings. The bindings are used only for this call:
// they are never stored on the compiled expression, so the same *Expr can be
// evaluated concurrently with different bindings.
//
// Acceptable binding values are bool, float64, string, a single NodeNavigator
// or an ordered []NodeNavigator. A declared variable that is missing from
// bindings binds to an empty node-set (XPath 1.0 has no "no value" state); a
// reference to an undeclared variable is a compile error instead.
func (expr *Expr) EvaluateWith(root NodeNavigator, bindings Bindings) interface{} {
	ctx := newEvalContext(root, expr.normalizeBindings(bindings))
	val := expr.q.Evaluate(ctx)
	switch val.(type) {
	case query:
		return &NodeIterator{query: expr.q.Clone(), node: root, bindings: ctx.bindings, cursors: make(map[int]*int)}
	}
	return val
}

// Select selects a node set using the specified XPath expression.
func (expr *Expr) Select(root NodeNavigator) *NodeIterator {
	return &NodeIterator{query: expr.q.Clone(), node: root, scalar: expr.q.ValueType() == xpathResultType.Boolean}
}

// SelectWith selects a node set using the specified XPath expression and
// variable bindings. As with EvaluateWith, the bindings apply only to this
// selection and do not mutate the compiled expression.
func (expr *Expr) SelectWith(root NodeNavigator, bindings Bindings) *NodeIterator {
	return &NodeIterator{
		query:    expr.q.Clone(),
		node:     root,
		scalar:   expr.q.ValueType() == xpathResultType.Boolean,
		bindings: expr.normalizeBindings(bindings),
		cursors:  make(map[int]*int),
	}
}

// normalizeBindings validates and copies the supplied bindings. Node-sets are
// copied into independent ordered slices so consuming one evaluation can never
// exhaust or mutate the caller's slice or an iterator. The returned map is
// safe to share across the cloned queries spawned by a single Evaluate/Select.
func (expr *Expr) normalizeBindings(bindings Bindings) map[VariableName]interface{} {
	if len(bindings) == 0 {
		return nil
	}
	normalized := make(map[VariableName]interface{}, len(bindings))
	for name, value := range bindings {
		v, err := normalizeVariableValue(value)
		if err != nil {
			panic(err)
		}
		normalized[name] = v
	}
	return normalized
}

// DeclareVariable marks name as a variable the expression may reference.
// Declarations only name a variable; values are supplied per evaluation via
// EvaluateWith or SelectWith. Use CompileWith to compile with declarations in
// one step; this mutator exists for callers that build an *Expr incrementally.
func (expr *Expr) DeclareVariable(name VariableName) {
	if expr.vars == nil {
		expr.vars = make(map[VariableName]struct{})
	}
	expr.vars[name] = struct{}{}
}

// String returns XPath expression string.
func (expr *Expr) String() string {
	return expr.s
}

// Compile compiles an XPath expression string.
func Compile(expr string) (*Expr, error) {
	if expr == "" {
		return nil, errors.New("expr expression is nil")
	}
	return compile(expr, nil, nil)
}

// MustCompile compiles an XPath expression string and ignored error.
func MustCompile(expr string) *Expr {
	exp, err := Compile(expr)
	if err != nil {
		return &Expr{s: expr, q: nopQuery{}}
	}
	return exp
}

// CompileWithNS compiles an XPath expression string, using given namespaces map.
func CompileWithNS(expr string, namespaces map[string]string) (*Expr, error) {
	if expr == "" {
		return nil, errors.New("expr expression is nil")
	}
	return compile(expr, namespaces, nil)
}

// CompileWith compiles an XPath expression that may reference the declared
// variables. namespaces resolves QName prefixes used both in the expression
// and in variable names; every declared variable's namespace prefix must be
// present there. An expression that references a variable not present in
// variables fails to compile.
//
// The compiled *Expr holds only the variable declarations, never their
// values; supply a binding map per evaluation via EvaluateWith or SelectWith.
func CompileWith(expr string, namespaces map[string]string, variables ...VariableName) (*Expr, error) {
	if expr == "" {
		return nil, errors.New("expr expression is nil")
	}
	declarations := make(map[VariableName]struct{}, len(variables))
	for _, v := range variables {
		declarations[v] = struct{}{}
	}
	return compile(expr, namespaces, declarations)
}

func compile(expr string, namespaces map[string]string, declarations map[VariableName]struct{}) (*Expr, error) {
	qy, err := build(expr, namespaces, declarations)
	if err != nil {
		return nil, err
	}
	if qy == nil {
		return nil, fmt.Errorf("undeclared variable in XPath expression: %s", expr)
	}
	return &Expr{s: expr, q: qy, vars: declarations}, nil
}
