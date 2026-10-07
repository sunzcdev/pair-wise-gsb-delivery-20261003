package xpath

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const extTestNS = "urn:example:ext"

func extTestNamespaces() map[string]string {
	return map[string]string{"ext": extTestNS}
}

// createExtItemsExample builds:
//
//	<root>
//		<item code="A1"/>
//		<item code="B2"/>
//		<item code="A3"/>
//	</root>
func createExtItemsExample(codes ...string) *TNode {
	doc := createNode("", RootNode)
	for _, code := range codes {
		item := doc.createChildNode("item", ElementNode)
		item.addAttribute("code", code)
	}
	return doc
}

func classifyRegistry(mapping map[string]string) *FunctionRegistry {
	r := NewFunctionRegistry()
	_ = r.Register(ExtensionFunction{
		NamespaceURI: extTestNS,
		Name:         "classify",
		Args:         []DataType{StringType},
		Result:       StringType,
		Func: func(fc *FunctionContext, args []interface{}) (interface{}, error) {
			return mapping[args[0].(string)], nil
		},
	})
	return r
}

// Two registries bind the same function name to different implementations;
// each compiled expression must use its own registry.
func TestExtensionFunctionPerExpressionRegistry(t *testing.T) {
	regA := classifyRegistry(map[string]string{"A1": "fruit", "B2": "veg", "A3": "fruit"})
	regB := classifyRegistry(map[string]string{"A1": "veg", "B2": "veg", "A3": "veg"})

	doc := createExtItemsExample("A1", "B2", "A3")
	expr := `//item[ext:classify(@code) = 'fruit']`

	expA, err := CompileWithFunctions(expr, extTestNamespaces(), regA)
	assertNoErr(t, err)
	assertEqual(t, 2, len(iterateNodes(expA.Select(createNavigator(doc)))))

	expB, err := CompileWithFunctions(expr, extTestNamespaces(), regB)
	assertNoErr(t, err)
	assertEqual(t, 0, len(iterateNodes(expB.Select(createNavigator(doc)))))

	// Without a registry the same expression does not compile.
	if _, err = Compile(expr); err == nil {
		t.Fatal("expected an error compiling an unregistered extension function")
	}
}

// Extension functions nested inside a predicate, and the position/size of
// the evaluation context.
func TestExtensionFunctionNestedInPredicate(t *testing.T) {
	r := NewFunctionRegistry()
	assertNoErr(t, r.Register(ExtensionFunction{
		NamespaceURI: extTestNS,
		Name:         "upper",
		Args:         []DataType{StringType},
		Result:       StringType,
		Func: func(fc *FunctionContext, args []interface{}) (interface{}, error) {
			return strings.ToUpper(args[0].(string)), nil
		},
	}))
	assertNoErr(t, r.Register(ExtensionFunction{
		NamespaceURI: extTestNS,
		Name:         "trim-prefix",
		Args:         []DataType{StringType},
		Result:       StringType,
		Func: func(fc *FunctionContext, args []interface{}) (interface{}, error) {
			return strings.TrimPrefix(args[0].(string), "EXT-"), nil
		},
	}))
	assertNoErr(t, r.Register(ExtensionFunction{
		NamespaceURI: extTestNS,
		Name:         "is-middle",
		Result:       BooleanType,
		Func: func(fc *FunctionContext, args []interface{}) (interface{}, error) {
			return fc.Position*2 == fc.Size+1, nil
		},
	}))

	doc := createExtItemsExample("EXT-a1", "EXT-B2", "EXT-a3")

	exp, err := CompileWithFunctions(`//item[ext:upper(ext:trim-prefix(@code)) = 'A1']`, extTestNamespaces(), r)
	assertNoErr(t, err)
	nodes := iterateNodes(exp.Select(createNavigator(doc)))
	assertEqual(t, 1, len(nodes))
	assertEqual(t, "EXT-a1", nodes[0].getAttribute("code"))

	// The context position/size reflect the node list being filtered.
	exp, err = CompileWithFunctions(`//item[ext:is-middle()]`, extTestNamespaces(), r)
	assertNoErr(t, err)
	nodes = iterateNodes(exp.Select(createNavigator(doc)))
	assertEqual(t, 1, len(nodes))
	assertEqual(t, "EXT-B2", nodes[0].getAttribute("code"))
}

