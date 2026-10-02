package vm

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/builtin"
	"github.com/expr-lang/expr/file"
	"github.com/expr-lang/expr/internal/deref"
	"github.com/expr-lang/expr/vm/runtime"
)

// SessionOption configures an incremental Session.
type SessionOption func(*Session)

// ReusableFunction registers a host function as a deterministic, side-effect
// free function that an incremental Session is allowed to cache.
func ReusableFunction(name string, fn Function) SessionOption {
	return func(session *Session) {
		session.fns[name] = fn
		delete(session.hostFns, name)
	}
}

// FieldChange describes one environment update. A Session keeps its environment
// synchronized and uses the changed path to invalidate dependent caches.
type FieldChange struct {
	Path  []any
	Value any
}

// Session incrementally evaluates one compiled program. Every successful or
// failed evaluation is memoized, including errors produced by reusable host
// functions.
type Session struct {
	program  *Program
	env      map[string]any
	fns      map[string]Function
	hostFns  map[string]Function
	cache    map[string]*sessionCacheEntry
	versions map[string]int
	rev      int

	frames   []contextFrame
	locals   []map[string]localValue
	deps     map[string]struct{}
	depPaths map[string][]any
}

type sessionCacheEntry struct {
	value        any
	err          error
	versions     map[string]int
	dependencies map[string]struct{}
	paths        map[string][]any
}

type contextFrame struct {
	base    []any
	item    any
	acc     any
	pointer bool
}

type localValue struct {
	value    any
	err      error
	versions map[string]int
}

// NewSession creates an incremental evaluation session. Only functions passed
// through ReusableFunction participate in host-function caching; functions
// supplied to expr.Compile are still called normally unless registered here.
func NewSession(program *Program, env map[string]any, options ...SessionOption) (*Session, error) {
	if program == nil {
		return nil, fmt.Errorf("program is nil")
	}
	if program.Node() == nil {
		return nil, fmt.Errorf("program has no AST")
	}
	session := &Session{
		program:  program,
		env:      env,
		fns:      make(map[string]Function),
		hostFns:  make(map[string]Function),
		cache:    make(map[string]*sessionCacheEntry),
		versions: make(map[string]int),
		depPaths: make(map[string][]any),
	}
	for index, fn := range program.functions {
		name := program.debugInfo[fmt.Sprintf("func_%d", index)]
		if name != "" {
			session.hostFns[name] = fn
		}
	}
	for _, option := range options {
		option(session)
	}
	for name, fn := range session.fns {
		if name == "" || fn == nil {
			return nil, fmt.Errorf("reusable function requires a name and implementation")
		}
	}
	return session, nil
}

// Submit applies field changes, updates the session environment, and evaluates
// the program without discarding unaffected cached results.
func (s *Session) Submit(changes ...FieldChange) (any, error) {
	for _, change := range changes {
		if err := s.applyChange(change); err != nil {
			return nil, err
		}
	}
	return s.Run()
}

// SetEnv replaces the complete environment. Every cache depending on an
// environment path is invalidated, while expression constants remain reusable.
func (s *Session) SetEnv(env map[string]any) (any, error) {
	s.env = env
	s.rev++
	s.versions[""] = s.rev
	return s.Run()
}

// Run evaluates the program against the current environment.
func (s *Session) Run() (output any, err error) {
	s.frames = nil
	s.locals = nil
	s.deps = make(map[string]struct{})
	defer func() {
		if recovered := recover(); recovered != nil {
			if recoveredErr, ok := recovered.(error); ok {
				err = recoveredErr
			} else {
				err = fmt.Errorf("%v", recovered)
			}
			var location file.Location
			if node := s.program.Node(); node != nil {
				location = node.Location()
			}
			fileErr := &file.Error{Location: location, Message: err.Error()}
			if cause, ok := recovered.(error); ok {
				fileErr.Wrap(cause)
			}
			err = fileErr.Bind(s.program.Source())
		}
	}()
	return s.eval(s.program.Node())
}

func (s *Session) applyChange(change FieldChange) error {
	if len(change.Path) == 0 {
		return fmt.Errorf("field change path is empty")
	}
	if err := validateNestedChange(s.env, change.Path); err != nil {
		return err
	}
	if err := setNested(s.env, change.Path, change.Value); err != nil {
		return err
	}
	s.bump(change.Path)
	return nil
}

func validateNestedChange(env map[string]any, path []any) error {
	_, err := nestedAt(env, path, false)
	return err
}

