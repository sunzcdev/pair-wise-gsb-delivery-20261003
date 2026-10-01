package parser_test

import (
	"testing"

	. "github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/internal/testify/assert"
	"github.com/expr-lang/expr/internal/testify/require"
	"github.com/expr-lang/expr/parser"
)

func TestParseMatch(t *testing.T) {
	tree, err := parser.Parse(`
match event {
  Created{ID: id} -> { id }
  Paid if id > 0 -> { id }
  _ -> { 0 }
}`)
	require.NoError(t, err)

	match, ok := tree.Node.(*MatchNode)
	require.True(t, ok)
	assert.IsType(t, &IdentifierNode{}, match.Expr)
	require.Equal(t, 3, len(match.Cases))

	first := match.Cases[0]
	variant, ok := first.Pattern.(*VariantPatternNode)
	require.True(t, ok)
	assert.Equal(t, "Created", variant.Name)
	require.Equal(t, 1, len(variant.Fields))
	assert.Equal(t, "ID", variant.Fields[0].Name)
	assert.IsType(t, &BindingPatternNode{}, variant.Fields[0].Pattern)

	second := match.Cases[1]
	assert.IsType(t, &BindingPatternNode{}, second.Pattern)
	assert.NotNil(t, second.Guard)

	third := match.Cases[2]
	assert.IsType(t, &WildcardPatternNode{}, third.Pattern)
	assert.Nil(t, third.Guard)
}

func TestParseMatchLiteralPatterns(t *testing.T) {
	tree, err := parser.Parse(`match n { 1 -> { "one" } "x" -> { "x" } nil -> { 0 } }`)
	require.NoError(t, err)

	match := tree.Node.(*MatchNode)
	require.Equal(t, 3, len(match.Cases))
	assert.IsType(t, &LiteralPatternNode{}, match.Cases[0].Pattern)
	assert.IsType(t, &LiteralPatternNode{}, match.Cases[1].Pattern)
	assert.IsType(t, &NilPatternNode{}, match.Cases[2].Pattern)
}

func TestParseMatchErrors(t *testing.T) {
	tests := []struct{ name, input string }{
		{"missing arrow", `match e { A -> 1 }`},
		{"missing body braces", `match e { A -> }`},
		{"unclosed", `match e { A -> { 1 }`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parser.Parse(tt.input)
			assert.Error(t, err)
		})
	}
}

func TestPrintMatch(t *testing.T) {
	tree, err := parser.Parse(`match e { A{x: x} if x > 0 -> { x } _ -> { 0 } }`)
	require.NoError(t, err)
	assert.Contains(t, tree.Node.String(), "match e")
	assert.Contains(t, tree.Node.String(), "-> {")
}
