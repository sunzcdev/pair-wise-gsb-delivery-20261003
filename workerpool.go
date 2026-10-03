package workerpool

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gammazero/deque"
)

var ErrStopped = errors.New("submitting work to stopped workerpool")

// New creates and starts a pool of worker goroutines.
//
// The maxWorkers parameter specifies the maximum number of workers that can
// execute tasks concurrently. When there are no incoming tasks, workers are
// gradually stopped until there are no remaining workers.
func New(maxWorkers int, options ...Option) *WorkerPool {
	// There must be at least one worker.
	if maxWorkers < 1 {
		maxWorkers = 1
	}

	cfg := config{
		idleTimeout: DefaultIdleTimeout,
	}
	for _, opt := range options {
		opt(&cfg)
	}

	pool := &WorkerPool{
		maxWorkers:   maxWorkers,
		taskQueue:    make(chan func()),
		keyTaskQueue: make(chan keyedTask),
		workerQueue:  make(chan job),
		keyDone:      make(chan any),
		keyExit:      make(chan struct{}),
		drainAbort:   make(chan struct{}),
		stopSignal:   make(chan struct{}),
		stoppedChan:  make(chan struct{}),
		keyStates:    map[any]*keyState{},
	}

	// Start the task dispatcher.
	go pool.dispatch(cfg.idleTimeout)

	return pool
}

// WorkerPool is a collection of goroutines, where the number of concurrent
// goroutines processing requests does not exceed the specified maximum.
type WorkerPool struct {
	maxWorkers   int
	taskQueue    chan func()
	keyTaskQueue chan keyedTask
	workerQueue  chan job
	keyDone      chan any
	keyExit      chan struct{}
	drainAbort   chan struct{}
	stoppedChan  chan struct{}
	stopSignal   chan struct{}
	waitingQueue deque.Deque[queuedTask]
	keyStates    map[any]*keyState
	busyKeys     int
	stopLock     sync.Mutex
	stopOnce     sync.Once
	stopped      bool
	waiting      atomic.Int32
	wait         bool
}

// queuedTask is a unit of work that is runnable now: either a plain task or
// the head task of a key whose previous task has completed. A running key has
// at most one queuedTask in the waiting queue; the rest of its tasks remain in
// the key's own deque and never occupy a worker slot.
type queuedTask struct {
	task  func()
	key   any
	keyed bool
}

// job is a unit of work handed to a worker. A keyed job reports its key's
// completion to the dispatcher before the worker accepts another job, so the
// dispatcher never dispatches a successor to a still-running key.
type job struct {
	task  func()
	key   any
	keyed bool
}

// keyedTask pairs a task with its ordering key on the keyTaskQueue channel.
type keyedTask struct {
	key  any
	task func()
}

// keyState holds the per-key FIFO task deque and the running flag. All fields
// are touched only by the dispatcher.
type keyState struct {
	tasks   deque.Deque[func()]
	running bool
	// scheduled is true while a queuedTask for this key is in the waiting
	// queue waiting for a worker. Together with running it guards against
	// enqueuing two runnable entries for the same key.
	scheduled bool
}

// Size returns the maximum number of concurrent workers.
func (p *WorkerPool) Size() int {
	return p.maxWorkers
}

// Stop stops the worker pool and waits for only currently running tasks to
// complete. Pending tasks that are not currently running are abandoned. Tasks
// must not be submitted to the worker pool after calling stop.
//
// Since creating the worker pool starts at least one goroutine, for the
// dispatcher, Stop() or StopWait() should be called when the worker pool is no
// longer needed.
func (p *WorkerPool) Stop() {
	p.stop(false)
}

// StopWait stops the worker pool and waits for all queued tasks tasks to
// complete. No additional tasks may be submitted, but all pending tasks are
// executed by workers before this function returns.
func (p *WorkerPool) StopWait() {
	p.stop(true)
}

// Stopped returns true if this worker pool has been stopped.
func (p *WorkerPool) Stopped() bool {
	p.stopLock.Lock()
	defer p.stopLock.Unlock()
	return p.stopped
}

