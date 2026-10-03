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

// Stats is a consistent snapshot of the worker pool's task and worker
// counters. All counters are captured under a single lock, so the values
// never describe a state the pool could not have been in.
//
// The counters always satisfy the following conservation relations:
//
//	Received == Queued + Running + Completed + Dropped
//	Started  == Running + Completed
//
// Received counts only tasks accepted by the pool; tasks submitted after the
// pool is stopped are rejected and not counted. Tasks used internally by
// Pause are not counted.
type Stats struct {
	// Received is the total number of tasks accepted by the pool.
	Received int
	// Started is the total number of tasks handed to a worker for execution.
	Started int
	// Completed is the total number of tasks that finished executing. A task
	// that panics is recovered and counted as completed.
	Completed int
	// Dropped is the total number of queued tasks discarded by Stop.
	Dropped int
	// Queued is the number of tasks currently waiting for an available
	// worker.
	Queued int
	// Running is the number of tasks currently executing.
	Running int
	// Workers is the number of worker goroutines currently alive. Workers
	// are created on demand and retired when idle; this is independent of
	// the task counters above.
	Workers int
}

// taskRequest is a task submitted to the dispatcher. Internal control tasks,
// such as those used by Pause, are excluded from the pool's Stats.
type taskRequest struct {
	fn       func()
	internal bool
}

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
		maxWorkers:  maxWorkers,
		taskQueue:   make(chan taskRequest),
		workerQueue: make(chan func()),
		stopSignal:  make(chan struct{}),
		stoppedChan: make(chan struct{}),
	}

	// Start the task dispatcher.
	go pool.dispatch(cfg.idleTimeout)

	return pool
}

// WorkerPool is a collection of goroutines, where the number of concurrent
// goroutines processing requests does not exceed the specified maximum.
type WorkerPool struct {
	maxWorkers   int
	taskQueue    chan taskRequest
	workerQueue  chan func()
	stoppedChan  chan struct{}
	stopSignal   chan struct{}
	waitingQueue deque.Deque[taskRequest]
	stopLock     sync.Mutex
	stopOnce     sync.Once
	stopped      bool
	waiting      atomic.Int32
	wait         bool
	statsLock    sync.Mutex
	stats        Stats
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
	p.taskQueue <- taskRequest{fn: task}
	return
}