func setNested(env map[string]any, path []any, value any) error {
	target, err := nestedAt(env, path[:len(path)-1], true)
	if err != nil {
		return err
	}
	switch collection := target.(type) {
	case map[string]any:
		key, ok := path[len(path)-1].(string)
		if !ok {
			return fmt.Errorf("map key must be a string")
		}
		collection[key] = value
		return nil
	case []any:
		index := runtime.ToInt(path[len(path)-1])
		if index < 0 {
			index += len(collection)
		}
		if index < 0 || index >= len(collection) {
			return fmt.Errorf("index out of range: %v (array length is %v)", index, len(collection))
		}
		collection[index] = value
		return nil
	default:
		return fmt.Errorf("cannot update %T", target)
	}
}

func nestedAt(current any, path []any, createMissing bool) (any, error) {
	if len(path) == 0 {
		return current, nil
	}
	key, ok := path[0].(string)
	var next any
	var exists bool
	switch collection := current.(type) {
	case map[string]any:
		if !ok {
			return nil, fmt.Errorf("top-level field must be a string")
		}
		next, exists = collection[key]
		if !exists && createMissing {
			next = map[string]any{}
			collection[key] = next
		}
	case []any:
		index := runtime.ToInt(path[0])
		if index < 0 {
			index += len(collection)
		}
		if index < 0 || index >= len(collection) {
			return nil, fmt.Errorf("index out of range: %v (array length is %v)", index, len(collection))
		}
		next = collection[index]
	default:
		return nil, fmt.Errorf("cannot navigate %T", current)
	}
	return nestedAt(next, path[1:], createMissing)
}

func (s *Session) bump(path []any) {
	s.rev++
	s.versions[pathKey(path)] = s.rev
}

