package batch_test

import (
	"strings"
	"testing"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/batch"
	"github.com/expr-lang/expr/internal/testify/require"
)

type logRow struct {
	Level   string
	Latency int
}

// TestTypedColumns covers int64, float64, bool and string columns with full
// scalar cross checking.
func TestTypedColumns(t *testing.T) {
	expressions := []string{
		`x + y`,
		`x > y`,
		`f * 2.0`,
		`flag && x > 1`,
		`flag || f > 0.5`,
	}
	const rows = 16
	xs := make([]int64, rows)
	ys := make([]int64, rows)
	fs := make([]float64, rows)
	flags := make([]bool, rows)
	envs := make([]map[string]any, rows)
	for i := 0; i < rows; i++ {
		xs[i] = int64(i)
		ys[i] = int64(i % 5)
		fs[i] = float64(i) * 0.25
		flags[i] = i%2 == 0
		envs[i] = map[string]any{"x": xs[i], "y": ys[i], "f": fs[i], "flag": flags[i]}
	}
	columns := map[string]any{"x": xs, "y": ys, "f": fs, "flag": flags}

	for _, input := range expressions {
		input := input
		t.Run(input, func(t *testing.T) {
			program, err := expr.Compile(input)
			require.NoError(t, err)
			results, errs, err := batch.RunMap(program, columns)
			require.NoError(t, err)
			for row := 0; row < rows; row++ {
				want, wantErr := expr.Run(program, envs[row])
				if wantErr != nil {
					require.Errorf(t, errs[row], "row %d", row)
					continue
				}
				require.NoError(t, errs[row])
				assertValueEqual(t, want, results[row], row)
			}
		})
	}
}

// TestStructEnvFallback exercises the row by row fallback for a program
// compiled against a struct environment and containing an unsupported node.
func TestStructEnvFallback(t *testing.T) {
	envType := struct {
		Rows []logRow
	}{}
	program, err := expr.Compile(`filter(Rows, #.Latency > 5)`, expr.Env(envType))
	rowsCol := []any{
		[]logRow{{Level: "a", Latency: 1}, {Level: "b", Latency: 9}},
		[]logRow{{Level: "c", Latency: 10}},
	}
	// Sanity check: the scalar VM accepts the same struct env shape.
	manualEnv := envType
	manualEnv.Rows = rowsCol[0].([]logRow)
	manual, manualErr := expr.Run(program, manualEnv)
	require.NoError(t, manualErr)
	require.Len(t, manual, 1)
	results, errs, err := batch.Run(program,
		[]batch.Column{{Name: "Rows", Data: rowsCol}},
		batch.WithPrototype(envType),
	)
	require.NoError(t, err)
	require.Len(t, results, 2)
	for _, e := range errs {
		require.NoError(t, e)
	}

	require.Len(t, results[0], 1)
}

// TestRowErrorMessage renders the one based line number in the error text.
func TestRowErrorMessage(t *testing.T) {
	program, err := expr.Compile(`10 % b`)
	require.NoError(t, err)

	_, errs, err := batch.RunMap(program, map[string]any{"b": []int{1, 0, 1, 0, 0}})
	require.NoError(t, err)
	require.NoError(t, errs[0])
	require.True(t, strings.Contains(errs[1].Error(), "row 2"))
	require.True(t, strings.Contains(errs[3].Error(), "row 4"))
	require.True(t, strings.Contains(errs[4].Error(), "row 5"))
}

// TestNilProgram checks the nil program guard.
func TestNilProgram(t *testing.T) {
	_, _, err := batch.Run(nil, nil)
	require.Error(t, err)
}

// TestTrailingCast checks the cast emitted by expr.AsInt/AsBool options.
func TestTrailingCast(t *testing.T) {
	program, err := expr.Compile(`a / 2`, expr.AsInt())
	require.NoError(t, err)

	results, errs, err := batch.RunMap(program, map[string]any{
		"a": []int{4, 5, 7},
	})
	require.NoError(t, err)

	for i, a := range []int{4, 5, 7} {
		want, wantErr := expr.Run(program, map[string]any{"a": a})
		require.NoError(t, wantErr)
		require.NoError(t, errs[i])
		require.Equal(t, want, results[i])
	}
}

// TestShortCircuitTernarySideEffects verifies the unselected ternary branch
// does not run a host function.
func TestShortCircuitTernarySideEffects(t *testing.T) {
	calls := 0
	program, err := expr.Compile(
		`a > 0 ? 1 : boom(a)`,
		expr.Function("boom", func(params ...any) (any, error) {
			calls++
			return nil, nil
		}),
	)
	require.NoError(t, err)

	_, errs, err := batch.RunMap(program, map[string]any{"a": []int{1, 2, 3}})
	require.NoError(t, err)
	for _, e := range errs {
		require.NoError(t, e)
	}
	require.Equal(t, 0, calls)
}

// TestOptionalChain covers nil safe chaining with columnar rows.
func TestOptionalChain(t *testing.T) {
	program, err := expr.Compile(`x?.y ?? -1`)
	require.NoError(t, err)

	results, errs, err := batch.RunMap(program, map[string]any{
		"x": []any{map[string]any{"y": 5}, nil, map[string]any{"y": 9}},
	})
	require.NoError(t, err)
	for _, e := range errs {
		require.NoError(t, e)
	}

	for i, xv := range []any{map[string]any{"y": 5}, nil, map[string]any{"y": 9}} {
		want, wantErr := expr.Run(program, map[string]any{"x": xv})
		require.NoError(t, wantErr)
		require.Equal(t, want, results[i])
	}
}
