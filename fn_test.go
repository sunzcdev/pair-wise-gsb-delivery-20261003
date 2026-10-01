package expr_test

import (
	"fmt"
	"testing"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/internal/testify/assert"
	"github.com/expr-lang/expr/internal/testify/require"
	"github.com/expr-lang/expr/vm"
)

func TestFn_basic(t *testing.T) {
	out, err := expr.Eval(`fn add(a, b) { a + b }; add(1, 2)`, nil)
	require.NoError(t, err)
	assert.Equal(t, 3, out)
}

func TestFn_tiered_billing(t *testing.T) {
	code := `
		fn tier(price, qty) { price * qty };
		fn bill(amount) {
			amount <= 100 ? tier(5, amount) :
			amount <= 500 ? tier(5, 100) + tier(4, amount - 100) :
			                tier(5, 100) + tier(4, 400) + tier(3, amount - 500)
		};
		bill(600)
	`
	out, err := expr.Eval(code, nil)
	require.NoError(t, err)
	assert.Equal(t, 2400, out)
}

func TestFn_recursion(t *testing.T) {
	out, err := expr.Eval(`fn fact(n) { n <= 1 ? 1 : n * fact(n - 1) }; fact(10)`, nil)
	require.NoError(t, err)
	assert.Equal(t, 3628800, out)
}

func TestFn_mutual_recursion(t *testing.T) {
	code := `
		fn even(n) { n == 0 ? true : odd(n - 1) };
		fn odd(n) { n == 0 ? false : even(n - 1) };
		[even(10), odd(10), even(7), odd(7)]
	`
	out, err := expr.Eval(code, nil)
	require.NoError(t, err)
	assert.Equal(t, []any{true, false, false, true}, out)
}

func TestFn_lexical_scope(t *testing.T) {
	t.Run("param shadows outer variable", func(t *testing.T) {
		out, err := expr.Eval(`let a = 10; fn f(a) { a + 1 }; f(5) + a`, nil)
		require.NoError(t, err)
		assert.Equal(t, 16, out)
	})

	t.Run("let inside body", func(t *testing.T) {
		out, err := expr.Eval(`fn f(x) { let y = x * 2; y + 1 }; f(5)`, nil)
		require.NoError(t, err)
		assert.Equal(t, 11, out)
	})

	t.Run("let shadows function", func(t *testing.T) {
		out, err := expr.Eval(`fn f(x) { x }; let f = 42; f`, nil)
		require.NoError(t, err)
		assert.Equal(t, 42, out)
	})

	t.Run("locals are per call frame", func(t *testing.T) {
		code := `fn f(n) { let a = n * 2; n <= 1 ? a : a + f(n - 1) }; f(5)`
		out, err := expr.Eval(code, nil)
		require.NoError(t, err)
		assert.Equal(t, 30, out)
	})

	t.Run("body sees env", func(t *testing.T) {
		out, err := expr.Eval(`fn f(x) { x + tax }; f(100)`, map[string]any{"tax": 5})
		require.NoError(t, err)
		assert.Equal(t, 105, out)
	})
}

func TestFn_tail_recursion_deep(t *testing.T) {
	// Tail calls are not limited by the call budget and do not grow
	// the call stack, so very deep tail recursion works with a minimal
	// budget.
	program, err := expr.Compile(
		`fn loop(n, acc) { n == 0 ? acc : loop(n - 1, acc + n) }; loop(1000000, 0)`,
		expr.CallBudget(1),
	)
	require.NoError(t, err)

	out, err := expr.Run(program, nil)
	require.NoError(t, err)
	assert.Equal(t, 500000500000, out)
}

func TestFn_mutual_tail_recursion_deep(t *testing.T) {
	code := `
		fn even(n) { n == 0 ? true : odd(n - 1) };
		fn odd(n) { n == 0 ? false : even(n - 1) };
		even(1000000)
	`
	out, err := expr.Eval(code, nil)
	require.NoError(t, err)
	assert.Equal(t, true, out)
}

func TestFn_call_budget_exceeded(t *testing.T) {
	program, err := expr.Compile(`fn f(n) { n == 0 ? 0 : 1 + f(n - 1) }; f(20000)`)
	require.NoError(t, err)

	_, err = expr.Run(program, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `call budget exceeded while calling function "f"`)
	// The error points to the location of the recursive call.
	assert.Contains(t, err.Error(), "(1:28)")
}

