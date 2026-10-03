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
		maxWorkers:  maxWorkers,
		taskQueue:   make(chan func()),
		workerQueue: make(chan func()),
		doneChan:    make(chan bool, maxWorkers),
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
	taskQueue    chan func()
	workerQueue  chan func()
	doneChan     chan bool
	stoppedChan  chan struct{}
	stopSignal   chan struct{}
	waitingQueue deque.Deque[func()]
	stopLock     sync.Mutex
	stopOnce     sync.Once
	stopped      bool
	waiting      atomic.Int32
	wait         bool
	statsMu      sync.Mutex
	stats        Stats
}

// Size returns the maximum number of concurrent workers.
func (p *WorkerPool) Size() int {
	return p.maxWorkers
}

// Stats is a consistent snapshot of the worker pool's task and worker
// counters. All task state transitions are recorded by the dispatcher
// goroutine, so every snapshot satisfies the conservation relations:
//
//	Received == Started + Queued + Dropped
//	Started  == Completed + Running
//
// at the moment the snapshot is taken.
type Stats struct {
	// Received is the total number of tasks accepted by the dispatcher.
	// Tasks submitted concurrently with Stop that the dispatcher never
	// received are not counted.
	Received int
	// Started is the total number of tasks handed to a worker.
	Started int
	// Completed is the total number of tasks that finished executing,
	// including tasks that exited by panicking.
	Completed int
	// Panicked is the total number of tasks that exited by panicking. It
	// is a subset of Completed.
	Panicked int
	// Dropped is the total number of queued tasks discarded by Stop.
	Dropped int
	// Queued is the number of tasks currently in the waiting queue.
	Queued int
	// Running is the number of tasks currently executing.
	Running int
	// Workers is the number of worker goroutines currently alive. Worker
	// retirement (idle timeout or shutdown) changes this count without
	// affecting the task counters.
	Workers int
}

// Stats returns a consistent snapshot of the worker pool's counters. The
// returned value is a copy and satisfies the conservation relations
// documented on Stats.
func (p *WorkerPool) Stats() Stats {
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	return p.stats
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
// If a task panics, the worker recovers the panic and records it in the
// pool stats instead of crashing the program.
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
			select {
			case p.workerQueue <- task:
				p.updateStats(func(s *Stats) {
					s.Received++
					s.Started++
					s.Running++
				})
			default:
				// Create a new worker, if not at max.
				if workerCount < p.maxWorkers {
					wg.Add(1)
					go worker(task, p.workerQueue, p.doneChan, &wg)
					workerCount++
					p.updateStats(func(s *Stats) {
						s.Received++
						s.Started++
						s.Running++
						s.Workers++
					})
				} else {
					// Enqueue task to be executed by next available worker.
					p.waitingQueue.PushBack(task)
					p.waiting.Store(int32(p.waitingQueue.Len()))
					p.updateStats(func(s *Stats) {
						s.Received++
						s.Queued++
					})
				}
			}
			idle = false
		case panicked := <-p.doneChan:
			p.taskDone(panicked)
		case <-idleTO:
			// Timed out waiting for work to arrive. Kill a ready worker if
			// pool has been idle for a whole timeout.
			if idle && workerCount > 0 {
				if p.killIdleWorker() {
					workerCount--
					p.updateStats(func(s *Stats) { s.Workers-- })
				}
			}
			idle = true
			timeout.Reset(idleTimeout)
		}
	}

	// If instructed to wait, then run tasks that are already queued.
	if p.wait {
		p.runQueuedTasks()
	} else if p.waitingQueue.Len() != 0 {
		// Queued tasks are abandoned, so record them as dropped.
		dropped := p.waitingQueue.Len()
		p.waiting.Store(0)
		p.updateStats(func(s *Stats) {
			s.Dropped += dropped
			s.Queued -= dropped
		})
	}

	// Stop all remaining workers as they become ready.
	for workerCount > 0 {
		select {
		case p.workerQueue <- nil:
			workerCount--
			p.updateStats(func(s *Stats) { s.Workers-- })
		case panicked := <-p.doneChan:
			p.taskDone(panicked)
		}
	}
	wg.Wait()
	// All workers have exited, so every remaining completion report is
	// buffered in doneChan. Drain the reports so that the final stats are
	// complete before stoppedChan is closed.
drain:
	for {
		select {
		case panicked := <-p.doneChan:
			p.taskDone(panicked)
		default:
			break drain
		}
	}
}

// worker executes tasks and stops when it receives a nil task. After each
// task, the worker reports to the dispatcher whether the task panicked, so
// that the dispatcher can keep the pool stats consistent.
func worker(task func(), workerQueue chan func(), doneChan chan bool, wg *sync.WaitGroup) {
	for task != nil {
		doneChan <- runTask(task)
		task = <-workerQueue
	}
	wg.Done()
}

// runTask runs a task, recovering from any panic, and reports whether the
// task exited by panicking.
func runTask(task func()) (panicked bool) {
	defer func() {
		panicked = recover() != nil
	}()
	task()
	return false
}

// taskDone records the completion of a task in the pool stats. It is only
// called by the dispatcher goroutine.
func (p *WorkerPool) taskDone(panicked bool) {
	p.updateStats(func(s *Stats) {
		s.Completed++
		s.Running--
		if panicked {
			s.Panicked++
		}
	})
}

// updateStats applies a mutation to the pool stats. All mutations are
// performed by the dispatcher goroutine, so each locked update preserves
// the conservation relations documented on Stats.
func (p *WorkerPool) updateStats(f func(*Stats)) {
	p.statsMu.Lock()
	f(&p.stats)
	p.statsMu.Unlock()
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
	select {
	case task, ok := <-p.taskQueue:
		if !ok {
			return false
		}
		p.waitingQueue.PushBack(task)
		p.waiting.Store(int32(p.waitingQueue.Len()))
		p.updateStats(func(s *Stats) {
			s.Received++
			s.Queued++
		})
	case p.workerQueue <- p.waitingQueue.Front():
		// A worker was ready, so gave task to worker.
		p.waitingQueue.PopFront()
		p.waiting.Store(int32(p.waitingQueue.Len()))
		p.updateStats(func(s *Stats) {
			s.Queued--
			s.Started++
			s.Running++
		})
	case panicked := <-p.doneChan:
		p.taskDone(panicked)
	}
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
		select {
		case p.workerQueue <- p.waitingQueue.Front():
			// A worker is ready, so give task to worker.
			p.waitingQueue.PopFront()
			p.waiting.Store(int32(p.waitingQueue.Len()))
			p.updateStats(func(s *Stats) {
				s.Queued--
				s.Started++
				s.Running++
			})
		case panicked := <-p.doneChan:
			p.taskDone(panicked)
		}
	}
}
