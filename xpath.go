package xpath

import (
	"context"
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
	ctx    context.Context
}

// Current returns current node which matched.
func (t *NodeIterator) Current() NodeNavigator {
	return t.node
}

// callerContext returns the caller-provided context, if any.
func (t *NodeIterator) callerContext() context.Context {
	return t.ctx
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
	s string
	q query
}

type iteratorFunc func() NodeNavigator

func (f iteratorFunc) Current() NodeNavigator {
	return f()
}

// Evaluate returns the result of the expression.
// The result type of the expression is one of the follow: bool,float64,string,NodeIterator).
// An extension function that reports an error panics with a *FunctionError;
// use EvaluateWithContext to receive it as a returned error instead.
func (expr *Expr) Evaluate(root NodeNavigator) interface{} {
	val := expr.q.Evaluate(iteratorFunc(func() NodeNavigator { return root }))
	switch val.(type) {
	case query:
		return &NodeIterator{query: expr.q.Clone(), node: root}
	}
	return val
}

// EvaluateWithContext is like Evaluate, but carries ctx to extension
// functions (see FunctionContext) and returns an extension function failure
// as a *FunctionError error instead of panicking. The compiled expression
// remains safe to reuse after a returned error.
func (expr *Expr) EvaluateWithContext(ctx context.Context, root NodeNavigator) (val interface{}, err error) {
	defer func() {
		if r := recover(); r != nil {
			if fe, ok := r.(*FunctionError); ok {
				val, err = nil, fe
				return
			}
			panic(r)
		}
	}()
	it := &contextIterator{iterator: iteratorFunc(func() NodeNavigator { return root }), ctx: ctx}
	val = expr.q.Evaluate(it)
	switch val.(type) {
	case query:
		return &NodeIterator{query: expr.q.Clone(), node: root, ctx: ctx}, nil
	}
	return val, nil
}

// Select selects a node set using the specified XPath expression.
func (expr *Expr) Select(root NodeNavigator) *NodeIterator {
	return &NodeIterator{query: expr.q.Clone(), node: root, scalar: expr.q.ValueType() == xpathResultType.Boolean}
}

// SelectWithContext is like Select, but carries ctx to extension functions
// (see FunctionContext). An extension function failure during iteration
// panics with a *FunctionError, because NodeIterator.MoveNext cannot return
// an error.
func (expr *Expr) SelectWithContext(ctx context.Context, root NodeNavigator) *NodeIterator {
	return &NodeIterator{query: expr.q.Clone(), node: root, scalar: expr.q.ValueType() == xpathResultType.Boolean, ctx: ctx}
}

// String returns XPath expression string.
func (expr *Expr) String() string {
	return expr.s
}

// Compile compiles an XPath expression string.
func Compile(expr string) (*Expr, error) {
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
	return compile(expr, namespaces, nil)
}

// CompileWithFunctions compiles an XPath expression string, resolving
// namespace prefixes with the namespaces map and extension function calls
// with the funcs registry. The registry is bound to the compiled expression
// only, so each expression instance can use its own set of functions.
func CompileWithFunctions(expr string, namespaces map[string]string, funcs *FunctionRegistry) (*Expr, error) {
	return compile(expr, namespaces, funcs)
}

func compile(expr string, namespaces map[string]string, funcs *FunctionRegistry) (*Expr, error) {
	if expr == "" {
		return nil, errors.New("expr expression is nil")
	}
	qy, err := build(expr, namespaces, funcs)
	if err != nil {
		return nil, err
	}
	if qy == nil {
		return nil, fmt.Errorf(fmt.Sprintf("undeclared variable in XPath expression: %s", expr))
	}
	return &Expr{s: expr, q: qy}, nil
}
