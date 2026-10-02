package vm

import (
	"fmt"
	"reflect"
)

// runBatchFallback evaluates every row with the standard scalar VM.
//
// Map envs are rebuilt per row from the columns. Struct envs (prototype type
// provided) reuse a single value whose fields are set per row. Per-row panics
// are recovered as row errors, preserving the original row index.
func runBatchFallback(program *Program, input batchInput) (*BatchResult, error) {
	res := &BatchResult{Values: make([]any, input.n)}

	names := make([]string, 0, len(input.columns))
	for name := range input.columns {
		names = append(names, name)
	}

	scalar := VM{}

	if input.protoType != nil && input.protoType.Kind() == reflect.Struct {
		type setter struct {
			name  string
			index []int
			col   any
		}
		setters := make([]setter, 0, len(names))
		for _, name := range names {
			idx, ok := resolveStructPath(input.protoType, splitPath(name))
			if !ok {
				return nil, fmt.Errorf("batch column %q is not a settable field of %s", name, input.protoType)
			}
			setters = append(setters, setter{name: name, index: idx, col: input.columns[name]})
		}

		rowVal := reflect.New(input.protoType).Elem()
		for r := 0; r < input.n; r++ {
			for _, s := range setters {
				fv := rowVal.FieldByIndex(s.index)
				rv := reflect.ValueOf(columnAt(s.col, r))
				if !rv.IsValid() {
					fv.Set(reflect.Zero(fv.Type()))
				} else if rv.Type().AssignableTo(fv.Type()) {
					fv.Set(rv)
				} else if rv.Type().ConvertibleTo(fv.Type()) {
					fv.Set(rv.Convert(fv.Type()))
				} else {
					return nil, fmt.Errorf("batch column %q: %s not assignable to %s", s.name, rv.Type(), fv.Type())
				}
			}
			runOneRow(program, &scalar, res, r, rowVal.Interface())
		}
		return res, nil
	}

	for r := 0; r < input.n; r++ {
		env := make(map[string]any, len(names))
		for _, name := range names {
			env[name] = columnAt(input.columns[name], r)
		}
		runOneRow(program, &scalar, res, r, env)
	}
	return res, nil
}

// runOneRow executes the scalar VM for a single row, recovering panics into
// row errors. The scalar VM already returns a *file.Error for runtime panics,
// so the message/location is identical to a direct Run call.
func runOneRow(program *Program, scalar *VM, res *BatchResult, row int, env any) {
	out, err := scalar.Run(program, env)
	if err != nil {
		res.Errors = append(res.Errors, RowError{Row: row, Error: err})
		return
	}
	res.Values[row] = out
}

func resolveStructPath(typ reflect.Type, path []string) ([]int, bool) {
	var index []int
	t := typ
	for _, name := range path {
		f, ok := t.FieldByName(name)
		if !ok || !f.IsExported() {
			return nil, false
		}
		index = append(index, f.Index...)
		ft := f.Type
		for ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		if ft.Kind() != reflect.Struct {
			break
		}
		t = ft
	}
	return index, true
}

func splitPath(path string) []string {
	var out []string
	start := 0
	for i := 0; i < len(path); i++ {
		if path[i] == '.' {
			out = append(out, path[start:i])
			start = i + 1
		}
	}
	out = append(out, path[start:])
	return out
}