// Do enqueues a function for a worker to execute. Returns ErrStopped if the
// worker pool is stopped.
//
// Any external values needed by the task function must be captured in a
// closure. Any return values should be returned over a channel that is
// captured in the task function closure.
//
// Do will not block regardless of the number of tasks submitted. Each task
// is immediately given to an available worker or to a newly started worker. If
// there are no available workers, and the maximum number of workers are
// already created, then the task is put onto a waiting queue.
//
// When there are tasks on the waiting queue, any additional new tasks are put
// on the waiting queue. Tasks are removed from the waiting queue as workers
// become available.
//
// As long as no new tasks arrive, one available worker is shutdown each time
// period until there are no more idle workers. Since the time to start new
// goroutines is not significant, there is no need to retain idle workers
// indefinitely.
func (p *WorkerPool) Do(task func()) (err error) {
	if task == nil {
		return
	}
	defer func() {
		if recover() != nil {
			err = ErrStopped
		}
	}()
	p.taskQueue <- task
	return
}

// Submit enqueues the given function without blocking. Panics if the worker
// pool is stopped.
func (p *WorkerPool) Submit(task func()) {
	if err := p.Do(task); err != nil {
		panic(err)
	}
}

// SubmitKeyed enqueues a task associated with the given comparable key,
// without blocking. Panics if the worker pool is stopped.
//
// Tasks sharing a key execute strictly one at a time, in the order they were
// submitted. Tasks with different keys may execute concurrently and share the
// same workers as tasks submitted via Submit. A long backlog for one key never
// blocks tasks for other keys: only the head task of each key is eligible for
// a worker, and the remaining tasks of a busy key stay in a per-key queue
// instead of occupying workers or the waiting queue.
//
// Key state is removed when the key has no running or queued task, so the
// internal key table does not grow without bound. Stop and StopWait also
// release all key state.
func SubmitKeyed[K comparable](p *WorkerPool, key K, task func()) {
	if task == nil {
		return
	}
	defer func() {
		if recover() != nil {
			panic(ErrStopped)
		}
	}()
	p.keyTaskQueue <- keyedTask{key: key, task: task}
}

// SubmitWait enqueues the given function, without blocking, and waits for it
// to be executed. Panics if the worker pool is stopped.
func (p *WorkerPool) SubmitWait(task func()) {
	if task == nil {
		return
	}
	doneChan := make(chan struct{})
	p.Submit(func() {
		task()
		close(doneChan)
	})
	<-doneChan
}

// WaitingQueueSize returns the count of tasks waiting for an available
// worker. This includes plain queued tasks and keyed tasks waiting behind
// an in-flight task of the same key.
func (p *WorkerPool) WaitingQueueSize() int {
	return int(p.waiting.Load())
}

// Pause causes all workers to wait on the given Context, thereby making them
// unavailable to run tasks. Pause returns when all workers are waiting. Tasks
// can continue to be queued to the workerpool, but are not executed until the
// Context is canceled or times out.
//
// Calling Pause when the worker pool is already paused causes Pause to wait
// until all previous pauses are canceled. This allows a goroutine to take
// control of pausing and unpausing the pool as soon as other goroutines have
// unpaused it.
//
// When the workerpool is stopped, workers are unpaused and queued tasks are
// executed during StopWait.
func (p *WorkerPool) Pause(ctx context.Context) {
	p.stopLock.Lock()
	defer p.stopLock.Unlock()
	if p.stopped {
		return
	}
	ready := new(sync.WaitGroup)
	ready.Add(p.maxWorkers)
	for i := 0; i < p.maxWorkers; i++ {
		p.taskQueue <- func() {
			ready.Done()
			select {
			case <-ctx.Done():
			case <-p.stopSignal:
			}
		}
	}
	// Wait for workers to all be paused
	ready.Wait()
}

