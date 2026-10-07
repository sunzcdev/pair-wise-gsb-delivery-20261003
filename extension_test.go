package xpath

import (
	"errors"
	"testing"
)

const extBusinessNS = "http://example.com/business"

// newClassificationDoc builds:
//
//	<rules>
//	  <item code="A1">alpha</item>
//	  <item code="B2">beta</item>
//	  <item code="C3">gamma</item>
//	</rules>
func newClassificationDoc() *TNode {
	doc := createNode("", RootNode)
	rules := doc.createChildNode("rules", ElementNode)
	for _, c := range []struct{ code, text string }{
		{"A1", "alpha"},
		{"B2", "beta"},
		{"C3", "gamma"},
	} {
		item := rules.createChildNode("item", ElementNode)
		item.addAttribute("code", c.code)
		item.createChildNode(c.text, TextNode)
	}
	return doc
}

func classificationRegistry(table map[string]string, calls *[]string) *FunctionRegistry {
	r := NewFunctionRegistry()
	r.MustRegister(&FunctionSpec{
		NamespaceURI: extBusinessNS,
		LocalName:    "classify",
		Params:       []ExtensionType{ExtensionString},
		Return:       ExtensionString,
		Func: func(ctx ExtensionContext, args []interface{}) (interface{}, error) {
			if calls != nil {
				*calls = append(*calls, args[0].(string))
			}
			return table[args[0].(string)], nil
		},
	})
	return r
}

func extNS() map[string]string {
	return map[string]string{"biz": extBusinessNS}
}

func mustCompileExt(t *testing.T, expr string, ns map[string]string, r *FunctionRegistry) *Expr {
	t.Helper()
	e, err := CompileWithExtensions(expr, ns, r)
	assertNoErr(t, err)
	return e
}

func TestExtensionCompileSignatureChecks(t *testing.T) {
	r := classificationRegistry(map[string]string{"A1": "group-a"}, nil)
	for _, tc := range []struct {
		expr string
		ok   bool
	}{
		{`biz:classify('A1')`, true},
		{`biz:classify()`, false},           // too few args
		{`biz:classify('A1', 'B2')`, false}, // too many args
		{`other:classify('A1')`, false},     // unbound prefix
		{`biz:unknown('A1')`, false},        // unregistered local name
		{`biz:classify(//missing)`, true},   // node-set -> string is a legal XPath conversion
		{`biz:classify(@code)`, true},       // attribute node-set -> string
		{`biz:classify(1 > 2)`, true},       // boolean -> string conversion
		{`biz:classify(42)`, true},          // number -> string conversion
		{`biz:classify-nope('A1')`, false},  // bad token, still an error
	} {
		_, err := CompileWithExtensions(tc.expr, extNS(), r)
		if tc.ok {
			assertNoErr(t, err)
		} else {
			assertErr(t, err)
		}
	}

	// Plain Compile must not see extension functions: no package-global state.
	if _, err := Compile(`biz:classify('A1')`); err == nil {
		t.Fatal("plain Compile unexpectedly resolved extension function")
	}

	// Statically scalar argument for a node-set parameter is rejected.
	nodeSetOnly := NewFunctionRegistry()
	nodeSetOnly.MustRegister(&FunctionSpec{
		NamespaceURI: extBusinessNS,
		LocalName:    "count-em",
		Params:       []ExtensionType{ExtensionNodeSet},
		Return:       ExtensionNumber,
		Func: func(_ ExtensionContext, args []interface{}) (interface{}, error) {
			return float64(len(args[0].([]NodeNavigator))), nil
		},
	})
	if _, err := CompileWithExtensions(`biz:count-em('scalar')`, extNS(), nodeSetOnly); err == nil {
		t.Fatal("expected compile error for scalar argument to node-set parameter")
	}
	if _, err := CompileWithExtensions(`biz:count-em(//item)`, extNS(), nodeSetOnly); err != nil {
		t.Fatalf("node-set argument should compile, got %v", err)
	}
}

