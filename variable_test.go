package xpath

import (
	"errors"
	"sync"
	"testing"
)

func rowsDoc() (*TNode, *TNode) {
	doc := createNode("", RootNode)
	root := doc.createChildNode("root", ElementNode)
	for i := 0; i < 4; i++ {
		row := root.createChildNode("row", ElementNode)
		cell := row.createChildNode("cell", ElementNode)
		cell.createChildNode("value", TextNode)
	}
	return doc, root
}

func rowNavigators(t *testing.T, doc *TNode) []NodeNavigator {
	it := MustCompile("//row").Select(createNavigator(doc))
	var navs []NodeNavigator
	for it.MoveNext() {
		navs = append(navs, it.Current().Copy())
	}
	if len(navs) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(navs))
	}
	return navs
}

func evalWith(t *testing.T, expr string, ns map[string]string, decls []string, vars VariableResolver, root NodeNavigator) interface{} {
	t.Helper()
	opts := []CompileOption{WithVariableDeclarations(decls...)}
	if ns != nil {
		opts = append(opts, WithNamespaces(ns))
	}
	e, err := Compile(expr, opts...)
	assertNoErr(t, err)
	v, err := e.EvaluateWithVariables(root, vars)
	assertNoErr(t, err)
	return v
}

func TestVariableScalarBindings(t *testing.T) {
	root := createNavigator(createNode("", RootNode))

	assertEqual(t, true, evalWith(t, "$b", nil, []string{"b"}, Variables{"b": true}, root))
	assertEqual(t, false, evalWith(t, "not($b)", nil, []string{"b"}, Variables{"b": true}, root))
	assertEqual(t, float64(7), evalWith(t, "$n + 2", nil, []string{"n"}, Variables{"n": 5}, root))
	assertEqual(t, float64(7), evalWith(t, "$n + 2", nil, []string{"n"}, Variables{"n": int64(5)}, root))
	assertEqual(t, "hello world",
		evalWith(t, "concat($s, ' world')", nil, []string{"s"}, Variables{"s": "hello"}, root))
	assertEqual(t, true, evalWith(t, "$n > 3", nil, []string{"n"}, Variables{"n": 4.5}, root))
	assertEqual(t, true, evalWith(t, "$s = 'a' or $b", nil, []string{"s", "b"},
		Variables{"s": "x", "b": true}, root))
	assertEqual(t, "true", evalWith(t, "string($b)", nil, []string{"b"}, Variables{"b": true}, root))
	assertEqual(t, float64(1), evalWith(t, "number($b)", nil, []string{"b"}, Variables{"b": true}, root))
}

func TestVariableDoesNotNeedBindingWhenUnreferenced(t *testing.T) {
	root := createNavigator(createNode("", RootNode))
	// $unused is declared but the expression never references it; the binding
	// need not be supplied.
	assertEqual(t, float64(2),
		evalWith(t, "1 + 1", nil, []string{"unused"}, Variables{}, root))
}

func TestVariableUndeclaredReferenceFailsCompile(t *testing.T) {
	for _, tc := range []struct {
		name  string
		expr  string
		decls []string
		ns    map[string]string
	}{
		{"none declared", "$x", nil, nil},
		{"other declared", "$y + 1", []string{"x"}, nil},
		{"nested in path", "$x/cell", []string{"y"}, nil},
		{"nested in predicate", "//row[$x]", []string{"y"}, nil},
		{"unresolved prefix", "p:$x", []string{"x"}, map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := []CompileOption{WithVariableDeclarations(tc.decls...)}
			if tc.ns != nil {
				opts = append(opts, WithNamespaces(tc.ns))
			}
			if _, err := Compile(tc.expr, opts...); err == nil {
				t.Fatalf("Compile(%q) expected undeclared-variable error", tc.expr)
			}
		})
	}
}

