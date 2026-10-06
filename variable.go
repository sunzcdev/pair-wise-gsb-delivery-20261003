package xpath

import (
	"fmt"
	"reflect"
)

// VariableName identifies a declared XPath variable. It is the expanded name
// of a variable reference after the QName prefix has been resolved against the
// namespaces supplied at compile time: two variables that share a local name
// but live in different namespaces are distinct.
type VariableName struct {
	// Local is the local part of the variable QName.
	Local string

	// Namespace is the namespace URI the QName prefix resolved to. It is empty
	// for an unprefixed variable reference.
	Namespace string
}

// String returns the variable reference in "$prefix:local" form when a
// namespace is present, or "$local" otherwise.
func (n VariableName) String() string {
	if n.Namespace == "" {
		return "$" + n.Local
	}
	return fmt.Sprintf("${%s}%s", n.Namespace, n.Local)
}

// Var declares an XPath variable by local name.
//
//	Var("page")                       -> $page (no namespace)
//	Var("ns:x", map[string]string{"ns": "http://example.com"}) -> $ns:x
//
// A QName prefix that cannot be resolved against namespaces panics, since it
// is a mistake in the declaration itself; prefer reusing the same namespaces
// map passed to CompileWith so references and declarations agree.
func Var(qname string, namespaces ...map[string]string) VariableName {
	var nsmap map[string]string
	if len(namespaces) > 0 {
		nsmap = namespaces[0]
	}
	return resolveVariableName(qname, nsmap)
}

// Bind builds a variable binding map for a single EvaluateWith or SelectWith
// call. Pass the result of repeated Bind calls to bind several variables:
//
//	expr.EvaluateWith(root, xpath.Bind(xpath.Var("a"), 1).
//		Bind(xpath.Var("b"), "x"))
type Bindings map[VariableName]interface{}

// Bind associates name with value and returns the binding map so calls can be
// chained. The value must be a supported XPath 1.0 type: bool, float64,
// string, NodeNavigator or []NodeNavigator.
func Bind(name VariableName, value interface{}) Bindings {
	return Bindings{name: value}
}

// Bind associates name with value on the receiver map and returns it for
// chaining additional bindings.
func (b Bindings) Bind(name VariableName, value interface{}) Bindings {
	b[name] = value
	return b
}

// resolveVariableName expands a variable QName into a VariableName using the
// namespace prefix map supplied at compile time.
func resolveVariableName(qname string, namespaces map[string]string) VariableName {
	name := VariableName{Local: qname}
	for i := 0; i < len(qname); i++ {
		if qname[i] == ':' {
			prefix := qname[:i]
			local := qname[i+1:]
			uri, ok := namespaces[prefix]
			if !ok {
				panic(fmt.Sprintf("xpath: undeclared variable namespace prefix: %s", prefix))
			}
			name.Local = local
			name.Namespace = uri
			return name
		}
	}
	return name
}

// variableScope provides access to the variable bindings of a single
// evaluation. Every iterator passed through the query tree is either an
// *evalContext (for Evaluate) or a *NodeIterator (for Select), both of which
// implement this interface.
type variableScope interface {
	variableValue(VariableName) (interface{}, bool)
}

// lookupVariable resolves a declared variable against the current evaluation's
// bindings. The iterator carries no bindings when the legacy Compile/Evaluate
// entry points are used.
func lookupVariable(t iterator, name VariableName) (interface{}, bool) {
	if scope, ok := t.(variableScope); ok {
		return scope.variableValue(name)
	}
	return nil, false
}

// normalizeVariableValue validates an XPath 1.0 variable value and converts a
// node-set binding into an ordered, independently iterable slice of
// navigators. Node navigators supplied by the caller are copied so consuming
// the binding can never mutate, exhaust, or alias the caller's iterators.
//
// The supported scalar types are bool, float64 and string.
func normalizeVariableValue(v interface{}) (interface{}, error) {
	switch val := v.(type) {
	case bool, float64, string:
		return v, nil
	case nil:
		return nil, fmt.Errorf("xpath: variable value must not be nil")
	case NodeNavigator:
		return []NodeNavigator{val.Copy()}, nil
	case []NodeNavigator:
		nodes := make([]NodeNavigator, len(val))
		for i, n := range val {
			if n == nil {
				return nil, fmt.Errorf("xpath: node-set variable contains a nil node")
			}
			nodes[i] = n.Copy()
		}
		return nodes, nil
	case *NodeIterator:
		var nodes []NodeNavigator
		for val.MoveNext() {
			nodes = append(nodes, val.Current().Copy())
		}
		return nodes, nil
	default:
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Slice {
			return nil, fmt.Errorf("xpath: node-set variable must be []NodeNavigator, got %T", v)
		}
		return nil, fmt.Errorf("xpath: unsupported variable type %T (want bool, float64, string, NodeNavigator or []NodeNavigator)", v)
	}
}

