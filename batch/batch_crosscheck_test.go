package batch_test

import (
	"fmt"
	"math"
	"math/rand"
	"testing"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/batch"
	"github.com/expr-lang/expr/internal/testify/require"
)

// TestCrossCheck evaluates many expressions with both vm.Run (row by row) and
// batch.Run and asserts the results and the errors match for every row.
func TestCrossCheck(t *testing.T) {
	expressions := []string{
		`a + b`,
		`a - b`,
		`a * b`,
		`a / b`,
		`a % b`,
		`a ** 2`,
		`-a`,
		`a == b`,
		`a != b`,
		`a < b`,
		`a > b`,
		`a <= b`,
		`a >= b`,
		`(a > 3 || b > 3) && a + b < 100`,
		`a > 0 && b > 0`,
		`a == 0 || b == 0 || a / b > 1`,
		`s == "x" || s2 contains "ab"`,
		`s startsWith "a"`,
		`s endsWith "z"`,
		`s contains "cd"`,
		`s matches "^a"`,
		`a in [1, 2, 3]`,
		`a > 2 ? a * 10 : b - 1`,
		`maybe ?? a`,
		`maybe + 1`,
		`a > b ? a : b`,
		`abs(a - 7)`,
		`max(a, b)`,
		`min(a, b) + 1`,
		`string(a) + "!"`,
		`upper(s)`,
		`a + b * 2 - (a - b)`,
		`!(a == b)`,
		`a > 0 && b != 0 && a / b >= 1`,
		`a == 0 || 10 / a <= 3`,
		`float(a) + 0.5`,
		`round(float(a) / 3.0)`,
		`s + s2`,
		`s matches pattern`,
	}

	const rows = 64
	rng := rand.New(rand.NewSource(20260927))

	for _, input := range expressions {
		input := input
		t.Run(input, func(t *testing.T) {
			program, err := expr.Compile(input)
			require.NoError(t, err)

			ints := make([]int, rows)
			ints2 := make([]int, rows)
			strs := make([]string, rows)
			strs2 := make([]string, rows)
			patterns := make([]string, rows)
			maybe := make([]any, rows)
			rowMaps := make([]map[string]any, rows)

			for i := 0; i < rows; i++ {
				ints[i] = rng.Intn(8)
				ints2[i] = rng.Intn(6)
				strs[i] = pickString(rng, i)
				strs2[i] = pickString(rng, i+3)
				patterns[i] = []string{"^a", "z$", "b", "", "x+"}[i%5]
				if i%4 == 0 {
					maybe[i] = nil
				} else {
					maybe[i] = rng.Intn(5)
				}
				rowMaps[i] = map[string]any{
					"a":       ints[i],
					"b":       ints2[i],
					"s":       strs[i],
					"s2":      strs2[i],
					"pattern": patterns[i],
					"maybe":   maybe[i],
				}
			}

			results, errs, err := batch.RunMap(program, map[string]any{
				"a":       ints,
				"b":       ints2,
				"s":       strs,
				"s2":      strs2,
				"pattern": patterns,
				"maybe":   maybe,
			})
			require.NoError(t, err)
			require.Len(t, results, rows)
			require.Len(t, errs, rows)

			for row := 0; row < rows; row++ {
				want, wantErr := expr.Run(program, rowMaps[row])

				if wantErr != nil {
					require.Errorf(t, errs[row], "row %d: expected an error", row)
					assertSameError(t, wantErr, errs[row], row)
					require.Nil(t, results[row])
					continue
				}
				require.NoErrorf(t, errs[row], "row %d: unexpected error: %v", row, errs[row])
				assertValueEqual(t, want, results[row], row)
			}
		})
	}
}

func assertValueEqual(t *testing.T, want, got any, row int) {
	t.Helper()
	if wf, ok := want.(float64); ok {
		if gf, ok2 := got.(float64); ok2 {
			if math.IsNaN(wf) && math.IsNaN(gf) {
				return
			}
			require.Equalf(t, wf, gf, "row %d result mismatch", row)
			return
		}
	}
	require.Equalf(t, want, got, "row %d result mismatch", row)
}

