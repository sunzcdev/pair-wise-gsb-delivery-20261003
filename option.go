package gojq

import (
	"context"
	"fmt"
)

// CompilerOption is a compiler option.
type CompilerOption func(*compiler)

// WithModuleLoader is a compiler option for module loader.
// If you want to load modules from the filesystem, use [NewModuleLoader].
func WithModuleLoader(moduleLoader ModuleLoader) CompilerOption {
	return func(c *compiler) {
		c.moduleLoader = moduleLoader
	}
}

// WithEnvironLoader is a compiler option for environment variables loader.
// The OS environment variables are not accessible by default due to security
// reasons. You can specify [os.Environ] as argument if you allow to access.
func WithEnvironLoader(environLoader func() []string) CompilerOption {
	return func(c *compiler) {
		c.environLoader = environLoader
	}
}

// WithVariables is a compiler option for variable names. The variables can be
// used in the query. You have to give the values to [*Code.Run] in the same order.
func WithVariables(variables []string) CompilerOption {
	return func(c *compiler) {
		c.variables = variables
	}
}

// WithFunction is a compiler option for adding a custom internal function.
// Specify the minimum and maximum count of the function arguments. These
// values should satisfy 0 <= minarity <= maxarity <= 30, otherwise panics.
// On handling numbers, take account of int, float64, *big.Int, and json.Number.
// These are the number types you are allowed to return, so do not return int64.
// Refer to [ValueError] to return a value error just like built-in error
// function. If you want to emit multiple values, call the empty function,
// accept a filter for its argument, or call another built-in function, then
// use LoadInitModules of the module loader.
func WithFunction(name string, minarity, maxarity int, f func(any, []any) any) CompilerOption {
	return withFunction(name, minarity, maxarity, false, f)
}

// WithFunctionWithContext is a compiler option for adding a custom internal
// function with the current run context. This is like the [WithFunction]
// option, but the function receives the context passed to [Code.RunWithContext]
// or [Query.RunWithContext], so that long-running functions can cooperatively
// respond to cancellation. The function is called synchronously; no goroutine
// is spawned to interrupt it, so a function ignoring the context keeps
// running until it returns, just like a function added by [WithFunction].
func WithFunctionWithContext(name string, minarity, maxarity int, f func(context.Context, any, []any) any) CompilerOption {
	return withFunction(name, minarity, maxarity, false, f)
}

// WithIterFunction is a compiler option for adding a custom iterator function.
// This is like the [WithFunction] option, but you can add a function which
// returns an Iter to emit multiple values. You cannot define both iterator and
// non-iterator functions of the same name (with possibly different arities).
// See also [NewIter], which can be used to convert values or an error to an Iter.
// If the returned Iter implements [io.Closer], its Close method is called
// exactly once when the iteration finishes; see [WithIterFunctionWithContext]
// for details.
func WithIterFunction(name string, minarity, maxarity int, f func(any, []any) Iter) CompilerOption {
	return withFunction(name, minarity, maxarity, true,
		func(v any, args []any) any {
			return f(v, args)
		},
	)
}

// WithIterFunctionWithContext is a compiler option for adding a custom
// iterator function with the current run context. This is like the
// [WithIterFunction] option, but the function receives the context passed to
// [Code.RunWithContext] or [Query.RunWithContext], so that long-running
// functions can cooperatively respond to cancellation.
//
// If the returned Iter implements [io.Closer], its Close method is called
// exactly once when the iterator is exhausted, the run ends with an error
// or is canceled, or the caller explicitly closes the result iterator of
// [Code.Run] or [Code.RunWithContext] (which implements [io.Closer]).
// Each run of a [*Code] owns the iterators it creates; iterators created by
// different runs are tracked and closed independently.
func WithIterFunctionWithContext(name string, minarity, maxarity int, f func(context.Context, any, []any) Iter) CompilerOption {
	return withFunction(name, minarity, maxarity, true,
		func(ctx context.Context, v any, args []any) any {
			return f(ctx, v, args)
		},
	)
}

func withFunction(name string, minarity, maxarity int, iter bool, f any) CompilerOption {
	if !(0 <= minarity && minarity <= maxarity && maxarity <= 30) {
		panic(fmt.Sprintf("invalid arity for %q: %d, %d", name, minarity, maxarity))
	}
	argcount := 1<<(maxarity+1) - 1<<minarity
	return func(c *compiler) {
		if c.customFuncs == nil {
			c.customFuncs = make(map[string]function)
		}
		if fn, ok := c.customFuncs[name]; ok {
			if fn.iter != iter {
				panic(fmt.Sprintf("cannot define both iterator and non-iterator functions for %q", name))
			}
			c.customFuncs[name] = function{
				argcount | fn.argcount, iter,
				mergeFuncCallback(f, fn.callback, argcount),
			}
		} else {
			c.customFuncs[name] = function{argcount, iter, f}
		}
	}
}

func mergeFuncCallback(f, g any, argcount int) any {
	dispatch := func(ctx context.Context, x any, xs []any) any {
		if argcount&(1<<len(xs)) != 0 {
			return callFuncCallback(f, ctx, x, xs)
		}
		return callFuncCallback(g, ctx, x, xs)
	}
	if isCtxFuncCallback(f) || isCtxFuncCallback(g) {
		return dispatch
	}
	return func(x any, xs []any) any {
		return dispatch(context.Background(), x, xs)
	}
}

func isCtxFuncCallback(f any) bool {
	_, ok := f.(func(context.Context, any, []any) any)
	return ok
}

func callFuncCallback(f any, ctx context.Context, x any, xs []any) any {
	if fn, ok := f.(func(context.Context, any, []any) any); ok {
		return fn(ctx, x, xs)
	}
	return f.(func(any, []any) any)(x, xs)
}

// WithInputIter is a compiler option for input iterator used by input(s)/0.
// Note that input and inputs functions are not allowed by default. We have
// to distinguish the query input and the values for input(s) functions. For
// example, consider using inputs with --null-input. If you want to allow
// input(s) functions, create an [Iter] and use WithInputIter option.
func WithInputIter(inputIter Iter) CompilerOption {
	return func(c *compiler) {
		c.inputIter = inputIter
	}
}
