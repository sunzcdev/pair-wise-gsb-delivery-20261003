package xpath

import (
	"errors"
	"fmt"
	"sync"
)

// ExtensionType declares the XPath 1.0 type of an extension function
// parameter or return value.
type ExtensionType int

const (
	// ExtensionAny accepts or returns any XPath type (boolean, number,
	// string or node-set). For return values the concrete Go type of the
	// result determines the XPath type.
	ExtensionAny ExtensionType = iota
	// ExtensionBoolean is the XPath boolean type, represented in Go as bool.
	ExtensionBoolean
	// ExtensionNumber is the XPath number type, represented in Go as float64.
	ExtensionNumber
	// ExtensionString is the XPath string type, represented in Go as string.
	ExtensionString
	// ExtensionNodeSet is the XPath node-set type, represented in Go as
	// []NodeNavigator.
	ExtensionNodeSet
)

func (t ExtensionType) String() string {
	switch t {
	case ExtensionAny:
		return "any"
	case ExtensionBoolean:
		return "boolean"
	case ExtensionNumber:
		return "number"
	case ExtensionString:
		return "string"
	case ExtensionNodeSet:
		return "node-set"
	default:
		return fmt.Sprintf("unknown(%d)", int(t))
	}
}

func (t ExtensionType) resultType() resultType {
	switch t {
	case ExtensionBoolean:
		return xpathResultType.Boolean
	case ExtensionNumber:
		return xpathResultType.Number
	case ExtensionString:
		return xpathResultType.String
	case ExtensionNodeSet:
		return xpathResultType.NodeSet
	default:
		return xpathResultType.Any
	}
}

// ExtensionContext is passed to an ExtensionFunction on every invocation.
// It exposes the XPath evaluation context of the call site.
type ExtensionContext struct {
	// ContextNode is the node used as the context node when evaluating the
	// function call. It is a copy owned by the XPath engine; callers may
	// navigate it freely without affecting the evaluation.
	ContextNode NodeNavigator
	// Position is the context position (1-based, XPath position()) of the
	// call site within its context node-set. It is always >= 1.
	Position int
	// Last is the size of the context node-set (XPath last()). It is always
	// >= Position.
	Last int
	// CallerContext is the opaque context value supplied when the compiled
	// expression was evaluated (see Expr.Eval and Expr.SelectWithContext).
	// It is nil when no caller context was supplied.
	CallerContext interface{}
}

// ExtensionFunction is an application-provided XPath extension function.
//
// args holds the evaluated function arguments, converted to the Go
// representation of the declared parameter types using the XPath 1.0 type
// conversion rules (boolean(), number(), string(), and node-set identity):
//
//   - ExtensionBoolean arguments are bool
//   - ExtensionNumber arguments are float64
//   - ExtensionString arguments are string
//   - ExtensionNodeSet arguments are []NodeNavigator (never nil; empty when
//     the argument node-set is empty)
//   - ExtensionAny arguments are passed as bool, float64, string or
//     []NodeNavigator, depending on the dynamic value type.
//
// The declared return value is converted back using the same rules. A
// non-nil error aborts evaluation and is returned (or recovered via
// NodeIterator.Err) instead of being folded into a false/empty result.
//
// Callers that want errors as values must compile with
// CompileWithExtensions and evaluate through Expr.Eval /
// Expr.SelectWithContext. The legacy Expr.Evaluate and Expr.Select APIs do
// not return errors; an extension error panics as *ExtensionError through
// those entry points (Select iteration exposes it via NodeIterator.Err when
// SelectWithContext is used).
type ExtensionFunction func(ctx ExtensionContext, args []interface{}) (interface{}, error)

// FunctionSpec describes an extension function: its namespace URI, local
// name, parameter types and return type. The parameter count is fixed and is
// given by the length of Params (variadic functions are not supported).
type FunctionSpec struct {
	// NamespaceURI identifies the function namespace together with LocalName.
	// It must be non-empty; functions are invoked as prefix:local-name(),
	// where prefix is bound to NamespaceURI via the namespaces map passed to
	// CompileWithExtensions.
	NamespaceURI string
	// LocalName is the local part of the function QName.
	LocalName string
	// Params declares the number and XPath types of the function parameters.
	Params []ExtensionType
	// Return declares the XPath type of the function result.
	Return ExtensionType
	// Func is the implementation invoked at evaluation time.
	Func ExtensionFunction
}

// ExtensionError wraps an error returned by an ExtensionFunction so that it
// survives the engine evaluation without being converted into a boolean or
// otherwise swallowed.
type ExtensionError struct {
	// NamespaceURI and LocalName identify the function that failed.
	NamespaceURI string
	LocalName    string
	// Err is the error returned by the extension implementation.
	Err error
}

func (e *ExtensionError) Error() string {
	return fmt.Sprintf("xpath: extension function {%s}%s() failed: %s",
		e.NamespaceURI, e.LocalName, e.Err.Error())
}

// Unwrap exposes the underlying error for errors.Is/errors.As.
func (e *ExtensionError) Unwrap() error {
	return e.Err
}

// FunctionRegistry is a table of extension functions scoped to a single
// compiled expression. Unlike a package-level registry, every registry is an
// independent value: two registries may register the same namespace URI and
// local name with different implementations, and an implementation is only
// visible to expressions compiled with that registry.
//
// A FunctionRegistry is safe for concurrent Lookup calls. Registering new
// functions concurrently with lookups is not supported; register before
// compiling.
type FunctionRegistry struct {
	mu    sync.RWMutex
	funcs map[extensionKey]*FunctionSpec
}

type extensionKey struct {
	uri  string
	name string
}

// NewFunctionRegistry creates an empty, expression-scoped extension function
// registry.
func NewFunctionRegistry() *FunctionRegistry {
	return &FunctionRegistry{funcs: make(map[extensionKey]*FunctionSpec)}
}

// Register adds an extension function. It returns an error when spec is nil,
// the namespace URI or local name is empty, or a function with the same
// namespace URI and local name is already registered.
func (r *FunctionRegistry) Register(spec *FunctionSpec) error {
	if r == nil {
		return errors.New("xpath: nil extension function registry")
	}
	if spec == nil {
		return errors.New("xpath: extension function spec is nil")
	}
	if spec.NamespaceURI == "" {
		return errors.New("xpath: extension function namespace URI must not be empty")
	}
	if spec.LocalName == "" {
		return errors.New("xpath: extension function local name must not be empty")
	}
	if spec.Func == nil {
		return fmt.Errorf("xpath: extension function {%s}%s has no implementation",
			spec.NamespaceURI, spec.LocalName)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := extensionKey{uri: spec.NamespaceURI, name: spec.LocalName}
	if _, exists := r.funcs[key]; exists {
		return fmt.Errorf("xpath: extension function {%s}%s is already registered",
			spec.NamespaceURI, spec.LocalName)
	}
	r.funcs[key] = spec
	return nil
}

// MustRegister is like Register but panics on registration error.
func (r *FunctionRegistry) MustRegister(spec *FunctionSpec) {
	if err := r.Register(spec); err != nil {
		panic(err)
	}
}

// Lookup returns the function registered for the given namespace URI and
// local name, or nil if none is registered.
func (r *FunctionRegistry) Lookup(namespaceURI, localName string) *FunctionSpec {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.funcs[extensionKey{uri: namespaceURI, name: localName}]
}
