package batch

import (
	"regexp"
	"strings"

	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/vm/runtime"
)

func (e *evaluator) binaryNode(node *ast.BinaryNode) vector {
	switch node.Operator {
	case "and", "&&":
		return e.logicalAnd(node)
	case "or", "||":
		return e.logicalOr(node)
	case "??":
		return e.coalesce(node)
	}

	left := e.eval(node.Left)
	left = e.derefVector(node.Left, left)
	right := e.eval(node.Right)
	right = e.derefVector(node.Right, right)

	switch node.Operator {
	case "==":
		return e.compareEqual(left, right, false)
	case "!=":
		return e.compareEqual(left, right, true)
	case "<":
		return e.compareOrdered(left, right, cmpLess)
	case ">":
		return e.compareOrdered(left, right, cmpMore)
	case "<=":
		return e.compareOrdered(left, right, cmpLessOrEqual)
	case ">=":
		return e.compareOrdered(left, right, cmpMoreOrEqual)
	case "+":
		return e.arithmetic(left, right, arithmeticAdd)
	case "-":
		return e.arithmetic(left, right, arithmeticSubtract)
	case "*":
		return e.arithmetic(left, right, arithmeticMultiply)
	case "/":
		return e.arithmetic(left, right, arithmeticDivide)
	case "%":
		return e.arithmetic(left, right, arithmeticModulo)
	case "**", "^":
		return e.arithmetic(left, right, arithmeticExponent)
	case "in":
		return e.elementwiseBool(left, right, runtime.In)
	case "matches":
		return e.matches(left, right, node)
	case "contains":
		return e.stringsOp(left, right, strings.Contains)
	case "startsWith":
		return e.stringsOp(left, right, strings.HasPrefix)
	case "endsWith":
		return e.stringsOp(left, right, strings.HasSuffix)
	}

	panic("batch: unsupported binary operator " + node.Operator)
}

// logicalAnd evaluates left, then evaluates right only for rows where left
// is true. Rows where left is false short circuit to left's false. A non
// bool left is a row error.
func (e *evaluator) logicalAnd(node *ast.BinaryNode) vector {
	return e.shortCircuit(node, true)
}

// logicalOr evaluates left, then evaluates right only for rows where left
// is false. Rows where left is true short circuit to true.
func (e *evaluator) logicalOr(node *ast.BinaryNode) vector {
	return e.shortCircuit(node, false)
}

// shortCircuit implements && (requireRight=true) and || (requireRight=false).
//
// The active mask (true = skip) propagated by an enclosing short circuit is
// propagated to both operands. The left operand is evaluated for every row
// the ancestor has not skipped. Rows which short circuit at this operator
// are added to the mask used for the right operand, so a division or a host
// function on the right side never runs for those rows — matching scalar
// short circuit semantics per row.
func (e *evaluator) shortCircuit(node *ast.BinaryNode, requireRight bool) vector {
	ancestor := e.currentMask()

	left := e.evalWithMask(node.Left, ancestor)
	left = e.derefVector(node.Left, left)

	// local marks rows which short circuit at THIS operator: false for &&,
	// true for ||. Rows skipped by an ancestor are marked as well so the
	// masks compose uniformly.
	local := make([]bool, e.n)
	allShort := true
	for i := 0; i < e.n; i++ {
		if e.dead[i] || ancestor[i] {
			local[i] = true
			continue
		}
		b, ok := left.at(left.index(i)).(bool)
		if !ok {
			panic(interfaceBoolPanic(left.at(left.index(i))))
		}
		short := (requireRight && !b) || (!requireRight && b)
		if short {
			local[i] = true
		} else {
			allShort = false
		}
	}

	out := asGenericOr(left, e.n)
	if allShort {
		return out
	}

	// The right operand sees the ancestor mask plus this operator's local
	// short circuit mask.
	rightMask := make([]bool, e.n)
	for i := 0; i < e.n; i++ {
		rightMask[i] = ancestor[i] || local[i]
	}
	right := e.evalWithMask(node.Right, rightMask)
	right = e.derefVector(node.Right, right)

	for i := 0; i < e.n; i++ {
		if e.dead[i] || ancestor[i] || local[i] {
			continue
		}
		out.setAny(i, right.at(right.index(i)))
	}
	return out
}

