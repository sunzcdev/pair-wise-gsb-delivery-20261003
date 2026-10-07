package xpath

import (
	"context"
	"fmt"
)

// DataType identifies the XPath data type of an extension function
// parameter or result.
type DataType int

const (
	// AnyType accepts any XPath value. Node-set arguments are materialized
	// as []NodeNavigator; AnyType results must be a scalar.
	AnyType DataType = iota
	// BooleanType is an XPath boolean, exchanged as a Go bool.
	BooleanType
	// NumberType is an XPath number, exchanged as a Go float64.
	NumberType
	// StringType is an XPath string, exchanged as a Go string.
	StringType
	// NodeSetType is an XPath node-set, exchanged as a []NodeNavigator.
	NodeSetType
)

func validDataType(t DataType) bool {
	switch t {
	case AnyType, BooleanType, NumberType, StringType, NodeSetType:
		return true
	}
	return false
}

// FunctionContext carries the dynamic evaluation context of an extension
// function call.
type FunctionContext struct {
	// Context is the caller-provided context passed to
	// Expr.EvaluateWithContext or Expr.SelectWithContext. It is
	// context.Background() when the expression is evaluated without one.
	Context context.Context

	// Node is the current context node. The navigator is owned by the
	// engine; call Copy() before retaining it beyond the function call.
	Node NodeNavigator

	// Position is the 1-based position of the context node within the
	// context node list, mirroring the position() function.
	Position int

	// Size is the number of nodes in the context node list, mirroring the
	// last() function.
	Size int
}

// ExtensionFunction is a custom XPath function identified by a namespace
// URI and a local name.
type ExtensionFunction struct {
	// NamespaceURI is the namespace the function belongs to. It may be
	// empty, in which case the function is called without a prefix.
	NamespaceURI string

	// Name is the local name of the function as used in expressions.
	Name string

	// Args declares the parameter types; its length is the function arity.
	// Arguments are converted following the XPath type conversion rules
	// (the string(), number() and boolean() functions) before the function
	// is invoked. XPath defines no conversion from a scalar to a node-set,
	// so a NodeSetType parameter requires a node-set argument.
	Args []DataType

	// Result declares the result type. Scalar results are returned as
	// bool, float64 or string; a NodeSetType result is returned as
	// []NodeNavigator.
	Result DataType

	// Func is the function implementation. The args slice holds one value
	// per declared parameter, already converted to the declared type
	// (bool, float64, string or []NodeNavigator). A returned error aborts
	// the evaluation and is surfaced to the caller wrapped in a
	// *FunctionError; it is never silently folded into a boolean false.
	//
	// Node ownership: node-set arguments are copies owned by the function.
	// Node-set results transfer ownership to the engine, which may move
	// and copy the returned navigators during evaluation; the function
	// must not retain or mutate them after returning.
	Func func(ctx *FunctionContext, args []interface{}) (interface{}, error)
}

// FunctionError reports a failed extension function call. It wraps the
// error returned by the function, preserving it for errors.Is/As.
type FunctionError struct {
	// NamespaceURI and Name identify the function that failed.
	NamespaceURI string
	Name         string
	// Err is the error returned by the function, or a signature violation
	// detected while converting arguments or the result.
	Err error
}

func (e *FunctionError) Error() string {
	if e.NamespaceURI != "" {
		return fmt.Sprintf("xpath: extension function {%s}%s: %v", e.NamespaceURI, e.Name, e.Err)
	}
	return fmt.Sprintf("xpath: extension function %s: %v", e.Name, e.Err)
}

// Unwrap returns the error returned by the extension function.
func (e *FunctionError) Unwrap() error { return e.Err }

type functionKey struct {
	uri  string
	name string
}

// FunctionRegistry is an expression-level registry of extension functions.
// Each registry is passed explicitly to CompileWithFunctions, so different
// compiled expressions (and therefore different requests) can bind
// different implementations of the same function name; nothing is shared
// through a package-level global. Populate a registry before compiling and
// do not modify it concurrently with compilation.
type FunctionRegistry struct {
	funcs map[functionKey]*ExtensionFunction
}

// NewFunctionRegistry creates an empty extension function registry.
func NewFunctionRegistry() *FunctionRegistry {
	return &FunctionRegistry{funcs: make(map[functionKey]*ExtensionFunction)}
}

// Register adds fn to the registry. It returns an error if the declaration
// is invalid or a function with the same namespace URI and local name is
// already registered.
func (r *FunctionRegistry) Register(fn ExtensionFunction) error {
	if fn.Name == "" {
		return fmt.Errorf("xpath: extension function name is empty")
	}
	if fn.Func == nil {
		return fmt.Errorf("xpath: extension function %s has no implementation", fn.Name)
	}
	if !validDataType(fn.Result) {
		return fmt.Errorf("xpath: extension function %s has an invalid result type", fn.Name)
	}
	for _, arg := range fn.Args {
		if !validDataType(arg) {
			return fmt.Errorf("xpath: extension function %s has an invalid argument type", fn.Name)
		}
	}
	key := functionKey{fn.NamespaceURI, fn.Name}
	if _, ok := r.funcs[key]; ok {
		return fmt.Errorf("xpath: extension function {%s}%s is already registered", fn.NamespaceURI, fn.Name)
	}
	r.funcs[key] = &fn
	return nil
}

func (r *FunctionRegistry) lookup(uri, name string) *ExtensionFunction {
	if r == nil {
		return nil
	}
	return r.funcs[functionKey{uri, name}]
}

// callerContexter is implemented by iterators that carry a caller context.
type callerContexter interface {
	callerContext() context.Context
}