// Submit enqueues the given function without blocking. Panics if the worker
// pool is stopped.
func (p *WorkerPool) Submit(task func()) {
	if err := p.Do(task); err != nil {
		panic(err)
	}
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

// WaitingQueueSize returns the count of tasks in the waiting queue.
func (p *WorkerPool) WaitingQueueSize() int {
	return int(p.waiting.Load())
}

// Stats returns a consistent snapshot of the worker pool's counters. See the
// Stats type for the conservation relations the counters satisfy.
func (p *WorkerPool) Stats() Stats {
	p.statsLock.Lock()
	defer p.statsLock.Unlock()
	return p.stats
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
		p.taskQueue <- taskRequest{
			fn: func() {
				ready.Done()
				select {
				case <-ctx.Done():
				case <-p.stopSignal:
				}
			},
			internal: true,
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
		case req, ok := <-p.taskQueue:
			if !ok {
				break Loop
			}
			// Got a task to do.
			select {
			case p.workerQueue <- p.wrapTask(req):
				if !req.internal {
					p.noteStarted()
				}
			default:
				// Create a new worker, if not at max.
				if workerCount < p.maxWorkers {
					wg.Add(1)
					go worker(p.wrapTask(req), p.workerQueue, &wg)
					workerCount++
					p.noteWorker(1)
					if !req.internal {
						p.noteStarted()
					}
				} else {
					// Enqueue task to be executed by next available worker.
					p.waitingQueue.PushBack(req)
					p.waiting.Store(int32(p.waitingQueue.Len()))
					if !req.internal {
						p.noteQueued()
					}
				}
			}
			idle = false
		case <-idleTO:
			// Timed out waiting for work to arrive. Kill a ready worker if
			// pool has been idle for a whole timeout.
			if idle && workerCount > 0 {
				if p.killIdleWorker() {
					workerCount--
					p.noteWorker(-1)
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
		// Discard queued tasks, recording them as dropped.
		dropped := 0
		for i := 0; i < p.waitingQueue.Len(); i++ {
			if !p.waitingQueue.At(i).internal {
				dropped++
			}
		}
		if dropped > 0 {
			p.noteDropped(dropped)
		}
		p.waiting.Store(0)
	}

	// Stop all remaining workers as they become ready.
	for workerCount > 0 {
		p.workerQueue <- nil
		workerCount--
		p.noteWorker(-1)
	}
	wg.Wait()
}

// wrapTask returns the function to give to a worker. User tasks are wrapped
// to record completion in the pool's stats and to recover from panics, so a
// panicking task neither crashes the pool nor is lost from the accounting.
// Internal control tasks are excluded from the stats and run unwrapped.
func (p *WorkerPool) wrapTask(req taskRequest) func() {
	if req.internal {
		return req.fn
	}
	return func() {
		defer func() {
			_ = recover()
			p.noteCompleted()
		}()
		req.fn()
	}
}

// noteQueued records that a submitted task was accepted and put on the
// waiting queue.
func (p *WorkerPool) noteQueued() {
	p.statsLock.Lock()
	p.stats.Received++
	p.stats.Queued++
	p.statsLock.Unlock()
}

// noteStarted records that a submitted task was accepted and handed directly
// to a worker.
func (p *WorkerPool) noteStarted() {
	p.statsLock.Lock()
	p.stats.Received++
	p.stats.Started++
	p.stats.Running++
	p.statsLock.Unlock()
}

// noteDequeued records that a queued task was handed to a worker.
func (p *WorkerPool) noteDequeued() {
	p.statsLock.Lock()
	p.stats.Queued--
	p.stats.Started++
	p.stats.Running++
	p.statsLock.Unlock()
}

// noteCompleted records that a running task finished executing.
func (p *WorkerPool) noteCompleted() {
	p.statsLock.Lock()
	p.stats.Running--
	p.stats.Completed++
	p.statsLock.Unlock()
}

// noteDropped records that n queued tasks were discarded by Stop.
func (p *WorkerPool) noteDropped(n int) {
	p.statsLock.Lock()
	p.stats.Queued -= n
	p.stats.Dropped += n
	p.statsLock.Unlock()
}

// noteWorker records a change in the number of live worker goroutines.
func (p *WorkerPool) noteWorker(delta int) {
	p.statsLock.Lock()
	p.stats.Workers += delta
	p.statsLock.Unlock()
}

// worker executes tasks and stops when it receives a nil task.
func worker(task func(), workerQueue chan func(), wg *sync.WaitGroup) {
	for task != nil {
		task()
		task = <-workerQueue
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
	})
	<-p.stoppedChan
}

// processWaitingQueue puts new tasks onto the waiting queue, and removes
// tasks from the waiting queue as workers become available. Returns false if
// worker pool is stopped.
func (p *WorkerPool) processWaitingQueue() bool {
	front := p.waitingQueue.Front()
	select {
	case req, ok := <-p.taskQueue:
		if !ok {
			return false
		}
		p.waitingQueue.PushBack(req)
		if !req.internal {
			p.noteQueued()
		}
	case p.workerQueue <- p.wrapTask(front):
		// A worker was ready, so gave task to worker.
		p.waitingQueue.PopFront()
		if !front.internal {
			p.noteDequeued()
		}
	}
	p.waiting.Store(int32(p.waitingQueue.Len()))
	return true
}

func (p *WorkerPool) killIdleWorker() bool {
	select {
	case p.workerQueue <- nil:
		// Sent kill signal to worker.
		return true
	default:
		// No ready workers. All, if any, workers are busy.
		return false
	}
}

// runQueuedTasks removes each task from the waiting queue and gives it to
// workers until queue is empty.
func (p *WorkerPool) runQueuedTasks() {
	for p.waitingQueue.Len() != 0 {
		// A worker is ready, so give task to worker.
		req := p.waitingQueue.PopFront()
		p.workerQueue <- p.wrapTask(req)
		if !req.internal {
			p.noteDequeued()
		}
		p.waiting.Store(int32(p.waitingQueue.Len()))
	}
}
