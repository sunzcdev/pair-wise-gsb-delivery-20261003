package gojq

import "fmt"

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

// WithIterFunction is a compiler option for adding a custom iterator function.
// This is like the [WithFunction] option, but you can add a function which
// returns an Iter to emit multiple values. You cannot define both iterator and
// non-iterator functions of the same name (with possibly different arities).
// See also [NewIter], which can be used to convert values or an error to an Iter.
func WithIterFunction(name string, minarity, maxarity int, f func(any, []any) Iter) CompilerOption {
	return withFunction(name, minarity, maxarity, true,
		func(v any, args []any) any {
			return f(v, args)
		},
	)
}

func withFunction(name string, minarity, maxarity int, iter bool, f func(any, []any) any) CompilerOption {
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
				func(x any, xs []any) any {
					if argcount&(1<<len(xs)) != 0 {
						return f(x, xs)
					}
					return fn.callback(x, xs)
				},
			}
		} else {
			c.customFuncs[name] = function{argcount, iter, f}
		}
	}
}

// WithInputIter is a compiler option for the input iterator used by
// input(s)/0. The iterator is shared across every [*Code.Run] of the compiled
// code, so values consumed by one run are not available to the others. It is
// never closed automatically.
//
// To bind an isolated input source to each run (so a single compiled code can
// serve multiple requests without them consuming each other's data), use
// [*Code.RunWith] or [*Code.RunWithWithContext] instead. An iterator passed
// at run time takes precedence over this option; pass nil to fall back to the
// iterator configured here.
//
// We have to distinguish the query input and the values for input(s)
// functions. For example, consider using inputs with --null-input. When
// neither this option nor a run-time iterator is provided, input and inputs
// emit an error when executed.
func WithInputIter(inputIter Iter) CompilerOption {
	return func(c *compiler) {
		c.inputIter = inputIter
	}
}
