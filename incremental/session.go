// Package incremental evaluates a compiled expr program repeatedly against
// an evolving environment. A Session caches intermediate results per AST
// node and tracks the exact environment paths each node read (including
// array indices actually accessed and only the taken branch of
// conditionals). When the caller submits a field change, only the nodes
// affected by that change are re-evaluated; everything else is reused.
//
// Only host functions explicitly registered with Pure participate in
// caching. Calls to any other function are never cached: the call node
// (and everything derived from it) is re-evaluated on every Eval, exactly
// like in a full run.
package incremental

import (
	"fmt"

	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/vm"
)

// Option configures a Session.
type Option func(*Session)

// Pure registers host function names (as they appear in the expression)
// that are pure: same arguments always produce the same result and no side
// effects. Only registered functions participate in caching. Functions
// that are not registered keep normal call semantics and are executed on
// every evaluation that reaches them.
func Pure(names ...string) Option {
	return func(s *Session) {
		for _, name := range names {
			s.pure[name] = struct{}{}
		}
	}
}

// Session is an incremental evaluation session for a single compiled
// program and a single environment. It is not safe for concurrent use.
type Session struct {
	env    any
	root   ast.Node
	pure   map[string]struct{}
	states map[ast.Node]*nodeState

	// suppressOwn marks nodes whose own env read is only used as the
	// base of a member access; the member node registers the extended
	// path instead (e.g. in items[1].Qty the read of "items" itself is
	// not a dependency). shallowOwn marks nodes whose own env read is
	// only traversed (the array argument of predicate builtins), so
	// only wholesale replacement invalidates it. Both are static
	// properties of the AST, computed once.
	suppressOwn map[ast.Node]bool
	shallowOwn  map[ast.Node]bool

	// Transient evaluation state.
	scopes []map[string]out // let-variables and predicate pointers
	fresh  int              // >0 inside predicate bodies (cache bypass)
	sink   *[]dep           // dep sink while fresh > 0
	vol    int              // volatile-call counter, monotonic
}

// nodeState is the memoized evaluation record of a single AST node.
type nodeState struct {
	out      out
	err      error
	clean    bool
	volatile bool
	parents  map[ast.Node]struct{}
	children map[ast.Node]struct{}
	deps     []dep // env paths read directly by this node
}

// NewSession creates an incremental evaluation session for a compiled
// program. The environment must be the same value that would be passed to
// vm.Run. Use Set to submit field changes between Eval calls.
func NewSession(program *vm.Program, env any, opts ...Option) (*Session, error) {
	if program == nil {
		return nil, fmt.Errorf("program is nil")
	}
	s := &Session{
		env:         env,
		root:        program.Node(),
		pure:        make(map[string]struct{}),
		states:      make(map[ast.Node]*nodeState),
		suppressOwn: make(map[ast.Node]bool),
		shallowOwn:  make(map[ast.Node]bool),
	}
	for _, opt := range opts {
		opt(s)
	}
	ast.Walk(&s.root, &staticAnalysis{session: s})
	return s, nil
}

// staticAnalysis precomputes suppressOwn and shallowOwn; both are static
// properties of the AST.
type staticAnalysis struct {
	session *Session
}

func (w *staticAnalysis) Visit(node *ast.Node) {
	switch n := (*node).(type) {
	case *ast.MemberNode:
		w.session.suppressOwn[n.Node] = true
	case *ast.BuiltinNode:
		switch n.Name {
		case "all", "none", "any", "one", "filter", "map", "count", "sum",
			"find", "findIndex", "findLast", "findLastIndex":
			w.session.shallowOwn[n.Arguments[0]] = true
		}
	}
}

// Eval evaluates the expression, recomputing only the parts affected by
// changes submitted via Set or Invalidate since the previous Eval. The
// first call performs a full evaluation.
func (s *Session) Eval() (any, error) {
	o, err := s.eval(nil, s.root)
	if err != nil {
		return nil, err
	}
	return o.val, nil
}

