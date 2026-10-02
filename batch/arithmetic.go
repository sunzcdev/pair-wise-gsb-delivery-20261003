package batch

import "github.com/expr-lang/expr/vm/runtime"

type arithmeticOp int

const (
	arithmeticAdd arithmeticOp = iota
	arithmeticSubtract
	arithmeticMultiply
	arithmeticDivide
	arithmeticModulo
	arithmeticExponent
)

// arithmetic runs an arithmetic operator. When both operands are the same
// supported numeric type and neither is a vAny (which may contain nulls or
// narrower types), a typed fast path is used; otherwise the exact runtime
// helpers are invoked per row.
func (e *evaluator) arithmetic(left, right vector, op arithmeticOp) vector {
	if out, ok := numericFastPath(e, left, right, op); ok {
		return out
	}

	var fn func(a, b any) any
	switch op {
	case arithmeticAdd:
		fn = func(a, b any) any { return runtime.Add(a, b) }
	case arithmeticSubtract:
		fn = func(a, b any) any { return runtime.Subtract(a, b) }
	case arithmeticMultiply:
		fn = func(a, b any) any { return runtime.Multiply(a, b) }
	case arithmeticDivide:
		fn = func(a, b any) any { return runtime.Divide(a, b) }
	case arithmeticModulo:
		fn = func(a, b any) any { return runtime.Modulo(a, b) }
	case arithmeticExponent:
		fn = func(a, b any) any { return runtime.Exponent(a, b) }
	}

	out := newAnys(e.n)
	e.each(func(i int) {
		out.setAny(i, fn(left.at(left.index(i)), right.at(right.index(i))))
	})
	return out
}

// numericFastPath returns a typed result when both operands share a numeric
// type. Constants (which may widen, e.g. int vs float64) fall back to the
// elementwise runtime helpers so the result type matches the VM.
func numericFastPath(e *evaluator, left, right vector, op arithmeticOp) (vector, bool) {
	if left.kind != right.kind {
		return vector{}, false
	}

	switch left.kind {
	case vInt:
		switch op {
		case arithmeticAdd:
			out := newInts(e.n)
			e.each(func(i int) {
				li, ri := left.index(i), right.index(i)
				out.ints[i] = left.ints[li] + right.ints[ri]
			})
			return out, true
		case arithmeticSubtract:
			out := newInts(e.n)
			e.each(func(i int) {
				li, ri := left.index(i), right.index(i)
				out.ints[i] = left.ints[li] - right.ints[ri]
			})
			return out, true
		case arithmeticMultiply:
			out := newInts(e.n)
			e.each(func(i int) {
				li, ri := left.index(i), right.index(i)
				out.ints[i] = left.ints[li] * right.ints[ri]
			})
			return out, true
		case arithmeticDivide:
			out := newFloat64s(e.n)
			e.each(func(i int) {
				li, ri := left.index(i), right.index(i)
				out.float64s[i] = float64(left.ints[li]) / float64(right.ints[ri])
			})
			return out, true
		case arithmeticModulo:
			out := newInts(e.n)
			e.each(func(i int) {
				li, ri := left.index(i), right.index(i)
				out.ints[i] = left.ints[li] % right.ints[ri]
			})
			return out, true
		case arithmeticExponent:
			out := newFloat64s(e.n)
			e.each(func(i int) {
				li, ri := left.index(i), right.index(i)
				out.float64s[i] = runtime.Exponent(left.ints[li], right.ints[ri])
			})
			return out, true
		}

	case vFloat64:
		switch op {
		case arithmeticAdd:
			out := newFloat64s(e.n)
			e.each(func(i int) {
				li, ri := left.index(i), right.index(i)
				out.float64s[i] = left.float64s[li] + right.float64s[ri]
			})
			return out, true
		case arithmeticSubtract:
			out := newFloat64s(e.n)
			e.each(func(i int) {
				li, ri := left.index(i), right.index(i)
				out.float64s[i] = left.float64s[li] - right.float64s[ri]
			})
			return out, true
		case arithmeticMultiply:
			out := newFloat64s(e.n)
			e.each(func(i int) {
				li, ri := left.index(i), right.index(i)
				out.float64s[i] = left.float64s[li] * right.float64s[ri]
			})
			return out, true
		case arithmeticDivide:
			out := newFloat64s(e.n)
			e.each(func(i int) {
				li, ri := left.index(i), right.index(i)
				out.float64s[i] = left.float64s[li] / right.float64s[ri]
			})
			return out, true
		case arithmeticModulo:
			return vector{}, false
		case arithmeticExponent:
			out := newFloat64s(e.n)
			e.each(func(i int) {
				li, ri := left.index(i), right.index(i)
				out.float64s[i] = runtime.Exponent(left.float64s[li], right.float64s[ri])
			})
			return out, true
		}

	}

	return vector{}, false
}
