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
// tasks. Startup and teardown come with an overhead of around 1us, and each
// task has an overhead of around 300ns.
type Pool struct {
	handle conc.WaitGroup

	// limiter bounds the number of worker goroutines. A nil limiter means the
	// pool may spawn as many workers as there are tasks in flight.
	limiter limiter

	// m protects the scheduler state below as well as every Scope.
	m sync.Mutex
	// cond is used by idle workers and by scopes waiting for their children.
	cond *sync.Cond

	// tasks is the shared FIFO queue of tasks waiting to be executed. Both
	// worker goroutines and the goroutines of scopes waiting for their
	// children pull tasks out of it, so a parent never blocks a slot while
	// waiting for its own children.
	tasks []func()

	// workers is the number of live worker goroutines and idle is the number
	// of those workers currently parked and waiting for work.
	workers int
	idle    int

	// closing is set by Wait once all tasks have finished; parked workers use
	// it to know they should exit.
	closing bool

	// started is set on the first accepted task and never reset, so pool
	// configuration methods keep panicking after Wait has been called.
	started bool

	// active tracks every task that has been accepted by the pool but has not
	// finished yet. This includes tasks queued in tasks as well as tasks
	// currently running on a worker or inline on a waiting scope's goroutine.
	active sync.WaitGroup

	// pc catches panics from top-level tasks and all of their descendants.
	// Scopes share the catcher of the top-level scope they belong to.
	pc panics.Catcher

	initOnce sync.Once
}

// Go submits a task to be run in the pool. If all goroutines in the pool
// are busy, a call to Go() will block until the task can be started.
func (p *Pool) Go(f func()) {
	p.init()
	p.enqueue(nil, nil, f, true)
}

// Wait cleans up spawned goroutines, propagating any panics that were
// raised by a tasks.
func (p *Pool) Wait() {
	p.init()

	// After Wait() returns, reset the scheduler so the pool can be reused.
	// This better matches the behavior of sync.WaitGroup. The started flag is
	// intentionally not reset: configuration is forbidden after first use.
	defer func() {
		p.initOnce = sync.Once{}
		p.tasks = nil
		p.workers = 0
		p.idle = 0
		p.closing = false
		p.pc = panics.Catcher{}
	}()

	// Wait for every accepted task (and all of its descendants) to finish.
	p.active.Wait()

	// All tasks are done, so the task queue is empty and it is safe to shut
	// the workers down.
	p.m.Lock()
	p.closing = true
	p.cond.Broadcast()
	for p.workers > 0 {
		p.cond.Wait()
	}
	p.m.Unlock()
	p.handle.Wait()

	// Propagate the first panic caught from any task or descendant.
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
		p.cond = sync.NewCond(&p.m)
	})
}

// GoScope is the scoped concurrency entry point. It runs f as a task in the
// pool and hands f a submitter that can only be used for the duration of the
// scope.
//
// All work submitted through the scope shares the pool's concurrency limit.
// When the scope waits for its children (either explicitly with Wait or
// implicitly when f returns), the current goroutine cooperatively executes
// queued tasks instead of occupying a pool slot, so a pool with a limit of 1
// can safely nest work without deadlocking.
//
// When f returns, the scope does not end until every task submitted to it
// (including tasks spawned by nested scopes) has finished. If f panics, the
// already-accepted children are still collected before the panic is
// propagated. A submitter saved and used after the scope ends panics.
func (p *Pool) GoScope(f func(s *Scope)) {
	p.init()
	s := &Scope{pool: p, pc: &p.pc}
	// Even if f panics, finish joins every child accepted before the panic
	// before the task (and its panic) completes.
	p.enqueue(nil, s, func() {
		defer s.finish()
		f(s)
	}, true)
}

// GoInScope is an alias for GoScope.
func (p *Pool) GoInScope(f func(s *Scope)) {
	p.GoScope(f)
}

// panicIfInitialized triggers a panic if a configuration method is called
// after the pool has accepted a task for the first time. In the case that
// new settings are needed, a new pool should be created.
func (p *Pool) panicIfInitialized() {
	p.m.Lock()
	started := p.started
	p.m.Unlock()
	if started {
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

// enqueue accepts a task into the pool. The chain is the set of live scopes
// the task belongs to (the submitter and all of its ancestors); an empty
// chain means the task is a top-level pool task. When s is non-nil, panics
// from the task are routed to the shared catcher of s's top-level scope.
//
// When block is true, the call does not return until the task has been
// admitted for execution (a new worker was spawned or an existing worker, or
// a cooperating scope, picked the task up). This preserves Go's backpressure.
//
// Callers must not hold p.m.
func (p *Pool) enqueue(chain []*Scope, s *Scope, f func(), block bool) {
	p.active.Add(1)

	// admitted is closed once some executor (a worker or a scope that is
	// helping) starts running the task.
	var admitted chan struct{}
	if block {
		admitted = make(chan struct{})
	}

	task := func() {
		if admitted != nil {
			close(admitted)
		}
		p.runTask(chain, s, f)
	}

	p.m.Lock()
	p.started = true
	for _, sc := range chain {
		sc.pending++
	}
	p.tasks = append(p.tasks, task)
	spawned := p.spawnWorkerLocked()
	p.cond.Broadcast()
	p.m.Unlock()

	if block && !spawned {
		// The pool is saturated (or an idle worker will pick the task up),
		// so wait until the task is admitted. This mirrors the backpressure
		// of the channel-based implementation.
		<-admitted
	}
}

// runTask executes a single task, routing any panic to the shared panic
// catcher of the top-level scope it belongs to, then marks the task and every
// scope in its chain as finished.
func (p *Pool) runTask(chain []*Scope, s *Scope, f func()) {
	pc := &p.pc
	if s != nil {
		pc = s.pc
	}
	pc.Try(f)

	p.m.Lock()
	for _, sc := range chain {
		sc.pending--
	}
	// Wake up scopes waiting for children and workers parked on the queue.
	p.cond.Broadcast()
	p.m.Unlock()

	p.active.Done()
}

// spawnWorkerLocked spawns a worker goroutine if more parallelism is both
// permitted and useful. The caller must hold p.m.
func (p *Pool) spawnWorkerLocked() bool {
	if p.closing {
		return false
	}
	if p.idle > 0 {
		// An existing worker will pick the task up.
		return false
	}
	if p.limiter != nil && p.workers >= p.limiter.limit() {
		// The pool already has as many workers as it is allowed.
		return false
	}
	p.workers++
	p.handle.Go(p.worker)
	return true
}

// worker pulls and executes tasks until the pool shuts down. Worker
// goroutines never escape a task panic; panics are caught in runTask.
func (p *Pool) worker() {
	for {
		f := p.nextWorkerTask()
		if f == nil {
			return
		}
		f()
	}
}

// nextWorkerTask blocks until a task is available, returning nil once the pool
// is shutting down and the queue is drained.
func (p *Pool) nextWorkerTask() func() {
	p.m.Lock()
	defer p.m.Unlock()
	for {
		if len(p.tasks) > 0 {
			f := p.tasks[0]
			p.tasks = p.tasks[1:]
			return f
		}
		if p.closing {
			p.workers--
			p.cond.Broadcast()
			return nil
		}
		p.idle++
		p.cond.Wait()
		p.idle--
	}
}

type limiter chan struct{}

func (l limiter) limit() int {
	return cap(l)
}
