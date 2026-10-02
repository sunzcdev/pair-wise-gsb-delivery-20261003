package batch

import (
	"fmt"
	"reflect"

	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/file"
	"github.com/expr-lang/expr/internal/deref"
	"github.com/expr-lang/expr/vm"
	"github.com/expr-lang/expr/vm/runtime"
)

// evaluator is a columnar interpreter for the supported AST subset.
//
// Dead rows (rows which already produced an error, or rows short circuited
// inside an optional chain, a &&, ||, ?:, or ?? expression) are never fed to
// arithmetic, divisions or function calls. This keeps boolean short circuit
// semantics on a per row basis.
type evaluator struct {
	program *vm.Program
	n       int

	columns map[string]vector
	vars    map[string]vector

	// dead[i] is true when the row must not execute any further operation.
	dead []bool
	// errs holds the first error produced by every row.
	errs []error

	// chainOff marks rows switched off inside the current optional chain.
	chainOff []bool

	// mask is the per row activity mask installed by the nearest enclosing
	// short circuit operator (&&, ||, ??, ?:) or optional chain. A nil mask
	// means every live row is active.
	mask []bool

	// argBuf is a scratch buffer reused for per row function arguments.
	argBuf []any

	// loc is the source location of the node being evaluated.
	loc file.Location
}

// activeRow reports whether the row must execute operations.
func (e *evaluator) activeRow(i int) bool {
	if e.dead[i] {
		return false
	}
	if e.mask != nil && e.mask[i] {
		return false
	}
	return true
}

// currentMask returns the active mask or an all false mask when none is set.
func (e *evaluator) currentMask() []bool {
	if e.mask != nil {
		return e.mask
	}
	return make([]bool, e.n)
}

// evalWithMask evaluates node with a specific per row activity mask,
// restoring the previous mask afterwards.
func (e *evaluator) evalWithMask(node ast.Node, mask []bool) vector {
	prev := e.mask
	e.mask = mask
	v := e.eval(node)
	e.mask = prev
	return v
}

// fail records the first error of a row and deactivates it.
func (e *evaluator) fail(i int, recovered any) {
	if e.dead[i] {
		return
	}
	e.dead[i] = true
	var err error
	if rerr, ok := recovered.(error); ok {
		err = rerr
	} else {
		err = fmt.Errorf("%v", recovered)
	}
	fe := &file.Error{
		Location: e.loc,
		Message:  fmt.Sprintf("%v", err),
	}
	fe.Wrap(err)
	e.errs[i] = &RowError{
		Index: i,
		Line:  i + 1,
		Err:   fe.Bind(e.program.Source()),
	}
}

// each runs op for every live row. When the bulk run panics, the offending
// row is marked as failed and the remaining rows are executed in isolation so
// that the error is attributed to exactly the row which caused it.
func (e *evaluator) each(op func(i int)) {
	var panickedRow int
	var panickedValue any

	func() {
		defer func() {
			if r := recover(); r != nil {
				panickedValue = r
			}
		}()
		for i := 0; i < e.n; i++ {
			if !e.activeRow(i) {
				continue
			}
			panickedRow = i
			op(i)
		}
		panickedRow = -1
	}()

	if panickedValue == nil {
		return
	}

	e.fail(panickedRow, panickedValue)

	for i := panickedRow + 1; i < e.n; i++ {
		if !e.activeRow(i) {
			continue
		}
		e.runIsolated(i, op)
	}
}

// runIsolated runs op for a single row, converting a panic into a row error.
// It is used both by the recovery path of each and for operations which must
// not leak a single row's panic (always function/method/builtin calls).
func (e *evaluator) runIsolated(i int, op func(i int)) {
	defer func() {
		if r := recover(); r != nil {
			e.fail(i, r)
		}
	}()
	op(i)
}