func iteratorContext(t iterator) context.Context {
	if c, ok := t.(callerContexter); ok {
		if ctx := c.callerContext(); ctx != nil {
			return ctx
		}
	}
	return context.Background()
}

// contextIterator wraps an iterator with a caller-provided context.
type contextIterator struct {
	iterator
	ctx context.Context
}

func (c *contextIterator) callerContext() context.Context { return c.ctx }

// extensionFuncQuery is a query that evaluates a registered extension
// function.
type extensionFuncQuery struct {
	Input query // context input, used to derive position and size
	def   *ExtensionFunction
	args  []query

	nodes     []NodeNavigator // materialized node-set result
	posit     int
	evaluated bool
}

func (q *extensionFuncQuery) Select(t iterator) NodeNavigator {
	if q.def.Result != NodeSetType {
		return nil
	}
	if !q.evaluated {
		q.evaluated = true
		if v := q.call(t); v != nil {
			q.nodes = v.([]NodeNavigator)
		}
	}
	if q.posit >= len(q.nodes) {
		return nil
	}
	node := q.nodes[q.posit]
	q.posit++
	return node
}

func (q *extensionFuncQuery) Evaluate(t iterator) interface{} {
	if q.def.Result == NodeSetType {
		// Reset the iteration state; the function is re-invoked lazily on
		// the next Select, against the then-current context.
		q.nodes = nil
		q.posit = 0
		q.evaluated = false
		return q
	}
	return q.call(t)
}

func (q *extensionFuncQuery) Clone() query {
	args := make([]query, len(q.args))
	for i, arg := range q.args {
		args[i] = arg.Clone()
	}
	var input query
	if q.Input != nil {
		input = q.Input.Clone()
	}
	return &extensionFuncQuery{Input: input, def: q.def, args: args}
}

func (q *extensionFuncQuery) ValueType() resultType {
	switch q.def.Result {
	case BooleanType:
		return xpathResultType.Boolean
	case NumberType:
		return xpathResultType.Number
	case StringType:
		return xpathResultType.String
	case NodeSetType:
		return xpathResultType.NodeSet
	}
	return xpathResultType.Any
}

func (q *extensionFuncQuery) Properties() queryProp {
	return queryProps.Merge
}

// position returns the 1-based ordinal of the last returned node, so
// positional predicates such as ext:items()[2] can be resolved.
func (q *extensionFuncQuery) position() int {
	return q.posit
}

func (q *extensionFuncQuery) call(t iterator) interface{} {
	def := q.def
	args := make([]interface{}, len(q.args))
	// Snapshot the context so each argument evaluates and converts against
	// an equivalent context; evaluating a node-set argument moves the
	// current navigator.
	root := t.Current().Copy()
	for i, arg := range q.args {
		v := arg.Evaluate(t)
		t.Current().MoveTo(root)
		args[i] = q.convertArg(t, i, v)
		t.Current().MoveTo(root)
	}
	fc := &FunctionContext{
		Context: iteratorContext(t),
		Node:    t.Current().Copy(),
	}
	fc.Position, fc.Size = contextPositionSize(q.Input, t)
	ret, err := def.Func(fc, args)
	if err != nil {
		panic(&FunctionError{NamespaceURI: def.NamespaceURI, Name: def.Name, Err: err})
	}
	return q.checkResult(ret)
}

// convertArg converts an evaluated argument to the declared parameter type
// following the XPath type conversion rules.
func (q *extensionFuncQuery) convertArg(t iterator, i int, v interface{}) interface{} {
	def := q.def
	switch def.Args[i] {
	case BooleanType:
		return asBool(t, v)
	case NumberType:
		return asNumber(t, v)
	case StringType:
		return asString(t, v)
	case NodeSetType:
		nodes, ok := nodeSetArg(t, v)
		if !ok {
			panic(&FunctionError{NamespaceURI: def.NamespaceURI, Name: def.Name,
				Err: fmt.Errorf("argument %d cannot be converted to a node-set", i+1)})
		}
		return nodes
	default: // AnyType
		if nodes, ok := nodeSetArg(t, v); ok {
			return nodes
		}
		return v
	}
}

// nodeSetArg materializes a node-set value into a slice of navigators. The
// navigators are copies owned by the caller.
func nodeSetArg(t iterator, v interface{}) ([]NodeNavigator, bool) {
	q, ok := v.(query)
	if !ok {
		return nil, false
	}
	var nodes []NodeNavigator
	for node := q.Select(t); node != nil; node = q.Select(t) {
		nodes = append(nodes, node.Copy())
	}
	return nodes, true
}

// checkResult verifies the value returned by the function against the
// declared result type.
func (q *extensionFuncQuery) checkResult(v interface{}) interface{} {
	def := q.def
	var ok bool
	switch def.Result {
	case BooleanType:
		_, ok = v.(bool)
	case NumberType:
		_, ok = v.(float64)
	case StringType:
		_, ok = v.(string)
	case NodeSetType:
		if v == nil {
			return nil
		}
		_, ok = v.([]NodeNavigator)
	default: // AnyType
		switch v.(type) {
		case bool, float64, string:
			ok = true
		}
	}
	if !ok {
		panic(&FunctionError{NamespaceURI: def.NamespaceURI, Name: def.Name,
			Err: fmt.Errorf("result %v (%T) does not match the declared result type", v, v)})
	}
	return v
}

// contextPositionSize derives the position and size of the context node by
// walking its sibling axis, mirroring the position() and last() functions.
func contextPositionSize(input query, t iterator) (position, size int) {
	node := t.Current().Copy()
	test := predicate(input)
	position = 1
	for node.MoveToPrevious() {
		if test(node) {
			position++
		}
	}
	for {
		if test(node) {
			size++
		}
		if !node.MoveToNext() {
			break
		}
	}
	return position, size
}
