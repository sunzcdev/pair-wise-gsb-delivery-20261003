package vm_test

import (
	"testing"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/internal/testify/require"
	"github.com/expr-lang/expr/vm"
)

type artifactEnv struct {
	Foo   int
	Bar   string
	Items []int
}

type artifactEnvChanged struct {
	Bar   string
	Foo   int
	Items []int
}

func artifactHosts() expr.Hosts {
	return expr.Hosts{
		"double": {
			Fn:        func(args ...any) (any, error) { return args[0].(int) * 2, nil },
			Signature: "func(int) int",
		},
	}
}

func TestArtifactRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		code string
		env  any
	}{
		{"arith", "1 + 2 * 3", nil},
		{"ternary", "Foo > 1 ? Bar : 'default'", artifactEnv{Foo: 5, Bar: "hi"}},
		{"loop", "filter(Items, # > 1)", artifactEnv{Items: []int{1, 2, 3}}},
		{"regexp", "Bar matches `h.*`", artifactEnv{Bar: "hi"}},
		{"map", "{a: Foo, b: [1, 2]}", artifactEnv{Foo: 7}},
		{"host func", "double(Foo) + len(Items)", artifactEnv{Foo: 4, Items: []int{1, 2, 3}}},
		{"builtin", "sum(Items) > 3", artifactEnv{Items: []int{1, 2, 3}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			program, err := expr.Compile(tc.code, expr.Env(artifactEnv{}),
				expr.Function("double", artifactHosts()["double"].Fn, new(func(int) int)))
			require.NoError(t, err)

			want, err := expr.Run(program, tc.env)
			require.NoError(t, err)

			data, err := expr.SaveProgram(program)
			require.NoError(t, err)
			require.NotEmpty(t, data)

			restored, err := expr.LoadProgram(data, artifactHosts(), tc.env)
			require.NoError(t, err)

			got, err := expr.Run(restored, tc.env)
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}
}

func TestArtifactMissingHostRejected(t *testing.T) {
	program, err := expr.Compile("double(Foo)", expr.Env(artifactEnv{}),
		expr.Function("double", artifactHosts()["double"].Fn, new(func(int) int)))
	require.NoError(t, err)

	data, err := expr.SaveProgram(program)
	require.NoError(t, err)

	_, err = expr.LoadProgram(data, expr.Hosts{}, artifactEnv{})
	require.ErrorIs(t, err, vm.ErrArtifactBinding)
}

func TestArtifactSignatureMismatchRejected(t *testing.T) {
	program, err := expr.Compile("double(Foo)", expr.Env(artifactEnv{}),
		expr.Function("double", artifactHosts()["double"].Fn, new(func(int) int)))
	require.NoError(t, err)

	data, err := expr.SaveProgram(program)
	require.NoError(t, err)

	hosts := expr.Hosts{
		"double": {
			Fn:        func(args ...any) (any, error) { return args[0], nil },
			Signature: "func(string) string",
		},
	}
	_, err = expr.LoadProgram(data, hosts, artifactEnv{})
	require.ErrorIs(t, err, vm.ErrArtifactBinding)
}

func TestArtifactStructFieldMovedRejected(t *testing.T) {
	program, err := expr.Compile("Foo > 1", expr.Env(artifactEnv{}))
	require.NoError(t, err)

	data, err := expr.SaveProgram(program)
	require.NoError(t, err)

	// A different struct layout reorders Foo from field 0 to field 1.
	_, err = expr.LoadProgram(data, expr.Hosts{}, artifactEnvChanged{Foo: 5})
	require.ErrorIs(t, err, vm.ErrArtifactBinding)
}

func TestArtifactBytecodeVersionSentinel(t *testing.T) {
	require.NotZero(t, vm.BytecodeVersion)
}
