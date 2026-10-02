package batch

import (
	"fmt"
	"reflect"

	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/builtin"
	"github.com/expr-lang/expr/internal/deref"
	"github.com/expr-lang/expr/vm"
)

var errorType = reflect.TypeOf((*error)(nil)).Elem()

// callNode handles function and method calls. Registered expr.Function
// values are invoked directly; everything else falls back to a per row
// reflective invocation which mirrors OpCall. Calls are isolated per row so a
// panic is attributed to one row, and dead rows never invoke the call.
func (e *evaluator) callNode(node *ast.CallNode) vector {
	argVectors := make([]vector, len(node.Arguments))
	for i, arg := range node.Arguments {
		argVectors[i] = e.eval(arg)
	}

	if ident, ok := node.Callee.(*ast.IdentifierNode); ok {
		if fn := e.program.FunctionByName(ident.Value); fn != nil {
			return e.callRegistered(node, argVectors, fn)
		}
	}

	callee := e.eval(node.Callee)

	out := newAnys(e.n)
	for i := 0; i < e.n; i++ {
		if !e.activeRow(i) || e.isMaskedOff(i) {
			continue
		}
		idx := i
		e.runIsolated(idx, func(row int) {
			fnValue := reflect.ValueOf(callee.at(callee.index(row)))
			out.setAny(row, e.reflectCall(fnValue, node, argVectors, row))
		})
	}
	return out
}

func (e *evaluator) callRegistered(node *ast.CallNode, argVectors []vector, fn vm.Function) vector {
	out := newAnys(e.n)
	if cap(e.argBuf) < len(argVectors) {
		e.argBuf = make([]any, len(argVectors))
	}
	args := e.argBuf[:len(argVectors):len(argVectors)]
	for i := 0; i < e.n; i++ {
		if !e.activeRow(i) || e.isMaskedOff(i) {
			continue
		}
		row := i
		e.runIsolated(row, func(r int) {
			for j, av := range argVectors {
				args[j] = e.derefCallArg(node.Arguments[j], av.at(av.index(r)))
			}
			result, err := fn(args...)
			if err != nil {
				panic(err)
			}
			out.setAny(r, result)
		})
	}
	return out
}

// reflectCall mirrors the OpCall handling for arbitrary function values.
func (e *evaluator) reflectCall(fn reflect.Value, node *ast.CallNode, argVectors []vector, row int) any {
	if !fn.IsValid() {
		panic("invalid operation: cannot call nil")
	}
	if fn.Kind() != reflect.Func {
		panic(interfaceCallNonFunction(fn))
	}

	fnType := fn.Type()
	size := len(argVectors)
	isVariadic := fnType.IsVariadic()
	numIn := fnType.NumIn()
	if isVariadic {
		if size < numIn-1 {
			panicfInvalidArgs("expected at least %d, got %d", numIn-1, size)
		}
	} else if size != numIn {
		panicfInvalidArgs("expected %d, got %d", numIn, size)
	}

	in := make([]reflect.Value, size)
	for i := 0; i < size; i++ {
		param := e.derefCallArg(node.Arguments[i], argVectors[i].at(argVectors[i].index(row)))
		if param == nil {
			var inType reflect.Type
			if isVariadic && i >= numIn-1 {
				inType = fnType.In(numIn - 1).Elem()
			} else {
				inType = fnType.In(i)
			}
			in[i] = reflect.Zero(inType)
			continue
		}
		in[i] = reflect.ValueOf(param)
	}

	result := fn.Call(in)
	if len(result) == 2 && result[1].Type() == errorType && !result[1].IsNil() {
		panic(result[1].Interface().(error))
	}
	return result[0].Interface()
}

func (e *evaluator) derefCallArg(node ast.Node, value any) any {
	t := node.Type()
	if t == nil {
		return value
	}
	if t.Kind() == reflect.Ptr || t.Kind() == reflect.Interface {
		return deref.Interface(value)
	}
	return value
}

func interfaceCallNonFunction(fn reflect.Value) any {
	return "invalid operation: cannot call non-function of type " + fn.Type().String()
}

func panicfInvalidArgs(format string, args ...any) {
	panic(fmt.Sprintf("invalid number of arguments: "+format, args...))
}

// builtinNode invokes scalar builtins per row using the exact builtin
// implementation referenced by the compiled program.
func (e *evaluator) builtinNode(node *ast.BuiltinNode) vector {
	id, ok := builtin.Index[node.Name]
	if !ok {
		panic("unknown builtin " + node.Name)
	}
	f := builtin.Builtins[id]

	argVectors := make([]vector, len(node.Arguments))
	for i, arg := range node.Arguments {
		argVectors[i] = e.eval(arg)
	}

	out := newAnys(e.n)
	if cap(e.argBuf) < len(argVectors) {
		e.argBuf = make([]any, len(argVectors))
	}
	args := e.argBuf[:len(argVectors):len(argVectors)]
	for i := 0; i < e.n; i++ {
		if !e.activeRow(i) || e.isMaskedOff(i) {
			continue
		}
		row := i
		e.runIsolated(row, func(r int) {
			for j, av := range argVectors {
				value := av.at(av.index(r))
				argType := node.Arguments[j].Type()
				if argType != nil && (argType.Kind() == reflect.Ptr || argType.Kind() == reflect.Interface) {
					if f.Deref == nil || f.Deref(j, argType) {
						value = deref.Interface(value)
					}
				}
				args[j] = value
			}

			switch {
			case f.Fast != nil:
				out.setAny(r, f.Fast(args[0]))
			case f.Safe != nil:
				result, _, err := f.Safe(args...)
				if err != nil {
					panic(err)
				}
				out.setAny(r, result)
			case f.Func != nil:
				result, err := f.Func(args...)
				if err != nil {
					panic(err)
				}
				out.setAny(r, result)
			default:
				panic("builtin " + node.Name + " is not invocable")
			}
		})
	}
	return out
}
