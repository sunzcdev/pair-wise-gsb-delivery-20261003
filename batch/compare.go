package batch

import "github.com/expr-lang/expr/vm/runtime"

type cmpKind int

const (
	cmpLess cmpKind = iota
	cmpMore
	cmpLessOrEqual
	cmpMoreOrEqual
)

// compareOrdered implements <, >, <=, >=. Typed numeric columns and int64
// constants use a fast path without boxing; mixed numeric kinds, strings,
// time values and everything else fall back to the exact runtime helpers so
// semantics are preserved.
func (e *evaluator) compareOrdered(left, right vector, op cmpKind) vector {
	out := newBools(e.n)

	ln, lf, ls := numericKind(left)
	rn, rf, rs := numericKind(right)
	switch {
	case ls == kindString && rs == kindString:
		e.each(func(i int) {
			a, b := left.getString(i), right.getString(i)
			out.bools[i] = compareResult(op, a < b, a > b, a <= b, a >= b)
		})
		return out
	case ls == kindString || rs == kindString:
		// string vs non string: let runtime panic
	case ln && rn && left.kind == right.kind:
		// Same concrete integer kind: compare exactly.
		e.each(func(i int) {
			a, b := numericInt(left, i), numericInt(right, i)
			out.bools[i] = compareResult(op, a < b, a > b, a <= b, a >= b)
		})
		return out
	case (ln || lf) && (rn || rf) && !(ln && rn):
		// Integer vs float: runtime compares as float64.
		e.each(func(i int) {
			a := numericFloat(left, i)
			b := numericFloat(right, i)
			out.bools[i] = compareResult(op, a < b, a > b, a <= b, a >= b)
		})
		return out
	}

	fn := cmpRuntime(op)
	e.each(func(i int) {
		out.bools[i] = fn(left.get(i), right.get(i))
	})
	return out
}

type scalarKind int

const (
	kindOther scalarKind = iota
	kindInt
	kindFloat
	kindString
)

// numericKind reports whether the vector holds an integer or floating point
// scalar. vAny vectors are treated as unknown so the runtime fallback keeps
// null and mixed type semantics intact.
func numericKind(v vector) (isInt, isFloat bool, s scalarKind) {
	switch v.kind {
	case vInt, vInt64, vInt8, vInt16, vInt32, vUint, vUint16, vUint32, vUint64:
		return true, false, kindInt
	case vFloat64, vFloat32:
		return false, true, kindFloat
	case vString:
		return false, false, kindString
	}
	return false, false, kindOther
}

func numericFloat(v vector, i int) float64 {
	switch v.kind {
	case vInt:
		return float64(v.ints[i])
	case vInt64:
		return float64(v.int64s[i])
	case vInt8:
		return float64(v.int8s[i])
	case vInt16:
		return float64(v.int16s[i])
	case vInt32:
		return float64(v.int32s[i])
	case vUint:
		return float64(v.uints[i])
	case vUint16:
		return float64(v.uint16s[i])
	case vUint32:
		return float64(v.uint32s[i])
	case vUint64:
		return float64(v.uint64s[i])
	case vFloat64:
		return v.float64s[i]
	case vFloat32:
		return float64(v.float32s[i])
	}
	return 0
}

func compareResult(op cmpKind, less, more, leq, meq bool) bool {
	switch op {
	case cmpLess:
		return less
	case cmpMore:
		return more
	case cmpLessOrEqual:
		return leq
	case cmpMoreOrEqual:
		return meq
	}
	return false
}

func numericInt(v vector, i int) int {
	switch v.kind {
	case vInt:
		return v.ints[i]
	case vInt64:
		return int(v.int64s[i])
	case vInt8:
		return int(v.int8s[i])
	case vInt16:
		return int(v.int16s[i])
	case vInt32:
		return int(v.int32s[i])
	case vUint:
		return int(v.uints[i])
	case vUint16:
		return int(v.uint16s[i])
	case vUint32:
		return int(v.uint32s[i])
	case vUint64:
		return int(v.uint64s[i])
	}
	return 0
}

func cmpRuntime(op cmpKind) func(a, b any) bool {
	switch op {
	case cmpLess:
		return runtime.Less
	case cmpMore:
		return runtime.More
	case cmpLessOrEqual:
		return runtime.LessOrEqual
	case cmpMoreOrEqual:
		return runtime.MoreOrEqual
	}
	return runtime.Less
}
