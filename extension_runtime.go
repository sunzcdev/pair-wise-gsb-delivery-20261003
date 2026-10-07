package xpath

import "fmt"

// extensionFuncQuery evaluates a registered extension function. It is
// stateless apart from its compiled arguments: Evaluate can be called
// repeatedly (and concurrently on clones), so an error on one invocation
// never corrupts the compiled expression for reuse.
type extensionFuncQuery struct {
	spec  *FunctionSpec
	args  []query
	input query // enclosing node-set axis, used for Position/Last; nil at top level
}

func (f *extensionFuncQuery) Select(iterator) NodeNavigator { return nil }

func (f *extensionFuncQuery) Evaluate(t iterator) interface{} {
	evaluated := make([]interface{}, len(f.args))
	for i, arg := range f.args {
		value := functionArgs(arg).Evaluate(t)
		evaluated[i] = f.convertArg(t, i, value)
	}

	// Snapshot the context node before invoking the function: user code is
	// free to navigate the navigator it receives.
	nodeSetInput := f.input
	if nodeSetInput != nil {
		nodeSetInput = nodeSetInput.Clone()
	}
	ctx := ExtensionContext{
		ContextNode:   t.Current().Copy(),
		Position:      contextPosition(nodeSetInput, t),
		Last:          contextLast(nodeSetInput, t),
		CallerContext: callerContextOf(t),
	}

	result, err := f.spec.Func(ctx, evaluated)
	if err != nil {
		panic(&ExtensionError{
			NamespaceURI: f.spec.NamespaceURI,
			LocalName:    f.spec.LocalName,
			Err:          err,
		})
	}
	return f.coerceResult(t, result)
}

func (f *extensionFuncQuery) Clone() query {
	args := make([]query, len(f.args))
	for i, arg := range f.args {
		args[i] = arg.Clone()
	}
	var input query
	if f.input != nil {
		input = f.input.Clone()
	}
	return &extensionFuncQuery{spec: f.spec, args: args, input: input}
}

func (f *extensionFuncQuery) ValueType() resultType {
	return f.spec.Return.resultType()
}

func (f *extensionFuncQuery) Properties() queryProp {
	return queryProps.Merge
}

// functionStepQuery drives an extension function that appears as a step in a
// location path (e.g. "item/biz:lookup(.)"). For every node produced by the
// preceding step it sets the context, evaluates the function, and yields the
// resulting nodes. Scalar-returning functions are invalid as path steps; the
// runtime enforces this and reports it rather than silently dropping values.
type functionStepQuery struct {
	Input    query
	Function *extensionFuncQuery

	iterator func() NodeNavigator
}

func (f *functionStepQuery) Select(t iterator) NodeNavigator {
	if f.iterator == nil {
		f.iterator = f.iterate(t)
	}
	return f.iterator()
}

// iterate binds a fresh context node per input node, evaluates the function
// against it and streams the returned node-set. A fresh function clone per
// input node keeps internal iterator state isolated, mirroring how the
// engine evaluates predicates.
func (f *functionStepQuery) iterate(t iterator) func() NodeNavigator {
	var (
		pending  []NodeNavigator
		pendingI int
		done     bool
		root     = t.Current().Copy()
	)
	return func() NodeNavigator {
		for pendingI >= len(pending) {
			if done {
				return nil
			}
			node := f.Input.Select(t)
			if node == nil {
				done = true
				t.Current().MoveTo(root)
				return nil
			}
			node = node.Copy()
			t.Current().MoveTo(node)
			value := f.Function.Clone().Evaluate(t)
			ns, ok := value.(query)
			if !ok {
				panic(&ExtensionError{
					NamespaceURI: f.Function.spec.NamespaceURI,
					LocalName:    f.Function.spec.LocalName,
					Err: fmt.Errorf("function used as a path step must return a node-set, got %T",
						value),
				})
			}
			pending = pending[:0]
			pendingI = 0
			for n := ns.Select(t); n != nil; n = ns.Select(t) {
				pending = append(pending, n.Copy())
			}
		}
		node := pending[pendingI]
		pendingI++
		return node
	}
}

