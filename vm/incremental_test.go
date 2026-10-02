package vm_test

import (
	"errors"
	"testing"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/internal/testify/require"
	"github.com/expr-lang/expr/vm"
)

type incrementalHosts struct {
	prepare    int
	discount   int
	sideEffect int
	guard      int
}

type incrementalCounters struct {
	prepare    int
	discount   int
	sideEffect int
	guard      int
}

func TestIncrementalSession_BranchesAndPureFunctions(t *testing.T) {
	hosts := &incrementalHosts{}
	const code = `
		flag ? discount(prepare(amount)) + sideEffect(1) : prepare(amount) + sideEffect(1)
	`
	program := compileIncrementalProgram(t, code,
		expr.AllowUndefinedVariables(),
		incrementalFunction("prepare", func(args ...any) (any, error) {
			hosts.prepare++
			return args[0].(int), nil
		}),
		incrementalFunction("discount", func(args ...any) (any, error) {
			hosts.discount++
			return args[0].(int) - 2, nil
		}),
		incrementalFunction("sideEffect", func(args ...any) (any, error) {
			hosts.sideEffect++
			return args[0].(int), nil
		}),
	)

	initial := map[string]any{"flag": false, "amount": 10}
	session, err := vm.NewSession(program, cloneMap(initial),
		vm.ReusableFunction("prepare", testPrepare(hosts)),
		vm.ReusableFunction("discount", makeIncrementalFunction(hosts, "discount")),
	)
	require.NoError(t, err)

	cases := []struct {
		name     string
		change   vm.FieldChange
		counters incrementalCounters
	}{
		{
			name:     "initial false branch",
			counters: incrementalCounters{prepare: 1, sideEffect: 1},
		},
		{
			name:     "unchanged rerun reuses pure result but side effect runs",
			counters: incrementalCounters{sideEffect: 1},
		},
		{
			name:     "switch to true branch",
			change:   vm.FieldChange{Path: []any{"flag"}, Value: true},
			counters: incrementalCounters{discount: 1, sideEffect: 1},
		},
		{
			name:     "stay true and unchanged",
			counters: incrementalCounters{sideEffect: 1},
		},
		{
			name:     "return false branch",
			change:   vm.FieldChange{Path: []any{"flag"}, Value: false},
			counters: incrementalCounters{sideEffect: 1},
		},
		{
			name:     "change shared input",
			change:   vm.FieldChange{Path: []any{"amount"}, Value: 20},
			counters: incrementalCounters{prepare: 1, sideEffect: 1},
		},
		{
			name:     "switch true after shared input changed",
			change:   vm.FieldChange{Path: []any{"flag"}, Value: true},
			counters: incrementalCounters{discount: 1, sideEffect: 1},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := cloneMap(initial)
			applyFieldChange(env, tc.change)

			hosts.prepare = 0
			hosts.discount = 0
			hosts.sideEffect = 0
			full, fullErr := vm.Run(program, env)

			hosts.prepare = 0
			hosts.discount = 0
			hosts.sideEffect = 0
			var partial any
			var partialErr error
			if tc.change.Path == nil {
				partial, partialErr = session.Run()
			} else {
				partial, partialErr = session.Submit(tc.change)
			}

			require.Equal(t, fullErr == nil, partialErr == nil)
			require.Equal(t, full, partial)
			require.Equal(t, tc.counters.prepare, hosts.prepare, "prepare calls")
			require.Equal(t, tc.counters.discount, hosts.discount, "discount calls")
			require.Equal(t, tc.counters.sideEffect, hosts.sideEffect, "sideEffect calls")

			initial = env
		})
	}
}

func TestIncrementalSession_ArrayIndexDependencies(t *testing.T) {
	hosts := &incrementalHosts{}
	const code = `
		items[selector].active
			? prepare(items[selector].value)
			: prepare(items[other].value)
	`
	program := compileIncrementalProgram(t, code,
		incrementalFunction("prepare", func(args ...any) (any, error) {
			hosts.prepare++
			return args[0].(int), nil
		}),
	)
	initial := map[string]any{
		"selector": 0,
		"other":    1,
		"items": []any{
			map[string]any{"active": true, "value": 2},
			map[string]any{"active": false, "value": 3},
		},
	}
	session, err := vm.NewSession(program, cloneMap(initial),
		vm.ReusableFunction("prepare", testPrepare(hosts)),
	)
	require.NoError(t, err)

	cases := []struct {
		name         string
		change       vm.FieldChange
		prepareCalls int
	}{
		{name: "initial", prepareCalls: 1},
		{name: "unchanged", prepareCalls: 0},
		{name: "inactive index changed", change: fieldChange([]any{"items", 1, "value"}, 9), prepareCalls: 0},
		{name: "active index changed", change: fieldChange([]any{"items", 0, "value"}, 4), prepareCalls: 1},
		{name: "switch selected index", change: fieldChange([]any{"selector"}, 1), prepareCalls: 1},
		{name: "newly inactive change ignored", change: fieldChange([]any{"items", 0, "value"}, 5), prepareCalls: 0},
		{name: "newly active change used", change: fieldChange([]any{"items", 1, "value"}, 7), prepareCalls: 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := cloneMap(initial)
			applyFieldChange(env, tc.change)

			hosts.prepare = 0
			full, _ := vm.Run(program, env)
			fullCalls := hosts.prepare

			hosts.prepare = 0
			var partial any
			if tc.change.Path == nil {
				partial, _ = session.Run()
			} else {
				partial, _ = session.Submit(tc.change)
			}

			require.Equal(t, full, partial)
			require.Equal(t, fullCalls, 1)
			require.Equal(t, tc.prepareCalls, hosts.prepare)
			initial = env
		})
	}
}

