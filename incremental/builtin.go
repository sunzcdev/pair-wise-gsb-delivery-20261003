package incremental

import (
	"fmt"
	"reflect"

	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/builtin"
	"github.com/expr-lang/expr/vm/runtime"
)

var builtinsByName = func() map[string]*builtin.Function {
	m := make(map[string]*builtin.Function, len(builtin.Builtins))
	for _, b := range builtin.Builtins {
		m[b.Name] = b
	}
	return m
}()

// errFindNotFound mirrors the error the VM produces for find(...)[0] on an
// empty result (see the optimizer's filter_first pass).
var errFindNotFound = fmt.Errorf("reflect: slice index out of range")

func (s *Session) runBuiltin(node ast.Node, n *ast.BuiltinNode) (out, error) {
	switch n.Name {
	case "all", "none", "any", "one", "filter", "map", "count", "sum",
		"find", "findIndex", "findLast", "findLastIndex":
		return s.runPredicateBuiltin(node, n)
	case "groupBy", "sortBy", "reduce":
		return out{}, fmt.Errorf("incremental: builtin %q is not supported", n.Name)
	}

	b, ok := builtinsByName[n.Name]
	if !ok {
		return out{}, fmt.Errorf("incremental: unknown builtin %q", n.Name)
	}
	if n.Name == "now" {
		s.vol++
	}
	args := make([]any, len(n.Arguments))
	for i, arg := range n.Arguments {
		a, err := s.eval(node, arg)
		if err != nil {
			return out{}, err
		}
		args[i] = a.val
	}
	if b.Fast != nil && len(args) == 1 {
		return out{val: b.Fast(args[0])}, nil
	}
	if b.Func == nil {
		return out{}, fmt.Errorf("incremental: builtin %q is not supported", n.Name)
	}
	v, err := b.Func(args...)
	if err != nil {
		return out{}, err
	}
	return out{val: v}, nil
}

