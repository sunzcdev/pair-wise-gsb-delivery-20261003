package vm_test

import (
	"testing"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

const batchBenchRows = 50000

// benchFilter is a representative log-filter predicate with short-circuiting
// guards, comparisons and arithmetic, compiled against a map env.
func benchFilterProgram(b *testing.B) *vm.Program {
	b.Helper()
	env := map[string]any{
		"level":   "",
		"status":  0,
		"latency": 0.0,
		"ratio":   0,
	}
	program, err := expr.Compile(
		`(level == "error" || level == "fatal") && status >= 500 && ratio > 0 && latency/ratio > 0.01`,
		expr.Env(env))
	if err != nil {
		b.Fatal(err)
	}
	return program
}

func benchColumns() vm.Columns {
	levels := make([]string, batchBenchRows)
	status := make([]int, batchBenchRows)
	latency := make([]float64, batchBenchRows)
	ratio := make([]int, batchBenchRows)
	for i := 0; i < batchBenchRows; i++ {
		switch i % 5 {
		case 0:
			levels[i] = "error"
		case 1:
			levels[i] = "info"
		case 2:
			levels[i] = "fatal"
		default:
			levels[i] = "debug"
		}
		status[i] = 200 + (i%4)*100
		latency[i] = float64(i%17) * 0.01
		ratio[i] = i % 3 // includes zeros to exercise short-circuit
	}
	return vm.Columns{
		"level":   levels,
		"status":  status,
		"latency": latency,
		"ratio":   ratio,
	}
}

// Baseline: one scalar Run per row, the cost RunBatch avoids.
func BenchmarkBatch_ScalarPerRow(b *testing.B) {
	program := benchFilterProgram(b)
	cols := benchColumns()
	levels := cols["level"].([]string)
	status := cols["status"].([]int)
	latency := cols["latency"].([]float64)
	ratio := cols["ratio"].([]int)

	scalar := vm.VM{}
	b.ResetTimer()
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		matched := 0
		for r := 0; r < batchBenchRows; r++ {
			env := map[string]any{
				"level":   levels[r],
				"status":  status[r],
				"latency": latency[r],
				"ratio":   ratio[r],
			}
			out, err := scalar.Run(program, env)
			if err != nil {
				b.Fatal(err)
			}
			if out.(bool) {
				matched++
			}
		}
		if matched == 0 {
			b.Fatal("no matches")
		}
	}
}

// Columnar native execution.
func BenchmarkBatch_Native(b *testing.B) {
	program := benchFilterProgram(b)
	cols := benchColumns()
	b.ResetTimer()
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		res, err := vm.RunBatch(program, cloneColumns(cols))
		if err != nil {
			b.Fatal(err)
		}
		matched := 0
		for _, v := range res.Values {
			if v == true {
				matched++
			}
		}
		if matched == 0 {
			b.Fatal("no matches")
		}
	}
}

// Columnar native execution reusing one VM across batches, which also caches
// []any views of typed columns. This is the recommended hot-loop usage.
func BenchmarkBatch_NativeReuseVM(b *testing.B) {
	program := benchFilterProgram(b)
	cols := benchColumns()
	scalar := vm.VM{}
	b.ResetTimer()
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		res, err := scalar.RunBatch(program, cols)
		if err != nil {
			b.Fatal(err)
		}
		matched := 0
		for _, v := range res.Values {
			if v == true {
				matched++
			}
		}
		if matched == 0 {
			b.Fatal("no matches")
		}
	}
}

// Columnar execution forced onto the scalar fallback (host function call).
func BenchmarkBatch_Fallback(b *testing.B) {
	env := map[string]any{"level": "", "status": 0}
	program, err := expr.Compile(`level == "error" && classify(status)`, expr.Env(env),
		expr.Function("classify",
			func(params ...any) (any, error) {
				return params[0].(int) >= 500, nil
			},
			new(func(int) bool)))
	if err != nil {
		b.Fatal(err)
	}
	cols := vm.Columns{
		"level":  benchColumns()["level"],
		"status": benchColumns()["status"],
	}
	b.ResetTimer()
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		res, rerr := vm.RunBatch(program, cloneColumns(cols))
		if rerr != nil {
			b.Fatal(rerr)
		}
		if len(res.Values) != batchBenchRows {
			b.Fatal("bad row count")
		}
	}
}

func cloneColumns(in vm.Columns) vm.Columns {
	out := make(vm.Columns, len(in))
	for k, v := range in {
		out[k] = v // typed slices are reused; RunBatch never mutates input
	}
	return out
}
