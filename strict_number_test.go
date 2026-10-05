package gojq_test

import (
	"math"
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/itchyny/gojq"
)

func TestStrictNumbers(t *testing.T) {
	testcases := []struct {
		query string
		input any
		err   string
	}{
		{"1.0 - .", math.NaN(), "subtract produced a non-finite number (NaN)"},
		{"-.", math.Inf(1), "negate produced a non-finite number (-Infinity)"},
		{". / 1e-308", 1e308, "divide produced a non-finite number (+Infinity)"},
		{"1e308 * 10", nil, "multiply produced a non-finite number (+Infinity)"},
		{"1e308 / 1e-308", nil, "divide produced a non-finite number (+Infinity)"},
		{"infinite - infinite", nil, "infinite produced a non-finite number (+Infinity)"},
		{"nan % 2", nil, "nan produced a non-finite number (NaN)"},
		{"infinite % 256", nil, "infinite produced a non-finite number (+Infinity)"},
		{"pow(10; 1e4)", nil, "pow produced a non-finite number (+Infinity)"},
		{"1 | ldexp(.; 100000)", nil, "ldexp produced a non-finite number (+Infinity)"},
		{"-1 | sqrt", nil, "sqrt produced a non-finite number (NaN)"},
		{"400 | exp10", nil, "exp10 produced a non-finite number (+Infinity)"},
		{"nan", nil, "nan produced a non-finite number (NaN)"},
		{"infinite", nil, "infinite produced a non-finite number (+Infinity)"},
		{"-infinite", nil, "infinite produced a non-finite number (+Infinity)"},
		{"(-1e308) * 10", nil, "multiply produced a non-finite number (-Infinity)"},
	}
	for _, tc := range testcases {
		t.Run(tc.query, func(t *testing.T) {
			query, err := gojq.Parse(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			code, err := gojq.Compile(query, gojq.WithStrictNumbers())
			if err != nil {
				t.Fatal(err)
			}
			iter := code.Run(tc.input)
			var got string
			for {
				v, ok := iter.Next()
				if !ok {
					break
				}
				if e, ok := v.(error); ok {
					got = e.Error()
				} else {
					t.Errorf("expected an error but got: %v", v)
				}
			}
			if got != tc.err {
				t.Errorf("expected: %v, got: %v", tc.err, got)
			}
		})
	}
}

func TestStrictNumbersNumberLiteral(t *testing.T) {
	query, err := gojq.Parse("1e1000")
	if err != nil {
		t.Fatal(err)
	}
	_, err = gojq.Compile(query, gojq.WithStrictNumbers())
	if err == nil {
		t.Fatal("expected a compile error")
	}
	if expected := "number literal produced a non-finite number (+Infinity)"; err.Error() != expected {
		t.Errorf("expected: %v, got: %v", expected, err)
	}
}

func TestStrictNumbersTryCatch(t *testing.T) {
	query, err := gojq.Parse(
		`[(try (1e308 * 10) catch "mul"),
		  (try (nan) catch "nan"),
		  (try (-1 | sqrt) catch . | type)]`,
	)
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query, gojq.WithStrictNumbers())
	if err != nil {
		t.Fatal(err)
	}
	iter := code.Run(nil)
	v, ok := iter.Next()
	if !ok {
		t.Fatal("no value")
	}
	if e, ok := v.(error); ok {
		t.Fatal(e)
	}
	expected := []any{"mul", "nan", "string"}
	if !reflect.DeepEqual(v, expected) {
		t.Errorf("expected: %v, got: %v", expected, v)
	}
}

func TestStrictNumbersBigIntExact(t *testing.T) {
	query, err := gojq.Parse(`
		1000000000000000000000000000000 as $b |
		[$b, $b + 1, $b * 2, -$b, $b % 7, ($b + $b) * $b]
	`)
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query, gojq.WithStrictNumbers())
	if err != nil {
		t.Fatal(err)
	}
	v, ok := code.Run(nil).Next()
	if !ok {
		t.Fatal("no value")
	}
	if e, ok := v.(error); ok {
		t.Fatal(e)
	}
	xs, ok := v.([]any)
	if !ok {
		t.Fatalf("expected array, got: %T", v)
	}
	b := func(s string) *big.Int {
		x, _ := new(big.Int).SetString(s, 10)
		return x
	}
	expected := []*big.Int{
		b("1000000000000000000000000000000"),
		b("1000000000000000000000000000001"),
		b("2000000000000000000000000000000"),
		b("-1000000000000000000000000000000"),
		b("1"),
		b("2000000000000000000000000000000000000000000000000000000000000"),
	}
	if len(xs) != len(expected) {
		t.Fatalf("expected %d values, got: %v", len(expected), xs)
	}
	for i, x := range xs {
		if x, ok := x.(*big.Int); !ok || x.Cmp(expected[i]) != 0 {
			t.Errorf("%d: expected: %v (%T), got: %v (%T)",
				i, expected[i], expected[i], x, x)
		}
	}
}