// coalesce evaluates left and evaluates right only for rows whose left value
// is null, mirroring the JumpIfNotNil bytecode.
func (e *evaluator) coalesce(node *ast.BinaryNode) vector {
	parent := e.currentMask()

	left := e.evalWithMask(node.Left, parent)
	left = e.derefVector(node.Left, left)

	rightMask := make([]bool, e.n)
	allTakeLeft := true
	for i := 0; i < e.n; i++ {
		if e.dead[i] || parent[i] {
			rightMask[i] = true
			continue
		}
		isNil := runtime.IsNil(left.at(left.index(i)))
		if !isNil {
			rightMask[i] = true
		} else {
			allTakeLeft = false
		}
	}

	out := asGenericOr(left, e.n)
	if allTakeLeft {
		return out
	}

	right := e.evalWithMask(node.Right, rightMask)
	right = e.derefVector(node.Right, right)

	for i := 0; i < e.n; i++ {
		if e.dead[i] || parent[i] || rightMask[i] {
			continue
		}
		if runtime.IsNil(left.at(left.index(i))) {
			out.setAny(i, right.at(right.index(i)))
		}
	}
	return out
}

func interfaceBoolPanic(v any) any {
	return "interface conversion: interface {} is " + typeName(v) + ", not bool"
}

func typeName(v any) string {
	if v == nil {
		return "nil"
	}
	switch v.(type) {
	case int:
		return "int"
	case string:
		return "string"
	case float64:
		return "float64"
	default:
		return "non-bool"
	}
}

// asGenericOr materializes a (possibly constant) operand as a vAny vector.
func asGenericOr(v vector, n int) vector {
	out := newAnys(n)
	for i := 0; i < n; i++ {
		out.setAny(i, v.at(v.index(i)))
	}
	return out
}

// compareEqual implements == and !=. Same kind typed columns use a typed
// fast path; everything else (including nulls) uses runtime.Equal so that
// null semantics are preserved exactly.
func (e *evaluator) compareEqual(left, right vector, negate bool) vector {
	out := newBools(e.n)
	if fast, ok := equalFastPath(left, right); ok {
		e.each(func(i int) {
			// each already skips suppressed rows, but the bool result for
			// those rows is irrelevant; the caller keeps the left side.
			result := fast(i)
			out.bools[i] = result != negate
		})
		return out
	}
	e.each(func(i int) {
		result := runtime.Equal(left.get(i), right.get(i))
		out.bools[i] = result != negate
	})
	return out
}

type eqFunc func(row int) bool

func isShortCircuitNode(node ast.Node) bool {
	b, ok := node.(*ast.BinaryNode)
	if !ok {
		return false
	}
	switch b.Operator {
	case "and", "&&", "or", "||":
		return true
	}
	return false
}

func equalFastPath(left, right vector) (eqFunc, bool) {
	if left.kind != right.kind {
		return nil, false
	}
	if left.kind == vAny {
		return nil, false
	}
	switch left.kind {
	case vBool:
		return func(row int) bool { return left.getBool(row) == right.getBool(row) }, true
	case vString:
		return func(row int) bool { return left.getString(row) == right.getString(row) }, true
	case vInt:
		return func(row int) bool { return left.getInt(row) == right.getInt(row) }, true
	case vInt64:
		return func(row int) bool { return left.getInt64(row) == right.getInt64(row) }, true
	case vFloat64:
		return func(row int) bool { return left.getFloat64(row) == right.getFloat64(row) }, true
	case vFloat32:
		return func(row int) bool { return left.getFloat32(row) == right.getFloat32(row) }, true
	case vInt8:
	case vInt16:
	case vInt32:
	case vUint:
	case vUint16:
	case vUint32:
	case vUint64:
	}
	return nil, false
}

func (e *evaluator) elementwiseBool(left, right vector, fn func(a, b any) bool) vector {
	out := newBools(e.n)
	e.each(func(i int) {
		out.bools[i] = fn(left.at(left.index(i)), right.at(right.index(i)))
	})
	return out
}

func (e *evaluator) stringsOp(left, right vector, fn func(a, b string) bool) vector {
	out := newBools(e.n)
	e.each(func(i int) {
		a := left.at(left.index(i))
		b := right.at(right.index(i))
		if runtime.IsNil(a) || runtime.IsNil(b) {
			out.bools[i] = false
			return
		}
		out.bools[i] = fn(a.(string), b.(string))
	})
	return out
}

func (e *evaluator) matches(left, right vector, node *ast.BinaryNode) vector {
	var static *regexp.Regexp
	if str, ok := node.Right.(*ast.StringNode); ok {
		static = regexp.MustCompile(str.Value)
	}

	out := newBools(e.n)
	e.each(func(i int) {
		a := left.at(left.index(i))
		if runtime.IsNil(a) {
			out.bools[i] = false
			return
		}
		r := static
		if r == nil {
			b := right.at(right.index(i))
			if runtime.IsNil(b) {
				out.bools[i] = false
				return
			}
			r = regexp.MustCompile(b.(string))
		}
		switch s := a.(type) {
		case string:
			out.bools[i] = r.MatchString(s)
		case []byte:
			out.bools[i] = r.Match(s)
		default:
			panic("invalid matches operand")
		}
	})
	return out
}
