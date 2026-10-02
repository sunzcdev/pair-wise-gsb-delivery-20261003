package vm

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/expr-lang/expr/internal/deref"
	"github.com/expr-lang/expr/vm/runtime"
)

// errFallback signals that an opcode cannot be evaluated column-wise; the
// whole batch must instead be run row by row with the scalar VM.
type errFallback struct{ reason string }

func (e errFallback) Error() string { return "batch fallback: " + e.reason }

func fallback(reason string) errFallback { return errFallback{reason: reason} }

// batchSupported reports whether the program is entirely within the scalar
// subset that the columnar interpreter can execute.
func batchSupported(program *Program) bool {
	for _, op := range program.Bytecode {
		if !batchOpcodes[op] {
			return false
		}
	}
	return true
}

var batchOpcodes = [256]bool{
	OpPush:         true,
	OpInt:          true,
	OpPop:          true,
	OpStore:        true,
	OpLoadVar:      true,
	OpLoadConst:    true,
	OpLoadField:    true,
	OpLoadFast:     true,
	OpTrue:         true,
	OpFalse:        true,
	OpNil:          true,
	OpNegate:       true,
	OpNot:          true,
	OpEqual:        true,
	OpEqualInt:     true,
	OpEqualString:  true,
	OpJump:         true,
	OpJumpIfTrue:   true,
	OpJumpIfFalse:  true,
	OpJumpIfNil:    true,
	OpJumpIfNotNil: true,
	OpIn:           true,
	OpLess:         true,
	OpMore:         true,
	OpLessOrEqual:  true,
	OpMoreOrEqual:  true,
	OpAdd:          true,
	OpSubtract:     true,
	OpMultiply:     true,
	OpDivide:       true,
	OpModulo:       true,
	OpExponent:     true,
	OpMatches:      true,
	OpMatchesConst: true,
	OpContains:     true,
	OpStartsWith:   true,
	OpEndsWith:     true,
	OpAnd:          true,
	OpOr:           true,
	OpCast:         true,
	OpDeref:        true,
}

