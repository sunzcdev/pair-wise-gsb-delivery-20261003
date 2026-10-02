package expr_test

import (
	"errors"
	"testing"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/internal/testify/require"
	"github.com/expr-lang/expr/vm"
)

func TestRunBatch(t *testing.T) {
	env := map[string]any{"level": "", "status": 0}
	program, err := expr.Compile(`level == "error" && status >= 500`, expr.Env(env))
	require.NoError(t, err)

	res, err := expr.RunBatch(program, vm.Columns{
		"level":  []string{"error", "info", "error"},
		"status": []int{503, 200, 404},
	})
	require.NoError(t, err)
	require.Equal(t, []any{true, false, false}, res.Values)
	require.Empty(t, res.Errors)
}

func TestRunBatch_FallbackRowErrors(t *testing.T) {
	env := map[string]any{"n": 0}
	program, err := expr.Compile(`half(n)`, expr.Env(env),
		expr.Function("half",
			func(params ...any) (any, error) {
				if params[0].(int) == 0 {
					return nil, errZero
				}
				return params[0].(int) / 2, nil
			},
			new(func(int) (int, error))))
	require.NoError(t, err)

	res, err := expr.RunBatch(program, vm.Columns{"n": []int{4, 0, 8}})
	require.NoError(t, err)
	require.Len(t, res.Errors, 1)
	require.Equal(t, 1, res.Errors[0].Row)
	require.Equal(t, 2, res.Values[0])
	require.Nil(t, res.Values[1])
	require.Equal(t, 4, res.Values[2])
}

var errZero = errors.New("zero")
