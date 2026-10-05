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

// WithMeteredFunction is a compiler option for adding a custom internal
// function that can charge VM instructions, just like [WithFunction]. The
// callback additionally receives an [InstructionMeter], on which it can call
// Charge to account for the work done inside Go; such work is not counted
// automatically. When Charge returns false, the run is terminated with an
// [*InstructionLimitError].
func WithMeteredFunction(name string, minarity, maxarity int, f func(InstructionMeter, any, []any) any) CompilerOption {
	return withMeteredFunction(name, minarity, maxarity, false, f)
}

// WithMeteredIterFunction is a compiler option for adding a custom iterator
// function that can charge VM instructions, just like [WithIterFunction].
// The callback, and the [Iter] it returns, can call Charge on the given
// [InstructionMeter] to account for the work done inside Go.
func WithMeteredIterFunction(name string, minarity, maxarity int, f func(InstructionMeter, any, []any) Iter) CompilerOption {
	return withMeteredFunction(name, minarity, maxarity, true,
		func(m InstructionMeter, v any, args []any) any {
			return f(m, v, args)
		},
	)
}

func withMeteredFunction(name string, minarity, maxarity int, iter bool, f func(InstructionMeter, any, []any) any) CompilerOption {
	if !(0 <= minarity && minarity <= maxarity && maxarity <= 30) {
		panic(fmt.Sprintf("invalid arity for %q: %d, %d", name, minarity, maxarity))
	}
	argcount := 1<<(maxarity+1) - 1<<minarity
	return func(c *compiler) {
		if c.customFuncs == nil {
			c.customFuncs = make(map[string]function)
		}
		if fn, ok := c.customFuncs[name]; ok {
			if fn.iter != iter || !isMeteredCallback(fn.callback) {
				panic(fmt.Sprintf("cannot define both iterator and non-iterator functions for %q", name))
			}
			other := fn.callback.(func(InstructionMeter, any, []any) any)
			fn.callback = func(m InstructionMeter, x any, xs []any) any {
				if argcount&(1<<len(xs)) != 0 {
					return f(m, x, xs)
				}
				return other(m, x, xs)
			}
			fn.argcount |= argcount
			c.customFuncs[name] = fn
		} else {
			c.customFuncs[name] = function{argcount: argcount, iter: iter, callback: f}
		}
	}
}

func isMeteredCallback(callback any) bool {
	_, ok := callback.(func(InstructionMeter, any, []any) any)
	return ok
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
			if fn.iter != iter || isMeteredCallback(fn.callback) {
				panic(fmt.Sprintf("cannot define both iterator and non-iterator functions for %q", name))
			}
			other := fn.callback.(func(any, []any) any)
			fn.callback = func(x any, xs []any) any {
				if argcount&(1<<len(xs)) != 0 {
					return f(x, xs)
				}
				return other(x, xs)
			}
			fn.argcount |= argcount
			c.customFuncs[name] = fn
		} else {
			c.customFuncs[name] = function{argcount: argcount, iter: iter, callback: f}
		}
	}
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
