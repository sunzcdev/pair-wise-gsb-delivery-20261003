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
	vars   *scope
}

// scope implements variableScope so variableQuery can reach the per
// evaluation bindings through the iterator.
func (t *NodeIterator) scope() *scope {
	return t.vars
}

// Current returns current node which matched.
func (t *NodeIterator) Current() NodeNavigator {
	return t.node
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
	s         string
	q         query
	variables []string
}

type iteratorFunc func() NodeNavigator

func (f iteratorFunc) Current() NodeNavigator {
	return f()
}

// Evaluate returns the result of the expression.
// The result type of the expression is one of the follow: bool,float64,string,NodeIterator).
func (expr *Expr) Evaluate(root NodeNavigator) interface{} {
	result, err := expr.evaluate(root, nil)
	if err != nil {
		// Evaluate keeps its legacy, error-free contract. An expression
		// compiled without variable declarations can never fail to bind.
		panic(err)
	}
	return result
}

// Select selects a node set using the specified XPath expression.
func (expr *Expr) Select(root NodeNavigator) *NodeIterator {
	iter, err := expr.select_(root, nil)
	if err != nil {
		panic(err)
	}
	return iter
}

// EvaluateWithVariables evaluates the expression using the supplied variable
// bindings. It returns an error (rather than panicking) when a declared
// variable is missing or a binding has an unsupported type. Each call uses an
// independent binding scope and fresh query state, so one compiled Expr can be
// evaluated concurrently with different Variables.
func (expr *Expr) EvaluateWithVariables(root NodeNavigator, vars VariableResolver) (interface{}, error) {
	return expr.evaluate(root, vars)
}

// SelectWithVariables selects a node set using the supplied variable bindings.
// Bindings are validated up front; a missing or ill-typed binding is returned
// as an error. The returned iterator is independent of every other call.
func (expr *Expr) SelectWithVariables(root NodeNavigator, vars VariableResolver) (*NodeIterator, error) {
	return expr.select_(root, vars)
}

func (expr *Expr) evaluate(root NodeNavigator, resolver VariableResolver) (interface{}, error) {
	bindings, err := expr.validateBindings(resolver)
	if err != nil {
		return nil, err
	}
	qy := expr.q.Clone()
	t := &NodeIterator{query: qy, node: root, vars: bindings}
	val := qy.Evaluate(t)
	if _, ok := val.(query); ok {
		// Node-set result: reuse the same (already cloned, scoped) query so
		// the iterator keeps access to the variables while selecting.
		t.scalar = false
		return t, nil
	}
	return val, nil
}

func (expr *Expr) select_(root NodeNavigator, resolver VariableResolver) (*NodeIterator, error) {
	bindings, err := expr.validateBindings(resolver)
	if err != nil {
		return nil, err
	}
	return &NodeIterator{
		query:  expr.q.Clone(),
		node:   root,
		scalar: expr.q.ValueType() == xpathResultType.Boolean,
		vars:   bindings,
	}, nil
}

// String returns XPath expression string.
func (expr *Expr) String() string {
	return expr.s
}

// Compile compiles an XPath expression string.
//
// Without options the behavior is unchanged: any variable reference ($x)
// makes compilation fail because no variables are declared. Use
// WithVariableDeclarations (together with WithNamespaces for prefixed
// variable names) to declare variables, then bind values per evaluation with
// Expr.EvaluateWithVariables or Expr.SelectWithVariables.
func Compile(expr string, opts ...CompileOption) (*Expr, error) {
	if expr == "" {
		return nil, errors.New("expr expression is nil")
	}
	cfg := &compileConfig{}
	for _, opt := range opts {
		opt(cfg)
	}
	qy, used, err := build(expr, cfg)
	if err != nil {
		return nil, err
	}
	if qy == nil {
		return nil, fmt.Errorf("undeclared variable in XPath expression: %s", expr)
	}
	return &Expr{s: expr, q: qy, variables: used}, nil
}

// MustCompile compiles an XPath expression string and ignored error.
func MustCompile(expr string, opts ...CompileOption) *Expr {
	exp, err := Compile(expr, opts...)
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
	cfg := &compileConfig{namespaces: namespaces}
	qy, used, err := build(expr, cfg)
	if err != nil {
		return nil, err
	}
	if qy == nil {
		return nil, fmt.Errorf("undeclared variable in XPath expression: %s", expr)
	}
	return &Expr{s: expr, q: qy, variables: used}, nil
}