// The caller context and the current node are provided to the function.
func TestExtensionFunctionCallerContext(t *testing.T) {
	type ctxKey struct{}
	var gotValue interface{}
	var gotName string

	r := NewFunctionRegistry()
	assertNoErr(t, r.Register(ExtensionFunction{
		NamespaceURI: extTestNS,
		Name:         "probe",
		Result:       BooleanType,
		Func: func(fc *FunctionContext, args []interface{}) (interface{}, error) {
			gotValue = fc.Context.Value(ctxKey{})
			gotName = fc.Node.LocalName()
			return true, nil
		},
	}))

	exp, err := CompileWithFunctions(`//item[ext:probe()]`, extTestNamespaces(), r)
	assertNoErr(t, err)

	ctx := context.WithValue(context.Background(), ctxKey{}, "req-1")
	v, err := exp.EvaluateWithContext(ctx, createNavigator(createExtItemsExample("A1")))
	assertNoErr(t, err)
	assertEqual(t, 1, len(iterateNodes(v.(*NodeIterator))))
	assertEqual(t, "req-1", gotValue)
	assertEqual(t, "item", gotName)
}

// An extension function error must surface (not fold into false), and the
// compiled expression must stay reusable afterwards.
func TestExtensionFunctionErrorAndReuse(t *testing.T) {
	errUnknown := errors.New("unknown code")

	r := NewFunctionRegistry()
	assertNoErr(t, r.Register(ExtensionFunction{
		NamespaceURI: extTestNS,
		Name:         "lookup",
		Args:         []DataType{StringType},
		Result:       StringType,
		Func: func(fc *FunctionContext, args []interface{}) (interface{}, error) {
			code := args[0].(string)
			if code == "A1" {
				return "fruit", nil
			}
			return nil, errUnknown
		},
	}))

	exp, err := CompileWithFunctions(`count(//item[ext:lookup(@code) = 'fruit'])`, extTestNamespaces(), r)
	assertNoErr(t, err)

	bad := createExtItemsExample("A1", "XX")

	// The error is returned instead of silently counting only the nodes
	// seen before the failure, and the original error value is preserved.
	_, err = exp.EvaluateWithContext(context.Background(), createNavigator(bad))
	assertErr(t, err)
	var fe *FunctionError
	assertTrue(t, errors.As(err, &fe))
	assertEqual(t, "lookup", fe.Name)
	assertEqual(t, extTestNS, fe.NamespaceURI)
	assertTrue(t, errors.Is(err, errUnknown))

	// The same compiled expression keeps working after the error.
	good := createExtItemsExample("A1", "A1")
	v, err := exp.EvaluateWithContext(context.Background(), createNavigator(good))
	assertNoErr(t, err)
	assertEqual(t, float64(2), v)

	// Plain Evaluate surfaces the error as a panic carrying *FunctionError.
	func() {
		defer func() {
			r := recover()
			fe, ok := r.(*FunctionError)
			assertTrue(t, ok)
			assertTrue(t, errors.Is(fe.Err, errUnknown))
		}()
		exp.Evaluate(createNavigator(bad))
	}()

	// And it is still reusable after the panic.
	assertEqual(t, float64(2), exp.Evaluate(createNavigator(good)))
}

// Node-set results: returned nodes are iterable and support positional
// predicates; node-set arguments arrive as []NodeNavigator.
func TestExtensionFunctionNodeSet(t *testing.T) {
	r := NewFunctionRegistry()
	assertNoErr(t, r.Register(ExtensionFunction{
		NamespaceURI: extTestNS,
		Name:         "items",
		Result:       NodeSetType,
		Func: func(fc *FunctionContext, args []interface{}) (interface{}, error) {
			var nodes []NodeNavigator
			n := fc.Node.Copy()
			n.MoveToRoot()
			if !n.MoveToChild() {
				return nodes, nil
			}
			for {
				if n.LocalName() == "item" {
					nodes = append(nodes, n.Copy())
				}
				if !n.MoveToNext() {
					break
				}
			}
			return nodes, nil
		},
	}))
	assertNoErr(t, r.Register(ExtensionFunction{
		NamespaceURI: extTestNS,
		Name:         "count-em",
		Args:         []DataType{NodeSetType},
		Result:       NumberType,
		Func: func(fc *FunctionContext, args []interface{}) (interface{}, error) {
			return float64(len(args[0].([]NodeNavigator))), nil
		},
	}))

	doc := createExtItemsExample("A1", "B2", "A3")

	exp, err := CompileWithFunctions(`count(ext:items())`, extTestNamespaces(), r)
	assertNoErr(t, err)
	assertEqual(t, float64(3), exp.Evaluate(createNavigator(doc)))

	exp, err = CompileWithFunctions(`ext:items()`, extTestNamespaces(), r)
	assertNoErr(t, err)
	assertEqual(t, 3, len(iterateNodes(exp.Select(createNavigator(doc)))))

	exp, err = CompileWithFunctions(`ext:items()[2]`, extTestNamespaces(), r)
	assertNoErr(t, err)
	nodes := iterateNodes(exp.Select(createNavigator(doc)))
	assertEqual(t, 1, len(nodes))
	assertEqual(t, "B2", nodes[0].getAttribute("code"))

	exp, err = CompileWithFunctions(`ext:count-em(//item)`, extTestNamespaces(), r)
	assertNoErr(t, err)
	assertEqual(t, float64(3), exp.Evaluate(createNavigator(doc)))
}