// variableQuery resolves a declared variable reference. It holds only the
// variable's expanded name; it never stores a bound value or an iteration
// position, so a compiled expression is reusable across evaluations and safe
// for concurrent use with different bindings.
type variableQuery struct {
	// name is the expanded QName used to resolve the value in every binding.
	name VariableName

	// cluster groups queries that must share one node-set iteration cursor for
	// a single evaluation. A variable node-set and the positional predicate
	// that filters it ($x[position()], $x[last()]) are the same build-time
	// query and so share a cluster; separate references get separate clusters.
	// The cursor itself lives in the evaluation scope, never here, so the
	// compiled query stays value-free and safe for concurrent evaluations.
	cluster int
}

func newVariableQuery(name VariableName, cluster int) *variableQuery {
	return &variableQuery{name: name, cluster: cluster}
}

// cursorKeeper is implemented by the iterator that threads through a single
// evaluation (evalContext for Evaluate, *NodeIterator for Select). It hands
// out the one cursor shared by every clone carrying the same cluster id.
type cursorKeeper interface {
	variableCursor(cluster int) *int
}

// variableCursor resolves the shared iteration cursor for a variable
// node-set cluster. When no per-evaluation scope carries cursors (the legacy
// entry points, which cannot declare variables), a fresh cursor is returned:
// it behaves like a normal start-at-the-beginning sequence.
func variableCursor(t iterator, cluster int) *int {
	if k, ok := t.(cursorKeeper); ok {
		return k.variableCursor(cluster)
	}
	i := -1
	return &i
}

// nodes returns the node-set bound to the variable for the current
// evaluation, or nil when the binding is absent or not a node-set.
func (q *variableQuery) nodes(t iterator) []NodeNavigator {
	v, ok := lookupVariable(t, q.name)
	if !ok {
		return nil
	}
	nodes, _ := v.([]NodeNavigator)
	return nodes
}

func (q *variableQuery) Select(t iterator) NodeNavigator {
	nodes := q.nodes(t)
	cursor := variableCursor(t, q.cluster)
	*cursor++
	if *cursor < 0 || *cursor >= len(nodes) {
		return nil
	}
	return nodes[*cursor].Copy()
}

func (q *variableQuery) Evaluate(t iterator) interface{} {
	v, ok := lookupVariable(t, q.name)
	if !ok {
		return nil
	}
	if _, isNodes := v.([]NodeNavigator); isNodes {
		// Present the node-set as a query so the existing coercion and path
		// machinery treats it like any other node-set.
		return q
	}
	return v
}

func (q *variableQuery) Clone() query {
	return newVariableQuery(q.name, q.cluster)
}

func (q *variableQuery) ValueType() resultType {
	// The bound type is per-evaluation and unknown at compile time. Any keeps
	// scalar coercions working while node-sets are iterated through Select.
	return xpathResultType.Any
}

func (q *variableQuery) Properties() queryProp {
	return queryProps.Merge | queryProps.Position | queryProps.Count | queryProps.Cached
}

// Test satisfies the predicate interface used by position()/last() sibling
// counting. A variable node-set is the candidate set itself, so every node is
// accepted.
func (q *variableQuery) Test(NodeNavigator) bool { return true }

// position reports the 1-based ordinal of the current candidate node. It is
// used by numeric predicates ($x[1]) through getNodePosition, mirroring the
// positional interface the physical axis queries implement.
func (q *variableQuery) position(t iterator) int {
	cursor := variableCursor(t, q.cluster)
	if *cursor < 0 {
		return 0
	}
	return *cursor + 1
}

// contextPosition reports the 1-based ordinal of the candidate node the
// surrounding filter is evaluating. The filter's input and the position()
// input are independent clones that share the single per-evaluation cursor,
// so reading it reflects the node just produced by Select without advancing
// it. This keeps a node-set variable working even when its nodes come from a
// different document and cannot be located by walking siblings.
func (q *variableQuery) contextPosition(t iterator) int {
	cursor := variableCursor(t, q.cluster)
	if *cursor < 0 {
		return 1
	}
	return *cursor + 1
}

// contextSize reports the number of nodes in the bound node-set for this
// evaluation. It is consulted by last().
func (q *variableQuery) contextSize(t iterator) int {
	return len(q.nodes(t))
}

// resetCursor rewinds the shared node-set cursor to before the first node. It
// is invoked after a construct such as last() counts the sequence by consuming
// it, allowing the enclosing filter to iterate the same binding anew.
func (q *variableQuery) resetCursor(t iterator) {
	cursor := variableCursor(t, q.cluster)
	*cursor = -1
}
