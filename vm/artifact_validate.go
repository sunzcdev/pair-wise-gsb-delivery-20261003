package vm

import (
	"fmt"
	"reflect"
	"regexp"

	"github.com/expr-lang/expr/builtin"
	"github.com/expr-lang/expr/vm/runtime"
)

// verifyProgram performs all static, pointer-free checks on a decoded
// artifact program. It runs before host rebinding, so no instruction is
// trusted merely because it deserialized.
func verifyProgram(program *Program) error {
	code := program.Bytecode
	args := program.Arguments
	n := len(code)
	if n == 0 {
		return fmt.Errorf("%w: empty bytecode", ErrArtifactInstruction)
	}

	constCount := len(program.Constants)
	fnCount := len(program.functions)

	succ := make([][]int, n)

	for ip := 0; ip < n; ip++ {
		op := code[ip]
		arg := args[ip]
		if op > OpEnd {
			return fmt.Errorf("%w: unknown opcode %#x at %d", ErrArtifactInstruction, byte(op), ip)
		}

		switch op {
		case OpPush, OpLoadConst, OpLoadField, OpLoadFast, OpLoadMethod, OpFetchField,
			OpMethod, OpMatchesConst:
			if err := checkConstIndex(program, ip, op, arg, constCount); err != nil {
				return err
			}

		case OpLoadFunc, OpCall0, OpCall1, OpCall2, OpCall3:
			if arg < 0 || arg >= fnCount {
				return fmt.Errorf("%w: %s at %d references function slot %d, have %d",
					ErrArtifactInstruction, opcodeName(op), ip, arg, fnCount)
			}

		case OpCallBuiltin1:
			if arg < 0 || arg >= len(builtin.Builtins) {
				return fmt.Errorf("%w: %s at %d references builtin %d, runtime only has %d",
					ErrArtifactInstruction, opcodeName(op), ip, arg, len(builtin.Builtins))
			}

		case OpCallTyped:
			if arg <= 0 || arg >= len(FuncTypes) {
				return fmt.Errorf("%w: %s at %d references type signature %d, runtime has %d",
					ErrArtifactInstruction, opcodeName(op), ip, arg, len(FuncTypes))
			}
			if reflect.TypeOf(FuncTypes[arg]).Kind() != reflect.Ptr ||
				reflect.TypeOf(FuncTypes[arg]).Elem().Kind() != reflect.Func {
				return fmt.Errorf("%w: %s at %d signature slot %d is not a pointer-to-func",
					ErrArtifactInstruction, opcodeName(op), ip, arg)
			}

		case OpStore, OpLoadVar:
			if arg < 0 || arg >= program.variables {
				return fmt.Errorf("%w: %s at %d references variable %d, program declares %d",
					ErrArtifactInstruction, opcodeName(op), ip, arg, program.variables)
			}

		case OpCast:
			if arg < 0 || arg > 3 {
				return fmt.Errorf("%w: %s at %d carries unsupported cast %d",
					ErrArtifactInstruction, opcodeName(op), ip, arg)
			}

		case OpCreate:
			if arg != 1 && arg != 2 {
				return fmt.Errorf("%w: %s at %d carries unknown constructor %d",
					ErrArtifactInstruction, opcodeName(op), ip, arg)
			}

		case OpProfileStart, OpProfileEnd:
			return fmt.Errorf("%w: %s at %d: profiled programs cannot be restored from artifacts",
				ErrArtifactInstruction, opcodeName(op), ip)

		case OpJump, OpJumpIfTrue, OpJumpIfFalse, OpJumpIfNil, OpJumpIfNotNil, OpJumpIfEnd:
			if arg < 0 {
				return fmt.Errorf("%w: %s at %d has negative offset %d",
					ErrArtifactJump, opcodeName(op), ip, arg)
			}
			target := ip + 1 + arg
			if target < 0 || target > n {
				return fmt.Errorf("%w: %s at %d jumps to %d, program has %d instructions",
					ErrArtifactJump, opcodeName(op), ip, target, n)
			}
			succ[ip] = append(succ[ip], target)

		case OpJumpBackward:
			if arg <= 0 {
				return fmt.Errorf("%w: %s at %d has non-positive back offset %d",
					ErrArtifactJump, opcodeName(op), ip, arg)
			}
			target := ip + 1 - arg
			if target < 0 || target >= n {
				return fmt.Errorf("%w: %s at %d jumps back to %d, program has %d instructions",
					ErrArtifactJump, opcodeName(op), ip, target, n)
			}
			succ[ip] = append(succ[ip], target)

		case OpInt, OpCall, OpCallN, OpCallFast, OpCallSafe:
			if arg < 0 {
				return fmt.Errorf("%w: %s at %d carries negative argument %d",
					ErrArtifactInstruction, opcodeName(op), ip, arg)
			}
		}

		if hasSuccessor(code[ip]) {
			succ[ip] = append(succ[ip], ip+1)
		}
	}

	return verifyStack(code, args, program.Constants, succ)
}

