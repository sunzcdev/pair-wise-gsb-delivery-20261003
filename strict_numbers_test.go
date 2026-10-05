package gojq_test

import (
	"context"
	"errors"
	"math"
	"math/big"
	"testing"

	"github.com/itchyny/gojq"
)

func compileStrict(t *testing.T, src string, opts ...gojq.CompilerOption) *gojq.Code {
	t.Helper()
	query, err := gojq.Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	allOpts := append([]gojq.CompilerOption{gojq.WithStrictNumbers()}, opts...)
	code, err := gojq.Compile(query, allOpts...)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return code
}

// runStrict runs the code against nil input and returns the first emitted
// value (which may be an error) and whether the iterator terminated.
func runStrict(code *gojq.Code) (any, bool) {
	return code.Run(nil).Next()
}

func TestWithStrictNumbersErrors(t *testing.T) {
	testCases := []struct {
		name string
		src  string
		err  string
	}{
		{
			name: "addition overflow",
			src:  "1e308 + 1e308",
			err:  "cannot produce infinity as the result of: +",
		},
		{
			name: "subtraction negative overflow",
			src:  "-1e308 - 1e308",
			err:  "cannot produce negative infinity as the result of: -",
		},
		{
			name: "multiplication overflow",
			src:  "1e308 * 2",
			err:  "cannot produce infinity as the result of: *",
		},
		{
			name: "division overflow",
			src:  "1e308 / 1e-308",
			err:  "cannot produce infinity as the result of: /",
		},
		{
			name: "unary negate of infinity",
			src:  "(-1e308 * 2)",
			err:  "cannot produce negative infinity as the result of: *",
		},
		{
			name: "sqrt of negative number",
			src:  "(-1) | sqrt",
			err:  "cannot produce NaN as the result of: sqrt",
		},
		{
			name: "log of negative number",
			src:  "(-1) | log",
			err:  "cannot produce NaN as the result of: log",
		},
		{
			name: "pow overflow",
			src:  "pow(10; 400)",
			err:  "cannot produce infinity as the result of: pow",
		},
		{
			name: "exp overflow",
			src:  "1000 | exp",
			err:  "cannot produce infinity as the result of: exp",
		},
		{
			name: "nan builtin",
			src:  "nan",
			err:  "cannot produce NaN as the result of: nan",
		},
		{
			name: "infinite builtin",
			src:  "infinite",
			err:  "cannot produce infinity as the result of: infinite",
		},
		{
			name: "negative infinity via division",
			src:  "-1e308 / 1e-308",
			err:  "cannot produce negative infinity as the result of: /",
		},
		{
			name: "nested in array",
			src:  "[1, 2, (-1 | sqrt)]",
			err:  "cannot produce NaN as the result of: sqrt",
		},
		{
			name: "nested in object",
			src:  "{a: {b: pow(10; 400)}}",
			err:  "cannot produce infinity as the result of: pow",
		},
		{
			name: "collected by array construction",
			src:  "[1e308 + 1e308]",
			err:  "cannot produce infinity as the result of: +",
		},
		{
			name: "number literal infinity",
			src:  "1e309",
			err:  "number literal produces infinity",
		},
		{
			name: "number literal negative infinity",
			src:  "-1e309",
			err:  "number literal produces negative infinity",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			v, ok := runStrict(compileStrict(t, tc.src))
			if !ok {
				t.Fatal("expected an error value but the iterator terminated")
			}
			err, ok := v.(error)
			if !ok {
				t.Fatalf("expected an error but got: %v (%T)", v, v)
			}
			if err.Error() != tc.err {
				t.Errorf("expected: %s, got: %s", tc.err, err.Error())
			}
			var haltErr *gojq.HaltError
			if errors.As(err, &haltErr) {
				t.Errorf("strict number error must not be a HaltError: %v", err)
			}
		})
	}
}

func TestWithStrictNumbersTryCatch(t *testing.T) {
	// The error is an ordinary query error and is catchable by try-catch.
	v, ok := runStrict(compileStrict(t, `try (1e308 + 1e308) catch "caught: \(.)"`))
	if !ok {
		t.Fatal("expected a value but the iterator terminated")
	}
	if err, isErr := v.(error); isErr {
		t.Fatalf("expected caught error but got: %v", err)
	}
	if expected := "caught: cannot produce infinity as the result of: +"; v != expected {
		t.Errorf("expected: %s, got: %v", expected, v)
	}
	// try without catch suppresses the error (produces empty).
	if _, ok := runStrict(compileStrict(t, `try (pow(10; 400))`)); ok {
		t.Fatal("try without catch should produce empty")
	}
	// Errors are also catchable when the non-finite value is nested.
	v, ok = runStrict(compileStrict(t, `try [{a: (-1 | sqrt)}] catch .`))
	if !ok {
		t.Fatal("expected a value but the iterator terminated")
	}
	if expected := "cannot produce NaN as the result of: sqrt"; v != expected {
		t.Errorf("expected: %s, got: %v", expected, v)
	}
}

