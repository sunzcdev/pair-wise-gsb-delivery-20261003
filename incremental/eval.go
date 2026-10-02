package incremental

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/vm/runtime"
)

// out is the result of evaluating a node. If the value is env-rooted (read
// directly from the environment, possibly through a chain of member and
// index accesses), ok is true and path holds its exact location, including
// concrete array indices.
type out struct {
	val  any
	path Path
	ok   bool
}

// errChainNil is a sentinel panic used to short-circuit optional chains
// (foo?.bar.baz) when a nil is encountered mid-chain.
var errChainNil = fmt.Errorf("expr: nil chain")

// run evaluates a node without consulting or updating the cache. Children
// are evaluated through eval, so cached subtrees are reused unless the
// session is in fresh mode (inside predicate bodies).
func (s *Session) run(node ast.Node) (out, error) {
	switch n := node.(type) {
	case *ast.NilNode:
		return out{val: nil}, nil

	case *ast.IntegerNode:
		return out{val: n.Value}, nil

	case *ast.FloatNode:
		return out{val: n.Value}, nil

	case *ast.StringNode:
		return out{val: n.Value}, nil

	case *ast.BoolNode:
		return out{val: n.Value}, nil

	case *ast.BytesNode:
		return out{val: n.Value}, nil

	case *ast.ConstantNode:
		return out{val: n.Value}, nil

	case *ast.IdentifierNode:
		return s.runIdentifier(node, n)

	case *ast.PointerNode:
		if b, ok := s.lookup("#" + n.Name); ok {
			if b.ok {
				// Direct use of an env-rooted element (e.g. `# > 1`).
				// Suppressed statically when the pointer is only the
				// base of a member access (e.g. `#.Qty`).
				s.registerDep(node, b.path)
			}
			return b, nil
		}
		return out{}, fmt.Errorf("cannot find pointer accessor #%s", n.Name)

	case *ast.MemberNode:
		return s.runMember(node, n)

	case *ast.SliceNode:
		return s.runSlice(node, n)

	case *ast.ChainNode:
		return s.runChain(node, n)

	case *ast.UnaryNode:
		return s.runUnary(node, n)

	case *ast.BinaryNode:
		return s.runBinary(node, n)

	case *ast.ConditionalNode:
		cond, err := s.eval(node, n.Cond)
		if err != nil {
			return out{}, err
		}
		if runtime.ToBool(cond.val) {
			return s.eval(node, n.Exp1)
		}
		return s.eval(node, n.Exp2)

	case *ast.CallNode:
		return s.runCall(node, n)

	case *ast.BuiltinNode:
		return s.runBuiltin(node, n)

	case *ast.PredicateNode:
		return s.eval(node, n.Node)

	case *ast.VariableDeclaratorNode:
		return s.runVariableDeclarator(node, n)

	case *ast.SequenceNode:
		var o out
		for _, child := range n.Nodes {
			var err error
			o, err = s.eval(node, child)
			if err != nil {
				return out{}, err
			}
		}
		return o, nil

	case *ast.ArrayNode:
		array := make([]any, len(n.Nodes))
		for i, child := range n.Nodes {
			o, err := s.eval(node, child)
			if err != nil {
				return out{}, err
			}
			array[i] = o.val
		}
		return out{val: array}, nil

	case *ast.MapNode:
		m := make(map[string]any, len(n.Pairs))
		for _, pairNode := range n.Pairs {
			pair, ok := pairNode.(*ast.PairNode)
			if !ok {
				return out{}, fmt.Errorf("unexpected node %T in map literal", pairNode)
			}
			k, err := s.eval(node, pair.Key)
			if err != nil {
				return out{}, err
			}
			key, ok := k.val.(string)
			if !ok {
				return out{}, fmt.Errorf("map key must be a string, got %T", k.val)
			}
			v, err := s.eval(node, pair.Value)
			if err != nil {
				return out{}, err
			}
			m[key] = v.val
		}
		return out{val: m}, nil

	default:
		return out{}, fmt.Errorf("incremental: unsupported node %T", node)
	}
}

func (s *Session) runIdentifier(node ast.Node, n *ast.IdentifierNode) (out, error) {
	if b, ok := s.lookup(n.Value); ok {
		return b, nil
	}
	if n.Value == "$env" {
		return out{val: s.env, path: Path{}, ok: true}, nil
	}
	v := runtime.Fetch(s.env, n.Value)
	p := Path{n.Value}
	s.registerDep(node, p)
	return out{val: v, path: p, ok: true}, nil
}