func (f *functionStepQuery) Evaluate(t iterator) interface{} {
	f.Input.Evaluate(t)
	f.iterator = nil
	return f
}

func (f *functionStepQuery) Clone() query {
	return &functionStepQuery{
		Input:    f.Input.Clone(),
		Function: f.Function.Clone().(*extensionFuncQuery),
	}
}

func (f *functionStepQuery) ValueType() resultType {
	return xpathResultType.NodeSet
}

func (f *functionStepQuery) Properties() queryProp {
	return queryProps.Merge
}

// fail aborts evaluation with an ExtensionError so that type and conversion
// problems follow the same propagation path as implementation errors.
func (f *extensionFuncQuery) fail(format string, args ...interface{}) {
	panic(&ExtensionError{
		NamespaceURI: f.spec.NamespaceURI,
		LocalName:    f.spec.LocalName,
		Err:          fmt.Errorf(format, args...),
	})
}

// convertArg applies the XPath 1.0 argument conversion rules to one
// evaluated argument: boolean(), number(), string() for the scalar
// declarations, node-set identity for node-set parameters.
func (f *extensionFuncQuery) convertArg(t iterator, index int, value interface{}) interface{} {
	switch f.spec.Params[index] {
	case ExtensionBoolean:
		return asBool(t, value)
	case ExtensionNumber:
		return asNumber(t, value)
	case ExtensionString:
		return asString(t, value)
	case ExtensionNodeSet:
		q, ok := value.(query)
		if !ok {
			f.fail("parameter %d expects node-set, got %T", index+1, value)
		}
		return materializeNodeSet(t, q)
	default: // ExtensionAny: keep the dynamic XPath value as its Go form.
		if q, ok := value.(query); ok {
			return materializeNodeSet(t, q)
		}
		return value
	}
}

// coerceResult converts the implementation result to the declared return
// type using the XPath 1.0 conversion rules. Node-set results become a query
// that yields copies of the returned navigators.
func (f *extensionFuncQuery) coerceResult(t iterator, value interface{}) interface{} {
	if single, ok := value.(NodeNavigator); ok {
		value = []NodeNavigator{single}
	}
	nodes, isNodeSet := value.([]NodeNavigator)

	switch f.spec.Return {
	case ExtensionBoolean:
		switch v := value.(type) {
		case nil:
			return false
		case bool:
			return v
		case float64:
			return v != 0
		case string:
			return v != ""
		default:
			if isNodeSet {
				return len(nodes) > 0
			}
			f.fail("cannot convert %T to boolean result", value)
		}
	case ExtensionNumber:
		switch v := value.(type) {
		case bool:
			if v {
				return float64(1)
			}
			return float64(0)
		case float64:
			return v
		case string:
			return stringToNumber(v)
		case nil:
			return stringToNumber("")
		default:
			if isNodeSet {
				if len(nodes) > 0 {
					return stringToNumber(nodes[0].Value())
				}
				return stringToNumber("")
			}
			f.fail("cannot convert %T to number result", value)
		}
	case ExtensionString:
		if isNodeSet {
			if len(nodes) > 0 {
				value = nodes[0].Value()
			} else {
				value = ""
			}
		}
		if value == nil {
			return ""
		}
		return asString(t, value)
	case ExtensionNodeSet:
		if !isNodeSet {
			f.fail("function must return []NodeNavigator for node-set result, got %T", value)
		}
		return newExtensionNodeSet(nodes)
	default: // ExtensionAny: map by dynamic Go type.
		switch value.(type) {
		case nil, bool, float64, string:
			return value
		default:
			if isNodeSet {
				return newExtensionNodeSet(nodes)
			}
			f.fail("unsupported result type %T, want bool, float64, string or []NodeNavigator", value)
		}
	}
	panic("unreachable")
}

