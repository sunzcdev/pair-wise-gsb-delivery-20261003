package pool

import (
	"context"
	"sync"

	"github.com/sourcegraph/conc"
	"github.com/sourcegraph/conc/panics"
)

// New creates a new Pool.
func New() *Pool {
	return &Pool{}
}

// Pool is a pool of goroutines used to execute tasks concurrently.
//
// Tasks are submitted with Go(). Once all your tasks have been submitted, you
// must call Wait() to clean up any spawned goroutines and propagate any
// panics.
//
// Goroutines are started lazily, so creating a new pool is cheap. There will
// never be more goroutines spawned than there are tasks submitted.
//
// The configuration methods (With*) will panic if they are used after calling
// Go() for the first time.
//
// Pool is efficient, but not zero cost. It should not be used for very short
// tasks. Startup and teardown come with an overhead of around 1µs, and each
// task has an overhead of around 300ns.
type Pool struct {
	handle   conc.WaitGroup
	limiter  limiter
	tasks    chan func()
	initOnce sync.Once
	pc       panics.Catcher

	// outstanding tracks every task that has been accepted but not yet
	// finished, including tasks submitted at runtime by running scopes. The
	// task channel is only closed once outstanding reaches zero, which is
	// what lets a top-level Wait cover dynamically submitted descendants.
	outstanding sync.WaitGroup

	closeOnce sync.Once
}

// Go submits a task to be run in the pool. If all goroutines in the pool
// are busy, a call to Go() will block until the task can be started.
func (p *Pool) Go(f func()) {
	p.init()
	p.submit(f, true)
}

// GoScoped submits a task to the pool and hands it a Scope that can be used to
// submit child tasks for the duration of the call. This makes it possible to
// spawn a group of subtasks from inside a pool task and wait for them before
// continuing without deadlocking a pool whose capacity is smaller than the
// number of concurrently running parent and child tasks.
//
// Child tasks are submitted with the Scope's Go method and share the same
// concurrency limit as their parents: they never run on a separate, unbounded
// pool. When a task is blocked submitting to, or waiting on, its scope and no
// pool worker can take over immediately, the task executes pending work
// inline (and otherwise yields its capacity) so progress is always possible.
//
// The scope is automatically closed when f returns. By that point every child
// submitted to the scope - and transitively every descendant - has completed.
// This also holds if f panics: all children that were accepted before the
// panic are still reaped, and the panic is propagated to the pool's Wait.
// Once the scope has returned, a saved copy of the Scope must not be used and
// any call to its Go method panics.
func (p *Pool) GoScoped(f func(*Scope)) {
	p.Go(func() {
		scope := p.newScope()
		defer scope.finish()
		f(scope)
	})
}

// ScopedGo is an alias for GoScoped.
func (p *Pool) ScopedGo(f func(*Scope)) {
	p.GoScoped(f)
}

// Wait cleans up spawned goroutines, propagating any panics that were
// raised by a tasks.
func (p *Pool) Wait() {
	p.init()

	// After Wait() returns, reset the struct so tasks will be reinitialized on
	// next use. This better matches the behavior of sync.WaitGroup
	defer func() {
		p.initOnce = sync.Once{}
		p.closeOnce = sync.Once{}
		p.pc = panics.Catcher{}
	}()

	// Close the task channel only once every accepted task - and every task
	// it may submit through a scope - has finished. This both drains workers
	// and guarantees no submission can race a closed channel.
	go func() {
		p.outstanding.Wait()
		p.closeOnce.Do(func() { close(p.tasks) })
	}()

	p.handle.Wait()

	// Propagate a panic if we caught one from a task, including tasks that
	// were executed inline while a scope was waiting on its children.
	p.pc.Repanic()
}

// MaxGoroutines returns the maximum size of the pool.
func (p *Pool) MaxGoroutines() int {
	return p.limiter.limit()
}

// WithMaxGoroutines limits the number of goroutines in a pool.
// Defaults to unlimited. Panics if n < 1.
func (p *Pool) WithMaxGoroutines(n int) *Pool {
	p.panicIfInitialized()
	if n < 1 {
		panic("max goroutines in a pool must be greater than zero")
	}
	p.limiter = make(limiter, n)
	return p
}

// init ensures that the pool is initialized before use. This makes the
// zero value of the pool usable.
func (p *Pool) init() {
	p.initOnce.Do(func() {
		p.tasks = make(chan func())
	})
}

// submit enqueues a task for execution. If block is true, the caller is an
// external caller (a call to Pool.Go), so the call blocks until a worker has
// accepted the task and it never executes the task on the calling goroutine.
// If block is false, the caller is a task that is submitting to one of its
// scopes, so when no worker capacity is available it assists by running the
// task inline instead of blocking, which is what prevents nested scopes from
// deadlocking.
func (p *Pool) submit(f func(), block bool) {
	p.outstanding.Add(1)
	task := func() {
		defer p.outstanding.Done()
		f()
	}

	if p.limiter == nil {
		// No limit on the number of goroutines.
		if block {
			// Preserve the blocking semantics of Pool.Go: wait until a worker
			// accepts the task or, if no worker exists yet, start one.
			select {
			case p.tasks <- task:
			default:
				p.handle.Go(func() { p.worker(task) })
			}
			return
		}

		// Unlimited pool, submitted from inside a scope. If a worker accepts
		// the task immediately it runs concurrently; otherwise spawn a new
		// worker so child tasks still run in parallel with their parent.
		select {
		case p.tasks <- task:
		default:
			p.handle.Go(func() { p.worker(task) })
		}
		return
	}

	if block {
		select {
		case p.limiter <- struct{}{}:
			// If we are below our limit, spawn a new worker rather than
			// waiting for one to become available.
			p.handle.Go(func() {
				p.worker(task)
			})
		case p.tasks <- task:
			// A worker is available and has accepted the task.
		}
		return
	}

	select {
	case p.limiter <- struct{}{}:
		// Below the limit: hand the task to a new worker.
		p.handle.Go(func() {
			p.worker(task)
		})
	case p.tasks <- task:
		// A worker accepted the task.
	default:
		// The pool is saturated. Run the task inline rather than blocking,
		// allowing arbitrarily deep scopes to make progress even when the
		// pool has a capacity of one.
		p.run(task)
	}
}

