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

	// NamespaceNode is an in-scope namespace binding of an element, such as
	// the nodes exposed by the XPath namespace:: axis. A namespace node's
	// LocalName is the bound prefix ("" for the default namespace) and its
	// Value is the namespace URI.
	NamespaceNode

	// allNode is any types of node, used by xpath package only to predicate match.
	allNode
)

// ErrNamespaceAxisUnsupported is the error raised when an expression queries
// the namespace:: axis against a NodeNavigator adapter that does not
// implement the optional NamespaceNavigator capability. The namespace axis
// needs the in-scope namespace bindings of an element, which a legacy
// adapter cannot provide; evaluating it would silently yield a wrong (empty)
// result, so the query panics with this sentinel instead. It follows the
// same fail-fast convention the package uses for other run-time errors.
var ErrNamespaceAxisUnsupported = errors.New("xpath: namespace axis is not supported by this NodeNavigator (adapter does not implement NamespaceNavigator)")

// NamespaceNavigator is an optional capability of NodeNavigator for the
// XPath namespace:: axis. It is a backward-compatible extension: existing
// NodeNavigator implementations keep working unchanged, and querying the
// namespace axis on such an adapter surfaces ErrNamespaceAxisUnsupported
// instead of returning an empty node-set.
//
// MoveToNextNamespace enumerates the namespace nodes in scope for the
// current element. The first call moves to the first in-scope namespace
// binding; returning false means there are no more. An implementation must:
//
//   - return every namespace binding in scope for the element, including
//     those inherited from ancestor elements (prefix shadowing resolves to
//     the nearest declaration),
//   - omit the default namespace when it is undeclared with xmlns="",
//   - always include the implicit binding xml ->
//     http://www.w3.org/XML/1998/namespace,
//   - yield each in-scope prefix at most once.
//
// While positioned on a namespace node, NodeType reports NamespaceNode,
// LocalName the bound prefix ("" for the default namespace), Prefix an
// empty string and Value the namespace URI. MoveToParent moves back to the
// queried element. Copy and MoveTo preserve the namespace-node position so
// that the node acts as a complete, stable query result.
type NamespaceNavigator interface {
	NodeNavigator

	// MoveToNextNamespace moves to the next namespace node in scope for the
	// current element. It returns false when there are no more namespace
	// nodes.
	MoveToNextNamespace() bool
}

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
	s string
	q query
}

type iteratorFunc func() NodeNavigator

func (f iteratorFunc) Current() NodeNavigator {
	return f()
}

// Evaluate returns the result of the expression.
// The result type of the expression is one of the follow: bool,float64,string,NodeIterator).
func (expr *Expr) Evaluate(root NodeNavigator) interface{} {
	val := expr.q.Evaluate(iteratorFunc(func() NodeNavigator { return root }))
	switch val.(type) {
	case query:
		return &NodeIterator{query: expr.q.Clone(), node: root}
	}
	return val
}

// Select selects a node set using the specified XPath expression.
func (expr *Expr) Select(root NodeNavigator) *NodeIterator {
	return &NodeIterator{query: expr.q.Clone(), node: root, scalar: expr.q.ValueType() == xpathResultType.Boolean}
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
	qy, err := build(expr, nil)
	if err != nil {
		return nil, err
	}
	if qy == nil {
		return nil, fmt.Errorf(fmt.Sprintf("undeclared variable in XPath expression: %s", expr))
	}
	return &Expr{s: expr, q: qy}, nil
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
	qy, err := build(expr, namespaces)
	if err != nil {
		return nil, err
	}
	if qy == nil {
		return nil, fmt.Errorf(fmt.Sprintf("undeclared variable in XPath expression: %s", expr))
	}
	return &Expr{s: expr, q: qy}, nil
}
