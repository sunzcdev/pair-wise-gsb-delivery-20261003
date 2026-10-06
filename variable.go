package xpath

import "fmt"

// CompileOption customizes how an XPath expression is compiled.
type CompileOption func(*compileConfig)

type compileConfig struct {
	namespaces map[string]string
	variables  map[string]struct{}
}

// WithNamespaces binds element/attribute and variable prefixes used while
// compiling the expression to namespace URIs. It is the option form of
// CompileWithNS.
func WithNamespaces(namespaces map[string]string) CompileOption {
	return func(c *compileConfig) {
		c.namespaces = namespaces
	}
}

// WithVariableDeclarations declares the variables that may be referenced by an
// expression. Each name is a QName exactly as written in the expression
// (e.g. "limit" or "ns:rows"); a prefixed name is resolved against the
// namespaces supplied via WithNamespaces at compile time.
//
// A reference to a variable that was not declared, or whose prefix cannot be
// resolved, makes Compile return an error. Declaring a variable does not give
// it a value: concrete bindings are supplied per evaluation through
// Expr.EvaluateWithVariables / Expr.SelectWithVariables.
func WithVariableDeclarations(names ...string) CompileOption {
	return func(c *compileConfig) {
		if c.variables == nil {
			c.variables = make(map[string]struct{})
		}
		for _, name := range names {
			c.variables[name] = struct{}{}
		}
	}
}

// VariableResolver resolves a variable reference to its value.
//
// name is the expanded QName produced at compile time: the local name for an
// unprefixed variable, or "{namespace-uri}local-name" (Clark notation) when
// the variable carried a prefix. This keeps variables that share a local
// name but live in different namespaces distinguishable.
//
// The returned value must be one of bool, a numeric type, string, *NodeIterator
// or NodeSet. Returning an error aborts the evaluation.
type VariableResolver interface {
	ResolveVariable(name string) (interface{}, error)
}

// Variables is a VariableResolver backed by a map. Keys follow the same
// expanded-QName convention used by VariableResolver: local name or
// "{namespace-uri}local-name".
type Variables map[string]interface{}

// ResolveVariable implements VariableResolver.
func (v Variables) ResolveVariable(name string) (interface{}, error) {
	val, ok := v[name]
	if !ok {
		return nil, fmt.Errorf("xpath: no binding supplied for variable $%s", displayName(name))
	}
	return val, nil
}

// NodeSet is an explicitly ordered, reusable XPath 1.0 node-set value.
//
// Unlike *NodeIterator, a NodeSet is not consumed by iteration: every
// evaluation receives its own iterator positioned before the first node, so
// the same binding can drive concurrent evaluations and repeated references
// inside one expression. Node order is the order of the slice and therefore
// defines position()/last() for predicates applied to the node-set.
type NodeSet struct {
	nodes []NodeNavigator
}

// NewNodeSet snapshots the navigators currently produced by iter into a
// NodeSet. Each navigator is copied, so the returned NodeSet keeps its own
// identity independent of iter and can be reused across evaluations.
func NewNodeSet(iter *NodeIterator) *NodeSet {
	ns := &NodeSet{}
	for iter.MoveNext() {
		ns.nodes = append(ns.nodes, iter.Current().Copy())
	}
	return ns
}

// NewNodeSetFromNavigators snapshots an ordered slice of navigators into a
// NodeSet. Each navigator is copied.
func NewNodeSetFromNavigators(nodes []NodeNavigator) *NodeSet {
	ns := &NodeSet{nodes: make([]NodeNavigator, 0, len(nodes))}
	for _, n := range nodes {
		ns.nodes = append(ns.nodes, n.Copy())
	}
	return ns
}

// Len returns the number of nodes in the node-set.
func (s *NodeSet) Len() int {
	return len(s.nodes)
}

// Node returns a copy of the node at the given 0-based index, or nil when out
// of range. The returned navigator is independent of the NodeSet.
func (s *NodeSet) Node(i int) NodeNavigator {
	if i < 0 || i >= len(s.nodes) {
		return nil
	}
	return s.nodes[i].Copy()
}

// expandedName turns a (prefix, local) pair, resolved against the compile
// time namespaces, into the expanded QName used as the variable identity.
func expandedName(prefix, local string, namespaces map[string]string) (string, error) {
	if prefix == "" {
		return local, nil
	}
	uri, ok := namespaces[prefix]
	if !ok {
		return "", fmt.Errorf("xpath: undeclared namespace prefix %q in variable $%s:%s", prefix, prefix, local)
	}
	return "{" + uri + "}" + local, nil
}

// displayName turns an expanded QName back into a readable form for errors.
func displayName(expanded string) string {
	if len(expanded) > 0 && expanded[0] == '{' {
		for i := 1; i < len(expanded); i++ {
			if expanded[i] == '}' {
				return expanded[i+1:]
			}
		}
	}
	return expanded
}

// normalizeVariableValue normalizes a single variable binding into the value
// consumed during one evaluation. Node-set bindings are snapshot into a
// *NodeSet so iteration never mutates caller state.
func normalizeVariableValue(v interface{}) (interface{}, error) {
	switch val := v.(type) {
	case nil:
		return nil, fmt.Errorf("xpath: variable binding must not be nil")
	case bool, float64, string, *NodeSet:
		return val, nil
	case *NodeIterator:
		return NewNodeSet(val), nil
	case NodeSet:
		return &val, nil
	case int:
		return float64(val), nil
	case int8:
		return float64(val), nil
	case int16:
		return float64(val), nil
	case int32:
		return float64(val), nil
	case int64:
		return float64(val), nil
	case uint:
		return float64(val), nil
	case uint8:
		return float64(val), nil
	case uint16:
		return float64(val), nil
	case uint32:
		return float64(val), nil
	case uint64:
		return float64(val), nil
	case float32:
		return float64(val), nil
	default:
		return nil, fmt.Errorf("xpath: unsupported variable binding type %T (want bool, number, string, NodeSet or *NodeIterator)", v)
	}
}

// scope is the per-evaluation binding table. It is created fresh for every
// Evaluate/Select call and is never shared, which is what allows one compiled
// expression to run concurrently against different bindings.
type scope struct {
	resolver VariableResolver
	cache    map[string]interface{}
}

func newScope(resolver VariableResolver) *scope {
	return &scope{resolver: resolver, cache: make(map[string]interface{})}
}

// lookup resolves and normalizes a variable, caching the normalized value so
// that several references to the same variable in one expression share a
// binding while their iterators stay independent.
func (s *scope) lookup(name string) (interface{}, error) {
	if v, ok := s.cache[name]; ok {
		return v, nil
	}
	if s.resolver == nil {
		return nil, fmt.Errorf("xpath: no binding supplied for variable $%s", displayName(name))
	}
	raw, err := s.resolver.ResolveVariable(name)
	if err != nil {
		return nil, err
	}
	v, err := normalizeVariableValue(raw)
	if err != nil {
		return nil, err
	}
	s.cache[name] = v
	return v, nil
}

// validateBindings eagerly resolves every variable referenced by the
// expression so a missing or ill-typed binding is reported before iteration
// starts, rather than surfacing midway through a node-set.
func (e *Expr) validateBindings(resolver VariableResolver) (*scope, error) {
	s := newScope(resolver)
	for _, name := range e.variables {
		if _, err := s.lookup(name); err != nil {
			return nil, err
		}
	}
	return s, nil
}