func TestExtensionRegistryIsolation(t *testing.T) {
	doc := newClassificationDoc()
	// Two independent registries bind the same QName to different
	// implementations. Neither may observe the other's function.
	r1 := classificationRegistry(map[string]string{"A1": "REGISTRY-1"}, nil)
	r2 := classificationRegistry(map[string]string{"A1": "REGISTRY-2"}, nil)

	// A scalar function used as a top-level expression returns its value
	// directly; here evaluate it without a location path.
	top1 := mustCompileExt(t, `biz:classify('A1')`, extNS(), r1)
	top2 := mustCompileExt(t, `biz:classify('A1')`, extNS(), r2)
	v1, err := top1.Eval(createNavigator(doc), nil)
	assertNoErr(t, err)
	v2, err := top2.Eval(createNavigator(doc), nil)
	assertNoErr(t, err)
	assertEqual(t, "REGISTRY-1", v1)
	assertEqual(t, "REGISTRY-2", v2)

	// Duplicate registration in the same registry is rejected.
	if err := r1.Register(&FunctionSpec{
		NamespaceURI: extBusinessNS, LocalName: "classify",
		Params: []ExtensionType{ExtensionString}, Return: ExtensionString,
		Func: func(ExtensionContext, []interface{}) (interface{}, error) { return "", nil },
	}); err == nil {
		t.Fatal("expected duplicate registration error")
	}
}

func TestExtensionScalarEvaluationAndConversions(t *testing.T) {
	doc := newClassificationDoc()
	var seen struct {
		boolArg   bool
		numberArg float64
		stringArg string
		ctxNode   string
	}
	r := NewFunctionRegistry()
	r.MustRegister(&FunctionSpec{
		NamespaceURI: extBusinessNS, LocalName: "score",
		Params: []ExtensionType{ExtensionBoolean, ExtensionNumber, ExtensionString},
		Return: ExtensionNumber,
		Func: func(ctx ExtensionContext, args []interface{}) (interface{}, error) {
			seen.boolArg = args[0].(bool)
			seen.numberArg = args[1].(float64)
			seen.stringArg = args[2].(string)
			seen.ctxNode = ctx.ContextNode.LocalName()
			if !seen.boolArg {
				return float64(0), nil
			}
			return seen.numberArg * 2, nil
		},
	})
	// '' -> boolean() is false; 10.5 is a number; 7 -> string "7".
	e := mustCompileExt(t, `biz:score('', 10.5, 7)`, extNS(), r)
	v, err := e.Eval(createNavigator(doc), nil)
	assertNoErr(t, err)
	assertEqual(t, float64(0), v)
	assertEqual(t, false, seen.boolArg)
	assertEqual(t, 10.5, seen.numberArg)
	assertEqual(t, "7", seen.stringArg)

	// Node-set string argument uses the string-value of the first node.
	calls := []string{}
	rc := classificationRegistry(map[string]string{"A1": "ok", "B2": "ok"}, &calls)
	ec := mustCompileExt(t, `//item[biz:classify(@code) = 'ok']`, extNS(), rc)
	iter, err := ec.Eval(createNavigator(doc), nil)
	assertNoErr(t, err)
	nodes := iterateNavs(iter.(*NodeIterator))
	assertEqual(t, 2, len(nodes))
	assertEqual(t, []string{"A1", "B2", "C3"}, calls)
	assertNoErr(t, iter.(*NodeIterator).Err())
}

