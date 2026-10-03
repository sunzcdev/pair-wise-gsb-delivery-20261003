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
		taskQueue:   make(chan queuedTask),
		doneQueue:   make(chan any),
		workerQueue: make(chan func()),
		stopSignal:  make(chan struct{}),
		stoppedChan: make(chan struct{}),
		keyQueues:   make(map[any]*keyState),
	}

	// Start the task dispatcher.
	go pool.dispatch(cfg.idleTimeout)

	return pool
}

// WorkerPool is a collection of goroutines, where the number of concurrent
// goroutines processing requests does not exceed the specified maximum.
type WorkerPool struct {
	maxWorkers   int
	taskQueue    chan queuedTask
	doneQueue    chan any
	workerQueue  chan func()
	stoppedChan  chan struct{}
	stopSignal   chan struct{}
	waitingQueue deque.Deque[func()]
	keyQueues    map[any]*keyState
	stopLock     sync.Mutex
	stopOnce     sync.Once
	stopped      bool
	waiting      atomic.Int32
	wait         bool
	keyedRunning int
}

// queuedTask is a unit of work handed to the dispatcher. A plain task,
// submitted via Submit/Do, has keyed == false. A keyed task carries its key,
// and its queue and lifecycle are tracked by the dispatcher so that tasks
// sharing a key run one at a time in submission order.
type queuedTask struct {
	task  func()
	key   any
	keyed bool
}

// keyState holds the tasks submitted for a key that have not yet started,
// excluding the single task (if any) currently running for that key.
type keyState struct {
	pending deque.Deque[func()]
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
	p.taskQueue <- queuedTask{task: task}
	return
}

// Submit enqueues the given function without blocking. Panics if the worker
// pool is stopped.
func (p *WorkerPool) Submit(task func()) {
	if err := p.Do(task); err != nil {
		panic(err)
	}
}

// DoKey enqueues a function associated with the given key for a worker to
// execute. Returns ErrStopped if the worker pool is stopped.
//
// Tasks sharing the same key are executed strictly one at a time, in the
// order in which they were submitted. Tasks with different keys, as well as
// tasks submitted with Submit, may execute concurrently with one another, up
// to the pool's maximum number of workers.
//
// Serialization is enforced by the dispatcher, not by the task itself: a
// long-running or blocked task for a busy key parks only that key. A large
// backlog for a hot key does not occupy workers, so tasks for independent
// keys that are submitted later are still scheduled promptly.
//
// Go does not permit methods to declare type parameters, so the keyed entry
// points are package-level functions taking a pool argument; K may be any
// comparable type.
func DoKey[K comparable](p *WorkerPool, key K, task func()) (err error) {
	if task == nil {
		return
	}
	defer func() {
		if recover() != nil {
			err = ErrStopped
		}
	}()
	p.taskQueue <- queuedTask{task: task, key: key, keyed: true}
	return
}

// SubmitKey enqueues the given function for the given key without blocking,
// panicking if the worker pool is stopped. See DoKey for key semantics.
func SubmitKey[K comparable](p *WorkerPool, key K, task func()) {
	if err := DoKey(p, key, task); err != nil {
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
		pauseTask := func() {
			ready.Done()
			select {
			case <-ctx.Done():
			case <-p.stopSignal:
			}
		}
		p.taskQueue <- queuedTask{task: pauseTask}
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
		case item, ok := <-p.taskQueue:
			if !ok {
				break Loop
			}
			if !item.keyed {
				// Plain task: dispatch it directly, or overflow to waiting.
				p.offer(item.task, &workerCount, &wg)
			} else {
				p.startOrParkKeyed(item, &workerCount, &wg)
			}
			idle = false
		case key := <-p.doneQueue:
			// A keyed task finished; the dispatcher is now free to run the
			// next task for that key. This is the only place a running key
			// becomes available, so same-key tasks never overlap.
			p.advanceKeyed(key, &workerCount, &wg)
			idle = false
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
	}
	// Key state is no longer needed: drop all key bookkeeping so a long-lived
	// pool cannot leave a key table that only ever grows.
	p.keyQueues = nil
	p.keyedRunning = 0

	// Stop all remaining workers as they become ready. When abandoning queued
	// work, in-flight keyed tasks still report completion on doneQueue; drain
	// those signals so their workers can reach the kill message.
	for workerCount > 0 {
		select {
		case p.workerQueue <- nil:
			workerCount--
		case <-p.doneQueue:
		}
	}
	wg.Wait()
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
	select {
	case item, ok := <-p.taskQueue:
		if !ok {
			return false
		}
		if !item.keyed {
			p.waitingQueue.PushBack(item.task)
		} else {
			p.parkOrQueueKeyed(item)
		}
	case p.workerQueue <- p.waitingQueue.Front():
		// A worker was ready, so gave task to worker.
		p.waitingQueue.PopFront()
	case key := <-p.doneQueue:
		// A worker freed up while a queue is backlogged. The key's next
		// task, if any, joins the waiting queue like any other runnable task.
		p.releaseKeyedToWaiting(key)
	}
	p.waiting.Store(int32(p.waitingQueue.Len()))
	return true
}

