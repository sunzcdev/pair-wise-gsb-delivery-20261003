package expr_test

import (
	"testing"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/batch"
	"github.com/expr-lang/expr/internal/testify/require"
)

func TestTopLevelRunBatch(t *testing.T) {
	program, err := expr.Compile(`a > 1 && b == "ok"`)
	require.NoError(t, err)

	results, errs, err := expr.RunBatch(program, []batch.Column{
		{Name: "a", Data: []int{2, 0, 5}},
		{Name: "b", Data: []string{"ok", "ok", "no"}},
	})
	require.NoError(t, err)
	require.Equal(t, []any{true, false, false}, results)
	for _, e := range errs {
		require.NoError(t, e)
	}

	results2, errs2, err := expr.RunBatchMap(program, map[string]any{
		"a": []int{2, 0, 5},
		"b": []string{"ok", "ok", "no"},
	})
	require.NoError(t, err)
	require.Equal(t, results, results2)
	require.Len(t, errs2, 3)
}