// TestShortCircuitHostFunctions verifies that divisions and host functions
// on a short circuited right hand side are never invoked.
func TestShortCircuitHostFunctions(t *testing.T) {
	var divisorCalls int
	var fnCalls int

	program, err := expr.Compile(
		`flag == 0 || (10 / divisor(flag) > 0 && hostfn(flag) == "ok")`,
		expr.Function("divisor", func(params ...any) (any, error) {
			divisorCalls++
			d := params[0].(int)
			if d == 0 {
				return nil, fmt.Errorf("divisor is zero")
			}
			return d, nil
		}),
		expr.Function("hostfn", func(params ...any) (any, error) {
			fnCalls++
			return "ok", nil
		}),
	)
	require.NoError(t, err)

	results, errs, err := batch.RunMap(program, map[string]any{
		"flag": []int{0, 1, 0, 2, 0},
	})
	require.NoError(t, err)

	for i, e := range errs {
		require.NoErrorf(t, e, "row %d", i)
	}
	require.Equal(t, []any{true, true, true, true, true}, results)

	// divisor/hostfn must be called exactly for the rows where flag != 0 and
	// the right side runs: rows 1 and 3 (2 rows).
	require.Equal(t, 2, divisorCalls)
	require.Equal(t, 2, fnCalls)
}

// TestShortCircuitDivisionByZero checks that rows short circuited before a
// division by zero succeed while other rows report the division error.
func TestShortCircuitDivisionByZero(t *testing.T) {
	program, err := expr.Compile(`b == 0 || 10 / b > 1`)
	require.NoError(t, err)

	results, errs, err := batch.RunMap(program, map[string]any{
		"b": []int{0, 2, 0, 0, 5},
	})
	require.NoError(t, err)

	require.True(t, results[0].(bool))
	require.True(t, results[1].(bool))
	require.True(t, results[2].(bool))
	require.True(t, results[3].(bool))
	require.True(t, results[4].(bool))
	for i, e := range errs {
		require.NoErrorf(t, e, "row %d", i)
	}
}

// TestDivisionByZeroErrors ensures genuine integer modulo by zero is
// reported per row and the error points back to the original input row.
func TestDivisionByZeroErrors(t *testing.T) {
	program, err := expr.Compile(`10 % b`)
	require.NoError(t, err)

	results, errs, err := batch.RunMap(program, map[string]any{
		"b": []int{2, 0, 5, 0},
	})
	require.NoError(t, err)

	require.Equal(t, 0, results[0])
	require.Nil(t, results[1])
	require.Equal(t, 0, results[2])
	require.Nil(t, results[3])

	require.NoError(t, errs[0])
	require.Errorf(t, errs[1], "row 2 must error")
	require.NoError(t, errs[2])
	require.Errorf(t, errs[3], "row 4 must error")

	re1 := requireRowError(t, errs[1])
	re3 := requireRowError(t, errs[3])
	require.Equal(t, 1, re1.Index)
	require.Equal(t, 2, re1.Line)
	require.Equal(t, 3, re3.Index)
	require.Equal(t, 4, re3.Line)
}

// TestNullSemantics makes sure nulls behave exactly like scalar evaluation:
// arithmetic on null fails per row, comparisons and coalescing preserve the
// scalar VM semantics.
func TestNullSemantics(t *testing.T) {
	cases := []struct {
		expr    string
		columns map[string]any
	}{
		{
			expr:    `x == nil`,
			columns: map[string]any{"x": []any{nil, 1, nil, 2}},
		},
		{
			expr:    `x != nil`,
			columns: map[string]any{"x": []any{nil, 1, nil, 2}},
		},
		{
			expr:    `x ?? 42`,
			columns: map[string]any{"x": []any{nil, 1, nil, 2}},
		},
		{
			expr:    `x == nil || x > 0`,
			columns: map[string]any{"x": []any{nil, 1, nil, -1}},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.expr, func(t *testing.T) {
			program, err := expr.Compile(tc.expr)
			require.NoError(t, err)

			results, errs, err := batch.RunMap(program, tc.columns)
			require.NoError(t, err)

			n := len(tc.columns["x"].([]any))
			for row := 0; row < n; row++ {
				env := map[string]any{"x": tc.columns["x"].([]any)[row]}
				want, wantErr := expr.Run(program, env)
				if wantErr != nil {
					require.Errorf(t, errs[row], "row %d expected error", row)
					continue
				}
				require.NoErrorf(t, errs[row], "row %d unexpected error: %v", row, errs[row])
				require.Equalf(t, want, results[row], "row %d", row)
			}
		})
	}
}

