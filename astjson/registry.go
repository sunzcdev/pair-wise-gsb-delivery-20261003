package astjson

import (
	"fmt"

	"github.com/yuin/goldmark/v2/ast"
)

// An Adapter exports node-type specific semantic properties of a single AST
// node kind.
//
// Implementations must not traverse or mutate children: child ordering and
// common structural properties (position, source, attributes,
// blankPreviousLines) are handled by the [Exporter].
//
// The returned properties are emitted in slice order, so adapters must build
// the slice directly rather than ranging over a map.
type Adapter interface {
	// Export returns the semantic properties of node for the given context.
	Export(node ast.Node, c *Context) ([]Property, error)
}

// The AdapterFunc type is an adapter to allow the use of ordinary functions as
// [Adapter] values.
type AdapterFunc func(node ast.Node, c *Context) ([]Property, error)

// Export implements [Adapter].
func (f AdapterFunc) Export(node ast.Node, c *Context) ([]Property, error) {
	return f(node, c)
}

// A Property is one node-specific semantic property.
//
// Key is emitted as the JSON property name and must be unique within a node.
// Value is typically built with [TextValue], [LinesValue] or the primitive
// constructors of [Context].
type Property struct {
	Key   string
	Value jsonValue
}

// Prop returns a [Property] with the given key and value.
func Prop(key string, value jsonValue) Property {
	return Property{Key: key, Value: value}
}

// A Registry maps node kinds to their [Adapter].
//
// A zero Registry is ready to use but exports no node kinds. [DefaultRegistry]
// is pre-populated with adapters for all node kinds defined in the core ast
// package.
type Registry struct {
	adapters map[ast.NodeKind]Adapter
}

// NewRegistry returns a new empty [Registry].
func NewRegistry() *Registry {
	return &Registry{adapters: map[ast.NodeKind]Adapter{}}
}

// Register registers an adapter for the given node kind.
//
// Register panics if adapter is nil. Registering a kind that is already
// registered replaces the previous adapter.
func (r *Registry) Register(kind ast.NodeKind, adapter Adapter) *Registry {
	if adapter == nil {
		panic("astjson: adapter must not be nil")
	}
	if r.adapters == nil {
		r.adapters = map[ast.NodeKind]Adapter{}
	}
	r.adapters[kind] = adapter
	return r
}

// Lookup returns the adapter registered for kind, or nil if none is
// registered.
func (r *Registry) Lookup(kind ast.NodeKind) Adapter {
	if r == nil {
		return nil
	}
	return r.adapters[kind]
}

// Clone returns a copy of the registry. The returned registry can be modified
// freely without affecting r.
func (r *Registry) Clone() *Registry {
	out := NewRegistry()
	for kind, adapter := range r.adapters {
		out.adapters[kind] = adapter
	}
	return out
}

// DefaultRegistry is the registry pre-populated with adapters for all node
// kinds defined in the core [github.com/yuin/goldmark/v2/ast] package.
//
// Extension packages register their adapters into registries derived from this
// one (or into it directly for process-wide configuration).
var DefaultRegistry = newDefaultRegistry()

// UnknownNodeError indicates that a node kind has no registered [Adapter].
//
// It wraps [ErrUnknownNode] so callers can detect it with errors.Is.
type UnknownNodeError struct {
	// Kind is the node kind that could not be exported.
	Kind ast.NodeKind
}

// Error implements error.
func (e *UnknownNodeError) Error() string {
	return fmt.Sprintf("astjson: no adapter registered for node kind %q", e.Kind.String())
}

// Unwrap returns [ErrUnknownNode].
func (e *UnknownNodeError) Unwrap() error {
	return ErrUnknownNode
}

// ErrUnknownNode is the sentinel error returned when a node kind has no
// registered adapter.
var ErrUnknownNode = fmt.Errorf("astjson: unknown node kind")