func TestWithStrictNumbersKeepsIntegerPrecision(t *testing.T) {
	// Large integers stay *big.Int and are never converted to float64.
	v, ok := runStrict(compileStrict(t,
		`111111111111111111111 * 111111111111111111111`))
	if !ok {
		t.Fatal("expected a value but the iterator terminated")
	}
	bi, ok := v.(*big.Int)
	if !ok {
		t.Fatalf("expected *big.Int but got: %T (%v)", v, v)
	}
	if expected := "12345679012345679012320987654320987654321"; bi.String() != expected {
		t.Errorf("expected: %s, got: %s", expected, bi.String())
	}
	// Exact integer division is preserved as well.
	v, _ = runStrict(compileStrict(t,
		`(111111111111111111111 * 111111111111111111111) / 3`))
	if _, ok := v.(*big.Int); !ok {
		t.Fatalf("expected *big.Int but got: %T (%v)", v, v)
	}
}

func TestWithStrictNumbersCustomFunctions(t *testing.T) {
	compile := func(src string) *gojq.Code {
		return compileStrict(t, src,
			gojq.WithFunction("badscalar", 0, 0, func(any, []any) any {
				return math.NaN()
			}),
			gojq.WithFunction("badnested", 0, 0, func(any, []any) any {
				return []any{map[string]any{"x": math.Inf(-1)}}
			}),
			gojq.WithFunction("okbig", 0, 0, func(any, []any) any {
				x, _ := new(big.Int).SetString("100000000000000000000", 10)
				return x
			}),
			gojq.WithIterFunction("baditer", 0, 0, func(any, []any) gojq.Iter {
				return gojq.NewIter[any](1.0, math.Inf(1), 3.0)
			}),
		)
	}
	if v, _ := runStrict(compile("badscalar")); fmtError(v) !=
		"cannot produce NaN as the result of: badscalar" {
		t.Errorf("scalar: %v", v)
	}
	if v, _ := runStrict(compile("[badscalar]")); fmtError(v) !=
		"cannot produce NaN as the result of: badscalar" {
		t.Errorf("nested call: %v", v)
	}
	if v, _ := runStrict(compile("badnested")); fmtError(v) !=
		"cannot produce negative infinity as the result of: badnested" {
		t.Errorf("nested value: %v", v)
	}
	// The second emitted value of the iterator is checked.
	iter := compile("baditer").Run(nil)
	if v, ok := iter.Next(); !ok || v != 1.0 {
		t.Fatalf("first value: %v", v)
	}
	if v, _ := iter.Next(); fmtError(v) !=
		"cannot produce infinity as the result of: baditer" {
		t.Errorf("iter value: %v", v)
	}
	// Big integers returned by extension functions are left untouched.
	var v any
	v, _ = runStrict(compile("okbig + 1"))
	bi, ok := v.(*big.Int)
	if !ok {
		t.Fatalf("okbig + 1 expected *big.Int, got %T (%v)", v, v)
	}
	if bi.String() != "100000000000000000001" {
		t.Errorf("okbig + 1: %s", bi.String())
	}
}

func fmtError(v any) string {
	if err, ok := v.(error); ok {
		return err.Error()
	}
	return ""
}

func TestWithStrictNumbersDoesNotCheckInput(t *testing.T) {
	// Non-finite values handed in as query input are not produced by the
	// query, so predicates such as isnan keep working in the strict mode.
	code := compileStrict(t, "isnan")
	if v, _ := code.Run(math.NaN()).Next(); v != true {
		t.Errorf("isnan on NaN input: %v", v)
	}
	code = compileStrict(t, "isfinite")
	if v, _ := code.Run(math.Inf(1)).Next(); v != false {
		t.Errorf("isfinite on infinite input: %v", v)
	}
}

func TestWithoutStrictNumbersKeepsJQBehavior(t *testing.T) {
	run := func(src string) any {
		query, err := gojq.Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		code, err := gojq.Compile(query)
		if err != nil {
			t.Fatal(err)
		}
		v, _ := code.Run(nil).Next()
		if err, ok := v.(error); ok {
			t.Fatalf("unexpected error: %v", err)
		}
		return v
	}
	marshal := func(v any) string {
		b, err := gojq.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// NaN is encoded as null at the JSON output phase.
	if got := marshal(run("nan")); got != "null" {
		t.Errorf("nan should be encoded as null, got: %s", got)
	}
	// Infinities are truncated to the maximum finite float64 value.
	if got := marshal(run("1e308 + 1e308")); got != "1.7976931348623157e+308" {
		t.Errorf("infinity should be truncated, got: %s", got)
	}
	if got := marshal(run("-(1e308 + 1e308)")); got != "-1.7976931348623157e+308" {
		t.Errorf("negative infinity should be truncated, got: %s", got)
	}
}

func TestWithStrictNumbersNotConfusedWithContextCancellation(t *testing.T) {
	// A host-side execution quota termination is reported through the
	// context and stays distinct from strict number errors.
	query, err := gojq.Parse("def f: f; f")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query, gojq.WithStrictNumbers())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v, _ := code.RunWithContext(ctx, nil).Next()
	if !errors.Is(v.(error), context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", v)
	}
}