// TestUnsupportedFallback uses a collection builtin which the columnar
// evaluator does not cover and checks the automatic fallback.
func TestUnsupportedFallback(t *testing.T) {
	program, err := expr.Compile(`filter(xs, # > 2)`)
	require.NoError(t, err)

	supported, reason := batch.Supports(program)
	require.False(t, supported)
	require.NotEmpty(t, reason)

	xs := [][]int{{1, 2, 3}, {3, 4, 5}, {}}
	xsCol := []any{xs[0], xs[1], xs[2]}
	results, errs, err := batch.RunMap(program, map[string]any{
		"xs": xsCol,
	})
	require.NoError(t, err)

	for row := 0; row < len(xs); row++ {
		require.NoError(t, errs[row])
		want, wantErr := expr.Run(program, map[string]any{"xs": xs[row]})
		require.NoError(t, wantErr)
		require.Equal(t, want, results[row])
	}
}

// TestNoFallback checks that WithNoFallback rejects an unsupported program
// explicitly instead of silently changing semantics.
func TestNoFallback(t *testing.T) {
	program, err := expr.Compile(`map(xs, # + 1)`)
	require.NoError(t, err)

	_, _, err = batch.RunMap(program, map[string]any{"xs": [][]int{{1}}}, batch.WithNoFallback())
	require.Error(t, err)
}

// TestColumnValidation checks column count and type validation.
func TestColumnValidation(t *testing.T) {
	program, err := expr.Compile(`a + b`)
	require.NoError(t, err)

	_, _, err = batch.RunMap(program, map[string]any{
		"a": []int{1, 2},
		"b": []int{1},
	})
	require.Error(t, err)

	_, _, err = batch.Run(program, []batch.Column{
		{Name: "a", Data: 5},
	})
	require.Error(t, err)
}

// TestEmptyColumns checks evaluation with zero rows.
func TestEmptyColumns(t *testing.T) {
	program, err := expr.Compile(`a + 1`)
	require.NoError(t, err)

	results, errs, err := batch.RunMap(program, map[string]any{"a": []int{}})
	require.NoError(t, err)
	require.Len(t, results, 0)
	require.Len(t, errs, 0)
}

func pickString(rng *rand.Rand, i int) string {
	choices := []string{"abc", "abcd", "xyz", "a", "", "zzz"}
	return choices[(i+int(rng.Intn(len(choices))))%len(choices)]
}

func requireRowError(t *testing.T, err error) *batch.RowError {
	t.Helper()
	var rowErr *batch.RowError
	require.ErrorAs(t, err, &rowErr)
	return rowErr
}

func assertSameError(t *testing.T, want, got error, row int) {
	t.Helper()
	var wantRow, gotRow *batch.RowError
	if asRow(want, &wantRow) {
		require.ErrorAsf(t, got, &gotRow, "row %d", row)
		require.Equalf(t, wantRow.Line, gotRow.Line, "row %d line", row)
		return
	}
	require.Errorf(t, got, "row %d expected an error", row)
}

func asRow(err error, target **batch.RowError) bool {
	for err != nil {
		if re, ok := err.(*batch.RowError); ok {
			*target = re
			return true
		}
		if u, ok := err.(interface{ Unwrap() error }); ok {
			err = u.Unwrap()
			continue
		}
		break
	}
	return false
}