func pathKey(path []any) string {
	encoded, err := json.Marshal(path)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func (s *Session) dependencyVersion(path []any) int {
	version := 0
	for length := 0; length <= len(path); length++ {
		if current := s.versions[pathKey(path[:length])]; current > version {
			version = current
		}
	}
	return version
}

func (s *Session) eval(node ast.Node) (any, error) {
	value, err := s.memo(node, func() (any, error) {
		return s.evalUncached(node)
	})
	return value, err
}

func (s *Session) memo(node ast.Node, compute func() (any, error)) (any, error) {
	key := s.cacheKey(node)
	if entry, ok := s.cache[key]; ok && s.entryValid(entry) {
		for dependency := range entry.dependencies {
			s.deps[dependency] = struct{}{}
		}
		return entry.value, entry.err
	}
	parentDeps := s.deps
	childDeps := make(map[string]struct{})
	s.deps = childDeps
	value, err := compute()
	s.deps = parentDeps

	versions := make(map[string]int, len(childDeps))
	paths := make(map[string][]any, len(childDeps))
	for dependency := range childDeps {
		path := s.depPaths[dependency]
		paths[dependency] = path
		versions[dependency] = s.dependencyVersion(path)
		s.deps[dependency] = struct{}{}
	}
	if s.isCacheable(node) {
		s.cache[key] = &sessionCacheEntry{
			value:        value,
			err:          err,
			versions:     versions,
			dependencies: childDeps,
			paths:        paths,
		}
	}
	return value, err
}

func (s *Session) isCacheable(node ast.Node) bool {
	if s.containsUncachedCall(node) {
		return false
	}
	if call, ok := node.(*ast.CallNode); ok {
		return s.isReusableCall(call)
	}
	if builtinNode, ok := node.(*ast.BuiltinNode); ok {
		_, reusable := s.fns[builtinNode.Name]
		return reusable
	}
	return true
}

func (s *Session) containsUncachedCall(node ast.Node) bool {
	found := false
	walkAST(node, func(current ast.Node) bool {
		switch n := current.(type) {
		case *ast.CallNode:
			if ident, ok := n.Callee.(*ast.IdentifierNode); ok {
				if _, reusable := s.fns[ident.Value]; reusable {
					return false
				}
				if _, registered := s.hostFns[ident.Value]; registered {
					found = true
					return true
				}
			}
			found = true
			return true
		case *ast.BuiltinNode:
			if _, reusable := s.fns[n.Name]; !reusable {
				found = true
				return true
			}
		}
		return false
	})
	return found
}

func walkAST(node ast.Node, visit func(ast.Node) bool) {
	if visit(node) {
		return
	}
	switch n := node.(type) {
	case *ast.UnaryNode:
		walkAST(n.Node, visit)
	case *ast.BinaryNode:
		walkAST(n.Left, visit)
		walkAST(n.Right, visit)
	case *ast.ChainNode:
		walkAST(n.Node, visit)
	case *ast.MemberNode:
		walkAST(n.Node, visit)
		walkAST(n.Property, visit)
	case *ast.SliceNode:
		walkAST(n.Node, visit)
		if n.From != nil {
			walkAST(n.From, visit)
		}
		if n.To != nil {
			walkAST(n.To, visit)
		}
	case *ast.CallNode:
		walkAST(n.Callee, visit)
		for _, arg := range n.Arguments {
			walkAST(arg, visit)
		}
	case *ast.BuiltinNode:
		for _, arg := range n.Arguments {
			walkAST(arg, visit)
		}
		if n.Map != nil {
			walkAST(n.Map, visit)
		}
	case *ast.PredicateNode:
		walkAST(n.Node, visit)
	case *ast.ConditionalNode:
		walkAST(n.Cond, visit)
		walkAST(n.Exp1, visit)
		walkAST(n.Exp2, visit)
	case *ast.VariableDeclaratorNode:
		walkAST(n.Value, visit)
		walkAST(n.Expr, visit)
	case *ast.SequenceNode:
		for _, child := range n.Nodes {
			walkAST(child, visit)
		}
	case *ast.ArrayNode:
		for _, child := range n.Nodes {
			walkAST(child, visit)
		}
	case *ast.MapNode:
		for _, pair := range n.Pairs {
			walkAST(pair, visit)
		}
	case *ast.PairNode:
		walkAST(n.Key, visit)
		walkAST(n.Value, visit)
	}
}

func (s *Session) isReusableCall(node *ast.CallNode) bool {
	if ident, ok := node.Callee.(*ast.IdentifierNode); ok {
		_, reusable := s.fns[ident.Value]
		return reusable
	}
	if member, ok := node.Callee.(*ast.MemberNode); ok && member.Method {
		if property, ok := member.Property.(*ast.StringNode); ok {
			_, reusable := s.fns[property.Value]
			return reusable
		}
	}
	return false
}

func (s *Session) entryValid(entry *sessionCacheEntry) bool {
	for dependency, version := range entry.versions {
		if s.dependencyVersion(entry.paths[dependency]) != version {
			return false
		}
	}
	return true
}

func (s *Session) cacheKey(node ast.Node) string {
	return fmt.Sprintf(
		"%T:%s|%s",
		node,
		node.String(),
		s.contextKey(),
	)
}

func (s *Session) contextKey() string {
	var builder strings.Builder
	for _, frame := range s.frames {
		builder.WriteString(pathKey(frame.base))
		builder.WriteByte(';')
	}
	return builder.String()
}

func (s *Session) depend(path []any) {
	if path == nil {
		return
	}
	key := pathKey(path)
	s.deps[key] = struct{}{}
	s.depPaths[key] = append([]any(nil), path...)
}

func (s *Session) dependAll(dependencies map[string]int) {
	for dependency := range dependencies {
		s.deps[dependency] = struct{}{}
		if path, ok := s.depPaths[dependency]; ok {
			s.depPaths[dependency] = path
		}
	}
}

func (s *Session) pushFrame(base []any, item any, pointer bool) {
	s.frames = append(s.frames, contextFrame{base: base, item: item, pointer: pointer})
}

func (s *Session) popFrame() {
	s.frames = s.frames[:len(s.frames)-1]
}

func (s *Session) pushLocals() int {
	level := len(s.locals)
	s.locals = append(s.locals, map[string]localValue{})
	return level
}

func (s *Session) popLocals(level int) {
	s.locals = s.locals[:level]
}

func (s *Session) local(name string) (localValue, bool) {
	for i := len(s.locals) - 1; i >= 0; i-- {
		if value, ok := s.locals[i][name]; ok {
			return value, true
		}
	}
	return localValue{}, false
}

func (s *Session) snapshotDeps() map[string]int {
	versions := make(map[string]int, len(s.deps))
	for dependency := range s.deps {
		versions[dependency] = s.dependencyVersion(s.depPaths[dependency])
	}
	return versions
}

func (s *Session) evalSequence(nodes []ast.Node) (any, error) {
	var result any
	for _, node := range nodes {
		value, err := s.eval(node)
		if err != nil {
			return nil, err
		}
		result = value
	}
	return result, nil
}

func (s *Session) evalIdentifier(name string) (any, error) {
	if name == "$env" {
		return s.env, nil
	}
	if value, ok := s.local(name); ok {
		s.dependAll(value.versions)
		return value.value, value.err
	}
	if fn, ok := s.fns[name]; ok {
		return fn, nil
	}
	if fn, ok := s.hostFns[name]; ok {
		return fn, nil
	}
	value, ok := s.env[name]
	if !ok {
		return nil, fmt.Errorf("cannot fetch %v from %T", name, s.env)
	}
	if reflect.ValueOf(value).Kind() != reflect.Func {
		s.depend([]any{name})
	}
	return value, nil
}

func (s *Session) evalPointer(name string) (any, error) {
	for i := len(s.frames) - 1; i >= 0; i-- {
		if s.frames[i].pointer {
			frame := s.frames[i]
			s.depend(frame.base)
			switch name {
			case "":
				return frame.item, nil
			case "index":
				return frame.base[len(frame.base)-1], nil
			case "acc":
				return frame.acc, nil
			default:
				return nil, fmt.Errorf("unknown pointer %q", name)
			}
		}
	}
	return nil, fmt.Errorf("pointer outside of predicate")
}

func (s *Session) evalUnary(node *ast.UnaryNode) (any, error) {
	value, err := s.eval(node.Node)
	if err != nil {
		return nil, err
	}
	switch node.Operator {
	case "!":
		return !value.(bool), nil
	case "not":
		return !value.(bool), nil
	case "-":
		return runtime.Negate(value), nil
	case "+":
		return value, nil
	default:
		return nil, fmt.Errorf("unknown operator (%v)", node.Operator)
	}
}

func (s *Session) evalBinary(node *ast.BinaryNode) (any, error) {
	switch node.Operator {
	case "and", "&&":
		left, err := s.eval(node.Left)
		if err != nil {
			return nil, err
		}
		if !left.(bool) {
			return false, nil
		}
		return s.eval(node.Right)
	case "or", "||":
		left, err := s.eval(node.Left)
		if err != nil {
			return nil, err
		}
		if left.(bool) {
			return true, nil
		}
		return s.eval(node.Right)
	case "??":
		left, err := s.eval(node.Left)
		if err != nil {
			return nil, err
		}
		if !runtime.IsNil(left) {
			return left, nil
		}
		return s.eval(node.Right)
	}

	left, err := s.eval(node.Left)
	if err != nil {
		return nil, err
	}
	right, err := s.eval(node.Right)
	if err != nil {
		return nil, err
	}
	switch node.Operator {
	case "==":
		return runtime.Equal(left, right), nil
	case "!=":
		return !runtime.Equal(left, right), nil
	case "<":
		return runtime.Less(left, right), nil
	case ">":
		return runtime.More(left, right), nil
	case "<=":
		return runtime.LessOrEqual(left, right), nil
	case ">=":
		return runtime.MoreOrEqual(left, right), nil
	case "+":
		return runtime.Add(left, right), nil
	case "-":
		return runtime.Subtract(left, right), nil
	case "*":
		return runtime.Multiply(left, right), nil
	case "/":
		return runtime.Divide(left, right), nil
	case "%":
		return runtime.Modulo(left, right), nil
	case "**", "^":
		return runtime.Exponent(left, right), nil
	case "..":
		return runtime.MakeRange(runtime.ToInt(left), runtime.ToInt(right)), nil
	case "in":
		return runtime.In(left, right), nil
	case "contains":
		return strings.Contains(left.(string), right.(string)), nil
	case "startsWith":
		return strings.HasPrefix(left.(string), right.(string)), nil
	case "endsWith":
		return strings.HasSuffix(left.(string), right.(string)), nil
	case "matches":
		matched, err := regexp.MatchString(right.(string), left.(string))
		if err != nil {
			return nil, err
		}
		return matched, nil
	default:
		return nil, fmt.Errorf("unknown operator (%v)", node.Operator)
	}
}

func (s *Session) evalConditionalUncached(node *ast.ConditionalNode) (any, error) {
	condition, err := s.eval(node.Cond)
	if err != nil {
		return nil, err
	}
	if condition.(bool) {
		return s.eval(node.Exp1)
	}
	return s.eval(node.Exp2)
}

func (s *Session) evalVariable(node *ast.VariableDeclaratorNode) (any, error) {
	value, err := s.eval(node.Value)
	versions := s.snapshotDeps()
	level := s.pushLocals()
	s.locals[level][node.Name] = localValue{
		value:    value,
		err:      err,
		versions: versions,
	}
	s.frames = append(s.frames, contextFrame{
		base:    []any{"__local", node.Name, fmt.Sprintf("%p", node)},
		pointer: false,
	})
	result, resultErr := s.eval(node.Expr)
	s.popFrame()
	s.popLocals(level)
	if err != nil {
		return nil, err
	}
	return result, resultErr
}

func (s *Session) evalChain(node *ast.ChainNode) (any, error) {
	return s.evalChainMember(node.Node)
}

func (s *Session) evalChainMember(node ast.Node) (any, error) {
	switch n := node.(type) {
	case *ast.ChainNode:
		return s.evalChainMember(n.Node)
	case *ast.MemberNode:
		base, err := s.evalChainBase(n.Node)
		if err != nil {
			return nil, err
		}
		if n.Optional && runtime.IsNil(base) {
			return nil, nil
		}
		return s.fetchMember(base, n)
	default:
		return s.eval(node)
	}
}

func (s *Session) evalChainBase(node ast.Node) (any, error) {
	switch n := node.(type) {
	case *ast.ChainNode:
		return s.evalChainMember(n.Node)
	case *ast.MemberNode:
		base, err := s.evalChainBase(n.Node)
		if err != nil {
			return nil, err
		}
		if n.Optional && runtime.IsNil(base) {
			return nil, nil
		}
		return s.fetchMember(base, n)
	default:
		return s.eval(node)
	}
}

func (s *Session) evalMember(node *ast.MemberNode) (any, error) {
	base, err := s.eval(node.Node)
	if err != nil {
		return nil, err
	}
	return s.fetchMember(base, node)
}

func (s *Session) fetchMember(base any, node *ast.MemberNode) (any, error) {
	if node.Method {
		if property, ok := node.Property.(*ast.StringNode); ok {
			return runtime.Fetch(base, property.Value), nil
		}
	}
	basePath := s.nodePath(node.Node)
	property, err := s.evalPropertyKey(node.Property)
	if err != nil {
		return nil, err
	}
	if basePath != nil {
		s.depend(append(append([]any{}, basePath...), property))
	}
	return runtime.Fetch(base, property), nil
}

func (s *Session) evalPropertyKey(node ast.Node) (any, error) {
	switch n := node.(type) {
	case *ast.IntegerNode:
		return n.Value, nil
	case *ast.StringNode:
		return n.Value, nil
	case *ast.BoolNode:
		return n.Value, nil
	default:
		return s.eval(node)
	}
}

func (s *Session) nodePath(node ast.Node) []any {
	switch n := node.(type) {
	case *ast.IdentifierNode:
		if n.Value == "$env" {
			return []any{}
		}
		if _, isLocal := s.local(n.Value); isLocal {
			return nil
		}
		if _, isFunction := s.fns[n.Value]; isFunction {
			return nil
		}
		if _, exists := s.env[n.Value]; exists {
			return []any{n.Value}
		}
		return nil
	case *ast.PointerNode:
		for i := len(s.frames) - 1; i >= 0; i-- {
			if s.frames[i].pointer {
				if n.Name == "" {
					return s.frames[i].base
				}
				return nil
			}
		}
		return nil
	case *ast.MemberNode:
		if n.Method {
			return nil
		}
		base := s.nodePath(n.Node)
		if base == nil {
			return nil
		}
		property, err := s.eval(n.Property)
		if err != nil {
			return nil
		}
		return append(base, property)
	case *ast.ChainNode:
		return s.nodePath(n.Node)
	default:
		return nil
	}
}

func (s *Session) evalSlice(node *ast.SliceNode) (any, error) {
	value, err := s.eval(node.Node)
	if err != nil {
		return nil, err
	}
	from := 0
	if node.From != nil {
		fromValue, err := s.eval(node.From)
		if err != nil {
			return nil, err
		}
		from = runtime.ToInt(fromValue)
	}
	to := runtime.Len(value)
	if node.To != nil {
		toValue, err := s.eval(node.To)
		if err != nil {
			return nil, err
		}
		to = runtime.ToInt(toValue)
	}
	return runtime.Slice(value, from, to), nil
}

func (s *Session) evalCall(node *ast.CallNode) (any, error) {
	if ident, ok := node.Callee.(*ast.IdentifierNode); ok {
		if fn, reusable := s.fns[ident.Value]; reusable {
			return s.callRegistered(ident.Value, fn, node.Arguments)
		}
	}
	if member, ok := node.Callee.(*ast.MemberNode); ok && member.Method {
		if property, ok := member.Property.(*ast.StringNode); ok {
			if fn, reusable := s.fns[property.Value]; reusable {
				base, err := s.eval(member.Node)
				if err != nil {
					return nil, err
				}
				args, err := s.evalArgs(node.Arguments, nil)
				if err != nil {
					return nil, err
				}
				return s.invokeRegistered(property.Value, fn, append([]any{base}, args...))
			}
		}
	}
	return s.callUncached(node)
}

func (s *Session) callRegistered(name string, fn Function, argumentNodes []ast.Node) (any, error) {
	args, err := s.evalArgs(argumentNodes, nil)
	if err != nil {
		return nil, err
	}
	return s.invokeRegistered(name, fn, args)
}

func (s *Session) invokeRegistered(name string, fn Function, args []any) (any, error) {
	return fn(args...)
}

func (s *Session) callUncached(node *ast.CallNode) (any, error) {
	args, err := s.evalArgs(node.Arguments, nil)
	if err != nil {
		return nil, err
	}
	callee, err := s.eval(node.Callee)
	if err != nil {
		return nil, err
	}
	if callee == nil {
		return nil, fmt.Errorf("invalid operation: cannot call nil")
	}
	return callValue(callee, args)
}

func callValue(callee any, args []any) (any, error) {
	if fn, ok := callee.(Function); ok {
		return fn(args...)
	}
	if fast, ok := callee.(func(...any) any); ok {
		return fast(args...), nil
	}
	if safe, ok := callee.(SafeFunction); ok {
		value, _, err := safe(args...)
		return value, err
	}
	return callReflect(callee, args)
}

func callReflect(callee any, args []any) (any, error) {
	fn := reflect.ValueOf(callee)
	if fn.Kind() != reflect.Func {
		return nil, fmt.Errorf("invalid operation: cannot call non-function of type %T", callee)
	}
	fnType := fn.Type()
	if !fnType.IsVariadic() && len(args) != fnType.NumIn() {
		return nil, fmt.Errorf("invalid number of arguments: expected %d, got %d", fnType.NumIn(), len(args))
	}
	if fnType.IsVariadic() && len(args) < fnType.NumIn()-1 {
		return nil, fmt.Errorf("invalid number of arguments: expected at least %d, got %d", fnType.NumIn()-1, len(args))
	}
	in := make([]reflect.Value, len(args))
	for i, arg := range args {
		if arg == nil {
			if fnType.IsVariadic() && i >= fnType.NumIn()-1 {
				in[i] = reflect.Zero(fnType.In(fnType.NumIn() - 1).Elem())
			} else {
				in[i] = reflect.Zero(fnType.In(i))
			}
			continue
		}
		value := reflect.ValueOf(arg)
		expected := value.Type()
		if fnType.IsVariadic() && i >= fnType.NumIn()-1 {
			expected = fnType.In(fnType.NumIn() - 1).Elem()
		} else {
			expected = fnType.In(i)
		}
		if !value.Type().AssignableTo(expected) {
			if !value.Type().ConvertibleTo(expected) {
				return nil, fmt.Errorf("invalid type %T for argument %d", arg, i)
			}
			value = value.Convert(expected)
		}
		in[i] = value
	}
	out := fn.Call(in)
	if len(out) == 2 {
		if errValue := out[1]; errValue.Type() == errorType && !errValue.IsNil() {
			return nil, errValue.Interface().(error)
		}
	}
	return out[0].Interface(), nil
}

type predicateClosure func(any) (any, error)

func (s *Session) evalArgs(nodes []ast.Node, base []any) ([]any, error) {
	args := make([]any, len(nodes))
	for i, node := range nodes {
		if predicate, ok := node.(*ast.PredicateNode); ok {
			predicateNode := predicate.Node
			args[i] = predicateClosure(func(item any) (any, error) {
				s.pushFrame(base, item, true)
				value, err := s.eval(predicateNode)
				s.popFrame()
				return value, err
			})
			continue
		}
		value, err := s.eval(node)
		if err != nil {
			return nil, err
		}
		args[i] = value
	}
	return args, nil
}

func (s *Session) evalBuiltin(node *ast.BuiltinNode) (any, error) {
	if fn, reusable := s.fns[node.Name]; reusable {
		args, err := s.evalArgs(node.Arguments, nil)
		if err != nil {
			return nil, err
		}
		return s.invokeRegistered(node.Name, fn, args)
	}

	if isPredicateBuiltin(node.Name) {
		return s.evalPredicateBuiltin(node)
	}

	args, err := s.evalArgs(node.Arguments, nil)
	if err != nil {
		return nil, err
	}
	id, ok := builtin.Index[node.Name]
	if !ok {
		return nil, fmt.Errorf("unknown builtin %v", node.Name)
	}
	fn := builtin.Builtins[id]
	if fn.Fast != nil {
		return fn.Fast(args[0]), nil
	}
	if fn.Safe != nil {
		value, _, err := fn.Safe(args...)
		return value, err
	}
	if fn.Func != nil {
		return fn.Func(args...)
	}
	return nil, fmt.Errorf("builtin %v has no implementation", node.Name)
}

func isPredicateBuiltin(name string) bool {
	switch name {
	case "all", "none", "any", "one", "filter", "map", "find", "findIndex",
		"findLast", "findLastIndex", "count", "sum", "groupBy", "sortBy", "reduce":
		return true
	default:
		return false
	}
}

func (s *Session) evalPredicateBuiltin(node *ast.BuiltinNode) (any, error) {
	collectionValue, err := s.eval(node.Arguments[0])
	if err != nil {
		return nil, err
	}
	collection, err := toAnySlice(collectionValue)
	if err != nil {
		return nil, err
	}
	basePath := s.nodePath(node.Arguments[0])

	var predicate ast.Node
	hasPredicate := len(node.Arguments) > 1
	if hasPredicate {
		predicate = node.Arguments[1].(*ast.PredicateNode).Node
	}

	frameBaseFor := func(index int) []any {
		if basePath != nil {
			return append(append([]any{}, basePath...), index)
		}
		return []any{"__predicate", fmt.Sprintf("%p", node), index}
	}

	evaluate := func(index int, item any, acc any) (any, error) {
		if !hasPredicate {
			return item, nil
		}
		s.pushFrame(frameBaseFor(index), item, true)
		s.frames[len(s.frames)-1].acc = acc
		value, err := s.eval(predicate)
		s.popFrame()
		return value, err
	}

	switch node.Name {
	case "all":
		for i, item := range collection {
			value, err := evaluate(i, item, nil)
			if err != nil {
				return nil, err
			}
			if !value.(bool) {
				return false, nil
			}
		}
		return true, nil
	case "none":
		for i, item := range collection {
			value, err := evaluate(i, item, nil)
			if err != nil {
				return nil, err
			}
			if value.(bool) {
				return false, nil
			}
		}
		return true, nil
	case "any":
		for i, item := range collection {
			value, err := evaluate(i, item, nil)
			if err != nil {
				return nil, err
			}
			if value.(bool) {
				return true, nil
			}
		}
		return false, nil
	case "one":
		count := 0
		for i, item := range collection {
			value, err := evaluate(i, item, nil)
			if err != nil {
				return nil, err
			}
			if value.(bool) {
				count++
			}
		}
		return count == 1, nil
	case "filter":
		result := make([]any, 0)
		for i, item := range collection {
			value, err := evaluate(i, item, nil)
			if err != nil {
				return nil, err
			}
			if value.(bool) {
				if node.Map != nil {
					mapped, err := s.evaluateNodeWithItem(node.Map, frameBaseFor(i), item)
					if err != nil {
						return nil, err
					}
					result = append(result, mapped)
				} else {
					result = append(result, item)
				}
			}
		}
		return result, nil
	case "map":
		result := make([]any, len(collection))
		for i, item := range collection {
			value, err := evaluate(i, item, nil)
			if err != nil {
				return nil, err
			}
			result[i] = value
		}
		return result, nil
	case "find":
		for i, item := range collection {
			value, err := evaluate(i, item, nil)
			if err != nil {
				return nil, err
			}
			if value.(bool) {
				if node.Map != nil {
					return s.evaluateNodeWithItem(node.Map, frameBaseFor(i), item)
				}
				return item, nil
			}
		}
		if node.Throws {
			return nil, fmt.Errorf("reflect: slice index out of range")
		}
		return nil, nil
	case "findIndex", "findLastIndex":
		if node.Name == "findLastIndex" {
			for i := len(collection) - 1; i >= 0; i-- {
				value, err := evaluate(i, collection[i], nil)
				if err != nil {
					return nil, err
				}
				if value.(bool) {
					return i, nil
				}
			}
			return nil, nil
		}
		for i, item := range collection {
			value, err := evaluate(i, item, nil)
			if err != nil {
				return nil, err
			}
			if value.(bool) {
				return i, nil
			}
		}
		return nil, nil
	case "findLast":
		for i := len(collection) - 1; i >= 0; i-- {
			item := collection[i]
			value, err := evaluate(i, item, nil)
			if err != nil {
				return nil, err
			}
			if value.(bool) {
				if node.Map != nil {
					return s.evaluateNodeWithItem(node.Map, frameBaseFor(i), item)
				}
				return item, nil
			}
		}
		if node.Throws {
			return nil, fmt.Errorf("reflect: slice index out of range")
		}
		return nil, nil
	case "count":
		if !hasPredicate {
			return len(collection), nil
		}
		count := 0
		for i, item := range collection {
			value, err := evaluate(i, item, nil)
			if err != nil {
				return nil, err
			}
			if value.(bool) {
				count++
			}
		}
		return count, nil
	case "sum":
		var total any
		if hasPredicate {
			total = 0
		}
		for i, item := range collection {
			value, err := evaluate(i, item, total)
			if err != nil {
				return nil, err
			}
			if hasPredicate {
				total = runtime.Add(total, value)
			} else {
				if total == nil {
					total = item
				} else {
					total = runtime.Add(total, item)
				}
			}
		}
		if total == nil {
			return 0, nil
		}
		return total, nil
	case "groupBy":
		groups := make(map[any][]any)
		for i, item := range collection {
			key, err := evaluate(i, item, nil)
			if err != nil {
				return nil, err
			}
			groups[key] = append(groups[key], item)
		}
		return groups, nil
	case "sortBy":
		desc := false
		if len(node.Arguments) == 3 {
			order, err := s.eval(node.Arguments[2])
			if err != nil {
				return nil, err
			}
			switch order.(string) {
			case "asc":
				desc = false
			case "desc":
				desc = true
			default:
				return nil, fmt.Errorf("unknown order, use asc or desc")
			}
		}
		sortable := &runtime.SortBy{Array: append([]any(nil), collection...), Values: make([]any, len(collection))}
		for i, item := range collection {
			value, err := evaluate(i, item, nil)
			if err != nil {
				return nil, err
			}
			sortable.Values[i] = value
		}
		sortable.Desc = desc
		sort.Sort(sortable)
		return sortable.Array, nil
	case "reduce":
		var acc any
		start := 0
		if len(node.Arguments) == 3 {
			acc, err = s.eval(node.Arguments[2])
			if err != nil {
				return nil, err
			}
		} else {
			if len(collection) == 0 {
				return nil, fmt.Errorf("reduce of empty array with no initial value")
			}
			acc = collection[0]
			start = 1
		}
		for i := start; i < len(collection); i++ {
			acc, err = evaluate(i, collection[i], acc)
			if err != nil {
				return nil, err
			}
		}
		return acc, nil
	default:
		return nil, fmt.Errorf("unsupported predicate builtin %v", node.Name)
	}
}

func (s *Session) evaluateNodeWithItem(node ast.Node, base []any, item any) (any, error) {
	s.pushFrame(base, item, true)
	value, err := s.eval(node)
	s.popFrame()
	return value, err
}

func toAnySlice(value any) ([]any, error) {
	if value == nil {
		return nil, fmt.Errorf("cannot iterate over %T", value)
	}
	switch collection := value.(type) {
	case []any:
		return collection, nil
	case []int:
		result := make([]any, len(collection))
		for i, value := range collection {
			result[i] = value
		}
		return result, nil
	case []float64:
		result := make([]any, len(collection))
		for i, value := range collection {
			result[i] = value
		}
		return result, nil
	case []string:
		result := make([]any, len(collection))
		for i, value := range collection {
			result[i] = value
		}
		return result, nil
	}
	reflected := reflect.ValueOf(value)
	reflected = deref.Value(reflected)
	if reflected.Kind() != reflect.Array && reflected.Kind() != reflect.Slice {
		return nil, fmt.Errorf("cannot iterate over %T", value)
	}
	result := make([]any, reflected.Len())
	for i := range result {
		result[i] = reflected.Index(i).Interface()
	}
	return result, nil
}

func (s *Session) evalUncached(node ast.Node) (any, error) {
	switch n := node.(type) {
	case *ast.NilNode:
		return nil, nil
	case *ast.IntegerNode:
		return n.Value, nil
	case *ast.FloatNode:
		return n.Value, nil
	case *ast.BoolNode:
		return n.Value, nil
	case *ast.StringNode:
		return n.Value, nil
	case *ast.BytesNode:
		return n.Value, nil
	case *ast.ConstantNode:
		return n.Value, nil
	case *ast.IdentifierNode:
		return s.evalIdentifier(n.Value)
	case *ast.PointerNode:
		return s.evalPointer(n.Name)
	case *ast.UnaryNode:
		return s.evalUnary(n)
	case *ast.BinaryNode:
		return s.evalBinary(n)
	case *ast.ChainNode:
		return s.evalChain(n)
	case *ast.MemberNode:
		return s.evalMember(n)
	case *ast.SliceNode:
		return s.evalSlice(n)
	case *ast.CallNode:
		return s.evalCall(n)
	case *ast.BuiltinNode:
		return s.evalBuiltin(n)
	case *ast.PredicateNode:
		return s.eval(n.Node)
	case *ast.ConditionalNode:
		return s.evalConditionalUncached(n)
	case *ast.VariableDeclaratorNode:
		return s.evalVariable(n)
	case *ast.SequenceNode:
		return s.evalSequence(n.Nodes)
	case *ast.ArrayNode:
		values := make([]any, len(n.Nodes))
		for i, child := range n.Nodes {
			value, err := s.eval(child)
			if err != nil {
				return nil, err
			}
			values[i] = value
		}
		return values, nil
	case *ast.MapNode:
		result := make(map[string]any)
		for _, pairNode := range n.Pairs {
			pair := pairNode.(*ast.PairNode)
			value, err := s.eval(pair)
			if err != nil {
				return nil, err
			}
			result[pair.Key.(*ast.StringNode).Value] = value
		}
		return result, nil
	case *ast.PairNode:
		return s.eval(n.Value)
	default:
		return nil, fmt.Errorf("unsupported AST node %T in incremental session", node)
	}
}
