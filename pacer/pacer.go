/*
Package pacer provides a utility to limit the rate at which concurrent
goroutines begin execution. This addresses situations where running the
concurrent goroutines is OK, as long as their execution does not start at
the same time.

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
// Stop shuts the pacer down and immediately unblocks every current and
// future waiter: NextContext returns false and Next returns without
// waiting. Stop is safe to call multiple times.
type Pacer struct {
	delay    time.Duration
	requests chan *nextRequest
	stopCh   chan struct{}
	done     chan struct{}
	stopOnce sync.Once

	pause  chan struct{}
	paused chan struct{}

	// newTimer creates the timer used to wait out the delay between
	// releases. It is a field so that tests can substitute a fake clock.
	newTimer func(time.Duration) (<-chan time.Time, func())
}

// nextRequest is a single waiter's request for a release. The run
// goroutine grants the release by closing grant.
type nextRequest struct {
	ctx   context.Context
	grant chan struct{}
}

// NewPacer creates and runs a new Pacer.
func NewPacer(delay time.Duration) *Pacer {
	return newPacer(delay, func(d time.Duration) (<-chan time.Time, func()) {
		t := time.NewTimer(d)
		return t.C, func() { t.Stop() }
	})
}

func newPacer(delay time.Duration, newTimer func(time.Duration) (<-chan time.Time, func())) *Pacer {
	p := &Pacer{
		delay:    delay,
		requests: make(chan *nextRequest),
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
		pause:    make(chan struct{}, 1),
		paused:   make(chan struct{}, 1),
		newTimer: newTimer,
	}

	go p.run()
	return p
}

// Pace wraps a function in a paced function. The returned paced function can
// then be submitted to the workerpool, using Submit or SubmitWait, and
// starting the tasks is paced according to the pacer's delay. A paced task
// whose wait ends because the pacer was stopped does not run.
func (p *Pacer) Pace(task func()) func() {
	return func() {
		if p.NextContext(context.Background()) {
			task()
		}
	}
}

// Next submits a run request and returns when it is time to run. It is
// equivalent to NextContext with a context that is never canceled, and
// shares the same release sequence with it; the two may be mixed freely.
// After Stop, Next returns immediately without waiting.
func (p *Pacer) Next() {
	p.NextContext(context.Background())
}

// NextContext submits a run request and waits until it is time to run, the
// pacer is stopped, or ctx is canceled. It returns true when a release was
// acquired, and false when the wait ended due to cancellation or Stop.
//
// A paused pacer retains its waiters; Resume continues releasing them at
// the configured rate. A waiter whose context is canceled while queued
// does not consume a release opportunity. If a release and a cancellation
// are both ready, the release wins and NextContext returns true.
//
// After Stop, every blocked and future caller gets a consistent stop
// result: NextContext returns false immediately.
func (p *Pacer) NextContext(ctx context.Context) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	req := &nextRequest{ctx: ctx, grant: make(chan struct{})}

	// Register the request with the run goroutine.
	select {
	case p.requests <- req:
	case <-p.stopCh:
		return false
	case <-ctx.Done():
		return false
	}

	// Wait for the release to be granted.
	select {
	case <-req.grant:
		return true
	case <-p.stopCh:
	case <-ctx.Done():
	}

	// Stopped or canceled. A release that raced with the cancellation
	// still counts, so that a granted release is never reported as lost.
	select {
	case <-req.grant:
		return true
	default:
		return false
	}
}

// Stop stops the Pacer from running. All blocked and future waiters are
// released immediately: NextContext returns false and Next returns without
// waiting. Stop is idempotent and waits for the pacer to shut down.
func (p *Pacer) Stop() {
	p.stopOnce.Do(func() {
		close(p.stopCh)
	})
	<-p.done
}

// IsPaused returns true if execution is paused.
func (p *Pacer) IsPaused() bool {
	return len(p.paused) != 0
}

// Pause suspends execution of any tasks by the pacer. Waiters are retained
// while paused and are released, at the configured rate, after Resume.
func (p *Pacer) Pause() {
	p.pause <- struct{}{}  // block this channel
	p.paused <- struct{}{} // set flag to indicate paused
}

// Resume continues execution after Pause.
func (p *Pacer) Resume() {
	<-p.paused // clear flag to indicate paused
	<-p.pause  // unblock this channel
}

func (p *Pacer) run() {
	defer close(p.done)
	for {
		var req *nextRequest
		select {
		case req = <-p.requests:
		case <-p.stopCh:
			return
		}
		// A waiter that was canceled while queued must not consume a
		// release opportunity.
		select {
		case <-req.ctx.Done():
			continue
		default:
		}
		close(req.grant)
		if !p.waitDelay() {
			return
		}
	}
}

// waitDelay waits out the delay between releases and then passes the pause
// barrier. It returns false if the pacer is stopped while waiting.
func (p *Pacer) waitDelay() bool {
	timer, stop := p.newTimer(p.delay)
	defer stop()
	select {
	case <-timer:
	case <-p.stopCh:
		return false
	}
	// Pause barrier: blocks here while the pacer is paused.
	select {
	case p.pause <- struct{}{}: // will wait here if channel blocked
		<-p.pause // clear channel
	case <-p.stopCh:
		return false
	}
	return true
}
