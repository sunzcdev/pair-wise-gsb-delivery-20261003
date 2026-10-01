package expr_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/file"
	"github.com/expr-lang/expr/internal/testify/assert"
	"github.com/expr-lang/expr/internal/testify/require"
	"github.com/expr-lang/expr/vm"
)

func TestFn(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  any
	}{
		{
			"named function",
			`fn fee(x) { x * 2 }; fee(21)`,
			42,
		},
		{
			"reused tiered pricing logic",
			`fn tier(x) { x < 100 ? 0 : x < 1000 ? 1 : 2 }; fn fee(x) { tier(x) * 10 + x }; fee(50) + fee(500) + fee(5000)`,
			5580,
		},
		{
			"lexical scope: function sees outer let",
			`let rate = 3; fn fee(x) { x * rate }; fee(7)`,
			21,
		},
		{
			"lexical scope: nested function declaration",
			`fn outer(x) { fn inner(y) { y * 2 }; inner(x) + 1 }; outer(20)`,
			41,
		},
		{
			"mutual recursion",
			`fn even(n) { n == 0 ? true : odd(n - 1) }; fn odd(n) { n == 0 ? false : even(n - 1) }; even(10) && odd(7)`,
			true,
		},
		{
			"mutual recursion, non-tail",
			`fn a(n) { n == 0 ? 0 : b(n - 1) + 1 }; fn b(n) { n == 0 ? 0 : a(n - 1) + 1 }; a(10)`,
			10,
		},
		{
			"let inside function body is frame-local",
			`fn f(n) { let x = n * 2; n <= 1 ? x : x + f(n - 1) }; f(3)`,
			12,
		},
		{
			"function called from predicate",
			`fn inc(x) { x + 1 }; map(1..3, inc(#))`,
			[]any{2, 3, 4},
		},
		{
			"recursion within default call budget",
			`fn fib(n) { n < 2 ? n : fib(n - 1) + fib(n - 2) }; fib(15)`,
			610,
		},
		{
			"deep tail recursion",
			`fn sumTo(n, acc) { n == 0 ? acc : sumTo(n - 1, acc + n) }; sumTo(1000000, 0)`,
			500000500000,
		},
		{
			"deep mutual tail recursion",
			`fn even(n) { n == 0 ? true : odd(n - 1) }; fn odd(n) { n == 0 ? false : even(n - 1) }; even(1000000)`,
			true,
		},
		{
			"tail call through if-else and sequence",
			`fn f(n) { let done = n <= 0; if done { n } else { f(n - 1) } }; f(100000)`,
			0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			program, err := expr.Compile(tt.input)
			require.NoError(t, err)

			out, err := expr.Run(program, nil)
			require.NoError(t, err)
			assert.Equal(t, tt.want, out)
		})
	}
}

func TestFn_host_functions(t *testing.T) {
	double := expr.Function("double", func(params ...any) (any, error) {
		return params[0].(int) * 2, nil
	})

	// Host functions keep working and can be called from local functions.
	program, err := expr.Compile(`fn quad(x) { double(double(x)) }; quad(5) + double(1)`, double)
	require.NoError(t, err)

	out, err := expr.Run(program, nil)
	require.NoError(t, err)
	assert.Equal(t, 22, out)

	// Local functions cannot override host functions.
	_, err = expr.Compile(`fn double(x) { x }; double(1)`, double)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot redeclare function double")
}

func TestFn_compile_errors(t *testing.T) {
	tests := []struct {
		name  string
		input string
		err   string
	}{
		{
			"duplicate definition",
			`fn f() { 1 }; fn f() { 2 }; f()`,
			"cannot redeclare function f",
		},
		{
			"cannot override builtin",
			`fn len(x) { x }; len("a")`,
			"cannot redeclare builtin len",
		},
		{
			"cannot redeclare variable",
			`let f = 1; fn f() { 2 }; f()`,
			"cannot redeclare variable f",
		},
		{
			"let cannot redeclare function",
			`fn f() { 1 }; let f = 2; f`,
			"cannot redeclare function f",
		},
		{
			"duplicate parameter",
			`fn f(x, x) { x }; f(1, 2)`,
			"cannot redeclare parameter x",
		},
		{
			"not enough arguments",
			`fn f(a, b) { a }; f(1)`,
			"not enough arguments to call f",
		},
		{
			"too many arguments",
			`fn f(a) { a }; f(1, 2)`,
			"too many arguments to call f",
		},
		{
			"function used as value",
			`fn f() { 1 }; let g = f; g`,
			"local function f can only be used as a callee",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := expr.Compile(tt.input)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.err)
		})
	}
}

func TestFn_cannot_override_env(t *testing.T) {
	env := map[string]any{"fee": 42}

	_, err := expr.Compile(`fn fee(x) { x }; fee(1)`, expr.Env(env))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot redeclare fee")
}

func TestFn_cannot_capture_outer_function_variable(t *testing.T) {
	// Variables of an enclosing function cannot be captured by a nested
	// function declaration; with a strict env this is a compile-time error.
	_, err := expr.Compile(`fn f(x) { fn g() { x }; g() }; f(1)`, expr.Env(map[string]any{}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown name x")
}

func TestFn_wrong_argument_type(t *testing.T) {
	program, err := expr.Compile(`fn f(x) { x + 1 }; f("oops")`)
	require.NoError(t, err)

	_, err = expr.Run(program, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid operation: string + int")
}

func TestFn_call_budget_exceeded(t *testing.T) {
	input := `fn f(n) { n == 0 ? 0 : f(n - 1) + 1 }; f(100)`

	program, err := expr.Compile(input, expr.CallBudget(10))
	require.NoError(t, err)

	_, err = expr.Run(program, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `call budget exceeded: cannot call "f"`)

	// The error location points at the recursive call site.
	var fileErr *file.Error
	require.True(t, errors.As(err, &fileErr))
	assert.Equal(t, strings.Index(input, "f(n - 1)"), fileErr.Location.From)
}

func TestFn_call_budget_default(t *testing.T) {
	// Without an explicit budget, conf.DefaultCallBudget (1024) applies and
	// unbounded (non-tail) recursion is rejected instead of growing without
	// limits.
	program, err := expr.Compile(`fn f(n) { n == 0 ? 0 : f(n - 1) + 1 }; f(100000)`)
	require.NoError(t, err)

	_, err = expr.Run(program, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "call budget exceeded")
}

func TestFn_tail_call_resource_usage(t *testing.T) {
	// Deep tail recursion runs in constant frame memory: the variables
	// buffer must not grow with the recursion depth.
	program, err := expr.Compile(`fn sumTo(n, acc) { n == 0 ? acc : sumTo(n - 1, acc + n) }; sumTo(1000000, 0)`)
	require.NoError(t, err)

	v := &vm.VM{}
	out, err := v.Run(program, nil)
	require.NoError(t, err)
	assert.Equal(t, 500000500000, out)
	assert.True(t, len(v.Variables) <= 4, "expected constant frame memory, got %d variable slots", len(v.Variables))
}

func TestFn_identifier_still_works(t *testing.T) {
	// "fn" is a contextual keyword: it can still be used as a plain
	// identifier, e.g. an env function named fn.
	env := map[string]any{
		"fn": func(a int) int { return a * 2 },
	}
	out, err := expr.Eval(`fn(21)`, env)
	require.NoError(t, err)
	assert.Equal(t, 42, out)
}