func TestExtensionNestedCallsInPredicate(t *testing.T) {
	doc := newClassificationDoc()
	r := NewFunctionRegistry()
	// lookup(code) -> category string; category(name) -> boolean.
	r.MustRegister(&FunctionSpec{
		NamespaceURI: extBusinessNS, LocalName: "lookup",
		Params: []ExtensionType{ExtensionString}, Return: ExtensionString,
		Func: func(_ ExtensionContext, args []interface{}) (interface{}, error) {
			switch args[0] {
			case "A1":
				return "premium", nil
			case "B2":
				return "standard", nil
			default:
				return "other", nil
			}
		},
	})
	r.MustRegister(&FunctionSpec{
		NamespaceURI: extBusinessNS, LocalName: "is-category",
		Params: []ExtensionType{ExtensionString, ExtensionString}, Return: ExtensionBoolean,
		Func: func(_ ExtensionContext, args []interface{}) (interface{}, error) {
			return args[0] == args[1], nil
		},
	})
	// Nested extension call inside a predicate: the outer function receives
	// the string result of the inner one.
	e := mustCompileExt(t,
		`//item[biz:is-category(biz:lookup(@code), 'premium')]`,
		extNS(), r)
	iter := e.Select(createNavigator(doc))
	var selectedCodes []string
	for iter.MoveNext() {
		cur := iter.Current()
		attr := cur.Copy()
		for attr.MoveToNextAttribute() {
			if attr.LocalName() == "code" {
				selectedCodes = append(selectedCodes, attr.Value())
			}
		}
	}
	assertNoErr(t, iter.Err())
	if len(selectedCodes) != 1 || selectedCodes[0] != "A1" {
		t.Fatalf("expected only the A1 item, got %v", selectedCodes)
	}

	// Nested call feeding an XPath comparison and arithmetic.
	rm := NewFunctionRegistry()
	rm.MustRegister(&FunctionSpec{
		NamespaceURI: extBusinessNS, LocalName: "rank",
		Params: []ExtensionType{ExtensionString}, Return: ExtensionNumber,
		Func: func(_ ExtensionContext, args []interface{}) (interface{}, error) {
			return map[string]float64{"A1": 1, "B2": 2, "C3": 3}[args[0].(string)], nil
		},
	})
	em := mustCompileExt(t, `//item[biz:rank(@code) >= 2]`, extNS(), rm)
	gotIter := em.Select(createNavigator(doc))
	gotCount := 0
	for gotIter.MoveNext() {
		gotCount++
	}
	assertNoErr(t, gotIter.Err())
	assertEqual(t, 2, gotCount)

	// Number-returning extension used positionally in the predicate path:
	// [biz:rank(@code)] selects the node at that position within //item.
	ep := mustCompileExt(t, `//item[biz:rank(@code)]`, extNS(), rm)
	posIter := ep.Select(createNavigator(doc))
	positional := 0
	for posIter.MoveNext() {
		positional++
	}
	assertNoErr(t, posIter.Err())
	// ranks A1=1,B2=2,C3=3 select themselves by position.
	assertEqual(t, 3, positional)
}

var errClassificationUnavailable = errors.New("classification service unavailable")

func TestExtensionErrorPreservedAndExpressionReused(t *testing.T) {
	doc := newClassificationDoc()
	var invocations int
	r := NewFunctionRegistry()
	sentinel := errClassificationUnavailable
	r.MustRegister(&FunctionSpec{
		NamespaceURI: extBusinessNS, LocalName: "classify",
		Params: []ExtensionType{ExtensionString}, Return: ExtensionString,
		Func: func(ctx ExtensionContext, args []interface{}) (interface{}, error) {
			invocations++
			if fail, _ := ctx.CallerContext.(bool); fail {
				return nil, sentinel
			}
			return "ok-" + args[0].(string), nil
		},
	})

	// Scalar Eval path: the error is returned, wrapped but unwrappable,
	// never folded into false.
	e := mustCompileExt(t, `biz:classify('A1')`, extNS(), r)
	v, err := e.Eval(createNavigator(doc), true)
	if err == nil {
		t.Fatal("expected error from extension function")
	}
	if v != nil {
		t.Fatalf("expected nil value on error, got %v", v)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected errors.Is to find sentinel, got %v", err)
	}
	var extErr *ExtensionError
	if !errors.As(err, &extErr) {
		t.Fatalf("expected *ExtensionError, got %T", err)
	}
	if extErr.NamespaceURI != extBusinessNS || extErr.LocalName != "classify" {
		t.Fatalf("unexpected error identity: %+v", extErr)
	}

	// Iterator path inside a predicate: the failing node must not silently
	// match; iteration terminates with Err() set.
	ep := mustCompileExt(t, `//item[biz:classify(@code) = 'ok-A1']`, extNS(), r)
	iter := ep.SelectWithContext(createNavigator(doc), true)
	if iter.MoveNext() {
		t.Fatal("predicate with an erroring extension must not select a node")
	}
	if iter.Err() == nil || !errors.Is(iter.Err(), sentinel) {
		t.Fatalf("expected iterator Err to retain sentinel, got %v", iter.Err())
	}

	// Reuse the compiled expressions after the failures: each evaluation
	// clones the query tree, so the failed run left no poisoned state.
	invocations = 0
	cleanDoc := newClassificationDoc()
	if got, err := e.Eval(createNavigator(cleanDoc), false); err != nil || got != "ok-A1" {
		t.Fatalf("scalar reuse failed: got %v err %v", got, err)
	}
	iter2 := ep.SelectWithContext(createNavigator(cleanDoc), false)
	var reuseCodes []string
	for iter2.MoveNext() {
		attr := iter2.Current().Copy()
		for attr.MoveToNextAttribute() {
			if attr.LocalName() == "code" {
				reuseCodes = append(reuseCodes, attr.Value())
			}
		}
	}
	if err := iter2.Err(); err != nil {
		t.Fatalf("reuse predicate iteration error: %v", err)
	}
	if len(reuseCodes) != 1 || reuseCodes[0] != "A1" {
		t.Fatalf("expected exactly A1 after reuse, got %v", reuseCodes)
	}

	// The erroring expression itself is reusable with a healthy context.
	v, err = ep.Eval(createNavigator(cleanDoc), false)
	if err != nil {
		t.Fatalf("unexpected error on clean data: %v", err)
	}
	it := v.(*NodeIterator)
	if n := len(iterateNavs(it)); n != 1 || it.Err() != nil {
		t.Fatalf("expected one clean match, n=%d err=%v", n, it.Err())
	}
}

