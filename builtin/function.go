package builtin

import (
	"reflect"
)

type Function struct {
	Name      string
	Fast      func(arg any) any
	Func      func(args ...any) (any, error)
	Safe      func(args ...any) (any, uint, error)
	Types     []reflect.Type
	Validate  func(args []reflect.Type) (reflect.Type, error)
	Deref     func(i int, arg reflect.Type) bool
	Predicate bool
	// Impure marks builtins that read external state (current time, randomness,
	// environment) or produce side effects. Impure builtins are rejected when
	// compiling with pure rule mode enabled (expr.Pure).
	Impure bool
}

func (f *Function) Type() reflect.Type {
	if len(f.Types) > 0 {
		return f.Types[0]
	}
	return reflect.TypeOf(f.Func)
}