// Set assigns value to the environment location identified by path (for
// example "user.Name" or "items[2].Price") and invalidates every cached
// result that depends on it. The next Eval recomputes only the affected
// parts of the expression.
func (s *Session) Set(path string, value any) error {
	p, err := ParsePath(path)
	if err != nil {
		return err
	}
	if err := setEnv(s.env, p, value); err != nil {
		return err
	}
	s.invalidate(p)
	return nil
}

// Invalidate marks every cached result depending on path as stale without
// modifying the environment. Use it when the environment was mutated
// directly instead of via Set.
func (s *Session) Invalidate(path string) error {
	p, err := ParsePath(path)
	if err != nil {
		return err
	}
	s.invalidate(p)
	return nil
}

// invalidate dirties all clean nodes that read a path affected by the
// change, propagating dirtiness up to the root.
func (s *Session) invalidate(change Path) {
	for node, st := range s.states {
		if !st.clean {
			continue
		}
		for _, d := range st.deps {
			if d.affectedBy(change) {
				s.markDirty(node)
				break
			}
		}
	}
}

func (s *Session) markDirty(node ast.Node) {
	st := s.states[node]
	if st == nil || !st.clean {
		return
	}
	st.clean = false
	for p := range st.parents {
		s.markDirty(p)
	}
}

func (s *Session) stateOf(node ast.Node) *nodeState {
	st := s.states[node]
	if st == nil {
		st = &nodeState{
			parents:  make(map[ast.Node]struct{}),
			children: make(map[ast.Node]struct{}),
		}
		s.states[node] = st
	}
	return st
}

// unlink detaches child from parent and prunes the child's subtree if it
// is no longer referenced by any active node. Pruned nodes forget their
// dependencies: when a conditional switches branches, the dependencies of
// the abandoned branch leave the graph.
func (s *Session) unlink(parent, child ast.Node) {
	cst := s.states[child]
	if cst == nil {
		return
	}
	delete(cst.parents, parent)
	if len(cst.parents) > 0 {
		return
	}
	s.prune(child, cst)
}

func (s *Session) prune(node ast.Node, st *nodeState) {
	st.clean = false
	st.deps = nil
	children := st.children
	st.children = make(map[ast.Node]struct{})
	for c := range children {
		s.unlink(node, c)
	}
}

// eval evaluates node as a child of parent (nil for the root), reusing the
// cached result when the node is clean.
func (s *Session) eval(parent, node ast.Node) (o out, err error) {
	if s.fresh > 0 {
		// Inside predicate bodies: no caching, dependencies are
		// attributed to the enclosing builtin node via the sink.
		return s.run(node)
	}
	st := s.stateOf(node)
	if parent != nil {
		pst := s.stateOf(parent)
		if _, ok := pst.children[node]; !ok {
			pst.children[node] = struct{}{}
			st.parents[parent] = struct{}{}
		}
	}
	if st.clean {
		return st.out, st.err
	}
	st.deps = st.deps[:0]
	old := st.children
	st.children = make(map[ast.Node]struct{})
	volatileBefore := s.vol
	defer func() {
		if r := recover(); r != nil {
			if r == errChainNil {
				panic(r)
			}
			o = out{}
			err = asError(r)
		}
		for c := range old {
			if _, ok := st.children[c]; !ok {
				s.unlink(node, c)
			}
		}
		st.out, st.err = o, err
		st.volatile = s.vol > volatileBefore
		st.clean = !st.volatile
	}()
	o, err = s.run(node)
	return o, err
}

// registerDep records an environment read. Inside predicates the read is
// attributed to the enclosing builtin node.
func (s *Session) registerDep(node ast.Node, p Path) {
	if s.suppressOwn[node] {
		return
	}
	d := dep{path: p, deep: !s.shallowOwn[node]}
	if s.fresh > 0 {
		*s.sink = append(*s.sink, d)
		return
	}
	st := s.stateOf(node)
	st.deps = append(st.deps, d)
}

func asError(r any) error {
	if err, ok := r.(error); ok {
		return err
	}
	return fmt.Errorf("%v", r)
}
