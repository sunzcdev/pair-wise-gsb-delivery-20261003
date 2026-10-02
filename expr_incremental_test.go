package expr_test

import (
	"testing"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/internal/testify/require"
)

func TestNewSession(t *testing.T) {
	calls := 0
	fn := func(args ...any) (any, error) {
		calls++
		return args[0].(int), nil
	}
	program, err := expr.Compile(
		"cached(input) + 1",
		expr.Function("cached", fn, new(func(int) int)),
		expr.Optimize(false),
	)
	require.NoError(t, err)

	session, err := expr.NewSession(
		program,
		map[string]any{"input": 2},
		expr.ReusableFunction("cached", fn),
	)
	require.NoError(t, err)

	value, err := session.Run()
	require.NoError(t, err)
	require.Equal(t, 3, value)

	value, err = session.Submit(expr.FieldChange{Path: []any{"other"}, Value: true})
	require.NoError(t, err)
	require.Equal(t, 3, value)
	require.Equal(t, 1, calls)
}
