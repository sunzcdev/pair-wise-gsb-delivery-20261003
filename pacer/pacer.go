/*
Package pacer provides a utility to limit the rate at which concurrent
goroutines begin execution. This addresses situations where running the
concurrent goroutines is OK, as long as their execution does not start at the
same time.

The pacer package is independent of the workerpool package. Paced functions can
be submitted to a workerpool or can be run as goroutines, and execution will be
paced in both cases.

*/
package pacer

import (
	"context"
	"sync"
	"time"
)

// Pacer is a goroutine rate limiter. When concurrent goroutines call
// Pacer.Next(), the call returns in a single goroutine at a time, at a rate no
// faster than one per delay time.
//
// Waiters call Next or NextContext to acquire a beat. Both methods share a
// single FIFO queue, so waiters are released in call order regardless of
// which method they use; Next is equivalent to NextContext with
// context.Background(), discarding the result.
//
// Pause retains queued waiters instead of releasing them, and Resume
// continues releasing them one per delay. Stop terminates the pacer: all
// queued waiters are released immediately with a consistent stop result
// (NextContext reports false and Next returns), and later calls get the same
// result without blocking.
//
// To use Pacer, create a new Pacer giving the interval that must elapse
// between the start of one task the start of the next task. Then call Pace(),
// passing your task function. A new paced task function is returned that can
// then be passed to WorkerPool's Submit() or SubmitWait(), or called as a go
// routine. Paced functions, that are run as goroutines, are also paced. For
// example:
//
//     pacer := pacer.NewPacer(time.Second)
//
//     pacedTask := pacer.Pace(func() {
//         fmt.Println("Hello World")
//     })
//
//     wp := workerpool.New(5)
//     wp.Submit(pacedTask)
//
//     go pacedTask()
//
type Pacer struct {
	delay time.Duration
	after func(time.Duration) <-chan time.Time

	mu      sync.Mutex
	queue   []*waiter
	paused  bool
	stopped bool
	changed chan struct{}
}

// waiter tracks a single caller waiting for a beat.
type waiter struct {
	result  chan bool       // receives true on release, false on stop
	cancel  <-chan struct{} // caller's context done channel, nil for Next
	granted bool            // set when the pacer has committed a result
}

// NewPacer creates and runs a new Pacer.
func NewPacer(delay time.Duration) *Pacer {
	return newPacer(delay, time.After)
}

// newPacer creates a Pacer with a replaceable timer source, for tests.
func newPacer(delay time.Duration, after func(time.Duration) <-chan time.Time) *Pacer {
	p := &Pacer{
		delay:   delay,
		after:   after,
		changed: make(chan struct{}, 1),
	}
	go p.run()
	return p
}

// Pace wraps a function in a paced function. The returned paced function can
// then be submitted to the workerpool, using Submit or SubmitWait, and
// starting the tasks is paced according to the pacer's delay.
func (p *Pacer) Pace(task func()) func() {
	return func() {
		p.Next()
		task()
	}
}

// Next submits a run request to the gate and returns when it is time to run.
// Next returns immediately once the pacer is stopped.
func (p *Pacer) Next() {
	w := &waiter{result: make(chan bool, 1)}
	if !p.enqueue(w) {
		return
	}
	<-w.result
}

// NextContext submits a run request and waits until it is time to run, the
// pacer is stopped, or ctx is done. It returns true if a beat was acquired,
// and false if the pacer was stopped or ctx was done first.
//
// A waiter whose context is done before the pacer commits a release to it
// does not consume that release; the release passes to the next waiter. If
// the pacer commits the release first, the beat is considered acquired and
// NextContext returns true even if ctx is also done.
//
// NextContext and Next share one FIFO queue and may be mixed freely; a
// context that cannot be done (e.g. context.Background()) makes NextContext
// behave exactly like Next.
func (p *Pacer) NextContext(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	w := &waiter{result: make(chan bool, 1), cancel: ctx.Done()}
	if !p.enqueue(w) {
		return false
	}
	select {
	case granted := <-w.result:
		return granted
	case <-ctx.Done():
		return p.withdraw(w)
	}
}

// Stop stops the Pacer from running. All queued waiters are released
// immediately with the stop result: pending NextContext calls return false
// and pending Next calls return. Calls made after Stop get the same result
// without blocking. Stop is idempotent and safe to call multiple times.
func (p *Pacer) Stop() {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	for _, w := range p.queue {
		w.granted = true
		w.result <- false
	}
	p.queue = nil
	p.mu.Unlock()
	p.signal()
}

// IsPaused returns true if execution is paused.
func (p *Pacer) IsPaused() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paused
}

// Pause suspends execution of any tasks by the pacer. Queued waiters are
// retained, and no beats are released until Resume is called. Pause is
// idempotent.
func (p *Pacer) Pause() {
	p.mu.Lock()
	p.paused = true
	p.mu.Unlock()
	p.signal()
}

// Resume continues execution after Pause. Resume is idempotent.
func (p *Pacer) Resume() {
	p.mu.Lock()
	p.paused = false
	p.mu.Unlock()
	p.signal()
}

// enqueue adds a waiter to the queue, returning false if the pacer stopped.
func (p *Pacer) enqueue(w *waiter) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return false
	}
	p.queue = append(p.queue, w)
	p.signal()
	return true
}

// withdraw removes a cancelled waiter from the queue. If the pacer already
// committed a result to the waiter, that result is reported instead.
func (p *Pacer) withdraw(w *waiter) bool {
	p.mu.Lock()
	if w.granted {
		p.mu.Unlock()
		return <-w.result
	}
	for i, q := range p.queue {
		if q == w {
			p.queue = append(p.queue[:i], p.queue[i+1:]...)
			break
		}
	}
	p.mu.Unlock()
	return false
}

// signal notifies the run loop that the pacer state changed.
func (p *Pacer) signal() {
	select {
	case p.changed <- struct{}{}:
	default:
	}
}

func (p *Pacer) run() {
	needDelay := false
	for {
		// Wait until at least one waiter is queued.
		p.mu.Lock()
		for !p.stopped && len(p.queue) == 0 {
			p.mu.Unlock()
			<-p.changed
			p.mu.Lock()
		}
		if p.stopped {
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()

		// Wait out the interval since the previous release.
		if needDelay && !p.waitDelay() {
			return
		}

		// Wait while paused, then release the next waiter that has not
		// been cancelled.
		p.mu.Lock()
		for !p.stopped && p.paused {
			p.mu.Unlock()
			<-p.changed
			p.mu.Lock()
		}
		if p.stopped {
			p.mu.Unlock()
			return
		}
		released := false
		for len(p.queue) > 0 {
			w := p.queue[0]
			p.queue = p.queue[1:]
			select {
			case <-w.cancel:
				// A cancelled waiter does not consume the release.
				continue
			default:
			}
			w.granted = true
			w.result <- true
			released = true
			break
		}
		p.mu.Unlock()
		if released {
			needDelay = true
		}
	}
}

// waitDelay waits out the release interval, returning false if the pacer was
// stopped while waiting.
func (p *Pacer) waitDelay() bool {
	timer := p.after(p.delay)
	for {
		select {
		case <-timer:
			return true
		case <-p.changed:
			p.mu.Lock()
			stopped := p.stopped
			p.mu.Unlock()
			if stopped {
				return false
			}
		}
	}
}
