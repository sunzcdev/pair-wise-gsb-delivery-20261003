package vm_test

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

var mapEnv = map[string]any{
	"a": false, "b": 0, "c": 0, "d": 0.0,
	"s": "", "t": "", "x": nil, "ok": false, "u": 0,
}

func compileMap(t testing.TB, code string, opts ...expr.Option) *vm.Program {
	t.Helper()
	all := append([]expr.Option{expr.Env(mapEnv)}, opts...)
	program, err := expr.Compile(code, all...)
	if err != nil {
		t.Fatalf("compile %q: %v", code, err)
	}
	return program
}

func errorAt(res *vm.BatchResult, row int) bool {
	for _, e := range res.Errors {
		if e.Row == row {
			return true
		}
	}
	return false
}

// TestBatch_MatchesScalar runs every supported scalar expression across rows
// with nils and type noise, comparing RunBatch row by row against the scalar
// VM (value when it succeeds, error-presence when it panics).
func TestBatch_MatchesScalar(t *testing.T) {
	cases := []string{
		`a && b > 0`,
		`a || b == 0`,
		`a ? b : c`,
		`x ?? b`,
		`(x ?? b) + c`,
		`b + c * 2`,
		`b - c`,
		`b / c`,
		`b % c`,
		`b ^ c`,
		`-b`,
		`!a`,
		`b < c`,
		`b <= c`,
		`b > c`,
		`b >= c`,
		`b == c`,
		`b != c`,
		`s == "abc"`,
		`s != t`,
		`s contains t`,
		`s startsWith t`,
		`s endsWith t`,
		`s matches t`,
		`b in [1, 2, 3]`,
		`(a && b > 0) || (c == 2)`,
		`a ? (b > 0 ? 10 : 20) : c`,
		`s != nil && s contains "x"`,
		`s == nil`,
		`d > 1.5 && b/d > 0`,
		`u`,
		`let z = b + c; z * 2`,
		`let z = b; let y = c; z - y`,
		`a ? (let q = b; q) : c`,
	}

	const rows = 64
	rng := rand.New(rand.NewSource(1))
	for _, code := range cases {
		program := compileMap(t, code)
		columns := randomColumns(rows, rng)

		res, err := vm.RunBatch(program, columns)
		if err != nil {
			t.Fatalf("%s: RunBatch: %v", code, err)
		}

		for row := 0; row < rows; row++ {
			want, wantErr := vm.Run(program, rowEnv(columns, row))
			gotErr := errorAt(res, row)

			if (wantErr != nil) != gotErr {
				t.Fatalf("%s row %d: error presence mismatch scalar=%v batch-error=%v",
					code, row, wantErr, gotErr)
			}
			if wantErr != nil {
				continue
			}
			if !equalResult(want, res.Values[row]) {
				t.Fatalf("%s row %d: value mismatch scalar=%#v batch=%#v",
					code, row, want, res.Values[row])
			}
		}
	}
}

func equalResult(a, b any) bool {
	if af, ok := a.(float64); ok {
		bf, ok2 := b.(float64)
		if !ok2 {
			return false
		}
		if af != af || bf != bf { // NaN
			return (af != af) == (bf != bf)
		}
		return af == bf
	}
	return reflect.DeepEqual(a, b)
}

