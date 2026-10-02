package batch

import (
	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/vm/runtime"
)

// memberNode implements field/map/index access. Access is performed per row
// with runtime.Fetch (for dynamic properties) or runtime.FetchField (for
// static struct fields), preserving exact null and panic semantics.
func (e *evaluator) memberNode(node *ast.MemberNode) vector {
	base := e.eval(node.Node)

	if prop, ok := node.Property.(*ast.StringNode); ok && isStaticField(node) {
		return e.fetchStaticField(base, prop.Value, node)
	}

	property := e.evalProperty(node)

	out := newAnys(e.n)
	e.each(func(i int) {
		if e.isMaskedOff(i) {
			return
		}
		a := base.at(base.index(i))
		if node.Optional && !node.Method && runtime.IsNil(a) {
			e.skipForChain(i)
			return
		}
		b := property.at(property.index(i))
		out.setAny(i, runtime.Fetch(a, b))
	})
	return out
}

// fetchStaticField handles the common obj.field form against structs and
// maps using the cached field path computed by the checker.
func (e *evaluator) fetchStaticField(base vector, name string, node *ast.MemberNode) vector {
	// When the base column originates from a top level identifier resolved
	// against a map environment, the value is typically already a map/struct
	// per row. runtime.Fetch implements the exact required behavior for both
	// (methods, maps, structs, slices) and keeps semantics identical to the
	// scalar VM.
	out := newAnys(e.n)
	e.each(func(i int) {
		if e.isMaskedOff(i) {
			return
		}
		a := base.at(base.index(i))
		if node.Optional && !node.Method && runtime.IsNil(a) {
			e.skipForChain(i)
			return
		}
		out.setAny(i, runtime.Fetch(a, name))
	})
	return out
}

// evalProperty evaluates the property node. Inside an optional chain the
// property expression is not evaluated for rows whose base is null, matching
// the JumpIfNil bytecode.
func (e *evaluator) evalProperty(node *ast.MemberNode) vector {
	if !node.Optional || e.chainOff == nil {
		return e.eval(node.Property)
	}
	mask := make([]bool, e.n)
	for i := 0; i < e.n; i++ {
		if e.dead[i] || e.isMaskedOff(i) || (e.mask != nil && e.mask[i]) {
			mask[i] = true
		}
	}
	return e.evalWithMask(node.Property, mask)
}

// chainNode installs a fresh activity mask for the chain body. Rows which
// become null while traversing optional members are masked off and end up as
// null, mirroring the compiler's JumpIfNil chain jumps.
func (e *evaluator) chainNode(node *ast.ChainNode) vector {
	prevOff := e.chainOff
	e.chainOff = make([]bool, e.n)
	v := e.eval(node.Node)

	// Rows switched off inside the chain produce null.
	out := asGenericOr(v, e.n)
	for i := 0; i < e.n; i++ {
		if e.chainOff[i] {
			out.setAny(i, nil)
		}
	}

	e.chainOff = prevOff
	return out
}

// skipForChain deactivates a row for the remainder of the surrounding chain.
func (e *evaluator) skipForChain(i int) {
	if e.chainOff != nil {
		e.chainOff[i] = true
	}
}

// isMaskedOff reports whether the row has been switched off inside the
// current optional chain.
func (e *evaluator) isMaskedOff(i int) bool {
	return e.chainOff != nil && e.chainOff[i]
}

// isStaticField reports whether the member access is a static property
// (foo.bar) rather than a dynamic index (foo[bar]).
func isStaticField(node *ast.MemberNode) bool {
	_, ok := node.Property.(*ast.StringNode)
	return ok
}

// conditionalNode implements both the ternary and the if/else operator.
func (e *evaluator) conditionalNode(node *ast.ConditionalNode) vector {
	parent := e.currentMask()

	cond := e.evalWithMask(node.Cond, parent)
	cond = e.derefVector(node.Cond, cond)

	take := make([]bool, e.n)
	suppressExp1 := make([]bool, e.n)
	suppressExp2 := make([]bool, e.n)
	e.each(func(i int) {
		if parent[i] {
			suppressExp1[i] = true
			suppressExp2[i] = true
			return
		}
		b, ok := cond.at(cond.index(i)).(bool)
		if !ok {
			panic(interfaceBoolPanic(cond.at(cond.index(i))))
		}
		take[i] = b
		if b {
			// cond true: Exp1 runs, Exp2 is suppressed.
			suppressExp2[i] = true
		} else {
			// cond false: Exp2 runs, Exp1 is suppressed.
			suppressExp1[i] = true
		}
	})

	first := e.evalWithMask(node.Exp1, suppressExp1)
	second := e.evalWithMask(node.Exp2, suppressExp2)

	out := newAnys(e.n)
	for i := 0; i < e.n; i++ {
		if e.dead[i] || parent[i] {
			continue
		}
		if take[i] {
			out.setAny(i, first.at(first.index(i)))
		} else {
			out.setAny(i, second.at(second.index(i)))
		}
	}
	return out
}
