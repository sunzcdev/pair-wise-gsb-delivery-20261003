// Package batch provides columnar (column-oriented) batch evaluation of a
// compiled expression program.
//
// Instead of evaluating an expression once per row with vm.Run, the input
// data is provided as columns: one slice per variable referenced by the
// expression, where each slice holds the values for every row. Run returns
// the result (or the error) for each input row.
//
// The batch evaluator covers the commonly used scalar subset of the language
// (constants, identifiers, field/map access, arithmetic, comparison,
// boolean connectives, ternary and coalescing operators, string operators,
// the "in" operator and calls to scalar builtins and registered functions).
// When the program contains a node that is not covered, evaluation falls back
// to running the ordinary VM row by row unless WithNoFallback is set.
package batch

import (
	"fmt"
	"reflect"

	"github.com/expr-lang/expr/vm"
)

// Column holds the values of one variable for every row.
//
// The supported concrete types are the typed scalar slices
//
//	[]bool, []string, []int, []int64, []float64, []float32,
//	[]int8, []int16, []int32, []uint, []uint8, []uint16,
//	[]uint32, []uint64 and []byte
//
// plus []any. Nullable values must be passed as []any with nil elements.
// Every column must contain exactly the same number of elements.
type Column struct {
	Name string
	Data any
}

// RowError is an error that occurred while evaluating a single row.
//
// Index is the zero based position of the row in the input columns, Line is
// the same position expressed as a one based line number.
type RowError struct {
	Index int
	Line  int
	Err   error
}

func (e *RowError) Error() string {
	return fmt.Sprintf("batch: row %d: %v", e.Line, e.Err)
}

func (e *RowError) Unwrap() error {
	return e.Err
}

// Options controls batch evaluation.
type Options struct {
	// NoFallback disables the automatic row by row fallback for programs
	// which contain unsupported nodes. Unsupported programs then return an
	// error from Run instead.
	NoFallback bool

	// Prototype is a value of the same type as the environment the program
	// was compiled against. It is only needed by the row by row fallback when
	// the program was compiled with a struct environment (expr.Env(SomeStruct{})).
	// A pointer or a value may be provided.
	Prototype any
}

// Option configures Options.
type Option func(*Options)

// WithNoFallback disables the automatic row by row fallback for programs
// which contain unsupported nodes.
func WithNoFallback() Option {
	return func(o *Options) {
		o.NoFallback = true
	}
}

// WithPrototype provides a value of the same type as the environment the
// program was compiled against. It is used by the row by row fallback when
// the program was compiled with a struct environment.
func WithPrototype(prototype any) Option {
	return func(o *Options) {
		o.Prototype = prototype
	}
}

// Run evaluates the program for every row provided as columns and returns the
// result and the error of each row. results[i] is nil when err[i] is not nil.
func Run(program *vm.Program, columns []Column, opts ...Option) (results []any, errs []error, err error) {
	options := Options{}
	for _, opt := range opts {
		opt(&options)
	}

	if program == nil {
		return nil, nil, fmt.Errorf("batch: program is nil")
	}

	n, vecs, err := normalize(columns)
	if err != nil {
		return nil, nil, err
	}

	if reason := unsupported(program.Node()); reason != "" {
		if options.NoFallback {
			return nil, nil, fmt.Errorf("batch: program is not supported by the columnar evaluator (%s)", reason)
		}
		return fallback(program, n, vecs, options.Prototype)
	}

	e := &evaluator{
		program: program,
		n:       n,
		columns: vecs,
		vars:    make(map[string]vector),
		errs:    make([]error, n),
		dead:    make([]bool, n),
	}

	out := e.eval(program.Node())

	results = make([]any, n)
	errs = make([]error, n)
	for i := 0; i < n; i++ {
		if e.errs[i] != nil {
			errs[i] = e.errs[i]
			results[i] = nil
			continue
		}
		results[i] = out.at(i)
	}

	// A trailing cast emitted for AsInt/AsBool/... options must be applied
	// to successful rows.
	if op, arg, ok := trailingCast(program); ok {
		for i := range results {
			if errs[i] != nil {
				continue
			}
			results[i] = cast(op, arg, results[i])
		}
	}

	return results, errs, nil
}

