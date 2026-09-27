# Configuration

## Return type

Usually, the return type of expression is anything. But we can instruct type checker to verify the return type of the
expression.
For example, in filter expressions, we expect the return type to be a boolean.

```go
program, err := expr.Compile(code, expr.AsBool())
if err != nil {
    panic(err)
}

output, err := expr.Run(program, env)
if err != nil {
    panic(err)
}

ok := output.(bool) // It is safe to assert the output to bool, if the expression is type checked as bool.
```

If `code` variable for example returns a string, the compiler will return an error.

Expr has a few options to specify the return type:

- [expr.AsBool()](https://pkg.go.dev/github.com/expr-lang/expr#AsBool) - expects the return type to be a bool.
- [expr.AsInt()](https://pkg.go.dev/github.com/expr-lang/expr#AsInt) - expects the return type to be an int (float64,
  uint, int32, and other will be cast to int).
- [expr.AsInt64()](https://pkg.go.dev/github.com/expr-lang/expr#AsInt64) - expects the return type to be an int64 (
  float64, uint, int32, and other will be cast to int64).
- [expr.AsFloat64()](https://pkg.go.dev/github.com/expr-lang/expr#AsFloat64) - expects the return type to be a float64 (
  float32 will be cast to float64).
- [expr.AsAny()](https://pkg.go.dev/github.com/expr-lang/expr#AsAny) - expects the return type to be anything.
- [expr.AsKind(reflect.Kind)](https://pkg.go.dev/github.com/expr-lang/expr#AsKind) - expects the return type to be a
  specific kind.

:::tip Warn on any
By default, type checker will accept any type, even if the return type is specified. Consider following examples:

```expr
let arr = [1, 2, 3]; arr[0]
```

The return type of the expression is `any`. Arrays created in Expr are of type `[]any`. The type checker will not return
an error if the return type is specified as `expr.AsInt()`. The output of the expression is `1`, which is an int, but the
type checker will not return an error.

But we can instruct the type checker to warn us if the return type is `any`. Use [`expr.WarnOnAny()`](https://pkg.go.dev/github.com/expr-lang/expr#WarnOnAny) to enable this behavior.

```go
program, err := expr.Compile(code, expr.AsInt(), expr.WarnOnAny())
```

The type checker will return an error if the return type is `any`. We need to modify the expression to return a specific
type.

```expr
let arr = [1, 2, 3]; int(arr[0])
```
:::


## WithContext

Although the compiled program is guaranteed to be terminated, some user defined functions may not be. For example, if a
user defined function calls a remote service, we may want to pass a context to the function.

This is possible via the [`WithContext`](https://pkg.go.dev/github.com/expr-lang/expr#WithContext) option.

This option will modify function calls to include the context as the first argument (only if the function signature
accepts a context).

```expr
customFunc(42)
// will be transformed to
customFunc(ctx, 42)
```

Function `expr.WithContext()` takes the name of context variable. The context variable must be defined in the environment.

```go
env := map[string]any{
    "ctx": context.Background(),
    "customFunc": func(ctx context.Context, a int) int {
        return a
    },
}

program, err := expr.Compile(code, expr.Env(env), expr.WithContext("ctx"))
```

## ConstExpr

For some user defined functions, we may want to evaluate the expression at compile time. This is possible via the
[`ConstExpr`](https://pkg.go.dev/github.com/expr-lang/expr#ConstExpr) option. 

```go
func fib(n int) int {
    if n <= 1 {
        return n
    }
    return fib(n-1) + fib(n-2)
}

env := map[string]any{
    "fib": fib,
}

program, err := expr.Compile(`fib(10)`, expr.Env(env), expr.ConstExpr("fib"))
```

If all arguments of the function are constants, the function will be evaluated at compile time. The result of the function
will be used as a constant in the expression.

```expr
fib(10)    // will be transformed to 55 during the compilation
fib(12+12) // will be transformed to 267914296 during the compilation
fib(x)     // will **not** be transformed and will be evaluated at runtime
```

## Timezone

By default, the timezone is set to `time.Local`. We can change the timezone via the [`Timezone`](https://pkg.go.dev/github.com/expr-lang/expr#Timezone) option.

```go
program, err := expr.Compile(code, expr.Timezone(time.UTC))
```

The timezone is used for the following functions:
```expr
date("2024-11-23 12:00:00") // parses the date in the specified timezone
now() // returns the current time in the specified timezone
```

## Pure rule mode

Rules that must be archived and replayed deterministically (e.g. risk-control rules) cannot read the current time,
randomness, or any external state at evaluation time. The [`Pure`](https://pkg.go.dev/github.com/expr-lang/expr#Pure)
option enables pure rule mode, in which the host explicitly declares the pure functions allowed to be called:

```go
program, err := expr.Compile(
    `riskScore(Amount) >= 50 && len(Tags) > 0`,
    expr.Env(env),
    expr.Function("riskScore", riskScore),
    expr.Pure("riskScore"), // host-declared pure functions
)
```

In pure rule mode an expression may only call:

- functions explicitly declared via `expr.Pure(...)`;
- builtins known to be pure (e.g. `len`, `abs`, `filter`).

The following are rejected at compile time:

- calls to undeclared functions;
- object method calls (e.g. `User.IsAdult()`);
- indirect calls through variables or computed function values (e.g. `let f = riskScore; f(x)`);
- builtins that read external state (e.g. `now()`), even if the host declares them;
- `ConstExpr` functions not declared via `expr.Pure(...)`, as they execute host code at compile time.

The restriction is enforced during type checking and re-validated after the optimization phase; the compile error
reports the call chain that broke the constraint:

```text
pure mode: call to undeclared function "auditLog" is not allowed (call chain: len(...) -> filter(...) -> auditLog(...))
```

The host declaration is the trust boundary: expr does not verify that a declared function is actually free of side
effects.