// dispatch sends the next queued task to an available worker.
func (p *WorkerPool) dispatch(idleTimeout time.Duration) {
	defer close(p.stoppedChan)
	var idle bool
	var idleTO <-chan time.Time
	var workerCount int
	var wg sync.WaitGroup

	var timeout *time.Timer
	if idleTimeout > 0 {
		timeout = time.NewTimer(idleTimeout)
		defer timeout.Stop()
		idleTO = timeout.C
	}

	startWorker := func(j job) {
		wg.Add(1)
		go worker(j, p.workerQueue, p.keyDone, p.drainAbort, p.keyExit, &wg)
		workerCount++
	}

Loop:
	for {
		// As long as tasks are in the waiting queue, incoming tasks are put
		// into the waiting queue and tasks to run are taken from the waiting
		// queue. Once the waiting queue is empty, then go back to submitting
		// incoming tasks directly to available workers.
		if p.waitingQueue.Len() != 0 {
			if !p.processWaitingQueue() {
				break Loop
			}
			continue
		}

		select {
		case task, ok := <-p.taskQueue:
			if !ok {
				break Loop
			}
			// Got a task to do.
			nextJob := job{task: task}
			select {
			case p.workerQueue <- nextJob:
			default:
				// Create a new worker, if not at max.
				if workerCount < p.maxWorkers {
					startWorker(nextJob)
				} else {
					// Enqueue task to be executed by next available worker.
					p.waitingQueue.PushBack(queuedTask{task: task})
					p.waiting.Add(1)
				}
			}
			idle = false
		case kt, ok := <-p.keyTaskQueue:
			if !ok {
				break Loop
			}
			// A key that already has a running task cannot run this one
			// directly, even if a worker is ready; otherwise two same-key
			// tasks could execute at once.
			st := p.keyState(kt.key)
			if !st.running && !st.scheduled {
				nextJob := job{task: kt.task, key: kt.key, keyed: true}
				select {
				case p.workerQueue <- nextJob:
					p.markKeyRunning(st)
				default:
					if workerCount < p.maxWorkers {
						startWorker(nextJob)
						p.markKeyRunning(st)
					} else {
						p.acceptKeyed(kt.key, kt.task)
					}
				}
			} else {
				p.acceptKeyed(kt.key, kt.task)
			}
			idle = false
		case key := <-p.keyDone:
			p.finishKey(key)
		case <-idleTO:
			// Timed out waiting for work to arrive. Kill a ready worker if
			// pool has been idle for a whole timeout.
			if idle && workerCount > 0 {
				if p.killIdleWorker() {
					workerCount--
				}
			}
			idle = true
			timeout.Reset(idleTimeout)
		}
	}

	// If instructed to wait, then run tasks that are already queued.
	if p.wait {
		p.runQueuedTasks()
	} else {
		// Stop abandons queued work: discard per-key backlogs and release
		// in-flight keyed workers from reporting key completion.
		p.abandonQueuedTasks()
		close(p.drainAbort)
	}
	p.clearKeyState()

	// Retire every worker. reapWorker also services keyDone reports, since a
	// worker finishing a keyed task reports before it is ready for the kill
	// job and listening on only one side would deadlock.
	for workerCount > 0 {
		workerCount = p.reapWorker(workerCount)
	}
	// Release any worker still blocked reporting key completion.
	close(p.keyExit)
	wg.Wait()
}

// worker executes jobs and stops when it receives an empty job. After a
// keyed job it reports the key's completion before accepting another job,
// which is what makes same-key successors strictly serial.
func worker(first job, workerQueue chan job, keyDone chan any, drainAbort, keyExit <-chan struct{}, wg *sync.WaitGroup) {
	j := first
	for j.task != nil {
		j.task()
		if j.keyed {
			// Report key completion before accepting another job; this is
			// what makes same-key tasks strictly serial. During StopWait
			// the report is always delivered, since drainAbort is open;
			// Stop closes drainAbort, and keyExit is closed after all
			// workers have been retired, to abandon any straggler report.
			select {
			case keyDone <- j.key:
			case <-drainAbort:
			case <-keyExit:
			}
		}
		j = <-workerQueue
	}
	wg.Done()
}

// stop tells the dispatcher to exit, and whether or not to complete queued
// tasks.
func (p *WorkerPool) stop(wait bool) {
	p.stopOnce.Do(func() {
		// Signal that workerpool is stopping, to unpause any paused workers.
		close(p.stopSignal)
		// Acquire stopLock to wait for any pause in progress to complete. All
		// in-progress pauses will complete because the stopSignal unpauses the
		// workers.
		p.stopLock.Lock()
		// The stopped flag prevents any additional paused workers. This makes
		// it safe to close the taskQueue.
		p.stopped = true
		p.stopLock.Unlock()
		p.wait = wait
		// Close task queue and wait for currently running tasks to finish.
		close(p.taskQueue)
		close(p.keyTaskQueue)
	})
	<-p.stoppedChan
}

// processWaitingQueue puts new tasks onto the waiting queue, and removes
// tasks from the waiting queue as workers become available. Returns false if
// worker pool is stopped.
func (p *WorkerPool) processWaitingQueue() bool {
	head := p.waitingQueue.Front()
	select {
	case task, ok := <-p.taskQueue:
		if !ok {
			return false
		}
		p.waitingQueue.PushBack(queuedTask{task: task})
		p.waiting.Add(1)
	case kt, ok := <-p.keyTaskQueue:
		if !ok {
			return false
		}
		p.acceptKeyed(kt.key, kt.task)
	case p.workerQueue <- p.frontJob(head):
		p.commitFront()
	case key := <-p.keyDone:
		p.finishKey(key)
	}
	return true
}