// eval evaluates a node for all currently live rows and returns the result.
func (e *evaluator) eval(node ast.Node) vector {
	prev := e.loc
	e.loc = node.Location()
	defer func() { e.loc = prev }()

	switch n := node.(type) {
	case *ast.NilNode:
		return e.allConst(nil)

	case *ast.IntegerNode:
		return e.integerNode(n)

	case *ast.FloatNode:
		return e.allConst(n.Value)

	case *ast.BoolNode:
		return e.allConst(n.Value)

	case *ast.StringNode:
		return e.allConst(n.Value)

	case *ast.BytesNode:
		return e.allConst(n.Value)

	case *ast.ConstantNode:
		return e.allConst(n.Value)

	case *ast.IdentifierNode:
		return e.identifierNode(n)

	case *ast.UnaryNode:
		return e.unaryNode(n)

	case *ast.BinaryNode:
		return e.binaryNode(n)

	case *ast.ChainNode:
		return e.chainNode(n)

	case *ast.MemberNode:
		return e.memberNode(n)

	case *ast.ConditionalNode:
		return e.conditionalNode(n)

	case *ast.CallNode:
		return e.callNode(n)

	case *ast.BuiltinNode:
		return e.builtinNode(n)

	case *ast.VariableDeclaratorNode:
		return e.variableDeclaratorNode(n)

	case *ast.SequenceNode:
		return e.sequenceNode(n)

	case *ast.ArrayNode:
		return e.arrayNode(n)

	case *ast.MapNode:
		return e.mapNode(n)
	}

	panic(fmt.Sprintf("batch: unsupported node %T", node))
}

// allConst materializes a literal as a full length vector. Literals are
// broadcast up front so every operator can index vectors by row without
// special casing constants.
func (e *evaluator) allConst(value any) vector {
	return e.materialize(value)
}

// materialize builds a full length vector for a scalar constant, preserving
// the concrete Go types the compiler emits for typed literals.
func (e *evaluator) materialize(value any) vector {
	switch x := value.(type) {
	case nil:
		return newAnys(e.n)
	case bool:
		out := newBools(e.n)
		for i := 0; i < e.n; i++ {
			out.bools[i] = x
		}
		return out
	case string:
		out := newStrings(e.n)
		for i := 0; i < e.n; i++ {
			out.strings[i] = x
		}
		return out
	case int:
		out := newInts(e.n)
		for i := 0; i < e.n; i++ {
			out.ints[i] = x
		}
		return out
	case int64:
		out := newInt64s(e.n)
		for i := 0; i < e.n; i++ {
			out.int64s[i] = x
		}
		return out
	case float64:
		out := newFloat64s(e.n)
		for i := 0; i < e.n; i++ {
			out.float64s[i] = x
		}
		return out
	case float32:
		out := newFloat32s(e.n)
		for i := 0; i < e.n; i++ {
			out.float32s[i] = x
		}
		return out
	case int8:
		out := vector{kind: vInt8, int8s: make([]int8, e.n)}
		for i := 0; i < e.n; i++ {
			out.int8s[i] = x
		}
		return out
	case int16:
		out := vector{kind: vInt16, int16s: make([]int16, e.n)}
		for i := 0; i < e.n; i++ {
			out.int16s[i] = x
		}
		return out
	case int32:
		out := vector{kind: vInt32, int32s: make([]int32, e.n)}
		for i := 0; i < e.n; i++ {
			out.int32s[i] = x
		}
		return out
	case uint:
		out := vector{kind: vUint, uints: make([]uint, e.n)}
		for i := 0; i < e.n; i++ {
			out.uints[i] = x
		}
		return out
	case uint16:
		out := vector{kind: vUint16, uint16s: make([]uint16, e.n)}
		for i := 0; i < e.n; i++ {
			out.uint16s[i] = x
		}
		return out
	case uint32:
		out := vector{kind: vUint32, uint32s: make([]uint32, e.n)}
		for i := 0; i < e.n; i++ {
			out.uint32s[i] = x
		}
		return out
	case uint64:
		out := vector{kind: vUint64, uint64s: make([]uint64, e.n)}
		for i := 0; i < e.n; i++ {
			out.uint64s[i] = x
		}
		return out
	}

	out := newAnys(e.n)
	for i := 0; i < e.n; i++ {
		out.setAny(i, value)
	}
	return out
}

func (e *evaluator) integerNode(node *ast.IntegerNode) vector {
	// Mirror the compiler's typed constant emission.
	t := node.Type()
	if t == nil {
		// The compiler emits OpPush with the raw int64-ish constant value.
		return e.allConst(node.Value)
	}
	switch t.Kind() {
	case reflect.Float32:
		return e.allConst(float32(node.Value))
	case reflect.Float64:
		return e.allConst(float64(node.Value))
	case reflect.Int8:
		return e.allConst(int8(node.Value))
	case reflect.Int16:
		return e.allConst(int16(node.Value))
	case reflect.Int32:
		return e.allConst(int32(node.Value))
	case reflect.Int64:
		return e.allConst(int64(node.Value))
	case reflect.Uint:
		return e.allConst(uint(node.Value))
	case reflect.Uint8:
		return e.allConst(uint8(node.Value))
	case reflect.Uint16:
		return e.allConst(uint16(node.Value))
	case reflect.Uint32:
		return e.allConst(uint32(node.Value))
	case reflect.Uint64:
		return e.allConst(uint64(node.Value))
	default:
		return e.allConst(node.Value)
	}
}