func TestFn_call_budget_option(t *testing.T) {
	code := `fn f(n) { n == 0 ? 0 : 1 + f(n - 1) }; f(100)`

	program, err := expr.Compile(code, expr.CallBudget(10))
	require.NoError(t, err)
	_, err = expr.Run(program, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "call budget exceeded")

	program, err = expr.Compile(code, expr.CallBudget(200))
	require.NoError(t, err)
	out, err := expr.Run(program, nil)
	require.NoError(t, err)
	assert.Equal(t, 100, out)
}

func TestFn_duplicate_definition(t *testing.T) {
	_, err := expr.Compile(`fn f(x) { x }; fn f(x) { x }; f(1)`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot redeclare function f")
}

func TestFn_duplicate_parameter(t *testing.T) {
	_, err := expr.Compile(`fn f(x, x) { x }; f(1, 2)`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate parameter x")
}

func TestFn_cannot_redeclare_host_function(t *testing.T) {
	_, err := expr.Compile(
		`fn foo(x) { x }; foo(1)`,
		expr.Function("foo", func(params ...any) (any, error) { return params[0], nil }),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot redeclare function foo")
}

func TestFn_cannot_redeclare_env_member(t *testing.T) {
	_, err := expr.Compile(`fn foo(x) { x }; foo(1)`, expr.Env(map[string]any{"foo": 1}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot redeclare foo")
}

func TestFn_cannot_redeclare_builtin(t *testing.T) {
	_, err := expr.Compile(`fn len(x) { x }; len(1)`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot redeclare builtin len")
}

func TestFn_wrong_argument_type(t *testing.T) {
	program, err := expr.Compile(`fn f(x) { x + 1 }; f("str")`)
	require.NoError(t, err)

	_, err = expr.Run(program, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid operation: string + int")
}

func TestFn_invalid_number_of_arguments(t *testing.T) {
	_, err := expr.Compile(`fn f(x) { x }; f(1, 2)`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid number of arguments (expected 1, got 2)")
}

func TestFn_cannot_use_function_as_value(t *testing.T) {
	_, err := expr.Compile(`fn f(x) { x }; f`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot use function f as a value")
}

func TestFn_declaration_not_top_level(t *testing.T) {
	_, err := expr.Compile(`fn f(x) { fn g(y) { y }; g(x) }; f(1)`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "function declarations are only allowed at the top level")

	_, err = expr.Compile(`(fn f(x) { x }; f(1))`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "function declarations are only allowed at the top level")
}

func TestFn_calls_host_function(t *testing.T) {
	out, err := expr.Eval(
		`fn f(x) { double(x) + 1 }; f(20)`,
		map[string]any{"double": func(x int) int { return x * 2 }},
	)
	require.NoError(t, err)
	assert.Equal(t, 41, out)
}

func TestFn_with_predicates(t *testing.T) {
	out, err := expr.Eval(`fn f(xs) { map(xs, # * 2) }; f([1, 2, 3])`, nil)
	require.NoError(t, err)
	assert.Equal(t, []any{2, 4, 6}, out)
}

func TestFn_hoisting(t *testing.T) {
	// Functions are visible in the whole sequence, even before
	// their declaration statement.
	out, err := expr.Eval(`f(40); fn f(x) { x + 2 }`, nil)
	require.NoError(t, err)
	assert.Equal(t, nil, out)
}

func TestFn_disassemble(t *testing.T) {
	program, err := expr.Compile(`fn f(x) { x }; f(1)`)
	require.NoError(t, err)
	assert.Contains(t, program.Disassemble(), "OpCallLocal")
	require.Len(t, program.LocalFunctions, 1)
	assert.Equal(t, vm.LocalFunction{Name: "f", Arity: 1, Entry: program.LocalFunctions[0].Entry}, program.LocalFunctions[0])
}

func ExampleEval_fn() {
	code := `
		fn tier(price, qty) { price * qty };
		fn bill(amount) {
			amount <= 100 ? tier(5, amount) :
			                tier(5, 100) + tier(4, amount - 100)
		};
		bill(120)
	`
	output, err := expr.Eval(code, nil)
	if err != nil {
		fmt.Printf("err: %v", err)
		return
	}
	fmt.Printf("%v", output)

	// Output: 580
}