func hasSuccessor(op Opcode) bool {
	return op != OpJump && op != OpJumpBackward && op != OpThrow
}

func checkConstIndex(program *Program, ip int, op Opcode, arg, constCount int) error {
	if arg < 0 || arg >= constCount {
		return fmt.Errorf("%w: %s at %d references constant %d, have %d",
			ErrArtifactInstruction, opcodeName(op), ip, arg, constCount)
	}
	c := program.Constants[arg]
	var want string
	switch op {
	case OpLoadField, OpFetchField:
		if _, ok := c.(*runtime.Field); !ok {
			want = "*runtime.Field"
		}
	case OpLoadFast:
		if _, ok := c.(string); !ok {
			want = "string"
		}
	case OpLoadMethod, OpMethod:
		if _, ok := c.(*runtime.Method); !ok {
			want = "*runtime.Method"
		}
	case OpMatchesConst:
		if _, ok := c.(*regexp.Regexp); !ok {
			want = "*regexp.Regexp"
		}
	}
	if want != "" {
		return fmt.Errorf("%w: %s at %d expected constant %d to be %s, got %T",
			ErrArtifactInstruction, opcodeName(op), ip, arg, want, c)
	}
	return nil
}

// verifyStack performs static stack-usage analysis.
//
// It runs a worklist dataflow where each instruction is associated with the
// minimum stack depth at which it is reachable. Compiled code legitimately
// reaches loop headers at more than one depth (values pushed on one branch
// are consumed by a later OpArray/OpMap), so depths are not required to be
// equal; instead the minimum is tracked, which is exactly the conservative
// bound needed to prove no instruction pops more values than some real
// execution path could have pushed. Any simulated underflow, or a terminal
// fall-through stack that is not scalar, rejects the program.
func verifyStack(code []Opcode, args []int, constants []any, succ [][]int) error {
	n := len(code)
	const unknown = -1
	depthAt := make([]int, n)
	for i := range depthAt {
		depthAt[i] = unknown
	}

	type frame struct{ ip, depth int }
	worklist := []frame{{0, 0}}

	for len(worklist) > 0 {
		f := worklist[len(worklist)-1]
		worklist = worklist[:len(worklist)-1]

		if depthAt[f.ip] != unknown && f.depth >= depthAt[f.ip] {
			// Already visited with a depth no greater than this one; the
			// smaller depth is the more conservative underflow bound.
			continue
		}
		depthAt[f.ip] = f.depth

		delta := stackDeltaAt(code, args, constants, f.ip)
		if f.depth+delta < 0 {
			return fmt.Errorf("%w: stack underflow at %d (%s): minimum depth %d, delta %d",
				ErrArtifactStack, f.ip, opcodeName(code[f.ip]), f.depth, delta)
		}
		next := f.depth + delta

		for _, t := range succ[f.ip] {
			if t == n {
				continue // falling off the end; checked separately below
			}
			worklist = append(worklist, frame{t, next})
		}
	}

	// Terminal scalar check. Programs without jumps have a single linear
	// fall-through path that produces the result; loop-bearing programs reach
	// merge points at several legitimate depths (their builders consume
	// accumulated values), and the minimum-depth analysis above already
	// proves absence of underflow for them.
	hasJump := false
	for _, op := range code {
		switch op {
		case OpJump, OpJumpBackward, OpJumpIfTrue, OpJumpIfFalse, OpJumpIfNil,
			OpJumpIfNotNil, OpJumpIfEnd:
			hasJump = true
		}
	}
	if hasJump {
		return nil
	}
	depth := 0
	for ip := 0; ip < n; ip++ {
		depth += stackDeltaAt(code, args, constants, ip)
	}
	if depth > 1 {
		return fmt.Errorf("%w: program ends with %d values on stack, want 0 or 1",
			ErrArtifactStack, depth)
	}
	return nil
}

