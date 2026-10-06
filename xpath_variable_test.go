package xpath

import (
	"fmt"
	"sync"
	"testing"
)

func errf(format string, args ...interface{}) error {
	return fmt.Errorf(format, args...)
}

func variableBookDoc() (*TNode, []NodeNavigator) {
	doc := createNode("", RootNode)
	books := doc.createChildNode("books", ElementNode)
	navs := make([]NodeNavigator, 0, 4)
	for i := 1; i <= 3; i++ {
		book := books.createChildNode("book", ElementNode)
		book.createChildNode(string(rune('0'+i)), TextNode)
		navs = append(navs, createNavigator(book))
	}
	return doc, navs
}

func evalValues(t *testing.T, expr *Expr, root NodeNavigator, b Bindings) []string {
	t.Helper()
	it := expr.SelectWith(root, b)
	var out []string
	for it.MoveNext() {
		out = append(out, it.Current().Value())
	}
	return out
}

func TestCompileVariable_Undeclared(t *testing.T) {
	// A variable reference that was never declared is a compile error in every
	// position: bare, nested in a path, an operand or a predicate.
	for _, expr := range []string{"$x", "$x/@attr", "$var + 1", "//a[$x]"} {
		if _, err := CompileWith(expr, nil); err == nil {
			t.Errorf("CompileWith(%q) expected an undeclared variable error, got nil", expr)
		}
	}
}

func TestCompileVariable_DeclaredButNotProvided(t *testing.T) {
	doc, _ := variableBookDoc()
	root := createNavigator(doc)

	// XPath 1.0 has no "unbound" state: a declared variable without a binding
	// behaves as an empty node-set.
	count, err := CompileWith("count($x)", nil, Var("x"))
	assertNoErr(t, err)
	assertEqual(t, float64(0), count.EvaluateWith(root, nil))

	boolean, err := CompileWith("boolean($x)", nil, Var("x"))
	assertNoErr(t, err)
	assertEqual(t, false, boolean.EvaluateWith(root, nil))

	str, err := CompileWith("string($x)", nil, Var("x"))
	assertNoErr(t, err)
	assertEqual(t, "", str.EvaluateWith(root, nil))
}

func TestCompileVariable_ScalarTypes(t *testing.T) {
	doc, _ := variableBookDoc()
	root := createNavigator(doc)

	num, _ := CompileWith("$n + 1", nil, Var("n"))
	assertEqual(t, float64(41), num.EvaluateWith(root, Bind(Var("n"), 40.0)))

	// ints are not XPath 1.0 numbers; only float64 is accepted.
	assertPanic(t, func() { num.EvaluateWith(root, Bind(Var("n"), 40)) })

	s, _ := CompileWith("concat('hello ', $s)", nil, Var("s"))
	assertEqual(t, "hello world", s.EvaluateWith(root, Bind(Var("s"), "world")))

	b, _ := CompileWith("$b and true()", nil, Var("b"))
	assertEqual(t, true, b.EvaluateWith(root, Bind(Var("b"), true)))
	assertEqual(t, false, b.EvaluateWith(root, Bind(Var("b"), false)))

	// unsupported type is rejected rather than silently mis-coerced
	assertPanic(t, func() { num.EvaluateWith(root, Bind(Var("n"), []int{1})) })
}

func TestCompileVariable_NodeSetIdentityOrderAndCopy(t *testing.T) {
	doc, navs := variableBookDoc()
	root := createNavigator(doc)

	// Order from the bound slice is preserved, and the values returned are
	// independent copies: mutating a result navigator must not affect the
	// binding or a later iteration.
	e, err := CompileWith("$bs", nil, Var("bs"))
	assertNoErr(t, err)

	for attempt := 0; attempt < 2; attempt++ {
		got := evalValues(t, e, root, Bind(Var("bs"), navs))
		assertEqual(t, []string{"1", "2", "3"}, got)
	}

	// A single NodeNavigator binding is a one-node node-set.
	one, _ := CompileWith("count($bs)", nil, Var("bs"))
	assertEqual(t, float64(1), one.EvaluateWith(root, Bind(Var("bs"), navs[0])))

	// Coercions take the first node for string/number and non-emptiness for
	// boolean.
	boolean, _ := CompileWith("boolean($bs)", nil, Var("bs"))
	assertEqual(t, true, boolean.EvaluateWith(root, Bind(Var("bs"), navs)))
	str, _ := CompileWith("string($bs)", nil, Var("bs"))
	assertEqual(t, "1", str.EvaluateWith(root, Bind(Var("bs"), navs)))
}

func TestCompileVariable_NodeSetNotConsumedAcrossEvaluations(t *testing.T) {
	doc, navs := variableBookDoc()
	root := createNavigator(doc)

	// The compiled expression stores no value; consuming the node-set in one
	// evaluation must not leave an exhausted iterator for the next call.
	e, _ := CompileWith("count($bs)", nil, Var("bs"))
	for i := 0; i < 3; i++ {
		assertEqual(t, float64(3), e.EvaluateWith(root, Bind(Var("bs"), navs)))
	}

	// Two independent references to the same variable in one expression each
	// iterate a fresh node-set rather than sharing a consumed cursor.
	twice, _ := CompileWith("count($bs) + count($bs)", nil, Var("bs"))
	assertEqual(t, float64(6), twice.EvaluateWith(root, Bind(Var("bs"), navs)))
}