func TestIncrementalSession_ErrorRecovery(t *testing.T) {
	hosts := &incrementalHosts{}
	boom := errors.New("boom")
	const code = `guard(flag) ? prepare(amount) : prepare(amount) - 1`
	program := compileIncrementalProgram(t, code,
		incrementalFunction("guard", func(args ...any) (any, error) {
			hosts.guard++
			if args[0].(bool) {
				return nil, boom
			}
			return false, nil
		}),
		incrementalFunction("prepare", func(args ...any) (any, error) {
			hosts.prepare++
			return args[0].(int), nil
		}),
	)
	initial := map[string]any{"flag": true, "amount": 10}
	session, err := vm.NewSession(program, cloneMap(initial),
		vm.ReusableFunction("guard", makeIncrementalFunction(hosts, "guard")),
		vm.ReusableFunction("prepare", testPrepare(hosts)),
	)
	require.NoError(t, err)

	cases := []struct {
		name    string
		change  vm.FieldChange
		wantErr bool
		prepare int
		guard   int
	}{
		{name: "fails before branch inputs", wantErr: true, guard: 1, prepare: 0},
		{name: "same failure is cached", wantErr: true, guard: 0, prepare: 0},
		{name: "recovers and evaluates branch", change: fieldChange([]any{"flag"}, false), wantErr: false, guard: 1, prepare: 1},
		{name: "successful reusable nodes remain", wantErr: false, guard: 0, prepare: 0},
		{name: "fails again without evaluating branch", change: fieldChange([]any{"flag"}, true), wantErr: true, guard: 1, prepare: 0},
		{name: "amount changes while failure persists", change: fieldChange([]any{"amount"}, 11), wantErr: true, guard: 0, prepare: 0},
		{name: "recovers with new amount", change: fieldChange([]any{"flag"}, false), wantErr: false, guard: 1, prepare: 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := cloneMap(initial)
			applyFieldChange(env, tc.change)

			hosts.guard = 0
			hosts.prepare = 0
			full, fullErr := vm.Run(program, env)

			hosts.guard = 0
			hosts.prepare = 0
			var partial any
			var partialErr error
			if tc.change.Path == nil {
				partial, partialErr = session.Run()
			} else {
				partial, partialErr = session.Submit(tc.change)
			}

			require.Equal(t, tc.wantErr, fullErr != nil)
			require.Equal(t, tc.wantErr, partialErr != nil)
			if !tc.wantErr {
				require.Equal(t, full, partial)
			}
			require.Equal(t, tc.guard, hosts.guard, "guard calls")
			require.Equal(t, tc.prepare, hosts.prepare, "prepare calls")
			initial = env
		})
	}
}

func TestIncrementalSession_IndexErrorRecovery(t *testing.T) {
	hosts := &incrementalHosts{}
	const code = `values[selector] ? prepare(amount) : prepare(amount) - 1`
	initial := map[string]any{
		"values":   []any{true},
		"selector": 0,
		"amount":   7,
	}
	program := compileIncrementalProgram(t, code,
		expr.Env(initial),
		incrementalFunction("prepare", func(args ...any) (any, error) {
			hosts.prepare++
			return args[0].(int), nil
		}),
	)
	session, err := vm.NewSession(program, cloneMap(initial),
		vm.ReusableFunction("prepare", testPrepare(hosts)),
	)
	require.NoError(t, err)

	cases := []struct {
		name    string
		change  vm.FieldChange
		wantErr bool
		prepare int
	}{
		{name: "valid index", prepare: 1},
		{name: "valid unchanged", prepare: 0},
		{name: "invalid index fails", change: fieldChange([]any{"selector"}, 1), wantErr: true, prepare: 0},
		{name: "amount changes while index error persists", change: fieldChange([]any{"amount"}, 8), wantErr: true, prepare: 0},
		{name: "recovers with new amount", change: fieldChange([]any{"selector"}, 0), wantErr: false, prepare: 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := cloneMap(initial)
			applyFieldChange(env, tc.change)

			hosts.prepare = 0
			full, fullErr := vm.Run(program, env)

			hosts.prepare = 0
			var partial any
			var partialErr error
			if tc.change.Path == nil {
				partial, partialErr = session.Run()
			} else {
				partial, partialErr = session.Submit(tc.change)
			}

			require.Equal(t, tc.wantErr, fullErr != nil)
			require.Equal(t, tc.wantErr, partialErr != nil)
			if !tc.wantErr {
				require.Equal(t, full, partial)
			}
			require.Equal(t, tc.prepare, hosts.prepare)
			initial = env
		})
	}
}

