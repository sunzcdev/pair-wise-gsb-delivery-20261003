package pool

import "github.com/sourcegraph/conc/panics"

// Submitter submits child work to the scope it was created by.
//
// A Submitter is only valid for the duration of the scope that produced it.
// Calling any of its methods after the scope has finished panics, so a
// submitter cannot be saved and reused outside of its scope.
type Submitter interface {
	// Go submits a child task to the scope. The task shares the pool's
	// concurrency limit with every other task in the pool.
	//
	// Go never blocks on pool capacity: the task is queued and run by a
	// pool worker or, while the scope is waiting for its children, inline on
	// the scope's own goroutine.
	Go(f func())

	// GoScope submits a child task that gets its own nested scope. All work
	// submitted through the returned scope is treated as a descendant of the
	// scope that created it and is joined by Wait just like a child task.
	GoScope(f func(s *Scope))
}

var _ Submitter = (*Scope)(nil)

// Scope is a handle given to a task started with Pool.GoScope. It is used to
// submit child work that is bound to the lifetime of the task.
//
// A scope ends only after the task function returns and every task submitted
// to the scope (directly or through nested scopes) has completed. If the
// task function panics, the accepted children are still collected before the
// panic propagates to the pool.
//
// A Scope must not be used after the task it belongs to has finished; doing
// so panics.
type Scope struct {
	pool *Pool

	// parent is the scope this scope was submitted to, or nil for scopes
	// created directly by the pool.
	parent *Scope

	// pc catches panics from every task in the top-level scope's subtree.
	// Scopes share the catcher of the top-level scope they belong to.
	pc *panics.Catcher

	// pending is the number of accepted descendant tasks that have not
	// finished. It is guarded by pool.m.
	pending int

	// sealed is set once the scope has ended, after which submissions and
	// Waits are rejected. It is guarded by pool.m.
	sealed bool
}

// Go submits a child task to the scope. See the Submitter interface.
func (s *Scope) Go(f func()) {
	s.checkUsable()
	s.pool.enqueue(scopeChain(s), s, f, false)
}

// GoScope submits a nested scope as a child task. See the Submitter
// interface.
func (s *Scope) GoScope(f func(child *Scope)) {
	s.checkUsable()

	child := &Scope{
		pool:   s.pool,
		parent: s,
		pc:     s.pc,
	}

	// The wrapper task counts against the parent's chain (the new scope is
	// a child of s), but not against child itself: the scope's own task is
	// not one of its children.
	s.pool.enqueue(scopeChain(s), s, func() {
		// Join every accepted child even if f panics.
		defer child.finish()
		f(child)
	}, false)
}

// GoInScope is an alias for GoScope.
func (s *Scope) GoInScope(f func(child *Scope)) {
	s.GoScope(f)
}

// Wait blocks until every task submitted to the scope (including tasks
// spawned by nested scopes) has finished.
//
// While waiting, the current goroutine cooperatively executes queued pool
// tasks instead of holding a pool slot idle. This is what allows nested work
// to complete even when the pool's concurrency limit is 1.
//
// If any task in the scope's subtree panicked, Wait re-panics with the first
// such panic after all accepted children have been collected.
//
// Wait may be called multiple times while the scope is active.
func (s *Scope) Wait() {
	s.checkUsable()
	s.join()
	s.pc.Repanic()
}

// finish is the final join performed when a scope task returns. It behaves
// like Wait but seals the scope afterwards so saved submitters can no longer
// be used.
func (s *Scope) finish() {
	s.join()
	s.seal()
	s.pc.Repanic()
}

// join runs or waits until all pending descendants have completed. It must
// be called without pool.m held; checkUsable is the caller's responsibility.
func (s *Scope) join() {
	p := s.pool
	p.m.Lock()
	for s.pending > 0 {
		if len(p.tasks) > 0 {
			// Help drain the queue rather than parking on a slot we are
			// currently occupying. The task may belong to another scope; that
			// is intentional and required for liveness.
			f := p.tasks[0]
			p.tasks = p.tasks[1:]
			p.m.Unlock()
			f()
			p.m.Lock()
			continue
		}
		p.cond.Wait()
	}
	p.m.Unlock()
}

func (s *Scope) seal() {
	p := s.pool
	p.m.Lock()
	s.sealed = true
	// Wake any other goroutines blocked in join so they observe the seal.
	p.cond.Broadcast()
	p.m.Unlock()
}

func (s *Scope) checkUsable() {
	p := s.pool
	p.m.Lock()
	sealed := s.sealed
	p.m.Unlock()
	if sealed {
		panic("scope submitter used after its scope has ended")
	}
}

// scopeChain returns the scope and all of its ancestors, nearest first.
// Submitted tasks count against every live scope in the chain, so waiting on
// an ancestor also waits for the task.
func scopeChain(s *Scope) []*Scope {
	var chain []*Scope
	for cur := s; cur != nil; cur = cur.parent {
		chain = append(chain, cur)
	}
	return chain
}
