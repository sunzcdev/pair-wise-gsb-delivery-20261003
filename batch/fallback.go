package batch

import (
	"fmt"
	"reflect"

	"github.com/expr-lang/expr/file"
	"github.com/expr-lang/expr/vm"
	"github.com/expr-lang/expr/vm/runtime"
)

// fallback runs the ordinary VM once per row. Every row gets its own error
// attributed to its original position; a failure in one row does not affect
// other rows.
func fallback(program *vm.Program, n int, columns map[string]vector, prototype any) (results []any, errs []error, err error) {
	results = make([]any, n)
	errs = make([]error, n)

	protoValue, protoType, err := makePrototype(prototype)
	if err != nil {
		return nil, nil, err
	}

	machine := vm.VM{}
	for i := 0; i < n; i++ {
		env := buildRowEnv(columns, i, protoValue, protoType)
		result, rerr := machine.Run(program, env)
		if rerr != nil {
			errs[i] = &RowError{
				Index: i,
				Line:  i + 1,
				Err:   bindRowError(program, rerr),
			}
			continue
		}
		results[i] = result
	}
	return results, errs, nil
}

// makePrototype returns a settable value and type of the prototype used when
// the program was compiled against a struct environment.
func makePrototype(prototype any) (reflect.Value, reflect.Type, error) {
	if prototype == nil {
		return reflect.Value{}, nil, nil
	}
	v := reflect.ValueOf(prototype)
	t := v.Type()
	if t.Kind() == reflect.Ptr {
		if v.IsNil() {
			return reflect.Value{}, nil, fmt.Errorf("batch: prototype pointer is nil")
		}
		return v, t.Elem(), nil
	}
	if t.Kind() != reflect.Struct {
		return reflect.Value{}, nil, fmt.Errorf("batch: prototype must be a struct or pointer to struct, got %T", prototype)
	}
	return v, t, nil
}

// buildRowEnv constructs the environment for one row. Without a prototype a
// map is used; with a struct prototype top level exported fields are filled
// reflectively.
func buildRowEnv(columns map[string]vector, row int, protoValue reflect.Value, protoType reflect.Type) any {
	if protoType == nil {
		env := make(map[string]any, len(columns))
		for name, col := range columns {
			env[name] = col.at(col.index(row))
		}
		return env
	}

	var dst reflect.Value
	if protoValue.Kind() == reflect.Ptr {
		dst = reflect.New(protoType)
	} else {
		dst = reflect.New(protoType).Elem()
	}
	elem := dst
	if elem.Kind() == reflect.Ptr {
		elem = elem.Elem()
	}

	for name, col := range columns {
		field := elem.FieldByName(name)
		if !field.IsValid() || !field.CanSet() {
			continue
		}
		value := col.at(col.index(row))
		if value == nil {
			continue
		}
		rv := reflect.ValueOf(value)
		if rv.Type().AssignableTo(field.Type()) {
			field.Set(rv)
		} else if rv.Type().ConvertibleTo(field.Type()) {
			field.Set(rv.Convert(field.Type()))
		}
	}

	if dst.Kind() == reflect.Ptr {
		return dst.Interface()
	}
	return dst.Interface()
}

// bindRowError ensures the row error is a *file.Error bound to the program
// source so the snippet is rendered exactly as for scalar evaluation.
func bindRowError(program *vm.Program, rerr error) error {
	var fe *file.Error
	if asFileError(rerr, &fe) {
		return fe
	}
	fe = &file.Error{Message: rerr.Error()}
	return fe.Bind(program.Source())
}

func asFileError(err error, target **file.Error) bool {
	for err != nil {
		if fe, ok := err.(*file.Error); ok {
			*target = fe
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

// trailingCast reports whether the program ends with an OpCast emitted by the
// AsInt/AsInt64/AsFloat64/AsBool options.
func trailingCast(program *vm.Program) (vm.Opcode, int, bool) {
	bytecode := program.Bytecode
	if len(bytecode) == 0 {
		return 0, 0, false
	}
	ip := len(bytecode) - 1
	op := bytecode[ip]
	if op == vm.OpCast {
		return op, program.Arguments[ip], true
	}
	return 0, 0, false
}

func cast(_ vm.Opcode, arg int, value any) any {
	switch arg {
	case 0:
		return runtime.ToInt(value)
	case 1:
		return runtime.ToInt64(value)
	case 2:
		return runtime.ToFloat64(value)
	case 3:
		return runtime.ToBool(value)
	}
	return value
}