func TestExtensionNodeSetResultAndOwnership(t *testing.T) {
	doc := newClassificationDoc()
	r := NewFunctionRegistry()
	// Returns all items whose code starts with the given letter.
	r.MustRegister(&FunctionSpec{
		NamespaceURI: extBusinessNS, LocalName: "items-with",
		Params: []ExtensionType{ExtensionString},
		Return: ExtensionNodeSet,
		Func: func(ctx ExtensionContext, args []interface{}) (interface{}, error) {
			prefix := args[0].(string)
			// Start from the document root via the context node copy and
			// collect matching item elements.
			nav := ctx.ContextNode.Copy()
			nav.MoveToRoot()
			// move to first item descendant
			if !nav.MoveToChild() { // rules
				return []NodeNavigator{}, nil
			}
			var out []NodeNavigator
			if nav.MoveToChild() { // first item
				for {
					code := ""
					attrNav := nav.Copy()
					for attrNav.MoveToNextAttribute() {
						if attrNav.LocalName() == "code" {
							code = attrNav.Value()
						}
					}
					if len(code) > 0 && code[:1] == prefix {
						out = append(out, nav.Copy())
					}
					if !nav.MoveToNext() {
						break
					}
				}
			}
			return out, nil
		},
	})

	e := mustCompileExt(t, `biz:items-with('B')/text()`, extNS(), r)
	iter := e.Select(createNavigator(doc))
	var values []string
	for iter.MoveNext() {
		values = append(values, iter.Current().Value())
	}
	assertNoErr(t, iter.Err())
	assertEqual(t, 1, len(values))
	assertEqual(t, "beta", values[0])

	// Boolean declared return with a node-set implementation result applies
	// XPath node-set-to-boolean (non-empty => true).
	rb := NewFunctionRegistry()
	rb.MustRegister(&FunctionSpec{
		NamespaceURI: extBusinessNS, LocalName: "has",
		Params: []ExtensionType{ExtensionNodeSet}, Return: ExtensionBoolean,
		Func: func(_ ExtensionContext, args []interface{}) (interface{}, error) {
			return args[0], nil // hand the node-set back; coerceResult maps non-empty to true
		},
	})
	eb := mustCompileExt(t, `biz:has(//item[@code='B2'])`, extNS(), rb)
	v, err := eb.Eval(createNavigator(doc), nil)
	assertNoErr(t, err)
	assertEqual(t, true, v)

	// Node-set argument is materialized as []NodeNavigator (empty for no match).
	assertNoErr(t, err)
	ebEmpty := mustCompileExt(t, `biz:has(//item[@code='ZZ'])`, extNS(), rb)
	v, err = ebEmpty.Eval(createNavigator(doc), nil)
	assertNoErr(t, err)
	assertEqual(t, false, v)

	// Ownership: navigating returned navigators does not move the source
	// document's current node. Iterate twice from the same compiled
	// expression to prove the engine copied the returned navigators.
	e2 := mustCompileExt(t, `biz:items-with('A')`, extNS(), r)
	for i := 0; i < 2; i++ {
		it2 := e2.Select(createNavigator(doc))
		var got int
		for it2.MoveNext() {
			got++
		}
		assertNoErr(t, it2.Err())
		assertEqual(t, 1, got)
	}
}