// TestBatch_ErrorRowIndexes uses integer modulo (mod by zero panics in the
// scalar VM) to check that errors carry original row indices.
func TestBatch_ErrorRowIndexes(t *testing.T) {
	program := compileMap(t, `b % 2`)
	res, err := vm.RunBatch(program, vm.Columns{
		"b": []any{2, "nope", 5, nil, 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) != 2 {
		t.Fatalf("want 2 errors, got %d: %#v", len(res.Errors), res.Errors)
	}
	got := map[int]bool{}
	for _, e := range res.Errors {
		got[e.Row] = true
		msg := e.Error.Error()
		if !strings.Contains(msg, "invalid operation") && !strings.Contains(msg, "%!") {
			t.Fatalf("row %d unexpected error message: %v", e.Row, e.Error)
		}
	}
	if !got[1] || !got[3] {
		t.Fatalf("errors on wrong rows: %#v", res.Errors)
	}
	if res.Values[0] != 0 || res.Values[2] != 1 || res.Values[4] != 1 {
		t.Fatalf("unexpected values: %#v", res.Values)
	}
}

// TestBatch_NilSemantics guards against silently turning nil into false/zero.
func TestBatch_NilSemantics(t *testing.T) {
	program := compileMap(t, `s == nil`)
	res, err := vm.RunBatch(program, vm.Columns{"s": []any{"x", nil, "y"}})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []any{false, true, false} {
		if res.Values[i] != want {
			t.Fatalf("row %d: want %v got %v", i, want, res.Values[i])
		}
	}

	// Missing column == missing map key: every value is nil.
	res2, err := vm.RunBatch(compileMap(t, `x ?? 7`), vm.Columns{"other": []any{1}})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Values[0] != 7 {
		t.Fatalf("missing column: want 7 got %v", res2.Values[0])
	}
}

// TestBatch_ShortCircuitNoSideEffects: rows with a false guard must never
// reach the division (same guarantee the scalar VM gives per row).
func TestBatch_ShortCircuitNoSideEffects(t *testing.T) {
	program := compileMap(t, `b != 0 && 10 / b > 1`)
	res, err := vm.RunBatch(program, vm.Columns{"b": []int{0, 0, 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("short-circuit must avoid division: %#v", res.Errors)
	}
	for i, v := range res.Values {
		if v != false {
			t.Fatalf("row %d: want false got %v", i, v)
		}
	}
}

// TestBatch_FallbackHostFunction: programs with host calls use the scalar
// fallback; per-row short-circuiting keeps side effects to the rows that take
// the branch.
func TestBatch_FallbackHostFunction(t *testing.T) {
	calls := 0
	env := map[string]any{"guard": false, "den": 0}
	program, err := expr.Compile(`guard && half(den) > 0`, expr.Env(env),
		expr.Function("half",
			func(params ...any) (any, error) {
				calls++
				return params[0].(int) / 2, nil
			},
			new(func(int) int)))
	if err != nil {
		t.Fatal(err)
	}

	res, err := vm.RunBatch(program, vm.Columns{
		"guard": []bool{false, false, true, false},
		"den":   []int{10, 20, 4, 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("half invoked %d times, want 1 (per-row short-circuit)", calls)
	}
	want := []any{false, false, true, false}
	for i := range want {
		if res.Values[i] != want[i] {
			t.Fatalf("row %d: want %v got %v", i, want[i], res.Values[i])
		}
	}
}

// TestBatch_FallbackHostErrorRow verifies a host error on one row is reported
// against that row index and does not abort the batch.
func TestBatch_FallbackHostErrorRow(t *testing.T) {
	env := map[string]any{"n": 0}
	program, err := expr.Compile(`half(n)`, expr.Env(env),
		expr.Function("half",
			func(params ...any) (any, error) {
				n := params[0].(int)
				if n == 0 {
					return nil, fmt.Errorf("zero")
				}
				return n / 2, nil
			},
			new(func(int) (int, error))))
	if err != nil {
		t.Fatal(err)
	}
	res, err := vm.RunBatch(program, vm.Columns{"n": []int{4, 0, 8}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) != 1 || res.Errors[0].Row != 1 {
		t.Fatalf("want one error on row 1, got %#v", res.Errors)
	}
	if res.Values[0] != 2 || res.Values[2] != 4 {
		t.Fatalf("unexpected values: %#v", res.Values)
	}
}

type logRow struct {
	Level string
	Code  int
}

// TestBatch_StructEnv covers struct field resolution.
func TestBatch_StructEnv(t *testing.T) {
	program, err := expr.Compile(`Level == "error" && Code >= 500`, expr.Env(logRow{}))
	if err != nil {
		t.Fatal(err)
	}
	res, err := vm.RunBatchStruct(program, logRow{}, vm.Columns{
		"Level": []string{"error", "info", "error"},
		"Code":  []int{503, 200, 404},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []any{true, false, false} {
		if res.Values[i] != want {
			t.Fatalf("row %d: want %v got %v", i, want, res.Values[i])
		}
	}
}

func TestBatch_ColumnMismatch(t *testing.T) {
	program := compileMap(t, `b + c`)
	_, err := vm.RunBatch(program, vm.Columns{
		"b": []int{1, 2},
		"c": []int{1, 2, 3},
	})
	if err == nil {
		t.Fatal("want error for mismatched columns")
	}
}

func TestBatch_EmptyBatch(t *testing.T) {
	program := compileMap(t, `b + 1`)
	res, err := vm.RunBatch(program, vm.Columns{"b": []int{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Values) != 0 || len(res.Errors) != 0 {
		t.Fatalf("empty batch: %#v", res)
	}
}

// TestBatch_FallbackErrorRowIndexes compares fallback error rows against
// scalar Run for every row, guaranteeing the reported row maps to the original
// input index even when evaluation falls back from the columnar engine.
func TestBatch_FallbackErrorRowIndexes(t *testing.T) {
	env := map[string]any{"n": 0}
	program, err := expr.Compile(`boom(n)`, expr.Env(env),
		expr.Function("boom",
			func(params ...any) (any, error) {
				if params[0].(int) < 0 {
					return nil, fmt.Errorf("negative")
				}
				return params[0].(int), nil
			},
			new(func(int) (int, error))))
	if err != nil {
		t.Fatal(err)
	}
	ns := []int{0, -1, 2, -3, 4}
	res, err := vm.RunBatch(program, vm.Columns{"n": ns})
	if err != nil {
		t.Fatal(err)
	}
	for i, n := range ns {
		_, scalarErr := vm.Run(program, map[string]any{"n": n})
		gotErr := errorAt(res, i)
		if (scalarErr != nil) != gotErr {
			t.Fatalf("row %d: scalar err=%v batch err=%v", i, scalarErr, gotErr)
		}
		if scalarErr == nil && res.Values[i] != n {
			t.Fatalf("row %d: value %v", i, res.Values[i])
		}
	}
	if len(res.Errors) != 2 || res.Errors[0].Row != 1 || res.Errors[1].Row != 3 {
		t.Fatalf("wrong error rows: %#v", res.Errors)
	}
}

func TestBatch_TypedColumns(t *testing.T) {
	res, err := vm.RunBatch(compileMap(t, `b`), vm.Columns{"b": []int64{1, 2, 3}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Values[0].(int64) != 1 {
		t.Fatalf("int64 column: %#v", res.Values)
	}

	res2, err := vm.RunBatch(compileMap(t, `d * 2`), vm.Columns{"d": []float32{1.5, 2.5}})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Values[0].(float64) != 3.0 {
		t.Fatalf("float32 column: %#v", res2.Values)
	}
}

// TestBatch_DifferentialSeeds repeats the row-by-row comparison across several
// random seeds and larger batches to guard semantic drift.
func TestBatch_DifferentialSeeds(t *testing.T) {
	code := `(s != nil && (s contains "x" || level == "error")) &&
	         (b == 0 || b*c >= 0) &&
	         (d < 0 || d + 1.0 > d) &&
	         (a ? (x ?? b) : c) >= 0`
	env := map[string]any{
		"s": "", "level": "", "b": 0, "c": 0,
		"d": 0.0, "a": false, "x": nil,
	}
	program, err := expr.Compile(code, expr.Env(env))
	if err != nil {
		t.Fatal(err)
	}

	for seed := int64(0); seed < 5; seed++ {
		rng := rand.New(rand.NewSource(seed))
		const rows = 256
		cols := vm.Columns{
			"s":     randomTyped(rows, rng, "str"),
			"level": randomTyped(rows, rng, "str"),
			"b":     randomTyped(rows, rng, "num"),
			"c":     randomTyped(rows, rng, "num"),
			"d":     randomTyped(rows, rng, "float"),
			"a":     randomTyped(rows, rng, "bool"),
			"x":     randomTyped(rows, rng, "num"),
		}
		res, rerr := vm.RunBatch(program, cols)
		if rerr != nil {
			t.Fatalf("seed %d: %v", seed, rerr)
		}
		for row := 0; row < rows; row++ {
			want, wantErr := vm.Run(program, rowEnv(cols, row))
			gotErr := errorAt(res, row)
			if (wantErr != nil) != gotErr {
				t.Fatalf("seed %d row %d: scalar err=%v batch err=%v", seed, row, wantErr, gotErr)
			}
			if wantErr == nil && !equalResult(want, res.Values[row]) {
				t.Fatalf("seed %d row %d: scalar=%#v batch=%#v", seed, row, want, res.Values[row])
			}
		}
	}
}

func randomTyped(n int, rng *rand.Rand, kind string) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = randomValue(kind, rng)
	}
	return out
}

// TestBatch_NoShortCircuitOps covers OpAnd/OpOr emitted by
// DisableShortCircuit, including nil operands that the scalar VM rejects.
func TestBatch_NoShortCircuitOps(t *testing.T) {
	for _, code := range []string{`a && ok`, `a || ok`} {
		program := compileMap(t, code, expr.DisableShortCircuit())
		columns := vm.Columns{
			"a":  []any{true, false, nil, true},
			"ok": []any{true, false, true, "notbool"},
		}
		res, err := vm.RunBatch(program, columns)
		if err != nil {
			t.Fatalf("%s: %v", code, err)
		}
		for row := 0; row < 4; row++ {
			want, wantErr := vm.Run(program, rowEnv(columns, row))
			gotErr := errorAt(res, row)
			if (wantErr != nil) != gotErr {
				t.Fatalf("%s row %d: scalar err=%v batch err=%v", code, row, wantErr, gotErr)
			}
			if wantErr == nil && want != res.Values[row] {
				t.Fatalf("%s row %d: scalar=%v batch=%v", code, row, want, res.Values[row])
			}
		}
	}
}

func randomColumns(n int, rng *rand.Rand) vm.Columns {
	cols := vm.Columns{}
	for key, kind := range map[string]string{
		"a": "bool", "b": "num", "c": "num", "d": "float",
		"s": "str", "t": "str", "x": "num", "ok": "bool", "u": "num",
	} {
		col := make([]any, n)
		for i := range col {
			col[i] = randomValue(kind, rng)
		}
		cols[key] = col
	}
	return cols
}

func randomValue(kind string, rng *rand.Rand) any {
	switch kind {
	case "bool":
		if rng.Intn(4) == 0 {
			return nil
		}
		return rng.Intn(2) == 0
	case "num":
		switch rng.Intn(5) {
		case 0:
			return nil
		case 1:
			return "oops"
		default:
			return rng.Intn(5)
		}
	case "float":
		if rng.Intn(4) == 0 {
			return nil
		}
		return rng.Float64()*4 - 2
	case "str":
		choices := []any{nil, "", "x", "abc", "xabc", "err", "timeout"}
		return choices[rng.Intn(len(choices))]
	}
	return nil
}

func rowEnv(cols vm.Columns, row int) map[string]any {
	env := make(map[string]any, len(cols))
	for k, v := range cols {
		env[k] = columnAt(v, row)
	}
	return env
}

func columnAt(col any, i int) any {
	switch v := col.(type) {
	case []any:
		return v[i]
	case []int:
		return v[i]
	case []bool:
		return v[i]
	case []string:
		return v[i]
	}
	return nil
}