// keyState returns the dispatcher-owned state for key, creating it on first
// use.
func (p *WorkerPool) keyState(key any) *keyState {
	st, ok := p.keyStates[key]
	if !ok {
		st = &keyState{}
		p.keyStates[key] = st
	}
	return st
}

// markKeyRunning records that key's head task has been handed to a worker.
func (p *WorkerPool) markKeyRunning(st *keyState) {
	st.running = true
	st.scheduled = false
	p.busyKeys++
}

// acceptKeyed adds a keyed task to the pool: if the key is idle it becomes
// a runnable waiting-queue entry; otherwise it queues behind the key's
// in-flight work without consuming an entry or a worker slot.
func (p *WorkerPool) acceptKeyed(key any, task func()) {
	st := p.keyState(key)
	st.tasks.PushBack(task)
	if !st.running && !st.scheduled {
		st.scheduled = true
		p.waitingQueue.PushBack(queuedTask{key: key, keyed: true})
	}
	p.waiting.Add(1)
}

// commitFront hands the front waiting-queue entry to a worker. A keyed entry
// pops the key's head task and marks the key running; the key's next task
// stays parked until the running task reports completion.
func (p *WorkerPool) commitFront() {
	head := p.waitingQueue.PopFront()
	if !head.keyed {
		p.waiting.Add(-1)
		return
	}
	st := p.keyStates[head.key]
	st.tasks.PopFront()
	st.running = true
	st.scheduled = false
	p.busyKeys++
	p.waiting.Add(-1)
}

// finishKey records that the running task for key completed and, if more
// tasks for that key remain, makes the next one runnable. When no tasks
// remain the key state is deleted, so the key table cannot grow without
// bound.
func (p *WorkerPool) finishKey(key any) {
	st, ok := p.keyStates[key]
	if !ok {
		return
	}
	st.running = false
	p.busyKeys--
	if st.tasks.Len() == 0 {
		delete(p.keyStates, key)
		return
	}
	st.scheduled = true
	p.waitingQueue.PushBack(queuedTask{key: key, keyed: true})
}

// reapWorker blocks until either a worker can be retired by a kill job (it
// returns the decremented worker count) or a keyed task reports completion
// (serviced here, count returned unchanged). The report must be listened on
// even while reaping or shutting down: a keyed worker reports before it is
// ready for the kill job, so listening on only the kill channel could
// deadlock. After the key table is cleared (shutdown), reports are
// discarded.
func (p *WorkerPool) reapWorker(workerCount int) int {
	select {
	case p.workerQueue <- job{}:
		return workerCount - 1
	case key := <-p.keyDone:
		if p.keyStates != nil {
			p.finishKey(key)
		}
		return workerCount
	}
}

// killIdleWorker retires one worker immediately if one is ready.
func (p *WorkerPool) killIdleWorker() bool {
	select {
	case p.workerQueue <- job{}:
		return true
	default:
		return false
	}
}

// runQueuedTasks drains every queued task during StopWait. Completing a
// keyed task may enqueue that key's next task, so keyDone reports are
// serviced until every key has drained as well.
func (p *WorkerPool) runQueuedTasks() {
	for p.waitingQueue.Len() != 0 || p.busyKeys != 0 {
		var workerQueue chan job
		var nextJob job
		if p.waitingQueue.Len() != 0 {
			workerQueue = p.workerQueue
			nextJob = p.frontJob(p.waitingQueue.Front())
		}
		select {
		case workerQueue <- nextJob:
			p.commitFront()
		case key := <-p.keyDone:
			p.finishKey(key)
		}
	}
}

// abandonQueuedTasks drops keyed task backlogs during Stop; plain tasks are
// left in the waiting queue, matching the existing abandonment behavior.
func (p *WorkerPool) abandonQueuedTasks() {
	var n int
	for _, st := range p.keyStates {
		n += st.tasks.Len()
	}
	if n > 0 {
		p.waiting.Add(int32(-n))
	}
}

// clearKeyState releases the dispatcher-owned key table and releases workers
// blocked reporting key completion during shutdown.
func (p *WorkerPool) clearKeyState() {
	p.keyStates = nil
	p.busyKeys = 0
}

// frontJob returns the job carried by the front waiting-queue entry.
func (p *WorkerPool) frontJob(head queuedTask) job {
	if head.keyed {
		return job{task: p.keyStates[head.key].tasks.Front(), key: head.key, keyed: true}
	}
	return job{task: head.task}
}
