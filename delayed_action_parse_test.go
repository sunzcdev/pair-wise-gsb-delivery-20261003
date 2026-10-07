package main

import (
	"testing"

	"github.com/mna/pigeon/ast"
)

func TestParseDelayedAction(t *testing.T) {
	cases := []struct {
		gram     string
		deferred bool
	}{
		{"R = \"a\" { return nil, nil }\n", false},
		{"R = \"a\" ~{ return nil, nil }\n", true},
		{"R = \"a\"~{ return nil, nil }\n", true},
	}
	for i, c := range cases {
		g, err := Parse("test", []byte(c.gram))
		if err != nil {
			t.Fatalf("%d: unexpected parse error: %v", i, err)
		}
		rules := g.(*ast.Grammar).Rules
		act, ok := rules[0].Expr.(*ast.ActionExpr)
		if !ok {
			t.Fatalf("%d: want *ast.ActionExpr, got %T", i, rules[0].Expr)
		}
		if act.Deferred != c.deferred {
			t.Errorf("%d: Deferred = %v, want %v", i, act.Deferred, c.deferred)
		}
	}
}