// stackDeltaAt is stackDelta with access to surrounding instructions, so the
// element-count operand of OpArray/OpMap (which lives on the stack, not in the
// opcode argument) can be recovered from the preceding OpInt/OpPush.
func stackDeltaAt(code []Opcode, args []int, constants []any, ip int) int {
	switch code[ip] {
	case OpArray:
		n := literalCount(code, args, constants, ip)
		return -n
	case OpMap:
		n := literalCount(code, args, constants, ip)
		return -2 * n
	}
	return stackDelta(code[ip], args[ip])
}

// literalCount recovers the element-count operand of an OpArray/OpMap. The
// compiler pushes the size either as OpInt or as an OpPush of an integer
// constant (deduplication may reuse an existing literal). It returns 0 when
// the size cannot be statically read; the structural constant checks plus
// minimum-depth analysis still keep verification sound in that case.
func literalCount(code []Opcode, args []int, constants []any, ip int) int {
	if ip < 1 {
		return 0
	}
	switch code[ip-1] {
	case OpInt:
		return args[ip-1]
	case OpPush:
		idx := args[ip-1]
		if idx >= 0 && idx < len(constants) {
			switch n := constants[idx].(type) {
			case int:
				return n
			case int64:
				return int(n)
			}
		}
	}
	return 0
}

// stackDelta returns the net change in operand stack depth caused by op,
// derived directly from vm.VM's push/pop behavior. Argument-bearing ops use
// their argument (function arity, variable/constant index, ...).
func stackDelta(op Opcode, arg int) int {
	switch op {
	// push one value
	case OpPush, OpInt, OpLoadVar, OpLoadConst, OpLoadField, OpLoadFast,
		OpLoadMethod, OpLoadFunc, OpLoadEnv, OpTrue, OpFalse, OpNil,
		OpGetIndex, OpGetCount, OpGetLen, OpGetAcc, OpPointer, OpLen, OpSort:
		return 1

	// pop one value, no result
	case OpPop, OpStore, OpSetAcc, OpSetIndex, OpGroupBy, OpSortBy, OpThrow:
		return -1

	// pop one and push one
	case OpNegate, OpNot, OpFetchField, OpMethod, OpCast, OpDeref,
		OpMatchesConst, OpCallBuiltin1:
		return 0

	// pop two and push one
	case OpFetch, OpEqual, OpEqualInt, OpEqualString, OpIn, OpLess, OpMore, OpLessOrEqual,
		OpMoreOrEqual, OpAdd, OpSubtract, OpMultiply, OpDivide, OpModulo,
		OpExponent, OpRange, OpMatches, OpContains, OpStartsWith, OpEndsWith, OpAnd, OpOr:
		return -1

	// pop three and push one
	case OpSlice:
		return -2

	// function calls
	case OpCall0:
		return 1
	case OpCall1, OpCall2, OpCall3:
		// Function comes from the function table; the VM pops the arity
		// arguments and pushes one result, netting zero stack change.
		return 0
	case OpCall:
		// Pops the callee and size arguments, pushes one result: -size.
		return -arg
	case OpCallN, OpCallFast, OpCallSafe:
		// Pops the callee and arg arguments, pushes one result: -arg.
		return -arg
	case OpCallTyped:
		// vm.call pops the callee plus every input parameter and pushes one.
		return typedCallDelta(arg)

	// iteration / control
	case OpIncrementIndex, OpDecrementIndex, OpIncrementCount,
		OpJump, OpJumpBackward, OpJumpIfTrue, OpJumpIfFalse, OpJumpIfNil,
		OpJumpIfNotNil, OpJumpIfEnd, OpProfileStart, OpProfileEnd, OpEnd:
		return 0

	case OpCreate:
		if arg == 2 {
			return 0 // pops the order string, pushes the accumulator
		}
		return 1 // groupBy accumulator

	case OpBegin:
		return -1
	}
	return 0
}

// typedCallDelta computes the stack effect of OpCallTyped: the VM pops the
// callee and every recorded input parameter, then pushes one result.
func typedCallDelta(kind int) int {
	if kind <= 0 || kind >= len(FuncTypes) {
		return 0
	}
	ft := reflect.TypeOf(FuncTypes[kind]).Elem()
	return -ft.NumIn()
}