func TestExtensionPositionLastAndCallerContext(t *testing.T) {
	doc := newClassificationDoc()
	type recorded struct {
		pos, last int
		caller    string
		nodeCode  string
	}
	var records []recorded
	r := NewFunctionRegistry()
	r.MustRegister(&FunctionSpec{
		NamespaceURI: extBusinessNS, LocalName: "where",
		Params: []ExtensionType{}, Return: ExtensionString,
		Func: func(ctx ExtensionContext, _ []interface{}) (interface{}, error) {
			code := ""
			attr := ctx.ContextNode.Copy()
			for attr.MoveToNextAttribute() {
				if attr.LocalName() == "code" {
					code = attr.Value()
				}
			}
			caller, _ := ctx.CallerContext.(string)
			records = append(records, recorded{
				pos: ctx.Position, last: ctx.Last,
				caller: caller, nodeCode: code,
			})
			return code, nil
		},
	})

	// The recorder runs inside a predicate (a non-empty string is true)
	// where position/last are defined.
	e := mustCompileExt(t, `//item[biz:where()]`, extNS(), r)
	v, err := e.Eval(createNavigator(doc), "request-42")
	assertNoErr(t, err)
	got := iterateNavs(v.(*NodeIterator))
	assertEqual(t, 3, len(got))

	for i, rec := range records {
		if rec.pos != i+1 {
			t.Fatalf("call %d: position = %d, want %d", i, rec.pos, i+1)
		}
		if rec.last != 3 {
			t.Fatalf("call %d: last = %d, want 3", i, rec.last)
		}
		if rec.caller != "request-42" {
			t.Fatalf("call %d: caller context = %q, want request-42", i, rec.caller)
		}
	}
	assertEqual(t, "A1", records[0].nodeCode)
	assertEqual(t, "B2", records[1].nodeCode)
	assertEqual(t, "C3", records[2].nodeCode)

	// Without a caller context the value is nil.
	records = nil
	v, err = e.Eval(createNavigator(doc), nil)
	assertNoErr(t, err)
	iterateNavs(v.(*NodeIterator))
	for _, rec := range records {
		if rec.caller != "" {
			t.Fatalf("expected empty caller, got %q", rec.caller)
		}
	}

	// SelectWithContext threads the same context through node iteration.
	records = nil
	ps := mustCompileExt(t, `//item[biz:where() = 'B2']`, extNS(), r)
	it := ps.SelectWithContext(createNavigator(doc), "iter-ctx")
	nodes := iterateNavs(it)
	assertNoErr(t, it.Err())
	assertEqual(t, 1, len(nodes))
	found := false
	for _, rec := range records {
		if rec.caller == "iter-ctx" {
			found = true
		}
	}
	if !found {
		t.Fatal("caller context was not propagated through SelectWithContext")
	}
}

func TestFunctionRegistryValidation(t *testing.T) {
	if err := NewFunctionRegistry().Register(nil); err == nil {
		t.Fatal("nil spec should be rejected")
	}
	valid := &FunctionSpec{
		NamespaceURI: extBusinessNS, LocalName: "f",
		Params: []ExtensionType{}, Return: ExtensionString,
		Func: func(ExtensionContext, []interface{}) (interface{}, error) { return "", nil },
	}
	r := NewFunctionRegistry()
	assertNoErr(t, r.Register(valid))
	for _, bad := range []*FunctionSpec{
		{NamespaceURI: "", LocalName: "f", Func: valid.Func},
		{NamespaceURI: extBusinessNS, LocalName: "", Func: valid.Func},
		{NamespaceURI: extBusinessNS, LocalName: "f", Func: valid.Func}, // duplicate
		{NamespaceURI: extBusinessNS, LocalName: "g"},                   // nil Func
	} {
		if err := r.Register(bad); err == nil {
			t.Fatalf("expected error registering %+v", bad)
		}
	}
}