func TestCompileVariable_NodeSetPathAndPositional(t *testing.T) {
	doc, navs := variableBookDoc()
	root := createNavigator(doc)

	// A node-set variable is a usable starting sequence for further paths.
	text, _ := CompileWith("$bs/text()", nil, Var("bs"))
	assertEqual(t, []string{"1", "2", "3"}, evalValues(t, text, root, Bind(Var("bs"), navs)))

	// Numeric predicates and position()/last() resolve against the bound
	// node-set's own sequence, not against physical siblings.
	cases := map[string][]string{
		"$bs[1]":                 {"1"},
		"$bs[2]":                 {"2"},
		"$bs[last()]":            {"3"},
		"$bs[position()=2]":      {"2"},
		"$bs[position()>1]":      {"2", "3"},
		"$bs[last()-1]":          {"2"},
		"$bs[position()=last()]": {"3"},
	}
	for expr, want := range cases {
		e, err := CompileWith(expr, nil, Var("bs"))
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		assertEqual(t, want, evalValues(t, e, root, Bind(Var("bs"), navs)))
	}

	// Positional filtering does not mutate the bound slice: run it twice.
	last, _ := CompileWith("$bs[last()]", nil, Var("bs"))
	assertEqual(t, []string{"3"}, evalValues(t, last, root, Bind(Var("bs"), navs)))
	assertEqual(t, []string{"3"}, evalValues(t, last, root, Bind(Var("bs"), navs)))

	// Grouped node-set variables take the group/lastFuncQuery path.
	grouped, _ := CompileWith("($bs)[2]", nil, Var("bs"))
	assertEqual(t, []string{"2"}, evalValues(t, grouped, root, Bind(Var("bs"), navs)))
	groupedLast, _ := CompileWith("($bs)[last()]", nil, Var("bs"))
	assertEqual(t, []string{"3"}, evalValues(t, groupedLast, root, Bind(Var("bs"), navs)))
}

func TestCompileVariable_NamespaceResolution(t *testing.T) {
	doc, _ := variableBookDoc()
	root := createNavigator(doc)
	ns := map[string]string{"a": "http://example.com/a", "b": "http://example.com/b"}

	// The same local name in two namespaces resolves to two distinct
	// variables according to the compile-time namespace map.
	e, err := CompileWith("$a:x = $b:x", ns, Var("a:x", ns), Var("b:x", ns))
	assertNoErr(t, err)
	bindings := Bind(Var("a:x", ns), "same").Bind(Var("b:x", ns), "different")
	assertEqual(t, false, e.EvaluateWith(root, bindings))
	bindings = Bind(Var("a:x", ns), "z").Bind(Var("b:x", ns), "z")
	assertEqual(t, true, e.EvaluateWith(root, bindings))

	// An unprefixed variable is in the empty namespace and differs from a
	// prefixed one with the same local name.
	plain, err := CompileWith("$x", ns, Var("x"), Var("a:x", ns))
	assertNoErr(t, err)
	assertEqual(t, "plain", plain.EvaluateWith(root,
		Bind(Var("x"), "plain").Bind(Var("a:x", ns), "namespaced")))

	// An undeclared prefix on the variable reference is a compile error.
	if _, err := CompileWith("$zz:x", nil, Var("x")); err == nil {
		t.Fatal("expected error for undeclared namespace prefix")
	}
}

func TestCompileVariable_ConcurrentDistinctBindings(t *testing.T) {
	doc, navs := variableBookDoc()
	root := createNavigator(doc)

	// One compiled scalar expression evaluated concurrently with different
	// bindings must compute independent results.
	num, _ := CompileWith("$n * 2", nil, Var("n"))
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(v float64) {
			defer wg.Done()
			got := num.EvaluateWith(root, Bind(Var("n"), v)).(float64)
			if got != v*2 {
				errs <- errf("scalar: got %v want %v", got, v*2)
			}
		}(float64(i))
	}

	// The same compiled positional node-set expression run concurrently must
	// independently select position 2 of each caller's own binding.
	pos, _ := CompileWith("$bs[position()=2]", nil, Var("bs"))
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			it := pos.SelectWith(root, Bind(Var("bs"), navs))
			var got []string
			for it.MoveNext() {
				got = append(got, it.Current().Value())
			}
			if len(got) != 1 || got[0] != "2" {
				errs <- errf("positional: got %v", got)
			}
		}()
	}

	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

func TestCompileVariable_LegacyEntryPointsUnaffected(t *testing.T) {
	doc, navs := variableBookDoc()
	root := createNavigator(doc)

	// Legacy Compile/Evaluate/Select keep working and never see variable
	// state: an undeclared variable remains a compile error.
	if _, err := Compile("$x + 1"); err == nil {
		t.Fatal("legacy Compile must reject undeclared variable")
	}
	plain := MustCompile("1 + 2")
	assertEqual(t, float64(3), plain.Evaluate(root))

	// A variable-free expression compiled with CompileWith behaves identically.
	e, err := CompileWith("count(//book)", nil)
	assertNoErr(t, err)
	_ = navs
	assertEqual(t, float64(3), e.Evaluate(root))

	// Evaluate with nil bindings equals the legacy path.
	e2, _ := CompileWith("true()", nil)
	assertEqual(t, true, e2.EvaluateWith(root, nil))
}
