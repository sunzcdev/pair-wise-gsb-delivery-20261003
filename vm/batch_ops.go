package vm

import (
	"fmt"
	"math"

	"github.com/expr-lang/expr/vm/runtime"
)

// pushConst pushes a scalar constant for every currently active row.
func (b *batchVM) pushConst(value any) {
	col := b.allocCol()
	for i := 0; i < b.n; i++ {
		if b.active.get(i) {
			col[i] = value
		}
	}
	b.pushOwned(col)
}

// pushColumn pushes an input column. A missing column yields nil for every
// row, as a missing map key does in the scalar VM.
func (b *batchVM) pushColumn(src []any) {
	// Input columns are read-only and shared directly: no copy, no pooling.
	// Inactive rows simply never read their slot, so an unmasked push is safe.
	col := src
	if col == nil {
		col = b.nilColumn()
	}
	b.stack = append(b.stack, col)
	b.owned = append(b.owned, false)
	b.sp++
}

// nilColumn is a shared read-only column of nils used when an input column is
// absent.
func (b *batchVM) nilColumn() []any {
	if b.nils == nil {
		b.nils = make([]any, b.n)
	}
	return b.nils
}

func (b *batchVM) popCol() []any {
	b.sp--
	col := b.stack[b.sp]
	b.stack = b.stack[:b.sp]
	b.owned = b.owned[:b.sp]
	return col
}

func (b *batchVM) topCol() []any {
	return b.stack[b.sp-1]
}

func (b *batchVM) pushOwned(col []any) {
	b.stack = append(b.stack, col)
	b.owned = append(b.owned, true)
	b.sp++
}

// unary applies fn to the top value of every active row. A row whose value
// triggers an error becomes dead and is recorded.
func (b *batchVM) unary(fn func(v any) (any, error)) {
	col := b.topCol()
	if !b.owned[b.sp-1] {
		col = b.materialize(col)
		b.stack[b.sp-1] = col
		b.owned[b.sp-1] = true
	}
	for i := 0; i < b.n; i++ {
		if !b.active.get(i) {
			continue
		}
		out, err := fn(col[i])
		if err != nil {
			b.fail(i, err)
			continue
		}
		col[i] = out
	}
}

// binary applies fn to the top two values of every active row. Rows parked by
// earlier short-circuit jumps are inactive and therefore never reach the
// operation, which is exactly the per-row short-circuit guarantee.
func (b *batchVM) binary(fn func(a, b any) (any, error)) {
	right, rightOwned := b.popOwned()
	left := b.writableTop()
	for i := 0; i < b.n; i++ {
		if !b.active.get(i) {
			continue
		}
		out, err := fn(left[i], right[i])
		if err != nil {
			b.fail(i, err)
			continue
		}
		left[i] = out
	}
	if rightOwned {
		b.releaseCol(right)
	}
}

// popOwned pops the top column, reporting whether it is a pooled working
// column (and thus reusable after the caller finishes with it).
func (b *batchVM) popOwned() (col []any, owned bool) {
	b.sp--
	col = b.stack[b.sp]
	owned = b.owned[b.sp]
	b.stack = b.stack[:b.sp]
	b.owned = b.owned[:b.sp]
	return col, owned
}

// writableTop returns the current top as a column safe to mutate in place,
// materializing an owned copy when the slot holds a read-only input column.
func (b *batchVM) writableTop() []any {
	if b.owned[b.sp-1] {
		return b.stack[b.sp-1]
	}
	col := b.materialize(b.stack[b.sp-1])
	b.stack[b.sp-1] = col
	b.owned[b.sp-1] = true
	return col
}

// materialize copies active rows of src into a fresh pooled column.
func (b *batchVM) materialize(src []any) []any {
	col := b.allocCol()
	if src != nil {
		for i := 0; i < b.n; i++ {
			if b.active.get(i) {
				col[i] = src[i]
			}
		}
	}
	return col
}

// arith runs an arithmetic opcode. Common int/float64 operands use an inline
// type switch with no per-row closure or recover; unusual types (other numeric
// widths, time.Duration, strings for +, etc.) or panics take a recover-guarded
// slow path that reproduces runtime.Add/Subtract/... exactly.
func (b *batchVM) arith(kind int) {
	right, rightOwned := b.popOwned()
	left := b.writableTop()
	for i := 0; i < b.n; i++ {
		if !b.active.get(i) {
			continue
		}
		a, c := left[i], right[i]

		if x, ok := a.(int); ok {
			if y, ok := c.(int); ok {
				switch kind {
				case arithAdd:
					left[i] = x + y
					continue
				case arithSub:
					left[i] = x - y
					continue
				case arithMul:
					left[i] = x * y
					continue
				case arithDiv:
					left[i] = float64(x) / float64(y)
					continue
				case arithPow:
					left[i] = math.Pow(float64(x), float64(y))
					continue
				case arithMod:
					if y == 0 {
						b.fail(i, errIntDivZero)
						continue
					}
					left[i] = x % y
					continue
				}
			}
			if y, ok := c.(float64); ok {
				if v, ok := floatArith(kind, float64(x), y); ok {
					left[i] = v
					continue
				}
			}
		}
		if x, ok := a.(float64); ok {
			switch y := c.(type) {
			case float64:
				if v, ok := floatArith(kind, x, y); ok {
					left[i] = v
					continue
				}
			case int:
				if v, ok := floatArith(kind, x, float64(y)); ok {
					left[i] = v
					continue
				}
			}
		}

		out, err := safeScalar(func() any { return scalarArith(kind, a, c) })
		if err != nil {
			b.fail(i, err)
			continue
		}
		left[i] = out
	}
	if rightOwned {
		b.releaseCol(right)
	}
}