func TestExtensionChainedFunctionSteps(t *testing.T) {
	doc := newClassificationDoc()
	r := NewFunctionRegistry()
	r.MustRegister(&FunctionSpec{
		NamespaceURI: extBusinessNS, LocalName: "self-items",
		Params: nil, Return: ExtensionNodeSet,
		Func: func(ctx ExtensionContext, _ []interface{}) (interface{}, error) {
			return []NodeNavigator{ctx.ContextNode}, nil
		},
	})
	r.MustRegister(&FunctionSpec{
		NamespaceURI: extBusinessNS, LocalName: "echo",
		Params: []ExtensionType{ExtensionNodeSet}, Return: ExtensionNodeSet,
		Func: func(_ ExtensionContext, args []interface{}) (interface{}, error) {
			return args[0].([]NodeNavigator), nil
		},
	})
	// preceding axis -> function step -> function step -> attribute axis.
	e := mustCompileExt(t, `//item/biz:self-items()/biz:echo(.)/@code`, extNS(), r)
	it := e.Select(createNavigator(doc))
	var codes []string
	for it.MoveNext() {
		codes = append(codes, it.Current().Value())
	}
	assertNoErr(t, it.Err())
	assertEqual(t, []string{"A1", "B2", "C3"}, codes)

	// A scalar-returning function is rejected statically as a path step.
	scalar := NewFunctionRegistry()
	scalar.MustRegister(&FunctionSpec{
		NamespaceURI: extBusinessNS, LocalName: "label",
		Params: nil, Return: ExtensionString,
		Func: func(ExtensionContext, []interface{}) (interface{}, error) { return "x", nil },
	})
	if _, err := CompileWithExtensions(`//item/biz:label()`, extNS(), scalar); err == nil {
		t.Fatal("expected compile error for scalar function used as a path step")
	}
}

func TestExtensionRepeatedAndConcurrentEvaluation(t *testing.T) {
	doc := newClassificationDoc()
	r := NewFunctionRegistry()
	r.MustRegister(&FunctionSpec{
		NamespaceURI: extBusinessNS, LocalName: "items-with",
		Params: []ExtensionType{ExtensionString},
		Return: ExtensionNodeSet,
		Func: func(ctx ExtensionContext, args []interface{}) (interface{}, error) {
			prefix := args[0].(string)
			nav := ctx.ContextNode.Copy()
			nav.MoveToRoot()
			nav.MoveToChild() // rules
			var out []NodeNavigator
			if nav.MoveToChild() {
				for {
					code := ""
					a := nav.Copy()
					for a.MoveToNextAttribute() {
						if a.LocalName() == "code" {
							code = a.Value()
						}
					}
					if code != "" && code[:1] == prefix {
						out = append(out, nav.Copy())
					}
					if !nav.MoveToNext() {
						break
					}
				}
			}
			return out, nil
		},
	})
	e := mustCompileExt(t, `biz:items-with('A')/text()`, extNS(), r)

	count := func() int {
		it := e.Select(createNavigator(doc))
		n := 0
		for it.MoveNext() {
			n++
		}
		assertNoErr(t, it.Err())
		return n
	}
	// Repeated evaluations on one compiled expression stay consistent.
	for i := 0; i < 5; i++ {
		assertEqual(t, 1, count())
	}

	// A single compiled expression is shared concurrently across goroutines;
	// each evaluation gets its own cloned query tree.
	done := make(chan int, 8)
	for g := 0; g < 8; g++ {
		go func() { done <- count() }()
	}
	for g := 0; g < 8; g++ {
		assertEqual(t, 1, <-done)
	}
}