func (e *evaluator) identifierNode(node *ast.IdentifierNode) vector {
	if v, ok := e.vars[node.Value]; ok {
		return v
	}
	if node.Value == "$env" {
		panic("batch: $env is not supported")
	}
	if col, ok := e.columns[node.Value]; ok {
		return e.derefVector(node, col)
	}
	// Mirror map env semantics: a missing key yields null.
	return e.allConst(nil)
}

func (e *evaluator) unaryNode(node *ast.UnaryNode) vector {
	v := e.eval(node.Node)
	v = e.derefVector(node.Node, v)

	switch node.Operator {
	case "!":
		out := newBools(e.n)
		e.each(func(i int) {
			out.bools[i] = !v.at(v.index(i)).(bool)
		})
		return out

	case "not":
		out := newBools(e.n)
		e.each(func(i int) {
			out.bools[i] = !v.at(v.index(i)).(bool)
		})
		return out

	case "+":
		return v

	case "-":
		return e.unaryNegate(v)
	}

	panic(fmt.Sprintf("batch: unknown unary operator %q", node.Operator))
}

func (e *evaluator) unaryNegate(v vector) vector {
	switch v.kind {
	case vInt:
		out := newInts(e.n)
		e.each(func(i int) {
			out.ints[i] = -v.ints[v.index(i)]
		})
		return out
	case vInt64:
		out := newInt64s(e.n)
		e.each(func(i int) {
			out.int64s[i] = -v.int64s[v.index(i)]
		})
		return out
	case vFloat64:
		out := newFloat64s(e.n)
		e.each(func(i int) {
			out.float64s[i] = -v.float64s[v.index(i)]
		})
		return out
	case vFloat32:
		out := newFloat32s(e.n)
		e.each(func(i int) {
			out.float32s[i] = -v.float32s[v.index(i)]
		})
		return out
	}

	out := newAnys(e.n)
	e.each(func(i int) {
		out.setAny(i, runtime.Negate(v.at(v.index(i))))
	})
	return out
}

// derefVector mirrors the compiler's derefInNeeded for the node: typed
// interface or pointer values are unwrapped before use.
func (e *evaluator) derefVector(node ast.Node, v vector) vector {
	if node == nil {
		return v
	}
	t := node.Type()
	if t == nil {
		return v
	}
	switch t.Kind() {
	case reflect.Interface, reflect.Ptr:
	default:
		return v
	}
	if v.kind != vAny {
		return v
	}
	out := newAnys(e.n)
	for i := 0; i < e.n; i++ {
		if e.dead[i] {
			continue
		}
		out.setAny(i, deref.Interface(v.anys[v.index(i)]))
	}
	return out
}

func (e *evaluator) sequenceNode(node *ast.SequenceNode) vector {
	var v vector
	for i, n := range node.Nodes {
		v = e.eval(n)
		if i < len(node.Nodes)-1 {
			// Mirrors OpPop; values are simply discarded.
			continue
		}
	}
	return v
}

func (e *evaluator) variableDeclaratorNode(node *ast.VariableDeclaratorNode) vector {
	value := e.eval(node.Value)
	e.vars[node.Name] = value
	defer delete(e.vars, node.Name)
	return e.eval(node.Expr)
}

func (e *evaluator) arrayNode(node *ast.ArrayNode) vector {
	elements := make([]vector, len(node.Nodes))
	for i, n := range node.Nodes {
		elements[i] = e.eval(n)
	}
	out := newAnys(e.n)
	e.each(func(i int) {
		arr := make([]any, len(elements))
		for j, ev := range elements {
			arr[j] = ev.at(ev.index(i))
		}
		out.anys[i] = arr
	})
	return out
}

func (e *evaluator) mapNode(node *ast.MapNode) vector {
	pairs := make([]struct{ key, value vector }, len(node.Pairs))
	for i, p := range node.Pairs {
		pairs[i].key = e.eval(p.(*ast.PairNode).Key)
		pairs[i].value = e.eval(p.(*ast.PairNode).Value)
	}
	out := newAnys(e.n)
	e.each(func(i int) {
		m := make(map[string]any, len(pairs))
		for _, p := range pairs {
			m[p.key.at(p.key.index(i)).(string)] = p.value.at(p.value.index(i))
		}
		out.anys[i] = m
	})
	return out
}