func (s *Session) runMember(node ast.Node, n *ast.MemberNode) (out, error) {
	base, err := s.eval(node, n.Node)
	if err != nil {
		return out{}, err
	}
	var prop any
	switch p := n.Property.(type) {
	case *ast.StringNode:
		prop = p.Value
	case *ast.IntegerNode:
		prop = p.Value
	default:
		po, err := s.eval(node, n.Property)
		if err != nil {
			return out{}, err
		}
		prop = po.val
	}
	if n.Optional && runtime.IsNil(base.val) {
		panic(errChainNil)
	}
	v := runtime.Fetch(base.val, prop)
	o := out{val: v}
	if base.ok {
		switch seg := prop.(type) {
		case string:
			o.path = appendPath(base.path, seg)
			o.ok = true
		case int:
			// Normalize negative indices so the dependency path
			// matches the concrete element, e.g. items[-1].
			if idx := normalizeIndex(base.val, seg); idx != seg {
				seg = idx
			}
			o.path = appendPath(base.path, seg)
			o.ok = true
		}
	}
	if o.ok {
		s.registerDep(node, o.path)
	}
	return o, nil
}

func normalizeIndex(array any, index int) int {
	if index >= 0 {
		return index
	}
	v := reflect.ValueOf(array)
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Array, reflect.Slice, reflect.String:
		return v.Len() + index
	}
	return index
}

func (s *Session) runSlice(node ast.Node, n *ast.SliceNode) (out, error) {
	base, err := s.eval(node, n.Node)
	if err != nil {
		return out{}, err
	}
	from := any(0)
	if n.From != nil {
		fo, err := s.eval(node, n.From)
		if err != nil {
			return out{}, err
		}
		from = fo.val
	}
	to := any(runtime.Len(base.val))
	if n.To != nil {
		to2, err := s.eval(node, n.To)
		if err != nil {
			return out{}, err
		}
		to = to2.val
	}
	return out{val: runtime.Slice(base.val, from, to)}, nil
}

func (s *Session) runChain(node ast.Node, n *ast.ChainNode) (o out, err error) {
	defer func() {
		if r := recover(); r != nil {
			if r == errChainNil {
				o, err = out{val: nil}, nil
				return
			}
			panic(r)
		}
	}()
	return s.eval(node, n.Node)
}

func (s *Session) runUnary(node ast.Node, n *ast.UnaryNode) (out, error) {
	o, err := s.eval(node, n.Node)
	if err != nil {
		return out{}, err
	}
	switch n.Operator {
	case "-":
		return out{val: runtime.Negate(o.val)}, nil
	case "+":
		return o, nil
	case "!", "not":
		return out{val: !runtime.ToBool(o.val)}, nil
	}
	return out{}, fmt.Errorf("unknown unary operator (%v)", n.Operator)
}

func (s *Session) runBinary(node ast.Node, n *ast.BinaryNode) (out, error) {
	switch n.Operator {
	case "and", "&&":
		l, err := s.eval(node, n.Left)
		if err != nil {
			return out{}, err
		}
		if !runtime.ToBool(l.val) {
			return l, nil
		}
		return s.eval(node, n.Right)

	case "or", "||":
		l, err := s.eval(node, n.Left)
		if err != nil {
			return out{}, err
		}
		if runtime.ToBool(l.val) {
			return l, nil
		}
		return s.eval(node, n.Right)

	case "??":
		l, err := s.eval(node, n.Left)
		if err != nil {
			return out{}, err
		}
		if runtime.IsNil(l.val) {
			return s.eval(node, n.Right)
		}
		return l, nil
	}

	l, err := s.eval(node, n.Left)
	if err != nil {
		return out{}, err
	}
	r, err := s.eval(node, n.Right)
	if err != nil {
		return out{}, err
	}
	a, b := l.val, r.val

	switch n.Operator {
	case "==":
		return out{val: runtime.Equal(a, b)}, nil
	case "!=":
		return out{val: !runtime.Equal(a, b)}, nil
	case "<":
		return out{val: runtime.Less(a, b)}, nil
	case "<=":
		return out{val: runtime.LessOrEqual(a, b)}, nil
	case ">":
		return out{val: runtime.More(a, b)}, nil
	case ">=":
		return out{val: runtime.MoreOrEqual(a, b)}, nil
	case "+":
		return out{val: runtime.Add(a, b)}, nil
	case "-":
		return out{val: runtime.Subtract(a, b)}, nil
	case "*":
		return out{val: runtime.Multiply(a, b)}, nil
	case "/":
		return out{val: runtime.Divide(a, b)}, nil
	case "%":
		return out{val: runtime.Modulo(a, b)}, nil
	case "**":
		return out{val: runtime.Exponent(a, b)}, nil
	case "..":
		return out{val: runtime.MakeRange(runtime.ToInt(a), runtime.ToInt(b))}, nil
	case "in":
		return out{val: runtime.In(a, b)}, nil
	case "not in":
		return out{val: !runtime.In(a, b)}, nil
	case "matches":
		return out{val: matches(a, b)}, nil
	case "contains":
		if runtime.IsNil(a) || runtime.IsNil(b) {
			return out{val: false}, nil
		}
		return out{val: strings.Contains(a.(string), b.(string))}, nil
	case "startsWith":
		if runtime.IsNil(a) || runtime.IsNil(b) {
			return out{val: false}, nil
		}
		return out{val: strings.HasPrefix(a.(string), b.(string))}, nil
	case "endsWith":
		if runtime.IsNil(a) || runtime.IsNil(b) {
			return out{val: false}, nil
		}
		return out{val: strings.HasSuffix(a.(string), b.(string))}, nil
	}
	return out{}, fmt.Errorf("unknown binary operator (%v)", n.Operator)
}