// offer hands a runnable, already-wrapped task to a ready worker, creates a
// new worker when below the limit, or overflows it onto the waiting queue.
func (p *WorkerPool) offer(task func(), workerCount *int, wg *sync.WaitGroup) {
	select {
	case p.workerQueue <- task:
	default:
		if *workerCount < p.maxWorkers {
			wg.Add(1)
			go worker(task, p.workerQueue, wg)
			*workerCount++
		} else {
			p.waitingQueue.PushBack(task)
			p.waiting.Store(int32(p.waitingQueue.Len()))
		}
	}
}

// startOrParkKeyed starts a keyed task immediately when the key has no
// running task; otherwise the task is parked on the key's own deque. The
// backlog never occupies a worker, so a hot key cannot block other keys.
func (p *WorkerPool) startOrParkKeyed(item queuedTask, workerCount *int, wg *sync.WaitGroup) {
	ks, ok := p.keyQueues[item.key]
	if ok {
		ks.pending.PushBack(item.task)
		return
	}
	ks = &keyState{}
	p.keyQueues[item.key] = ks
	p.keyedRunning++
	p.offer(p.wrapKeyedTask(item.key, item.task), workerCount, wg)
}

// parkOrQueueKeyed handles a keyed task arriving while the waiting queue is
// non-empty. Tasks for a busy key are parked; a free key's first task joins
// the waiting queue to preserve FIFO order among runnable tasks.
func (p *WorkerPool) parkOrQueueKeyed(item queuedTask) {
	ks, ok := p.keyQueues[item.key]
	if ok {
		ks.pending.PushBack(item.task)
		return
	}
	ks = &keyState{}
	p.keyQueues[item.key] = ks
	p.keyedRunning++
	p.waitingQueue.PushBack(p.wrapKeyedTask(item.key, item.task))
	p.waiting.Store(int32(p.waitingQueue.Len()))
}

// advanceKeyed handles a keyed task finishing while the waiting queue is
// empty: dispatch the key's next parked task now that a worker just freed up,
// or remove the key's state when nothing remains for it.
func (p *WorkerPool) advanceKeyed(key any, workerCount *int, wg *sync.WaitGroup) {
	ks, ok := p.keyQueues[key]
	if !ok {
		return
	}
	if ks.pending.Len() != 0 {
		next := ks.pending.PopFront()
		p.offer(p.wrapKeyedTask(key, next), workerCount, wg)
		return
	}
	delete(p.keyQueues, key)
	p.keyedRunning--
}

// releaseKeyedToWaiting handles a keyed task finishing while the waiting
// queue is non-empty: the key's next task joins the back of the waiting
// queue, or the key's state is removed.
func (p *WorkerPool) releaseKeyedToWaiting(key any) {
	ks, ok := p.keyQueues[key]
	if !ok {
		return
	}
	if ks.pending.Len() != 0 {
		next := ks.pending.PopFront()
		p.waitingQueue.PushBack(p.wrapKeyedTask(key, next))
	} else {
		delete(p.keyQueues, key)
		p.keyedRunning--
	}
}

// wrapKeyedTask wraps a keyed task so it reports completion to the
// dispatcher, which is what releases the key's next task. doneQueue is never
// closed, and the dispatcher always receives before the pool fully stops, so
// the send cannot block past shutdown.
func (p *WorkerPool) wrapKeyedTask(key any, task func()) func() {
	return func() {
		task()
		p.doneQueue <- key
	}
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
	for p.waitingQueue.Len() != 0 || p.keyedRunning != 0 {
		if p.waitingQueue.Len() != 0 {
			// A worker is ready, so give task to worker.
			p.workerQueue <- p.waitingQueue.PopFront()
			p.waiting.Store(int32(p.waitingQueue.Len()))
			continue
		}
		// Waiting queue drained while keyed tasks still run: wait for one to
		// finish and hand its key's next task (if any) to the now-free worker.
		key := <-p.doneQueue
		ks, ok := p.keyQueues[key]
		if !ok {
			continue
		}
		if ks.pending.Len() != 0 {
			next := ks.pending.PopFront()
			p.workerQueue <- p.wrapKeyedTask(key, next)
		} else {
			delete(p.keyQueues, key)
			p.keyedRunning--
		}
	}
}
