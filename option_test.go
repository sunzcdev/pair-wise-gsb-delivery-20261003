package gojq_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"

	"github.com/itchyny/gojq"
)

func TestWithModuleLoaderError(t *testing.T) {
	query, err := gojq.Parse(`
		import "module1" as m;
		def f: 42;
		m::f, f
	`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = gojq.Compile(query)
	if got, expected := err.Error(), `cannot load module: "module1"`; got != expected {
		t.Errorf("expected: %v, got: %v", expected, got)
	}

	query, err = gojq.Parse("modulemeta")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run("m")
	v, ok := iter.Next()
	if !ok {
		t.Fatal("should emit an error but got no output")
	}
	if err, ok := v.(error); ok {
		if expected := `cannot load module: "m"`; err.Error() != expected {
			t.Errorf("expected: %v, got: %v", expected, err)
		}
	} else {
		t.Errorf("should emit an error but got: %v", v)
	}
	v, ok = iter.Next()
	if ok {
		t.Errorf("should not emit a value but got: %v", v)
	}
}

func TestWithModuleLoader_modulemeta(t *testing.T) {
	query, err := gojq.Parse(`
		"module1", "module2", "module3" | modulemeta
	`)
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(
		query,
		gojq.WithModuleLoader(&moduleLoader{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run(nil)
	n := 0
	for {
		got, ok := iter.Next()
		if !ok {
			break
		}
		switch n {
		case 0:
			if expected := map[string]any{
				"defs": []any{"g/0"},
				"deps": []any{
					map[string]any{
						"relpath": "module2",
						"as":      "foo",
						"is_data": false,
					},
				},
				"name": "module1",
				"test": 42,
			}; !reflect.DeepEqual(got, expected) {
				t.Errorf("expected: %v, got: %v", expected, got)
			}
		case 1:
			if expected := map[string]any{
				"defs": []any{"f/0"},
				"deps": []any{}, // not a nil-slice
			}; !reflect.DeepEqual(got, expected) {
				t.Errorf("expected: %v, got: %v", expected, got)
			}
		default:
			if expected := map[string]any{
				"defs": []any{}, // not a nil-slice
				"deps": []any{}, // not a nil-slice
			}; !reflect.DeepEqual(got, expected) {
				t.Errorf("expected: %v, got: %v", expected, got)
			}
		}
		n++
	}
}

type moduleLoaderJSON struct{}

func (*moduleLoaderJSON) LoadJSON(name string) (any, error) {
	if name == "module1" {
		return []any{1.0, 42, json.Number("123")}, nil
	}
	return nil, fmt.Errorf("module not found: %q", name)
}

func TestWithModuleLoader_JSON(t *testing.T) {
	query, err := gojq.Parse(`
		import "module1" as $m;
		[$m, $m[1]*$m[2]]
	`)
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(
		query,
		gojq.WithModuleLoader(&moduleLoaderJSON{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run(nil)
	for {
		got, ok := iter.Next()
		if !ok {
			break
		}
		if expected := []any{[]any{1.0, 42, json.Number("123")}, 5166}; !reflect.DeepEqual(got, expected) {
			t.Errorf("expected: %v, got: %v", expected, got)
		}
	}
}

type moduleLoaderInitModules struct{}

func (*moduleLoaderInitModules) LoadInitModules() ([]*gojq.Query, error) {
	query, err := gojq.Parse(`
		def f: $x;
		def g: f * f;
	`)
	if err != nil {
		return nil, err
	}
	return []*gojq.Query{query}, nil
}

func TestWithModuleLoader_LoadInitModules(t *testing.T) {
	query, err := gojq.Parse("g")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(
		query,
		gojq.WithVariables([]string{"$x"}),
		gojq.WithModuleLoader(&moduleLoaderInitModules{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run(nil, 42)
	for {
		got, ok := iter.Next()
		if !ok {
			break
		}
		if expected := 42 * 42; !reflect.DeepEqual(got, expected) {
			t.Errorf("expected: %v, got: %v", expected, got)
		}
	}
}

func TestWithEnvironLoader(t *testing.T) {
	query, err := gojq.Parse("env")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(
		query,
		gojq.WithEnvironLoader(func() []string {
			return []string{"foo=42", "bar=128", "baz", "qux=", "=0"}
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run(nil)
	for {
		got, ok := iter.Next()
		if !ok {
			break
		}
		expected := map[string]any{"foo": "42", "bar": "128", "qux": ""}
		if !reflect.DeepEqual(got, expected) {
			t.Errorf("expected: %v, got: %v", expected, got)
		}
	}
}

func TestWithEnvironLoaderEmpty(t *testing.T) {
	query, err := gojq.Parse("env")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run(nil)
	for {
		got, ok := iter.Next()
		if !ok {
			break
		}
		if expected := map[string]any{}; !reflect.DeepEqual(got, expected) {
			t.Errorf("expected: %v, got: %v", expected, got)
		}
	}
}

func TestWithVariablesError0(t *testing.T) {
	query, err := gojq.Parse(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "$", "_x", "x", "$0x", "$$", "$x ", " $x"} {
		_, err = gojq.Compile(
			query,
			gojq.WithVariables([]string{name}),
		)
		if err == nil {
			t.Fatalf("%q is not a valid variable name", name)
		}
		if got, expected := err.Error(), "invalid variable name: "+name; got != expected {
			t.Errorf("expected: %v, got: %v", expected, got)
		}
	}
}

func TestWithVariablesError1(t *testing.T) {
	query, err := gojq.Parse(".")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(
		query,
		gojq.WithVariables([]string{"$x"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run(nil)
	v, ok := iter.Next()
	if !ok {
		t.Fatal("should emit an error but got no output")
	}
	if err, ok := v.(error); ok {
		if expected := "variable defined but not bound: $x"; err.Error() != expected {
			t.Errorf("expected: %v, got: %v", expected, err)
		}
	} else {
		t.Errorf("should emit an error but got: %v", v)
	}
	v, ok = iter.Next()
	if ok {
		t.Errorf("should not emit a value but got: %v", v)
	}
}

func TestWithVariablesError2(t *testing.T) {
	query, err := gojq.Parse(".")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(
		query,
		gojq.WithVariables([]string{"$x"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run(nil, 1, 2)
	v, ok := iter.Next()
	if !ok {
		t.Fatal("should emit an error but got no output")
	}
	if err, ok := v.(error); ok {
		if expected := "too many variable values provided"; err.Error() != expected {
			t.Errorf("expected: %v, got: %v", expected, err)
		}
	} else {
		t.Errorf("should emit an error but got: %v", v)
	}
	v, ok = iter.Next()
	if ok {
		t.Errorf("should not emit a value but got: %v", v)
	}
}

func TestWithFunction(t *testing.T) {
	options := []gojq.CompilerOption{
		gojq.WithFunction("f", 0, 0, func(x any, _ []any) any {
			if x, ok := x.(int); ok {
				return x * 2
			}
			return fmt.Errorf("f cannot be applied to: %v", x)
		}),
		gojq.WithFunction("g", 1, 1, func(x any, xs []any) any {
			if x, ok := x.(int); ok {
				if y, ok := xs[0].(int); ok {
					return x + y
				}
			}
			return fmt.Errorf("g cannot be applied to: %v, %v", x, xs)
		}),
	}
	query, err := gojq.Parse(".[] | f | g(3)")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query, options...)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run([]any{0, 1, 2, 3, 4})
	n := 0
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if expected := n*2 + 3; v != expected {
			t.Errorf("expected: %v, got: %v", expected, v)
		}
		n++
	}
	if expected := 5; n != expected {
		t.Errorf("expected: %v, got: %v", expected, n)
	}
	query, err = gojq.Parse(
		`("f/0", "f/1", "g/0", "g/1") as $f | builtins | any(. == $f)`,
	)
	if err != nil {
		t.Fatal(err)
	}
	code, err = gojq.Compile(query, options...)
	if err != nil {
		t.Fatal(err)
	}
	iter = code.Run(nil)
	n = 0
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if expected := n == 0 || n == 3; v != expected {
			t.Errorf("expected: %v, got: %v (n = %d)", expected, v, n)
		}
		n++
	}
	if expected := 4; n != expected {
		t.Errorf("expected: %v, got: %v", expected, n)
	}
}

func TestWithFunctionDuplicateName(t *testing.T) {
	options := []gojq.CompilerOption{
		gojq.WithFunction("f", 0, 0, func(x any, _ []any) any {
			if x, ok := x.(int); ok {
				return x * 2
			}
			return fmt.Errorf("f cannot be applied to: %v", x)
		}),
		gojq.WithFunction("f", 1, 1, func(x any, xs []any) any {
			if x, ok := x.(int); ok {
				if y, ok := xs[0].(int); ok {
					return x + y
				}
			}
			return fmt.Errorf("f cannot be applied to: %v, %v", x, xs)
		}),
		gojq.WithFunction("f", 0, 0, func(x any, _ []any) any {
			if x, ok := x.(int); ok {
				return x * 4
			}
			return fmt.Errorf("f cannot be applied to: %v", x)
		}),
		gojq.WithFunction("f", 2, 2, func(x any, xs []any) any {
			if x, ok := x.(int); ok {
				if y, ok := xs[0].(int); ok {
					if z, ok := xs[1].(int); ok {
						return x * y * z
					}
				}
			}
			return fmt.Errorf("f cannot be applied to: %v, %v", x, xs)
		}),
	}
	query, err := gojq.Parse(".[] | f | f(3) | f(4; 5)")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query, options...)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run([]any{0, 1, 2, 3, 4})
	n := 0
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if expected := (n*4 + 3) * 4 * 5; v != expected {
			t.Errorf("expected: %v, got: %v", expected, v)
		}
		n++
	}
	if expected := 5; n != expected {
		t.Errorf("expected: %v, got: %v", expected, n)
	}
	query, err = gojq.Parse(
		`("f/0", "f/1", "f/2", "f/3") as $f | builtins | any(. == $f)`,
	)
	if err != nil {
		t.Fatal(err)
	}
	code, err = gojq.Compile(query, options...)
	if err != nil {
		t.Fatal(err)
	}
	iter = code.Run(nil)
	n = 0
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if expected := n < 3; v != expected {
			t.Errorf("expected: %v, got: %v (n = %d)", expected, v, n)
		}
		n++
	}
	if expected := 4; n != expected {
		t.Errorf("expected: %v, got: %v", expected, n)
	}
}

func TestWithFunctionMultipleArities(t *testing.T) {
	options := []gojq.CompilerOption{
		gojq.WithFunction("f", 0, 4, func(x any, xs []any) any {
			if x, ok := x.(int); ok {
				x *= 2
				for _, y := range xs {
					if y, ok := y.(int); ok {
						x += y
					}
				}
				return x
			}
			return fmt.Errorf("f cannot be applied to: %v, %v", x, xs)
		}),
		gojq.WithFunction("f", 2, 3, func(x any, xs []any) any {
			if x, ok := x.(int); ok {
				for _, y := range xs {
					if y, ok := y.(int); ok {
						x *= y
					}
				}
				return x
			}
			return fmt.Errorf("f cannot be applied to: %v, %v", x, xs)
		}),
		gojq.WithFunction("g", 0, 30, func(any, []any) any {
			return nil
		}),
	}
	query, err := gojq.Parse(".[] | f | f(1) | f(2; 3) | f(4; 5; 6) | f(7; 8; 9; 10)")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query, options...)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run([]any{0, 1, 2, 3, 4})
	n := 0
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if expected := (((n*2*2+1)*2*3)*4*5*6)*2 + 7 + 8 + 9 + 10; v != expected {
			t.Errorf("expected: %v, got: %v", expected, v)
		}
		n++
	}
	if expected := 5; n != expected {
		t.Errorf("expected: %v, got: %v", expected, n)
	}
	query, err = gojq.Parse(
		`("f/0", "f/1", "f/2", "f/3", "f/4", "f/5") as $f | builtins | any(. == $f)`,
	)
	if err != nil {
		t.Fatal(err)
	}
	code, err = gojq.Compile(query, options...)
	if err != nil {
		t.Fatal(err)
	}
	iter = code.Run(nil)
	n = 0
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if expected := n < 5; v != expected {
			t.Errorf("expected: %v, got: %v (n = %d)", expected, v, n)
		}
		n++
	}
	if expected := 6; n != expected {
		t.Errorf("expected: %v, got: %v", expected, n)
	}
}

type valueError struct {
	v any
}

func (err *valueError) Error() string {
	return "error: " + fmt.Sprint(err.v)
}

func (err *valueError) Value() any {
	return err.v
}

func TestWithFunctionValueError(t *testing.T) {
	expected := map[string]any{"foo": 42}
	query, err := gojq.Parse("try f catch .")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query,
		gojq.WithFunction("f", 0, 0, func(any, []any) any {
			return &valueError{expected}
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run(nil)
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if !reflect.DeepEqual(v, expected) {
			t.Errorf("expected: %v, got: %v", expected, v)
		}
	}
}

func TestWithFunctionCompileArgsError(t *testing.T) {
	query, err := gojq.Parse("f(g)")
	if err != nil {
		t.Fatal(err)
	}
	_, err = gojq.Compile(query,
		gojq.WithFunction("f", 0, 1, func(any, []any) any {
			return 0
		}),
	)
	if got, expected := err.Error(), "function not defined: g/0"; got != expected {
		t.Errorf("expected: %v, got: %v", expected, got)
	}
}

func TestWithFunctionArityError(t *testing.T) {
	query, err := gojq.Parse("f")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ min, max int }{{3, 2}, {-1, 3}, {0, 31}, {-1, 31}} {
		func() {
			defer func() {
				expected := fmt.Sprintf(`invalid arity for "f": %d, %d`, tc.min, tc.max)
				if got := recover(); got != expected {
					t.Errorf("expected: %v, got: %v", expected, got)
				}
			}()
			t.Fatal(gojq.Compile(query,
				gojq.WithFunction("f", tc.min, tc.max, func(any, []any) any {
					return 0
				}),
			))
		}()
	}
}

func TestWithIterFunction(t *testing.T) {
	query, err := gojq.Parse("f * g(5; 10), h, 10")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query,
		gojq.WithIterFunction("f", 0, 0, func(any, []any) gojq.Iter {
			return gojq.NewIter(1, 2, 3)
		}),
		gojq.WithIterFunction("g", 2, 2, func(_ any, xs []any) gojq.Iter {
			if x, ok := xs[0].(int); ok {
				if y, ok := xs[1].(int); ok {
					return &rangeIter{x, y}
				}
			}
			return gojq.NewIter(fmt.Errorf("g cannot be applied to: %v", xs))
		}),
		gojq.WithIterFunction("h", 0, 0, func(any, []any) gojq.Iter {
			return gojq.NewIter[any]()
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run(nil)
	n := 0
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if expected := (1 + n%3) * (5 + n/3); v != expected {
			t.Errorf("expected: %v, got: %v", expected, v)
		}
		n++
	}
	if expected := 16; n != expected {
		t.Errorf("expected: %v, got: %v", expected, n)
	}
}

func TestWithIterFunctionError(t *testing.T) {
	query, err := gojq.Parse("range(3) * (f, 0), f")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query,
		gojq.WithIterFunction("f", 0, 0, func(any, []any) gojq.Iter {
			return gojq.NewIter[any](1, errors.New("error"), 3)
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run(nil)
	n := 0
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		switch n {
		case 0, 1, 2:
			if expected := n; v != expected {
				t.Errorf("expected: %v, got: %v", expected, v)
			}
		case 3, 5:
			if expected := "error"; v.(error).Error() != expected {
				t.Errorf("expected: %v, got: %v", expected, err)
			}
		default:
			if expected := n - 3; v != expected {
				t.Errorf("expected: %v, got: %v", expected, v)
			}
		}
		n++
	}
	if expected := 7; n != expected {
		t.Errorf("expected: %v, got: %v", expected, n)
	}
}

func TestWithIterFunctionPathIndexing(t *testing.T) {
	query, err := gojq.Parse(".[f] = 1")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query,
		gojq.WithIterFunction("f", 0, 0, func(any, []any) gojq.Iter {
			return gojq.NewIter(0, 1, 2)
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run(nil)
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if expected := []any{1, 1, 1}; !reflect.DeepEqual(v, expected) {
			t.Errorf("expected: %v, got: %v", expected, v)
		}
	}
}

func TestWithIterFunctionPathInputValue(t *testing.T) {
	query, err := gojq.Parse("{x: 0} | (f|.x) = 1")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query,
		gojq.WithIterFunction("f", 0, 0, func(v any, _ []any) gojq.Iter {
			return gojq.NewIter(v, v, v)
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run(nil)
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if expected := map[string]any{"x": 1}; !reflect.DeepEqual(v, expected) {
			t.Errorf("expected: %v, got: %v", expected, v)
		}
	}
}

func TestWithIterFunctionPathEmpty(t *testing.T) {
	query, err := gojq.Parse("{x: 0} | (f|.x) = 1")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query,
		gojq.WithIterFunction("f", 0, 0, func(any, []any) gojq.Iter {
			return gojq.NewIter[any]()
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run(nil)
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if expected := map[string]any{"x": 0}; !reflect.DeepEqual(v, expected) {
			t.Errorf("expected: %v, got: %v", expected, v)
		}
	}
}

func TestWithIterFunctionInvalidPathError(t *testing.T) {
	query, err := gojq.Parse("{x: 0} | (f|.x) = 1")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query,
		gojq.WithIterFunction("f", 0, 0, func(any, []any) gojq.Iter {
			return gojq.NewIter(map[string]any{"x": 1})
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run(nil)
	v, ok := iter.Next()
	if !ok {
		t.Fatal("should emit an error but got no output")
	}
	if err, ok := v.(error); ok {
		if expected := `invalid path against: object ({"x":1})`; err.Error() != expected {
			t.Errorf("expected: %v, got: %v", expected, err)
		}
	} else {
		t.Errorf("should emit an error but got: %v", v)
	}
}

func TestWithIterFunctionPathError(t *testing.T) {
	query, err := gojq.Parse("{x: 0} | (f|.x) = 1")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query,
		gojq.WithIterFunction("f", 0, 0, func(any, []any) gojq.Iter {
			return gojq.NewIter(errors.New("error"))
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run(nil)
	v, ok := iter.Next()
	if !ok {
		t.Fatal("should emit an error but got no output")
	}
	if err, ok := v.(error); ok {
		if expected := "error"; err.Error() != expected {
			t.Errorf("expected: %v, got: %v", expected, err)
		}
	} else {
		t.Errorf("should emit an error but got: %v", v)
	}
}

func TestWithIterFunctionDefineError(t *testing.T) {
	query, err := gojq.Parse("f")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		expected := `cannot define both iterator and non-iterator functions for "f"`
		if got := recover(); got != expected {
			t.Errorf("expected: %v, got: %v", expected, got)
		}
	}()
	t.Fatal(gojq.Compile(query,
		gojq.WithFunction("f", 0, 0, func(any, []any) any {
			return 0
		}),
		gojq.WithIterFunction("f", 0, 0, func(any, []any) gojq.Iter {
			return gojq.NewIter[any]()
		}),
	))
}

type moduleLoader2 struct{}

func (*moduleLoader2) LoadModule(name string) (*gojq.Query, error) {
	if name == "module1" {
		return gojq.Parse(`
			def g: def h: f * 3; h * 4;
		`)
	}
	return nil, fmt.Errorf("module not found: %q", name)
}

func TestWithFunctionWithModuleLoader(t *testing.T) {
	query, err := gojq.Parse(`include "module1"; .[] | g`)
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query,
		gojq.WithFunction("f", 0, 0, func(x any, _ []any) any {
			if x, ok := x.(int); ok {
				return x * 2
			}
			return fmt.Errorf("f cannot be applied to: %v", x)
		}),
		gojq.WithModuleLoader(&moduleLoader2{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run([]any{0, 1, 2, 3, 4})
	n := 0
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if expected := n * 2 * 3 * 4; v != expected {
			t.Errorf("expected: %v, got: %v", expected, v)
		}
		n++
	}
	if expected := 5; n != expected {
		t.Errorf("expected: %v, got: %v", expected, n)
	}
}

func TestWithInputIter(t *testing.T) {
	query, err := gojq.Parse("range(10) | input")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(
		query,
		gojq.WithInputIter(gojq.NewIter(0, 1, 2, 3, 4)),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run(nil)
	n := 0
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, ok := v.(error); ok {
			if expected := "break"; err.Error() != expected {
				t.Errorf("expected: %v, got: %v", expected, err)
			}
		} else if v != n {
			t.Errorf("expected: %v, got: %v", n, v)
		}
		n++
	}
	if expected := 10; n != expected {
		t.Errorf("expected: %v, got: %v", expected, n)
	}
}

func TestWithFunctionWithContext(t *testing.T) {
	type ctxKey struct{}
	query, err := gojq.Parse("f, f(1)")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(
		query,
		gojq.WithFunctionWithContext("f", 0, 1, func(ctx context.Context, v any, xs []any) any {
			if got := ctx.Value(ctxKey{}); got != "value" {
				return fmt.Errorf("unexpected context value: %v", got)
			}
			if len(xs) == 1 {
				return xs[0]
			}
			return v
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), ctxKey{}, "value")
	iter := code.RunWithContext(ctx, nil)
	for n := 0; ; n++ {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, ok := v.(error); ok {
			t.Fatal(err)
		}
		if n == 1 {
			if v != 1 {
				t.Errorf("expected: %v, got: %v", 1, v)
			}
		} else if v != nil {
			t.Errorf("expected: %v, got: %v", nil, v)
		}
	}
}

func TestWithFunctionWithContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	query, err := gojq.Parse("f")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(
		query,
		gojq.WithFunctionWithContext("f", 0, 0, func(ctx context.Context, v any, _ []any) any {
			// Cooperatively respond to cancellation; no goroutine is needed.
			cancel()
			<-ctx.Done()
			return ctx.Err()
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.RunWithContext(ctx, nil)
	v, ok := iter.Next()
	if !ok {
		t.Fatal("expected an error, got none")
	}
	if err, ok := v.(error); !ok || !errors.Is(err, context.Canceled) {
		t.Errorf("expected: %v, got: %v", context.Canceled, v)
	}
}

func TestWithFunctionWithContextMultipleArities(t *testing.T) {
	query, err := gojq.Parse("f, f(1)")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(
		query,
		gojq.WithFunction("f", 0, 0, func(any, []any) any {
			return 0
		}),
		gojq.WithFunctionWithContext("f", 1, 1, func(ctx context.Context, _ any, xs []any) any {
			if ctx == nil {
				return fmt.Errorf("unexpected nil context")
			}
			return xs[0]
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.RunWithContext(context.Background(), nil)
	for n := 0; ; n++ {
		v, ok := iter.Next()
		if !ok {
			if n != 2 {
				t.Errorf("expected: %v, got: %v", 2, n)
			}
			break
		}
		if err, ok := v.(error); ok {
			t.Fatal(err)
		}
		if v != n {
			t.Errorf("expected: %v, got: %v", n, v)
		}
	}
}

type closeCountIter struct {
	values []any
	closed int
}

func (iter *closeCountIter) Next() (any, bool) {
	if len(iter.values) == 0 {
		return nil, false
	}
	v := iter.values[0]
	iter.values = iter.values[1:]
	return v, true
}

func (iter *closeCountIter) Close() error {
	iter.closed++
	return nil
}

func TestWithIterFunctionWithContextCloseOnExhaustion(t *testing.T) {
	query, err := gojq.Parse("f")
	if err != nil {
		t.Fatal(err)
	}
	iters := []*closeCountIter{}
	code, err := gojq.Compile(
		query,
		gojq.WithIterFunctionWithContext("f", 0, 0, func(context.Context, any, []any) gojq.Iter {
			iter := &closeCountIter{values: []any{1, 2}}
			iters = append(iters, iter)
			return iter
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.RunWithContext(context.Background(), nil)
	n := 0
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, ok := v.(error); ok {
			t.Fatal(err)
		}
		n++
	}
	if n != 2 {
		t.Errorf("expected: %v, got: %v", 2, n)
	}
	if len(iters) != 1 {
		t.Fatalf("expected: %v, got: %v", 1, len(iters))
	}
	if closed := iters[0].closed; closed != 1 {
		t.Errorf("expected: %v, got: %v", 1, closed)
	}
}

func TestWithIterFunctionWithContextCloseOnError(t *testing.T) {
	query, err := gojq.Parse("f")
	if err != nil {
		t.Fatal(err)
	}
	iters := []*closeCountIter{}
	code, err := gojq.Compile(
		query,
		gojq.WithIterFunctionWithContext("f", 0, 0, func(context.Context, any, []any) gojq.Iter {
			iter := &closeCountIter{values: []any{1, errors.New("error"), 3}}
			iters = append(iters, iter)
			return iter
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.RunWithContext(context.Background(), nil)
	var gotErr error
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, ok := v.(error); ok {
			gotErr = err
			break
		}
	}
	if gotErr == nil || gotErr.Error() != "error" {
		t.Errorf("expected an error, got: %v", gotErr)
	}
	if len(iters) != 1 {
		t.Fatalf("expected: %v, got: %v", 1, len(iters))
	}
	if closed := iters[0].closed; closed != 1 {
		t.Errorf("expected: %v, got: %v", 1, closed)
	}
}

func TestWithIterFunctionWithContextCloseOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	query, err := gojq.Parse("f")
	if err != nil {
		t.Fatal(err)
	}
	iters := []*closeCountIter{}
	code, err := gojq.Compile(
		query,
		gojq.WithIterFunctionWithContext("f", 0, 0, func(context.Context, any, []any) gojq.Iter {
			iter := &closeCountIter{values: []any{1, 2, 3}}
			iters = append(iters, iter)
			return iter
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.RunWithContext(ctx, nil)
	v, ok := iter.Next()
	if !ok || v != 1 {
		t.Fatalf("expected: %v, got: %v", 1, v)
	}
	cancel()
	v, ok = iter.Next()
	if !ok {
		t.Fatal("expected an error, got none")
	}
	if err, ok := v.(error); !ok || !errors.Is(err, context.Canceled) {
		t.Errorf("expected: %v, got: %v", context.Canceled, v)
	}
	if len(iters) != 1 {
		t.Fatalf("expected: %v, got: %v", 1, len(iters))
	}
	if closed := iters[0].closed; closed != 1 {
		t.Errorf("expected: %v, got: %v", 1, closed)
	}
}

func TestWithIterFunctionWithContextCloseExplicitly(t *testing.T) {
	query, err := gojq.Parse("f")
	if err != nil {
		t.Fatal(err)
	}
	iters := []*closeCountIter{}
	code, err := gojq.Compile(
		query,
		gojq.WithIterFunctionWithContext("f", 0, 0, func(context.Context, any, []any) gojq.Iter {
			iter := &closeCountIter{values: []any{1, 2, 3}}
			iters = append(iters, iter)
			return iter
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.RunWithContext(context.Background(), nil)
	v, ok := iter.Next()
	if !ok || v != 1 {
		t.Fatalf("expected: %v, got: %v", 1, v)
	}
	closer, ok := iter.(io.Closer)
	if !ok {
		t.Fatal("result iterator does not implement io.Closer")
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if len(iters) != 1 {
		t.Fatalf("expected: %v, got: %v", 1, len(iters))
	}
	if closed := iters[0].closed; closed != 1 {
		t.Errorf("expected: %v, got: %v", 1, closed)
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if closed := iters[0].closed; closed != 1 {
		t.Errorf("expected: %v, got: %v", 1, closed)
	}
}

func TestWithIterFunctionWithContextSharedCode(t *testing.T) {
	type ctxKey struct{}
	query, err := gojq.Parse("f")
	if err != nil {
		t.Fatal(err)
	}
	iters := []*closeCountIter{}
	code, err := gojq.Compile(
		query,
		gojq.WithIterFunctionWithContext("f", 0, 0, func(ctx context.Context, _ any, _ []any) gojq.Iter {
			iter := &closeCountIter{values: []any{ctx.Value(ctxKey{})}}
			iters = append(iters, iter)
			return iter
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	iter1 := code.RunWithContext(context.WithValue(context.Background(), ctxKey{}, 1), nil)
	iter2 := code.RunWithContext(context.WithValue(context.Background(), ctxKey{}, 2), nil)
	if v, ok := iter1.Next(); !ok || v != 1 {
		t.Fatalf("expected: %v, got: %v", 1, v)
	}
	if v, ok := iter2.Next(); !ok || v != 2 {
		t.Fatalf("expected: %v, got: %v", 2, v)
	}
	if err := iter1.(io.Closer).Close(); err != nil {
		t.Fatal(err)
	}
	if len(iters) != 2 {
		t.Fatalf("expected: %v, got: %v", 2, len(iters))
	}
	if closed := iters[0].closed; closed != 1 {
		t.Errorf("expected: %v, got: %v", 1, closed)
	}
	if closed := iters[1].closed; closed != 0 {
		t.Errorf("expected: %v, got: %v", 0, closed)
	}
	for {
		if _, ok := iter2.Next(); !ok {
			break
		}
	}
	if closed := iters[0].closed; closed != 1 {
		t.Errorf("expected: %v, got: %v", 1, closed)
	}
	if closed := iters[1].closed; closed != 1 {
		t.Errorf("expected: %v, got: %v", 1, closed)
	}
}