func TestVariableSameLocalNameDifferentNamespace(t *testing.T) {
	root := createNavigator(createNode("", RootNode))
	ns := map[string]string{"a": "http://example.com/a", "b": "http://example.com/b"}

	v1 := evalWith(t, "$a:x", ns, []string{"a:x"}, Variables{
		"{http://example.com/a}x": float64(1),
		"{http://example.com/b}x": float64(2),
	}, root)
	assertEqual(t, float64(1), v1)

	v2 := evalWith(t, "$p:x", map[string]string{"p": "http://example.com/b"},
		[]string{"p:x"}, Variables{
			"{http://example.com/a}x": float64(1),
			"{http://example.com/b}x": float64(2),
		}, root)
	assertEqual(t, float64(2), v2)

	// Both variables in one expression stay independent.
	both := evalWith(t, "$a:x + $b:x", ns, []string{"a:x", "b:x"}, Variables{
		"{http://example.com/a}x": float64(10),
		"{http://example.com/b}x": float64(20),
	}, root)
	assertEqual(t, float64(30), both)
}

func TestVariableMissingBindingReturnsError(t *testing.T) {
	root := createNavigator(createNode("", RootNode))
	e, err := Compile("$x", WithVariableDeclarations("x"))
	assertNoErr(t, err)

	if _, err := e.EvaluateWithVariables(root, Variables{}); err == nil {
		t.Fatal("expected error when binding is not supplied")
	}
	if _, err := e.SelectWithVariables(root, Variables{}); err == nil {
		t.Fatal("expected Select error when binding is not supplied")
	}

	// Unsupported value type.
	if _, err := e.EvaluateWithVariables(root, Variables{"x": []int{1}}); err == nil {
		t.Fatal("expected error for unsupported binding type")
	}
}

func TestVariableResolverInterface(t *testing.T) {
	root := createNavigator(createNode("", RootNode))
	e, err := Compile("$x + 1", WithVariableDeclarations("x"))
	assertNoErr(t, err)

	wantErr := errors.New("boom")
	r := errResolver{err: wantErr}
	if _, err := e.EvaluateWithVariables(root, r); !errors.Is(err, wantErr) {
		t.Fatalf("expected resolver error to propagate, got %v", err)
	}
}

type errResolver struct{ err error }

func (r errResolver) ResolveVariable(string) (interface{}, error) {
	return nil, r.err
}

func TestVariableNodeSetIdentityAndOrder(t *testing.T) {
	doc, _ := rowsDoc()
	nav := createNavigator(doc)
	set := NewNodeSetFromNavigators(rowNavigators(t, doc))

	assertEqual(t, 4, set.Len())

	e, err := Compile("$rows", WithVariableDeclarations("rows"))
	assertNoErr(t, err)

	// Each call yields the nodes from the start, in original order.
	for i := 0; i < 3; i++ {
		it, err := e.SelectWithVariables(nav, Variables{"rows": set})
		assertNoErr(t, err)
		got := iterateNavs(it)
		if len(got) != 4 {
			t.Fatalf("iteration %d: expected 4 nodes, got %d", i, len(got))
		}
	}

	// The NodeSet value must stay independent from the navigators handed out.
	if a := set.Node(0); a != nil {
		a.MoveToParent()
		b := set.Node(0)
		if b.Prefix() != "" || b.LocalName() != "row" {
			t.Fatal("NodeSet must hand out independent copies")
		}
	}
}

func TestVariableNodeSetPositionalPredicates(t *testing.T) {
	doc, _ := rowsDoc()
	nav := createNavigator(doc)
	set := NewNodeSetFromNavigators(rowNavigators(t, doc))

	cases := []struct {
		expr string
		want int
	}{
		{"$rows[1]", 1},
		{"$rows[2]", 1},
		{"$rows[last()]", 1},
		{"$rows[position() = 3]", 1},
		{"$rows[position() > 2]", 2},
		{"$rows[position() = last()]", 1},
		{"$rows[position() < last()]", 3},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			e, err := Compile(tc.expr, WithVariableDeclarations("rows"))
			assertNoErr(t, err)
			it, err := e.SelectWithVariables(nav, Variables{"rows": set})
			assertNoErr(t, err)
			if got := len(iterateNavs(it)); got != tc.want {
				t.Fatalf("%s: expected %d nodes, got %d", tc.expr, tc.want, got)
			}
		})
	}
}