// runPredicateBuiltin evaluates builtins that take a predicate (filter,
// map, sum, ...). The predicate body is evaluated fresh for every element
// with # bound to the element; environment reads inside the predicate are
// recorded with concrete element paths (e.g. items[2].Price) and
// attributed to the builtin node itself.
func (s *Session) runPredicateBuiltin(node ast.Node, n *ast.BuiltinNode) (out, error) {
	arr, err := s.eval(node, n.Arguments[0])
	if err != nil {
		return out{}, err
	}
	var pred *ast.PredicateNode
	if len(n.Arguments) > 1 {
		p, ok := n.Arguments[1].(*ast.PredicateNode)
		if !ok {
			return out{}, fmt.Errorf("incremental: builtin %q expects a predicate", n.Name)
		}
		pred = p
	}

	array := reflect.ValueOf(arr.val)
	for array.Kind() == reflect.Pointer || array.Kind() == reflect.Interface {
		array = array.Elem()
	}
	length := 0
	if array.IsValid() {
		switch array.Kind() {
		case reflect.Slice, reflect.Array:
			length = array.Len()
		default:
			return out{}, fmt.Errorf("builtin %v: cannot get %v from %T", n.Name, "array", arr.val)
		}
	}

	// Dependencies recorded while evaluating predicate bodies are
	// attributed to this node (or forwarded to the enclosing predicate
	// when nested).
	topLevel := s.fresh == 0
	var pdeps []dep
	if topLevel {
		prev := s.sink
		s.sink = &pdeps
		defer func() { s.sink = prev }()
	}
	s.fresh++
	defer func() { s.fresh-- }()
	if topLevel {
		defer func() {
			st := s.stateOf(node)
			st.deps = append(st.deps, pdeps...)
		}()
	}

	elemAt := func(i int) out {
		o := out{val: array.Index(i).Interface()}
		if arr.ok {
			o.path = appendPath(arr.path, i)
			o.ok = true
		}
		return o
	}
	evalPred := func(i int) (out, error) {
		elem := elemAt(i)
		s.pushScope(map[string]out{
			"#":      elem,
			"#index": {val: i},
		})
		defer s.popScope()
		if pred == nil {
			if elem.ok {
				// The element itself is consumed (sum, count
				// without predicate).
				s.registerDep(node, elem.path)
			}
			return elem, nil
		}
		return s.eval(node, pred)
	}
	evalMap := func(i int) (out, error) {
		if n.Map == nil {
			return elemAt(i), nil
		}
		elem := elemAt(i)
		s.pushScope(map[string]out{
			"#":      elem,
			"#index": {val: i},
		})
		defer s.popScope()
		return s.eval(node, n.Map)
	}

	switch n.Name {
	case "all":
		for i := 0; i < length; i++ {
			r, err := evalPred(i)
			if err != nil {
				return out{}, err
			}
			if !runtime.ToBool(r.val) {
				return out{val: false}, nil
			}
		}
		return out{val: true}, nil

	case "none":
		for i := 0; i < length; i++ {
			r, err := evalPred(i)
			if err != nil {
				return out{}, err
			}
			if runtime.ToBool(r.val) {
				return out{val: false}, nil
			}
		}
		return out{val: true}, nil

	case "any":
		for i := 0; i < length; i++ {
			r, err := evalPred(i)
			if err != nil {
				return out{}, err
			}
			if runtime.ToBool(r.val) {
				return out{val: true}, nil
			}
		}
		return out{val: false}, nil

	case "one":
		count := 0
		for i := 0; i < length; i++ {
			r, err := evalPred(i)
			if err != nil {
				return out{}, err
			}
			if runtime.ToBool(r.val) {
				count++
			}
		}
		return out{val: count == 1}, nil

	case "filter":
		result := make([]any, 0)
		for i := 0; i < length; i++ {
			r, err := evalPred(i)
			if err != nil {
				return out{}, err
			}
			if runtime.ToBool(r.val) {
				m, err := evalMap(i)
				if err != nil {
					return out{}, err
				}
				s.trackElement(node, m)
				result = append(result, m.val)
			}
		}
		return out{val: result}, nil

	case "map":
		result := make([]any, 0, length)
		for i := 0; i < length; i++ {
			r, err := evalPred(i)
			if err != nil {
				return out{}, err
			}
			s.trackElement(node, r)
			result = append(result, r.val)
		}
		return out{val: result}, nil

	case "count":
		count := 0
		for i := 0; i < length; i++ {
			r, err := evalPred(i)
			if err != nil {
				return out{}, err
			}
			if runtime.ToBool(r.val) {
				count++
			}
		}
		return out{val: count}, nil

	case "sum":
		acc := any(0)
		for i := 0; i < length; i++ {
			r, err := evalPred(i)
			if err != nil {
				return out{}, err
			}
			acc = runtime.Add(acc, r.val)
		}
		return out{val: acc}, nil

	case "find":
		for i := 0; i < length; i++ {
			r, err := evalPred(i)
			if err != nil {
				return out{}, err
			}
			if runtime.ToBool(r.val) {
				m, err := evalMap(i)
				if err != nil {
					return out{}, err
				}
				s.trackElement(node, m)
				return m, nil
			}
		}
		if n.Throws {
			return out{}, errFindNotFound
		}
		return out{val: nil}, nil

	case "findIndex":
		for i := 0; i < length; i++ {
			r, err := evalPred(i)
			if err != nil {
				return out{}, err
			}
			if runtime.ToBool(r.val) {
				return out{val: i}, nil
			}
		}
		return out{val: nil}, nil

	case "findLast":
		for i := length - 1; i >= 0; i-- {
			r, err := evalPred(i)
			if err != nil {
				return out{}, err
			}
			if runtime.ToBool(r.val) {
				m, err := evalMap(i)
				if err != nil {
					return out{}, err
				}
				s.trackElement(node, m)
				return m, nil
			}
		}
		if n.Throws {
			return out{}, errFindNotFound
		}
		return out{val: nil}, nil

	case "findLastIndex":
		for i := length - 1; i >= 0; i-- {
			r, err := evalPred(i)
			if err != nil {
				return out{}, err
			}
			if runtime.ToBool(r.val) {
				return out{val: i}, nil
			}
		}
		return out{val: nil}, nil
	}

	return out{}, fmt.Errorf("incremental: builtin %q is not supported", n.Name)
}

// trackElement records a deep dependency when an env-rooted value becomes
// part of the result (a kept filter element, a mapped value, a found
// element). Downstream nodes may read inside it in ways the session cannot
// see, so any change below its path must invalidate this node.
func (s *Session) trackElement(node ast.Node, o out) {
	if o.ok {
		s.registerDep(node, o.path)
	}
}