func (b *batchVM) exec(program *Program, ip int, op Opcode, arg int) error {
	switch op {

	case OpPush:
		b.pushConst(program.Constants[arg])

	case OpInt:
		b.pushConst(arg)

	case OpTrue:
		b.pushConst(true)

	case OpFalse:
		b.pushConst(false)

	case OpNil:
		b.pushConst(nil)

	case OpPop:
		col, owned := b.popOwned()
		if owned {
			b.releaseCol(col)
		}

	case OpLoadFast:
		name := program.Constants[arg].(string)
		b.pushColumn(b.inputColumn(name))

	case OpLoadField:
		b.pushColumn(b.fieldColumn(program, arg))

	case OpLoadConst:
		// Only identifiers reach here (see compiler.IdentifierNode).
		name := program.Constants[arg].(string)
		b.pushColumn(b.inputColumn(name))

	case OpLoadVar:
		// Variables hold working columns; push a read-only view safely because
		// any later in-place op materializes the slot first.
		b.stack = append(b.stack, b.vars[arg])
		b.owned = append(b.owned, false)
		b.sp++

	case OpStore:
		col, _ := b.popOwned()
		for len(b.vars) <= arg {
			b.vars = append(b.vars, nil)
		}
		b.vars[arg] = col

	case OpNegate:
		b.unary(func(v any) (any, error) {
			return safeScalar(func() any { return runtime.Negate(v) })
		})

	case OpNot:
		b.unary(func(v any) (any, error) {
			x, ok := v.(bool)
			if !ok {
				return nil, fmt.Errorf("interface conversion: interface {} is %T, not bool", v)
			}
			return !x, nil
		})

	case OpDeref:
		b.unary(func(v any) (any, error) {
			return deref.Interface(v), nil
		})

	case OpEqual:
		b.binary(func(a, c any) (any, error) {
			return runtime.Equal(a, c), nil
		})

	case OpEqualInt:
		b.binary(func(a, c any) (any, error) {
			x, ok := a.(int)
			if !ok {
				return nil, fmt.Errorf("interface conversion: interface {} is %T, not int", a)
			}
			y, ok := c.(int)
			if !ok {
				return nil, fmt.Errorf("interface conversion: interface {} is %T, not int", c)
			}
			return x == y, nil
		})

	case OpEqualString:
		b.binary(func(a, c any) (any, error) {
			x, ok := a.(string)
			if !ok {
				return nil, fmt.Errorf("interface conversion: interface {} is %T, not string", a)
			}
			y, ok := c.(string)
			if !ok {
				return nil, fmt.Errorf("interface conversion: interface {} is %T, not string", c)
			}
			return x == y, nil
		})

	case OpIn:
		b.binary(func(a, c any) (any, error) {
			return safeScalarBool(func() bool { return runtime.In(a, c) })
		})

	case OpLess:
		b.compare(cmpLess)

	case OpMore:
		b.compare(cmpMore)

	case OpLessOrEqual:
		b.compare(cmpLE)

	case OpMoreOrEqual:
		b.compare(cmpGE)

	case OpAdd:
		b.arith(arithAdd)

	case OpSubtract:
		b.arith(arithSub)

	case OpMultiply:
		b.arith(arithMul)

	case OpDivide:
		b.arith(arithDiv)

	case OpModulo:
		b.arith(arithMod)

	case OpExponent:
		b.arith(arithPow)

	case OpMatches:
		b.binary(func(a, c any) (any, error) {
			if runtime.IsNil(a) || runtime.IsNil(c) {
				return false, nil
			}
			pattern, ok := c.(string)
			if !ok {
				return nil, fmt.Errorf("invalid operation: matches against %T", c)
			}
			if s, ok := a.(string); ok {
				matched, err := regexp.MatchString(pattern, s)
				if err != nil {
					return nil, err
				}
				return matched, nil
			}
			if s, ok := a.([]byte); ok {
				matched, err := regexp.Match(pattern, s)
				if err != nil {
					return nil, err
				}
				return matched, nil
			}
			return nil, fmt.Errorf("invalid operation: matches on %T", a)
		})

	case OpMatchesConst:
		re := program.Constants[arg].(*regexp.Regexp)
		b.unary(func(a any) (any, error) {
			if runtime.IsNil(a) {
				return false, nil
			}
			if s, ok := a.(string); ok {
				return re.MatchString(s), nil
			}
			if s, ok := a.([]byte); ok {
				return re.Match(s), nil
			}
			return nil, fmt.Errorf("invalid operation: matches on %T", a)
		})

	case OpContains:
		b.binary(stringTest(strings.Contains))

	case OpStartsWith:
		b.binary(stringTest(strings.HasPrefix))

	case OpEndsWith:
		b.binary(stringTest(strings.HasSuffix))

	case OpAnd:
		b.binary(boolAnd)

	case OpOr:
		b.binary(boolOr)

	case OpCast:
		b.unary(func(v any) (any, error) {
			return safeScalar(func() any {
				switch arg {
				case 0:
					return runtime.ToInt(v)
				case 1:
					return runtime.ToInt64(v)
				case 2:
					return runtime.ToFloat64(v)
				case 3:
					return runtime.ToBool(v)
				}
				return v
			})
		})

	case OpJump:
		return b.jump(ip, arg)

	case OpJumpIfTrue:
		return b.branch(ip, arg, true)

	case OpJumpIfFalse:
		return b.branch(ip, arg, false)

	case OpJumpIfNil:
		return b.branchNil(ip, arg, true)

	case OpJumpIfNotNil:
		return b.branchNil(ip, arg, false)

	default:
		return fallback(fmt.Sprintf("unsupported opcode %v", op))
	}

	return nil
}

func stringTest(fn func(s, substr string) bool) func(a, b any) (any, error) {
	return func(a, b any) (any, error) {
		if runtime.IsNil(a) || runtime.IsNil(b) {
			return false, nil
		}
		s, ok := a.(string)
		if !ok {
			return nil, fmt.Errorf("invalid operation: string test on %T", a)
		}
		sub, ok := b.(string)
		if !ok {
			return nil, fmt.Errorf("invalid operation: string test with %T", b)
		}
		return fn(s, sub), nil
	}
}

func assertBool(v any) (bool, error) {
	x, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("interface conversion: interface {} is %T, not bool", v)
	}
	return x, nil
}

// boolAnd/boolOr receive operands in stack order: a is the expression's left
// operand (popped second by the VM) and b is the right operand (popped first).
// The scalar VM evaluates the popped-right operand first and relies on Go's
// short-circuit, so the left operand is neither asserted nor required to be a
// bool once the right operand decides the result.
func boolAnd(a, b any) (any, error) {
	x, err := assertBool(b)
	if err != nil {
		return nil, err
	}
	if !x {
		return false, nil
	}
	y, err := assertBool(a)
	if err != nil {
		return nil, err
	}
	return y, nil
}

func boolOr(a, b any) (any, error) {
	x, err := assertBool(b)
	if err != nil {
		return nil, err
	}
	if x {
		return true, nil
	}
	y, err := assertBool(a)
	if err != nil {
		return nil, err
	}
	return y, nil
}

func safeScalar(fn func() any) (v any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return fn(), nil
}

func safeScalarBool(fn func() bool) (v any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return fn(), nil
}