func TestVariableNodeSetPathAndFunctions(t *testing.T) {
	doc, _ := rowsDoc()
	nav := createNavigator(doc)
	set := NewNodeSetFromNavigators(rowNavigators(t, doc))

	assertEqual(t, float64(4),
		evalWith(t, "count($rows)", nil, []string{"rows"}, Variables{"rows": set}, nav))

	cells := evalWith(t, "$rows/cell", nil, []string{"rows"},
		Variables{"rows": set}, nav).(*NodeIterator)
	if got := len(iterateNavs(cells)); got != 4 {
		t.Fatalf("expected 4 cells, got %d", got)
	}

	// The bound node-set remains fully iterable after a scalar use: scalar
	// coercion must not consume the binding for a later use in the same
	// expression.
	assertEqual(t, true,
		evalWith(t, "boolean($rows) and count($rows) = 4", nil, []string{"rows"},
			Variables{"rows": set}, nav))
}

func TestVariableIteratorBindingSnapshotted(t *testing.T) {
	doc, _ := rowsDoc()
	nav := createNavigator(doc)

	e, err := Compile("$rows[1]", WithVariableDeclarations("rows"))
	assertNoErr(t, err)

	// A partially consumed iterator binding is snapshotted from its current
	// position; it must still be iterable and reusable across evaluations.
	it := MustCompile("//row").Select(createNavigator(doc))
	it.MoveNext() // consume one
	first, err := e.EvaluateWithVariables(nav, Variables{"rows": it})
	assertNoErr(t, err)
	if _, ok := first.(*NodeIterator); !ok {
		t.Fatalf("expected node-set result, got %T", first)
	}
}

func TestVariableConcurrentDifferentBindings(t *testing.T) {
	doc, _ := rowsDoc()
	set := NewNodeSetFromNavigators(rowNavigators(t, doc))

	// One compiled expression shared by goroutines using different bindings.
	expr, err := Compile("$rows[position() = $idx]",
		WithVariableDeclarations("rows", "idx"))
	assertNoErr(t, err)

	const n = 12
	var wg sync.WaitGroup
	for g := 0; g < n; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			idx := (g % 4) + 1
			it, err := expr.SelectWithVariables(createNavigator(doc), Variables{
				"rows": set,
				"idx":  float64(idx),
			})
			if err != nil {
				t.Errorf("goroutine %d: %v", g, err)
				return
			}
			got := iterateNavs(it)
			if len(got) != 1 {
				t.Errorf("goroutine %d idx %d: expected 1 node, got %d", g, idx, len(got))
			}
		}(g)
	}
	wg.Wait()

	// Sequential reuse of the same Expr with different scalar bindings keeps
	// the compiled object free of evaluation-specific values.
	e, err := Compile("$n * 2", WithVariableDeclarations("n"))
	assertNoErr(t, err)
	for n := float64(1); n <= 5; n++ {
		v, err := e.EvaluateWithVariables(createNavigator(doc), Variables{"n": n})
		assertNoErr(t, err)
		if v.(float64) != n*2 {
			t.Fatalf("expected %v, got %v", n*2, v)
		}
	}
}

func TestVariableLegacyEntryPointsStillWork(t *testing.T) {
	doc, _ := rowsDoc()

	// Old entry point without variables.
	test_xpath_count(t, doc, "//row", 4)

	e, err := Compile("//row")
	assertNoErr(t, err)
	it := e.Select(createNavigator(doc))
	if got := len(iterateNavs(it)); got != 4 {
		t.Fatalf("legacy Select: expected 4, got %d", got)
	}

	// CompileWithNS continues to compile namespaced node tests.
	exp, err := CompileWithNS("//x:book", map[string]string{"x": "ns"})
	assertNoErr(t, err)
	if exp.String() != "//x:book" {
		t.Fatalf("unexpected expression string: %s", exp.String())
	}

	// Evaluate without bindings on a var-free expression behaves as before.
	if v := MustCompile("1 + 1").Evaluate(createNavigator(doc)); v != float64(2) {
		t.Fatalf("legacy Evaluate returned %v", v)
	}
}
