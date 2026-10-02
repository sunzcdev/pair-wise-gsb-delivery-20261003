package vm_test

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/builtin"
	"github.com/expr-lang/expr/conf"
	"github.com/expr-lang/expr/internal/testify/require"
	"github.com/expr-lang/expr/vm"
)

type serializeEnv struct {
	Name string   `expr:"name"`
	Age  int      `expr:"age"`
	Tags []string `expr:"tags"`
}

func doubleFn(params ...any) (any, error) {
	return params[0].(int) * 2, nil
}

func serializeOptions() []expr.Option {
	return []expr.Option{
		expr.Env(serializeEnv{}),
		expr.Function("double", doubleFn, new(func(int) int)),
	}
}

func serializeFunctions() map[string]*builtin.Function {
	c := conf.CreateNew()
	for _, opt := range serializeOptions() {
		opt(c)
	}
	return c.Functions
}

func serializeCompile(t *testing.T, input string) *vm.Program {
	t.Helper()
	program, err := expr.Compile(input, serializeOptions()...)
	require.NoError(t, err)
	return program
}

func TestProgram_Marshal_roundtrip(t *testing.T) {
	env := serializeEnv{Name: "alice", Age: 30, Tags: []string{"x1", "y2", "x3"}}

	tests := []struct {
		input string
		want  any
	}{
		{`1 + 2 * 3`, 7},
		{`1.5 + 2.5`, 4.0},
		{`name startsWith "a" and age > 18`, true},
		{`name matches "^a"`, true},
		{`age in [1, 2, 3]`, false},
		{`"x1" in ["x1", "y2"]`, true},
		{`age in 1..10`, false},
		{`double(age) + 1`, 61},
		{`len(tags)`, 3},
		{`map(tags, upper(#))`, []any{"X1", "Y2", "X3"}},
		{`filter(tags, # startsWith "x")`, []any{"x1", "x3"}},
		{`let x = age * 2; x + 1`, 61},
		{`[1, 2, 3]`, []any{1, 2, 3}},
		{`{"a": 1, "b": "x"}`, map[string]any{"a": 1, "b": "x"}},
		{`true ? "yes" : "no"`, "yes"},
		{`sum(map(tags, len(#)))`, 6},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			program := serializeCompile(t, tt.input)
			data, err := vm.Marshal(program)
			require.NoError(t, err)
			loaded, err := vm.Unmarshal(data, vm.WithFunctions(serializeFunctions()))
			require.NoError(t, err)

			require.Equal(t, program.Bytecode, loaded.Bytecode)
			require.Equal(t, program.Arguments, loaded.Arguments)

			want, err := expr.Run(program, env)
			require.NoError(t, err)
			require.Equal(t, tt.want, want)

			got, err := expr.Run(loaded, env)
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}
}

func TestProgram_Marshal_deterministic(t *testing.T) {
	program := serializeCompile(t, `age in [3, 1, 2] and name in ["b", "a"]`)
	first, err := vm.Marshal(program)
	require.NoError(t, err)
	second, err := vm.Marshal(program)
	require.NoError(t, err)
	require.Equal(t, first, second)
}

func TestProgram_Marshal_nil(t *testing.T) {
	_, err := vm.Marshal(nil)
	require.Error(t, err)
}

func TestProgram_Marshal_unsupported_constant(t *testing.T) {
	program := serializeCompile(t, `1 + 1`)
	program.Constants = append(program.Constants, func() {})
	program.Bytecode = append([]vm.Opcode{vm.OpPush}, program.Bytecode...)
	program.Arguments = append([]int{len(program.Constants) - 1}, program.Arguments...)
	_, err := vm.Marshal(program)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot marshal constant")
}

func requireFormatError(t *testing.T, err error, kind vm.ErrKind) {
	t.Helper()
	require.Error(t, err)
	var ferr *vm.FormatError
	require.True(t, errors.As(err, &ferr), "expected *vm.FormatError, got %T: %v", err, err)
	require.Equal(t, kind, ferr.Kind, "unexpected error kind: %v", ferr)
}

func TestProgram_Unmarshal_bad_magic(t *testing.T) {
	data, err := vm.Marshal(serializeCompile(t, `1 + 1`))
	require.NoError(t, err)
	data[0] = 'X'
	_, err = vm.Unmarshal(data)
	requireFormatError(t, err, vm.ErrKindMagic)
}

func TestProgram_Unmarshal_bad_version(t *testing.T) {
	data, err := vm.Marshal(serializeCompile(t, `1 + 1`))
	require.NoError(t, err)
	data[4] = 0xFF
	_, err = vm.Unmarshal(data)
	requireFormatError(t, err, vm.ErrKindVersion)
}

func TestProgram_Unmarshal_truncated(t *testing.T) {
	program := serializeCompile(t, `name + "suffix"`)
	data, err := vm.Marshal(program)
	require.NoError(t, err)
	for _, n := range []int{1, 2, 5, len(data) / 2, len(data) - 1} {
		_, err = vm.Unmarshal(data[:n])
		requireFormatError(t, err, vm.ErrKindTruncated)
	}
}

func TestProgram_Unmarshal_trailing_data(t *testing.T) {
	data, err := vm.Marshal(serializeCompile(t, `1 + 1`))
	require.NoError(t, err)
	_, err = vm.Unmarshal(append(data, 0x00))
	requireFormatError(t, err, vm.ErrKindTruncated)
}

func TestProgram_Unmarshal_unknown_opcode(t *testing.T) {
	program := serializeCompile(t, `1 + 1`)
	program.Bytecode[0] = vm.Opcode(0xFF)
	data, err := vm.Marshal(program)
	require.NoError(t, err)
	_, err = vm.Unmarshal(data)
	requireFormatError(t, err, vm.ErrKindOpcode)
}