// Arguments are converted following the XPath type conversion rules.
func TestExtensionFunctionArgConversion(t *testing.T) {
	var got []interface{}
	r := NewFunctionRegistry()
	assertNoErr(t, r.Register(ExtensionFunction{
		NamespaceURI: extTestNS,
		Name:         "combo",
		Args:         []DataType{NumberType, BooleanType, StringType},
		Result:       BooleanType,
		Func: func(fc *FunctionContext, args []interface{}) (interface{}, error) {
			got = args
			return true, nil
		},
	}))

	exp, err := CompileWithFunctions(`ext:combo('42', //item, 7)`, extTestNamespaces(), r)
	assertNoErr(t, err)
	assertEqual(t, true, exp.Evaluate(createNavigator(createExtItemsExample("A1"))))
	assertEqual(t, float64(42), got[0])
	assertEqual(t, true, got[1])
	assertEqual(t, "7", got[2])
}

// Signature problems detectable at compile time are reported by Compile.
func TestExtensionFunctionCompileErrors(t *testing.T) {
	r := classifyRegistry(nil)
	assertNoErr(t, r.Register(ExtensionFunction{
		NamespaceURI: extTestNS,
		Name:         "kids",
		Args:         []DataType{NodeSetType},
		Result:       NodeSetType,
		Func: func(fc *FunctionContext, args []interface{}) (interface{}, error) {
			return args[0], nil
		},
	}))

	// Arity mismatch.
	if _, err := CompileWithFunctions(`ext:classify('a', 'b')`, extTestNamespaces(), r); err == nil {
		t.Error("expected an arity error")
	}
	// A scalar constant cannot satisfy a node-set parameter.
	if _, err := CompileWithFunctions(`ext:kids('a')`, extTestNamespaces(), r); err == nil {
		t.Error("expected a node-set parameter error")
	}
	// Unknown function in a known namespace.
	if _, err := CompileWithFunctions(`ext:nope('a')`, extTestNamespaces(), r); err == nil {
		t.Error("expected an unknown function error")
	}
	// Undefined prefix.
	if _, err := CompileWithFunctions(`nope:classify('a')`, extTestNamespaces(), r); err == nil {
		t.Error("expected an undefined prefix error")
	}

	// Registry-level validation.
	assertErr(t, r.Register(classifyRegistryMustDecl()))
	assertErr(t, NewFunctionRegistry().Register(ExtensionFunction{
		Name:   "noimpl",
		Result: BooleanType,
	}))
	assertErr(t, NewFunctionRegistry().Register(ExtensionFunction{
		Name:   "badresult",
		Result: DataType(-1),
		Func:   func(fc *FunctionContext, args []interface{}) (interface{}, error) { return true, nil },
	}))
}

func classifyRegistryMustDecl() ExtensionFunction {
	return ExtensionFunction{
		NamespaceURI: extTestNS,
		Name:         "classify",
		Args:         []DataType{StringType},
		Result:       StringType,
		Func:         func(fc *FunctionContext, args []interface{}) (interface{}, error) { return "", nil },
	}
}

// A result that does not match the declared type is a call-time error.
func TestExtensionFunctionResultTypeMismatch(t *testing.T) {
	r := NewFunctionRegistry()
	assertNoErr(t, r.Register(ExtensionFunction{
		NamespaceURI: extTestNS,
		Name:         "broken",
		Result:       NumberType,
		Func: func(fc *FunctionContext, args []interface{}) (interface{}, error) {
			return "not a number", nil
		},
	}))

	exp, err := CompileWithFunctions(`ext:broken()`, extTestNamespaces(), r)
	assertNoErr(t, err)
	_, err = exp.EvaluateWithContext(context.Background(), createNavigator(empty_example))
	assertErr(t, err)
	var fe *FunctionError
	assertTrue(t, errors.As(err, &fe))
}