// RunMap is a convenience wrapper around Run which takes columns keyed by
// variable name.
func RunMap(program *vm.Program, columns map[string]any, opts ...Option) ([]any, []error, error) {
	cols := make([]Column, 0, len(columns))
	for name, data := range columns {
		cols = append(cols, Column{Name: name, Data: data})
	}
	return Run(program, cols, opts...)
}

// RunRows is a convenience wrapper which builds columns out of homogeneous
// row maps. Values missing from a row are treated as null.
func RunRows(program *vm.Program, rows []map[string]any, opts ...Option) ([]any, []error, error) {
	names := map[string]struct{}{}
	for _, row := range rows {
		for name := range row {
			names[name] = struct{}{}
		}
	}
	columns := make([]Column, 0, len(names))
	for name := range names {
		data := make([]any, len(rows))
		for i, row := range rows {
			data[i] = row[name]
		}
		columns = append(columns, Column{Name: name, Data: data})
	}
	return Run(program, columns, opts...)
}

// Supports reports whether the columnar evaluator handles the whole program
// without falling back. An empty reason means the program is supported.
func Supports(program *vm.Program) (supported bool, reason string) {
	if r := unsupported(program.Node()); r != "" {
		return false, r
	}
	return true, ""
}

func normalize(columns []Column) (int, map[string]vector, error) {
	vecs := make(map[string]vector, len(columns))
	n := -1
	for _, col := range columns {
		if _, exists := vecs[col.Name]; exists {
			return 0, nil, fmt.Errorf("batch: duplicate column %q", col.Name)
		}
		v, count, err := normalizeColumn(col.Data)
		if err != nil {
			return 0, nil, fmt.Errorf("batch: column %q: %w", col.Name, err)
		}
		if n == -1 {
			n = count
		} else if count != n {
			return 0, nil, fmt.Errorf("batch: column %q has %d rows, expected %d", col.Name, count, n)
		}
		vecs[col.Name] = v
	}
	if n == -1 {
		n = 0
	}
	return n, vecs, nil
}

func normalizeColumn(data any) (vector, int, error) {
	if data == nil {
		return vector{}, 0, fmt.Errorf("data is nil; use []any with nil elements for null values")
	}

	switch v := data.(type) {
	case []any:
		return vector{kind: vAny, anys: v}, len(v), nil
	case []bool:
		return vector{kind: vBool, bools: v}, len(v), nil
	case []string:
		return vector{kind: vString, strings: v}, len(v), nil
	case []int:
		return vector{kind: vInt, ints: v}, len(v), nil
	case []int64:
		return vector{kind: vInt64, int64s: v}, len(v), nil
	case []float64:
		return vector{kind: vFloat64, float64s: v}, len(v), nil
	case []float32:
		return vector{kind: vFloat32, float32s: v}, len(v), nil
	case []int8:
		return vector{kind: vInt8, int8s: v}, len(v), nil
	case []int16:
		return vector{kind: vInt16, int16s: v}, len(v), nil
	case []int32:
		return vector{kind: vInt32, int32s: v}, len(v), nil
	case []uint:
		return vector{kind: vUint, uints: v}, len(v), nil
	case []uint8:
		// []byte is handled as generic values, mirroring runtime
		// Matches/Contains byte slice behavior and avoiding ambiguity with
		// a string column.
		values := make([]any, len(v))
		for i := range v {
			values[i] = v[i]
		}
		return vector{kind: vAny, anys: values}, len(v), nil
	case []uint16:
		return vector{kind: vUint16, uint16s: v}, len(v), nil
	case []uint32:
		return vector{kind: vUint32, uint32s: v}, len(v), nil
	case []uint64:
		return vector{kind: vUint64, uint64s: v}, len(v), nil
	}

	rv := reflect.ValueOf(data)
	if rv.Kind() == reflect.Slice {
		return vector{}, 0, fmt.Errorf("unsupported column data type %T; use a typed scalar slice or []any", data)
	}
	return vector{}, 0, fmt.Errorf("column data must be a slice, got %T", data)
}