func TestProgram_Unmarshal_invalid_opcode(t *testing.T) {
	program := serializeCompile(t, `1 + 1`)
	program.Bytecode[0] = vm.OpInvalid
	data, err := vm.Marshal(program)
	require.NoError(t, err)
	_, err = vm.Unmarshal(data)
	requireFormatError(t, err, vm.ErrKindOpcode)
}

func TestProgram_Unmarshal_jump_out_of_bounds(t *testing.T) {
	program := serializeCompile(t, `age > 1 and age > 2 and age > 3`)
	ip := -1
	for i, op := range program.Bytecode {
		if op == vm.OpJumpIfFalse {
			ip = i
			break
		}
	}
	require.NotEqual(t, -1, ip)
	program.Arguments[ip] = 9999
	data, err := vm.Marshal(program)
	require.NoError(t, err)
	_, err = vm.Unmarshal(data)
	requireFormatError(t, err, vm.ErrKindJump)
}

func TestProgram_Unmarshal_backward_jump_out_of_bounds(t *testing.T) {
	program := serializeCompile(t, `map(tags, #)`)
	ip := -1
	for i, op := range program.Bytecode {
		if op == vm.OpJumpBackward {
			ip = i
			break
		}
	}
	require.NotEqual(t, -1, ip)
	program.Arguments[ip] = 9999
	data, err := vm.Marshal(program)
	require.NoError(t, err)
	_, err = vm.Unmarshal(data)
	requireFormatError(t, err, vm.ErrKindJump)
}

func TestProgram_Unmarshal_stack_underflow(t *testing.T) {
	program := serializeCompile(t, `1 + 1`)
	program.Bytecode[0] = vm.OpPop
	data, err := vm.Marshal(program)
	require.NoError(t, err)
	_, err = vm.Unmarshal(data)
	requireFormatError(t, err, vm.ErrKindStack)
}

func TestProgram_Unmarshal_unbalanced_scope(t *testing.T) {
	program := serializeCompile(t, `map(tags, #)`)
	for i, op := range program.Bytecode {
		if op == vm.OpBegin {
			program.Bytecode[i] = vm.OpNil
			break
		}
	}
	data, err := vm.Marshal(program)
	require.NoError(t, err)
	_, err = vm.Unmarshal(data)
	requireFormatError(t, err, vm.ErrKindScope)
}

func TestProgram_Unmarshal_constant_index_out_of_range(t *testing.T) {
	program := serializeCompile(t, `name + "suffix"`)
	ip := -1
	for i, op := range program.Bytecode {
		if op == vm.OpPush {
			ip = i
			break
		}
	}
	require.NotEqual(t, -1, ip)
	program.Arguments[ip] = 9999
	data, err := vm.Marshal(program)
	require.NoError(t, err)
	_, err = vm.Unmarshal(data)
	requireFormatError(t, err, vm.ErrKindArgument)
}

func TestProgram_Unmarshal_variable_index_out_of_range(t *testing.T) {
	program := serializeCompile(t, `let x = 1; x + 1`)
	ip := -1
	for i, op := range program.Bytecode {
		if op == vm.OpStore {
			ip = i
			break
		}
	}
	require.NotEqual(t, -1, ip)
	program.Arguments[ip] = 99
	data, err := vm.Marshal(program)
	require.NoError(t, err)
	_, err = vm.Unmarshal(data)
	requireFormatError(t, err, vm.ErrKindArgument)
}

func TestProgram_Unmarshal_unknown_constant_tag(t *testing.T) {
	program := serializeCompile(t, `name + "suffix"`)
	data, err := vm.Marshal(program)
	require.NoError(t, err)

	// Locate the constants section: header (6 bytes), variables, bytecode,
	// arguments; then the constants count followed by the first tag byte.
	off := 6
	skip := func() uint64 {
		v, n := binary.Uvarint(data[off:])
		require.Positive(t, n)
		off += n
		return v
	}
	skip() // variables
	bytecodeLen := skip()
	off += int(bytecodeLen)
	argumentsLen := skip()
	for i := 0; i < int(argumentsLen); i++ {
		_, n := binary.Varint(data[off:])
		require.Positive(t, n)
		off += n
	}
	constantsLen := skip()
	require.Positive(t, constantsLen)

	data[off] = 0xEE // not a valid constant tag
	_, err = vm.Unmarshal(data)
	requireFormatError(t, err, vm.ErrKindConstant)
}

func TestProgram_Unmarshal_unknown_function(t *testing.T) {
	program := serializeCompile(t, `double(age)`)
	data, err := vm.Marshal(program)
	require.NoError(t, err)
	_, err = vm.Unmarshal(data)
	requireFormatError(t, err, vm.ErrKindFunction)
}

func TestProgram_Unmarshal_signature_mismatch(t *testing.T) {
	program := serializeCompile(t, `double(age)`)
	data, err := vm.Marshal(program)
	require.NoError(t, err)

	c := conf.CreateNew()
	expr.Function("double", doubleFn, new(func(int, int) int))(c)
	_, err = vm.Unmarshal(data, vm.WithFunctions(c.Functions))
	requireFormatError(t, err, vm.ErrKindSignature)
	require.Contains(t, err.Error(), "func(int, int) int")
	require.Contains(t, err.Error(), "func(int) int")
}

func TestProgram_Unmarshal_builtin_rebound(t *testing.T) {
	program := serializeCompile(t, `upper(name)`)
	data, err := vm.Marshal(program)
	require.NoError(t, err)
	// Builtins resolve without providing any host functions.
	loaded, err := vm.Unmarshal(data)
	require.NoError(t, err)
	out, err := expr.Run(loaded, serializeEnv{Name: "alice"})
	require.NoError(t, err)
	require.Equal(t, "ALICE", out)
}