func matches(a, b any) bool {
	if runtime.IsNil(a) || runtime.IsNil(b) {
		return false
	}
	if re, ok := b.(*regexp.Regexp); ok {
		if str, ok := a.(string); ok {
			return re.MatchString(str)
		}
		return re.Match(a.([]byte))
	}
	var match bool
	var err error
	if str, ok := a.(string); ok {
		match, err = regexp.MatchString(b.(string), str)
	} else {
		match, err = regexp.Match(b.(string), a.([]byte))
	}
	if err != nil {
		panic(err)
	}
	return match
}

func (s *Session) runCall(node ast.Node, n *ast.CallNode) (out, error) {
	name := calleeName(n.Callee)
	if _, ok := s.pure[name]; !ok {
		// Not registered as pure: the call is never cached and the
		// node stays volatile, so every Eval re-executes it.
		s.vol++
	}
	fn, err := s.eval(node, n.Callee)
	if err != nil {
		return out{}, err
	}
	args := make([]any, len(n.Arguments))
	for i, arg := range n.Arguments {
		a, err := s.eval(node, arg)
		if err != nil {
			return out{}, err
		}
		args[i] = a.val
	}
	return out{val: callFunc(fn.val, args)}, nil
}

func calleeName(callee ast.Node) string {
	switch c := callee.(type) {
	case *ast.IdentifierNode:
		return c.Value
	case *ast.MemberNode:
		if prop, ok := c.Property.(*ast.StringNode); ok {
			return prop.Value
		}
	}
	return ""
}

// callFunc invokes a host function value via reflection, mirroring the
// calling convention of the VM (variadic support, nil arguments, error
// return as a panic).
func callFunc(fn any, args []any) any {
	if fn == nil {
		panic("invalid operation: cannot call nil")
	}
	v := reflect.ValueOf(fn)
	if v.Kind() != reflect.Func {
		panic(fmt.Sprintf("invalid operation: cannot call non-function of type %T", fn))
	}
	t := v.Type()
	isVariadic := t.IsVariadic()
	numIn := t.NumIn()
	size := len(args)
	if isVariadic {
		if size < numIn-1 {
			panic(fmt.Sprintf("invalid number of arguments: expected at least %d, got %d", numIn-1, size))
		}
	} else if size != numIn {
		panic(fmt.Sprintf("invalid number of arguments: expected %d, got %d", numIn, size))
	}
	in := make([]reflect.Value, size)
	for i := 0; i < size; i++ {
		if args[i] == nil {
			var inType reflect.Type
			if isVariadic && i >= numIn-1 {
				inType = t.In(numIn - 1).Elem()
			} else {
				inType = t.In(i)
			}
			in[i] = reflect.Zero(inType)
		} else {
			in[i] = reflect.ValueOf(args[i])
		}
	}
	res := v.Call(in)
	errorType := reflect.TypeOf((*error)(nil)).Elem()
	if len(res) == 2 && res[1].Type() == errorType && !res[1].IsNil() {
		panic(res[1].Interface().(error))
	}
	return res[0].Interface()
}

func (s *Session) runVariableDeclarator(node ast.Node, n *ast.VariableDeclaratorNode) (out, error) {
	v, err := s.eval(node, n.Value)
	if err != nil {
		return out{}, err
	}
	s.pushScope(map[string]out{n.Name: v})
	defer s.popScope()
	return s.eval(node, n.Expr)
}

func (s *Session) lookup(name string) (out, bool) {
	for i := len(s.scopes) - 1; i >= 0; i-- {
		if v, ok := s.scopes[i][name]; ok {
			return v, true
		}
	}
	return out{}, false
}

func (s *Session) pushScope(vars map[string]out) {
	s.scopes = append(s.scopes, vars)
}

func (s *Session) popScope() {
	s.scopes = s.scopes[:len(s.scopes)-1]
}
