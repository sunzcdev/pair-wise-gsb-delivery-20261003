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
	ctx    interface{}
	err    error
}

// Current returns current node which matched.
func (t *NodeIterator) Current() NodeNavigator {
	return t.node
}

// MoveNext moves Navigator to the next match node.
func (t *NodeIterator) MoveNext() (ok bool) {
	if t.scalar {
		return false
	}
	// Extension functions return errors instead of folding them into a
	// false/empty result; recover any such error on the node-selection path
	// and expose it through Err().
	defer func() {
		if e := recover(); e != nil {
			if extErr, isExt := e.(*ExtensionError); isExt {
				t.err = extErr
				ok = false
				return
			}
			panic(e)
		}
	}()
	n := t.query.Select(t)
	if n == nil {
		return false
	}
	if !t.node.MoveTo(n) {
		t.node = n.Copy()
	}
	return true
}

// Err returns the first error raised while iterating, typically an error
// returned by an extension function. It must be checked after MoveNext
// returns false; a nil error means iteration completed normally.
func (t *NodeIterator) Err() error {
	return t.err
}

// callerContext returns the opaque caller context associated with an
// iterator, if any. NodeIterator carries it explicitly; internal value
// iterators created by Expr.Eval expose the same value.
func (t *NodeIterator) callerContext() interface{} {
	return t.ctx
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

// rootQueryFor returns the query used to iterate an expression result. A
// top-level extension function returning a node-set has to be evaluated
// lazily during iteration, so it is wrapped in a root node-set query.
func rootQueryFor(q query) query {
	if fq, ok := q.(*extensionFuncQuery); ok &&
		fq.ValueType() == xpathResultType.NodeSet {
		return &extensionRootNodeSetQuery{Func: fq}
	}
	return q
}

// valueIterator is the evaluation-time iterator. Unlike iteratorFunc it owns
// a mutable current node (matching NodeIterator semantics) and carries the
// opaque caller context handed to extension functions.
type valueIterator struct {
	current NodeNavigator
	ctx     interface{}
}

func (t *valueIterator) Current() NodeNavigator {
	return t.current
}

func (t *valueIterator) callerContext() interface{} {
	return t.ctx
}

// callerContextOf extracts the opaque caller context carried by an iterator,
// defaulting to nil for iterators that do not supply one.
func callerContextOf(t iterator) interface{} {
	type contextCarrier interface {
		callerContext() interface{}
	}
	if c, ok := t.(contextCarrier); ok {
		return c.callerContext()
	}
	return nil
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

// Eval evaluates the expression with an opaque callerContext that extension
// functions receive as ExtensionContext.CallerContext. The result is one of
// bool, float64, string or *NodeIterator.
//
// An error returned by an extension function is propagated as the error
// return value instead of being folded into false or an empty result. The
// compiled expression stays reusable after an error: each evaluation clones
// the internal query tree, so a later call with different data is not
// affected by the failed invocation. For node-set results, inspect
// NodeIterator.Err after iteration.
func (expr *Expr) Eval(root NodeNavigator, callerContext interface{}) (result interface{}, err error) {
	t := &valueIterator{current: root, ctx: callerContext}
	qe := expr.q.Clone()
	defer func() {
		if e := recover(); e != nil {
			if x, isExt := e.(*ExtensionError); isExt {
				err = x
				result = nil
				return
			}
			panic(e)
		}
	}()
	val := qe.Evaluate(t)
	if q, ok := val.(query); ok {
		q = rootQueryFor(q.Clone())
		return &NodeIterator{query: q, node: root, scalar: false, ctx: callerContext}, nil
	}
	return val, nil
}

// Select selects a node set using the specified XPath expression.
func (expr *Expr) Select(root NodeNavigator) *NodeIterator {
	q := rootQueryFor(expr.q.Clone())
	return &NodeIterator{query: q, node: root, scalar: q.ValueType() == xpathResultType.Boolean}
}

// SelectWithContext is like Select but supplies an opaque caller context to
// extension functions during iteration.
func (expr *Expr) SelectWithContext(root NodeNavigator, callerContext interface{}) *NodeIterator {
	q := rootQueryFor(expr.q.Clone())
	return &NodeIterator{
		query:  q,
		node:   root,
		scalar: q.ValueType() == xpathResultType.Boolean,
		ctx:    callerContext,
	}
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
	qy, err := build(expr, nil, nil)
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
	qy, err := build(expr, namespaces, nil)
	if err != nil {
		return nil, err
	}
	if qy == nil {
		return nil, fmt.Errorf(fmt.Sprintf("undeclared variable in XPath expression: %s", expr))
	}
	return &Expr{s: expr, q: qy}, nil
}

// CompileWithExtensions compiles an XPath expression with a private table of
// extension functions. The registry is captured per compiled expression: it
// is never stored in package-global state, so concurrent callers may compile
// the same QName with different registries and implementations.
//
// Extension functions are referenced by QName. namespaces binds document
// prefixes to namespace URIs; a call prefix:local-name() resolves to the
// function registered with that namespace URI and local name. Compile-time
// checks validate that the prefix is bound, the function is registered, the
// argument count matches the declaration and statically known argument types
// are compatible with the declared parameter types.
//
// The returned *Expr retains the registry for its lifetime. The registry
// must not be modified after compilation.
func CompileWithExtensions(expr string, namespaces map[string]string, registry *FunctionRegistry) (*Expr, error) {
	if expr == "" {
		return nil, errors.New("expr expression is nil")
	}
	qy, err := build(expr, namespaces, registry)
	if err != nil {
		return nil, err
	}
	if qy == nil {
		return nil, fmt.Errorf(fmt.Sprintf("undeclared variable in XPath expression: %s", expr))
	}
	return &Expr{s: expr, q: qy}, nil
}
