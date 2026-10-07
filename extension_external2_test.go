package xpath_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/antchfx/xpath"
)

// TestExtensionExternalAPI exercises the extension API purely through the
// exported surface from a consumer package, mirroring real business usage
// such as mapping a code to a standard classification.
func TestExtensionExternalAPI2(t *testing.T) {
	classifications := map[string]string{
		"A1": "premium",
		"B2": "standard",
	}

	registry := xpath.NewFunctionRegistry()
	if err := registry.Register(&xpath.FunctionSpec{
		NamespaceURI: "http://example.com/biz",
		LocalName:    "classify",
		Params:       []xpath.ExtensionType{xpath.ExtensionString},
		Return:       xpath.ExtensionString,
		Func: func(ctx xpath.ExtensionContext, args []interface{}) (interface{}, error) {
			code := args[0].(string)
			if fail, _ := ctx.CallerContext.(bool); fail {
				return nil, errors.New("service unavailable")
			}
			name, ok := classifications[code]
			if !ok {
				return "", nil
			}
			return name, nil
		},
	}); err != nil {
		t.Fatal(err)
	}

	ns := map[string]string{"biz": "http://example.com/biz"}

	// Scalar result.
	expr, err := xpath.CompileWithExtensions(`biz:classify('A1')`, ns, registry)
	if err != nil {
		t.Fatal(err)
	}
	root := &NodeNavigator{curr: &Node{}, root: &Node{}}
	got, err := expr.Eval(root, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "premium" {
		t.Fatalf("got %v, want premium", got)
	}

	// Error is preserved, not folded to false/empty.
	_, err = expr.Eval(root, true)
	if err == nil || !strings.Contains(err.Error(), "service unavailable") {
		t.Fatalf("expected service error, got %v", err)
	}

	// Reuse after the error returns to normal.
	got, err = expr.Eval(root, false)
	if err != nil || got != "premium" {
		t.Fatalf("reuse failed: got %v err %v", got, err)
	}

	// A second, independent registry may bind the same QName to a different
	// implementation without affecting the first.
	other := xpath.NewFunctionRegistry()
	other.MustRegister(&xpath.FunctionSpec{
		NamespaceURI: "http://example.com/biz",
		LocalName:    "classify",
		Params:       []xpath.ExtensionType{xpath.ExtensionString},
		Return:       xpath.ExtensionString,
		Func: func(xpath.ExtensionContext, []interface{}) (interface{}, error) {
			return "OTHER", nil
		},
	})
	exprOther, err := xpath.CompileWithExtensions(`biz:classify('A1')`, ns, other)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := exprOther.Eval(root, false); err != nil || v != "OTHER" {
		t.Fatalf("second registry isolation failed: v=%v err=%v", v, err)
	}
	if v, err := expr.Eval(root, false); err != nil || v != "premium" {
		t.Fatalf("first registry changed by second: v=%v err=%v", v, err)
	}

	// Compile-time signature checks.
	for _, bad := range []string{
		`biz:classify()`,
		`biz:classify('a', 'b')`,
		`biz:missing('a')`,
		`unk:classify('a')`,
	} {
		if _, err := xpath.CompileWithExtensions(bad, ns, registry); err == nil {
			t.Fatalf("expected compile error for %q", bad)
		}
	}
}