const (
	arithAdd = iota
	arithSub
	arithMul
	arithDiv
	arithMod
	arithPow
)

func floatArith(kind int, x, y float64) (float64, bool) {
	switch kind {
	case arithAdd:
		return x + y, true
	case arithSub:
		return x - y, true
	case arithMul:
		return x * y, true
	case arithDiv:
		return x / y, true
	case arithPow:
		return math.Pow(x, y), true
	}
	return 0, false
}

func scalarArith(kind int, a, c any) any {
	switch kind {
	case arithAdd:
		return runtime.Add(a, c)
	case arithSub:
		return runtime.Subtract(a, c)
	case arithMul:
		return runtime.Multiply(a, c)
	case arithDiv:
		return runtime.Divide(a, c)
	case arithMod:
		return runtime.Modulo(a, c)
	case arithPow:
		return runtime.Exponent(a, c)
	}
	return nil
}

var errIntDivZero = fmt.Errorf("runtime error: integer divide by zero")

// compare runs a comparison opcode with an inline fast path for int/float64,
// falling back to the exact runtime helpers (which also cover time.Time,
// time.Duration and all other numeric widths) on anything else.
func (b *batchVM) compare(kind int) {
	right, rightOwned := b.popOwned()
	left := b.writableTop()
	for i := 0; i < b.n; i++ {
		if !b.active.get(i) {
			continue
		}
		a, c := left[i], right[i]

		if v, ok := fastCompare(kind, a, c); ok {
			left[i] = v
			continue
		}

		var out any
		var err error
		switch kind {
		case cmpLess:
			out, err = safeScalarBool(func() bool { return runtime.Less(a, c) })
		case cmpMore:
			out, err = safeScalarBool(func() bool { return runtime.More(a, c) })
		case cmpLE:
			out, err = safeScalarBool(func() bool { return runtime.LessOrEqual(a, c) })
		case cmpGE:
			out, err = safeScalarBool(func() bool { return runtime.MoreOrEqual(a, c) })
		}
		if err != nil {
			b.fail(i, err)
			continue
		}
		left[i] = out
	}
	if rightOwned {
		b.releaseCol(right)
	}
}

const (
	cmpLess = iota
	cmpMore
	cmpLE
	cmpGE
)

func fastCompare(kind int, a, c any) (any, bool) {
	if x, ok := a.(int); ok {
		if y, ok := c.(int); ok {
			switch kind {
			case cmpLess:
				return x < y, true
			case cmpMore:
				return x > y, true
			case cmpLE:
				return x <= y, true
			case cmpGE:
				return x >= y, true
			}
		}
		// Mixed int/float64: the runtime compares via float64.
		if y, ok := c.(float64); ok {
			return floatCompare(kind, float64(x), y)
		}
	}
	if x, ok := a.(float64); ok {
		switch y := c.(type) {
		case float64:
			return floatCompare(kind, x, y)
		case int:
			return floatCompare(kind, x, float64(y))
		}
	}
	return nil, false
}

func floatCompare(kind int, x, y float64) (any, bool) {
	switch kind {
	case cmpLess:
		return x < y, true
	case cmpMore:
		return x > y, true
	case cmpLE:
		return x <= y, true
	case cmpGE:
		return x >= y, true
	}
	return nil, false
}

// jump implements OpJump: every active row is parked at the target (with its
// current top value); no rows fall through.
func (b *batchVM) jump(ip, arg int) error {
	if arg < 0 {
		return fallback("negative jump offset")
	}
	col := b.topCol()
	target := ip + 1 + arg

	taken := b.active.clone()
	b.park(target, col, taken)

	// Keep slot depth; the slot is logically empty until the target merge.
	b.active.andNot(taken)
	b.suspended.or(taken)
	return nil
}

// branch implements OpJumpIfTrue / OpJumpIfFalse. Rows satisfying the jump
// condition are parked at the target (their condition value preserved, so the
// jump target can pop it); the rest stay active and fall through.
func (b *batchVM) branch(ip, arg int, jumpWhen bool) error {
	if arg < 0 {
		return fallback("negative jump offset")
	}
	col := b.topCol()
	target := ip + 1 + arg

	taken := newMask(b.n, false)
	for i := 0; i < b.n; i++ {
		if !b.active.get(i) {
			continue
		}
		cond, ok := col[i].(bool)
		if !ok {
			b.fail(i, fmt.Errorf("interface conversion: interface {} is %T, not bool", col[i]))
			continue
		}
		if cond == jumpWhen {
			taken.set(i)
		}
	}

	if taken.any() {
		b.park(target, col, taken)
		b.active.andNot(taken)
		b.suspended.or(taken)
	}
	return nil
}

// branchNil implements OpJumpIfNil / OpJumpIfNotNil.
func (b *batchVM) branchNil(ip, arg int, jumpWhenNil bool) error {
	if arg < 0 {
		return fallback("negative jump offset")
	}
	col := b.topCol()
	target := ip + 1 + arg

	taken := newMask(b.n, false)
	for i := 0; i < b.n; i++ {
		if !b.active.get(i) {
			continue
		}
		if runtime.IsNil(col[i]) == jumpWhenNil {
			taken.set(i)
		}
	}

	if taken.any() {
		b.park(target, col, taken)
		b.active.andNot(taken)
		b.suspended.or(taken)
	}
	return nil
}