func TestStrictNumbersCustomFunction(t *testing.T) {
	query, err := gojq.Parse("f")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(
		query,
		gojq.WithStrictNumbers(),
		gojq.WithFunction("f", 0, 0, func(any, []any) any {
			return map[string]any{"a": []any{1.0, math.NaN()}}
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := code.Run(nil).Next()
	if !ok {
		t.Fatal("no value")
	}
	e, ok := v.(error)
	if !ok {
		t.Fatalf("expected error, got: %v", v)
	}
	if expected := `f produced a non-finite number (NaN at ."a".1)`; e.Error() != expected {
		t.Errorf("expected: %v, got: %v", expected, e)
	}
}

func TestStrictNumbersCustomFunctionNested(t *testing.T) {
	for _, tc := range []struct {
		name     string
		value    any
		expected string
	}{
		{"array", []any{1.0, []any{math.Inf(-1)}}, "f produced a non-finite number (-Infinity at .1.0)"},
		{"object", map[string]any{"a": map[string]any{"b": math.NaN()}},
			`f produced a non-finite number (NaN at ."a"."b")`},
		{"finite", []any{1, 2, big.NewInt(3), 1.5, nil, "x", true}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query, err := gojq.Parse("f")
			if err != nil {
				t.Fatal(err)
			}
			code, err := gojq.Compile(
				query,
				gojq.WithStrictNumbers(),
				gojq.WithFunction("f", 0, 0, func(any, []any) any {
					return tc.value
				}),
			)
			if err != nil {
				t.Fatal(err)
			}
			v, ok := code.Run(nil).Next()
			if !ok {
				t.Fatal("no value")
			}
			if tc.expected == "" {
				if e, ok := v.(error); ok {
					t.Fatalf("unexpected error: %v", e)
				}
				return
			}
			e, ok := v.(error)
			if !ok {
				t.Fatalf("expected error, got: %v", v)
			}
			if e.Error() != tc.expected {
				t.Errorf("expected: %v, got: %v", tc.expected, e)
			}
		})
	}
}

func TestStrictNumbersCustomIterFunction(t *testing.T) {
	query, err := gojq.Parse("try f catch .")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(
		query,
		gojq.WithStrictNumbers(),
		gojq.WithIterFunction("f", 0, 0, func(any, []any) gojq.Iter {
			return gojq.NewIter(1.0, math.Inf(1), 3.0)
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	var out []any
	iter := code.Run(nil)
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		out = append(out, v)
	}
	expected := []any{
		1.0,
		"f produced a non-finite number (+Infinity)",
	}
	if len(out) != len(expected) {
		t.Fatalf("expected: %v, got: %v", expected, out)
	}
	for i, v := range out {
		if v != expected[i] {
			t.Errorf("%d: expected: %v, got: %v", i, expected[i], v)
		}
	}
}

func TestStrictNumbersDefaultModeUnchanged(t *testing.T) {
	queries := []string{"nan", "infinite", "-infinite", "1e308 * 10", "infinite - infinite"}
	for _, src := range queries {
		query, err := gojq.Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		code, err := gojq.Compile(query)
		if err != nil {
			t.Fatal(err)
		}
		v, ok := code.Run(nil).Next()
		if !ok {
			t.Fatalf("%s: no value", src)
		}
		if _, ok := v.(error); ok {
			t.Errorf("%s: unexpected error in default mode: %v", src, v)
		}
	}
}

func TestStrictNumbersInputPassThrough(t *testing.T) {
	query, err := gojq.Parse(". | type")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query, gojq.WithStrictNumbers())
	if err != nil {
		t.Fatal(err)
	}
	v, ok := code.Run(math.Inf(1)).Next()
	if !ok {
		t.Fatal("no value")
	}
	if e, ok := v.(error); ok {
		t.Fatalf("input values should pass through: %v", e)
	}
	if v != "number" {
		t.Errorf("expected number, got: %v", v)
	}
}

func TestStrictNumbersErrorMessage(t *testing.T) {
	query, err := gojq.Parse("1e308 * 10")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query, gojq.WithStrictNumbers())
	if err != nil {
		t.Fatal(err)
	}
	v, ok := code.Run(nil).Next()
	if !ok {
		t.Fatal("no value")
	}
	e, ok := v.(error)
	if !ok {
		t.Fatalf("expected error, got: %v", v)
	}
	err = e
	if !strings.Contains(err.Error(), "non-finite") {
		t.Errorf("error should mention non-finite number: %s", err)
	}
}