// materializeNodeSet evaluates a node-set argument query into a slice of
// navigator copies owned by the invocation.
func materializeNodeSet(t iterator, q query) []NodeNavigator {
	// Mirror the built-in node-set functions: clone the argument, evaluate
	// it against the current context (this resets axis iterator state) and
	// then walk the query returned by Evaluate.
	q = functionArgs(q)
	evaluated := q.Evaluate(t)
	qq, ok := evaluated.(query)
	if !ok {
		// Should be unreachable for a node-set typed value.
		return []NodeNavigator{}
	}
	nodes := make([]NodeNavigator, 0)
	for node := qq.Select(t); node != nil; node = qq.Select(t) {
		nodes = append(nodes, node.Copy())
	}
	return nodes
}

// extensionNodeSetQuery exposes node navigators returned by an extension
// function through the internal query interface.
type extensionNodeSetQuery struct {
	nodes []NodeNavigator
	pos   int
}

func newExtensionNodeSet(nodes []NodeNavigator) query {
	copies := make([]NodeNavigator, len(nodes))
	for i, node := range nodes {
		copies[i] = node.Copy()
	}
	return &extensionNodeSetQuery{nodes: copies}
}

func (q *extensionNodeSetQuery) Select(iterator) NodeNavigator {
	if q.pos >= len(q.nodes) {
		return nil
	}
	node := q.nodes[q.pos]
	q.pos++
	return node
}

func (q *extensionNodeSetQuery) Evaluate(iterator) interface{} {
	q.pos = 0
	return q
}

func (q *extensionNodeSetQuery) Clone() query {
	return &extensionNodeSetQuery{nodes: q.nodes}
}

func (q *extensionNodeSetQuery) ValueType() resultType {
	return xpathResultType.NodeSet
}

func (q *extensionNodeSetQuery) Properties() queryProp {
	return queryProps.Merge
}

// extensionRootNodeSetQuery is the root of a compiled expression whose
// top-level node is a node-set-returning extension function. Select must
// work without an explicit Evaluate first (Expr.Select clones the query and
// iterates directly), so this query lazily evaluates the function on the
// first Select and then streams its nodes, resetting on each Evaluate.
type extensionRootNodeSetQuery struct {
	Func *extensionFuncQuery

	initialized bool
	ns          query
}

func (q *extensionRootNodeSetQuery) Select(t iterator) NodeNavigator {
	if !q.initialized {
		value := q.Func.Evaluate(t)
		nq, ok := value.(query)
		if !ok {
			panic(&ExtensionError{
				NamespaceURI: q.Func.spec.NamespaceURI,
				LocalName:    q.Func.spec.LocalName,
				Err:          fmt.Errorf("function must return a node-set, got %T", value),
			})
		}
		q.ns = nq
		q.initialized = true
	}
	return q.ns.Select(t)
}

func (q *extensionRootNodeSetQuery) Evaluate(t iterator) interface{} {
	q.initialized = false
	return q
}

func (q *extensionRootNodeSetQuery) Clone() query {
	return &extensionRootNodeSetQuery{Func: q.Func.Clone().(*extensionFuncQuery)}
}

func (q *extensionRootNodeSetQuery) ValueType() resultType {
	return xpathResultType.NodeSet
}

func (q *extensionRootNodeSetQuery) Properties() queryProp {
	return queryProps.Merge
}

// contextPosition computes the 1-based context position using the same logic
// as the built-in position(): previous sibling nodes matching the enclosing
// axis predicate. A nil input (top-level call) yields 1.
func contextPosition(q query, t iterator) int {
	if q == nil {
		return 1
	}
	count := 1
	node := t.Current().Copy()
	test := predicate(q)
	for node.MoveToPrevious() {
		if test(node) {
			count++
		}
	}
	return count
}

// contextLast computes the context size using the same logic as the
// built-in last(): matching siblings of the current node. A nil input or an
// empty set yields 1, matching the XPath invariant last() >= position().
func contextLast(q query, t iterator) int {
	if q == nil {
		return 1
	}
	count := 0
	node := t.Current().Copy()
	test := predicate(q)
	node.MoveToFirst()
	for {
		if test(node) {
			count++
		}
		if !node.MoveToNext() {
			break
		}
	}
	if count == 0 {
		return 1
	}
	return count
}