func TestIncrementalSession_ReplacedArrayInvalidatesElements(t *testing.T) {
	hosts := &incrementalHosts{}
	const code = `prepare(items[0].value)`
	program := compileIncrementalProgram(t, code,
		expr.AllowUndefinedVariables(),
		incrementalFunction("prepare", func(args ...any) (any, error) {
			hosts.prepare++
			return args[0].(int), nil
		}),
	)
	initial := map[string]any{
		"items": []any{map[string]any{"value": 1}},
	}
	session, err := vm.NewSession(program, cloneMap(initial),
		vm.ReusableFunction("prepare", testPrepare(hosts)),
	)
	require.NoError(t, err)

	value, err := session.Run()
	require.NoError(t, err)
	require.Equal(t, 1, value)

	value, err = session.Run()
	require.NoError(t, err)
	require.Equal(t, 1, value)
	require.Equal(t, 1, hosts.prepare)

	value, err = session.Submit(vm.FieldChange{
		Path:  []any{"items"},
		Value: []any{map[string]any{"value": 2}},
	})
	require.NoError(t, err)
	require.Equal(t, 2, value)
	require.Equal(t, 2, hosts.prepare)
}

func TestIncrementalSession_EnvironmentFunctionIsNotCached(t *testing.T) {
	calls := 0
	env := map[string]any{
		"amount": 2,
		"regular": func(args ...any) (any, error) {
			calls++
			return args[0].(int), nil
		},
	}
	program := compileIncrementalProgram(t, `regular(amount)`,
		expr.Env(map[string]any{
			"amount":  0,
			"regular": func(...any) (any, error) { return nil, nil },
		}),
	)
	session, err := vm.NewSession(program, cloneMap(env))
	require.NoError(t, err)

	value, err := session.Run()
	require.NoError(t, err)
	require.Equal(t, 2, value)

	value, err = session.Run()
	require.NoError(t, err)
	require.Equal(t, 2, value)
	require.Equal(t, 2, calls)
}

type incrementalTestFunction = func(args ...any) (any, error)

func compileIncrementalProgram(t *testing.T, code string, functions ...expr.Option) *vm.Program {
	t.Helper()
	options := append([]expr.Option{expr.Optimize(false)}, functions...)
	program, err := expr.Compile(code, options...)
	require.NoError(t, err)
	return program
}

func incrementalFunction(name string, fn incrementalTestFunction) expr.Option {
	return expr.Function(name, fn, new(func(...any) (any, error)))
}

func makeIncrementalFunction(hosts *incrementalHosts, name string) vm.Function {
	return func(args ...any) (any, error) {
		switch name {
		case "prepare":
			hosts.prepare++
			return args[0].(int) + 1, nil
		case "discount":
			hosts.discount++
			return args[0].(int) - 2, nil
		case "sideEffect":
			hosts.sideEffect++
			return args[0].(int), nil
		case "guard":
			hosts.guard++
			if args[0].(bool) {
				return nil, errors.New("boom")
			}
			return false, nil
		default:
			return nil, errors.New("unknown test function")
		}
	}
}

func testPrepare(hosts *incrementalHosts) vm.Function {
	return func(args ...any) (any, error) {
		hosts.prepare++
		return args[0].(int), nil
	}
}

func fieldChange(path []any, value any) vm.FieldChange {
	return vm.FieldChange{Path: path, Value: value}
}

func applyFieldChange(env map[string]any, change vm.FieldChange) {
	if change.Path == nil {
		return
	}
	current := any(env)
	for i, segment := range change.Path {
		if i == len(change.Path)-1 {
			switch key := segment.(type) {
			case string:
				current.(map[string]any)[key] = change.Value
			case int:
				current.([]any)[key] = change.Value
			}
			continue
		}
		switch key := segment.(type) {
		case string:
			current = current.(map[string]any)[key]
		case int:
			current = current.([]any)[key]
		}
	}
}

func cloneMap(input map[string]any) map[string]any {
	result := make(map[string]any, len(input))
	for key, value := range input {
		result[key] = cloneValue(value)
	}
	return result
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMap(typed)
	case []any:
		result := make([]any, len(typed))
		for i, item := range typed {
			result[i] = cloneValue(item)
		}
		return result
	default:
		return value
	}
}