// run executes a single task, catching any panic it raises so that it can be
// propagated by the pool's Wait.
func (p *Pool) run(f func()) {
	p.pc.Try(f)
}

// panicIfInitialized will trigger a panic if a configuration method is called
// after the pool has started any goroutines for the first time. In the case that
// new settings are needed, a new pool should be created.
func (p *Pool) panicIfInitialized() {
	if p.tasks != nil {
		panic("pool can not be reconfigured after calling Go() for the first time")
	}
}

// WithErrors converts the pool to an ErrorPool so the submitted tasks can
// return errors.
func (p *Pool) WithErrors() *ErrorPool {
	p.panicIfInitialized()
	return &ErrorPool{
		pool: p.deref(),
	}
}

// deref is a helper that creates a shallow copy of the pool with the same
// settings. We don't want to just dereference the pointer because that makes
// the copylock lint angry.
func (p *Pool) deref() Pool {
	p.panicIfInitialized()
	return Pool{
		limiter: p.limiter,
	}
}

// WithContext converts the pool to a ContextPool for tasks that should
// run under the same context, such that they each respect shared cancellation.
// For example, WithCancelOnError can be configured on the returned pool to
// signal that all goroutines should be cancelled upon the first error.
func (p *Pool) WithContext(ctx context.Context) *ContextPool {
	p.panicIfInitialized()
	ctx, cancel := context.WithCancel(ctx)
	return &ContextPool{
		errorPool: p.WithErrors().deref(),
		ctx:       ctx,
		cancel:    cancel,
	}
}

func (p *Pool) worker(initialFunc func()) {
	// The only time this matters is if the task panics.
	// This makes it possible to spin up new workers in that case.
	defer p.limiter.release()

	if initialFunc != nil {
		p.run(initialFunc)
	}

	for f := range p.tasks {
		p.run(f)
	}
}

// newScope returns a Scope tied to this pool.
func (p *Pool) newScope() *Scope {
	return &Scope{pool: p}
}

// Scope is a handle to the group of child tasks spawned during a single call to
// Pool.GoScoped. It is passed to the scoped task and is the only way for that
// task to submit children.
//
// A Scope must not be used after the scoped task has returned. The zero value
// is not usable; Scope values are always created by Pool.GoScoped. A Scope
// must not be copied after first use.
type Scope struct {
	pool *Pool

	mu       sync.Mutex
	children sync.WaitGroup
	closed   bool
}

// Go submits a child task to the scope. The child shares the owning pool's
// concurrency limit and may submit its own children with nested scopes.
//
// Go panics if it is called after the scope has finished. Saving the Scope
// and using it once the scoped call has returned is therefore a programming
// error that is always detected.
func (s *Scope) Go(f func()) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		panic("scope: cannot submit a task after the scope has finished")
	}
	s.children.Add(1)
	s.mu.Unlock()

	s.pool.submit(func() {
		defer s.children.Done()
		f()
	}, false)
}

// GoScoped submits a child task to the scope and hands it a nested Scope,
// allowing arbitrarily deep scope nesting. It is the scoped counterpart to Go
// and follows the same capacity-sharing and deadlock-avoidance rules.
func (s *Scope) GoScoped(f func(*Scope)) {
	s.Go(func() {
		nested := s.pool.newScope()
		defer nested.finish()
		f(nested)
	})
}

// Wait blocks until every task submitted to the scope - including descendant
// tasks - has completed. While blocked it assists with running pending pool
// work (or, when there is no pending work, yields its pool capacity) so that
// the scope's children can always make progress, even in a capacity-one pool.
//
// It is safe to call Wait multiple times, and it is called automatically when
// the scoped task returns, so an explicit call is only needed to wait at a
// specific point.
func (s *Scope) Wait() {
	done := make(chan struct{})
	go func() {
		s.children.Wait()
		close(done)
	}()

	for {
		select {
		case <-done:
			return
		case f := <-s.pool.tasks:
			// While waiting for our children, execute other pool work instead
			// of sitting idle. This both assists throughput and frees the
			// logical slot our caller occupies when the pool is saturated.
			if f != nil {
				s.pool.run(f)
			}
		}
	}
}

// finish reaps any outstanding children and then marks the scope as closed so
// that any later use of a saved Scope is rejected.
func (s *Scope) finish() {
	s.Wait()

	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

type limiter chan struct{}

func (l limiter) limit() int {
	return cap(l)
}

func (l limiter) release() {
	if l != nil {
		<-l
	}
}
