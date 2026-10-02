package batch_test

import (
	"testing"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/batch"
	"github.com/expr-lang/expr/internal/testify/require"
	"github.com/expr-lang/expr/vm"
)

// benchExpr is a realistic log filter combining boolean short circuiting,
// string/int comparisons and guarded integer arithmetic. It uses only
// covered scalar nodes, which is where columnar evaluation helps most.
const benchExpr = `(level == "error" || level == "warn") && (latency > 500 || retries > 0) && code - retries*10 < 1000`

// benchFnExpr additionally calls a host function; it measures the case
// where short circuiting must skip a per row function call.
const benchFnExpr = `level == "info" && safe(retries) == 1`

var benchSizes = []int{1000, 10000}

func benchColumns(n int) map[string]any {
	levels := make([]string, n)
	latency := make([]int, n)
	retries := make([]int, n)
	code := make([]int, n)
	for i := 0; i < n; i++ {
		switch i % 4 {
		case 0:
			levels[i] = "error"
		case 1:
			levels[i] = "warn"
		default:
			levels[i] = "info"
		}
		latency[i] = (i * 37) % 1000
		retries[i] = (i * 7) % 5 // includes zeros; safe() avoids division by zero
		code[i] = 10 + (i*13)%500
	}
	return map[string]any{
		"level":   levels,
		"latency": latency,
		"retries": retries,
		"code":    code,
	}
}

func benchProgram(b *testing.B) *vm.Program {
	b.Helper()
	program, err := expr.Compile(
		benchExpr,
	)
	require.NoError(b, err)
	return program
}

func benchFnProgram(b *testing.B) *vm.Program {
	b.Helper()
	program, err := expr.Compile(
		benchFnExpr,
		expr.Function("safe", func(params ...any) (any, error) {
			v := params[0].(int)
			if v == 0 {
				return 1, nil
			}
			return v, nil
		}),
	)
	require.NoError(b, err)
	return program
}

func BenchmarkBatch(b *testing.B) {
	program := benchProgram(b)
	for _, size := range benchSizes {
		columns := benchColumns(size)
		b.Run(itoa(size), func(b *testing.B) {
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				_, _, err := batch.RunMap(program, columns)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkRunRowByRow(b *testing.B) {
	program := benchProgram(b)
	for _, size := range benchSizes {
		columns := benchColumns(size)
		levels := columns["level"].([]string)
		latency := columns["latency"].([]int)
		retries := columns["retries"].([]int)
		code := columns["code"].([]int)

		envs := make([]map[string]any, size)
		for i := 0; i < size; i++ {
			envs[i] = map[string]any{
				"level":   levels[i],
				"latency": latency[i],
				"retries": retries[i],
				"code":    code[i],
			}
		}

		b.Run(itoa(size), func(b *testing.B) {
			machine := vm.VM{}
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				for i := 0; i < size; i++ {
					_, _ = machine.Run(program, envs[i])
				}
			}
		})
	}
}

func BenchmarkBatchHostFn(b *testing.B) {
	program := benchFnProgram(b)
	for _, size := range benchSizes {
		columns := benchColumns(size)
		b.Run(itoa(size), func(b *testing.B) {
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				_, _, err := batch.RunMap(program, columns)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkRunRowByRowHostFn(b *testing.B) {
	program := benchFnProgram(b)
	for _, size := range benchSizes {
		columns := benchColumns(size)
		levels := columns["level"].([]string)
		retries := columns["retries"].([]int)
		envs := make([]map[string]any, size)
		for i := 0; i < size; i++ {
			envs[i] = map[string]any{"level": levels[i], "retries": retries[i]}
		}
		b.Run(itoa(size), func(b *testing.B) {
			machine := vm.VM{}
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				for i := 0; i < size; i++ {
					_, _ = machine.Run(program, envs[i])
				}
			}
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
