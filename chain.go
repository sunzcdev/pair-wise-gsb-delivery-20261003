package cron

import (
	"fmt"
	"runtime"
	"sync"
	"time"
)

// JobWrapper decorates the given Job with some behavior.
type JobWrapper func(Job) Job

// Chain is a sequence of JobWrappers that decorates submitted jobs with
// cross-cutting behaviors like logging or synchronization.
type Chain struct {
	wrappers []JobWrapper
}

// NewChain returns a Chain consisting of the given JobWrappers.
func NewChain(c ...JobWrapper) Chain {
	return Chain{c}
}

// Then decorates the given job with all JobWrappers in the chain.
//
// This:
//     NewChain(m1, m2, m3).Then(job)
// is equivalent to:
//     m1(m2(m3(job)))
func (c Chain) Then(j Job) Job {
	for i := range c.wrappers {
		j = c.wrappers[len(c.wrappers)-i-1](j)
	}
	return j
}

// timedFuncJob adapts a function that accepts a trigger time into a Job
// that also implements TriggerJob, so that trigger times propagate through
// a chain of wrappers to wrapped jobs that implement TriggerJob.
type timedFuncJob func(triggerTime time.Time)

func (t timedFuncJob) Run() { t(time.Now()) }

func (t timedFuncJob) RunAt(triggerTime time.Time) { t(triggerTime) }

// Recover panics in wrapped jobs and log them with the provided logger.
func Recover(logger Logger) JobWrapper {
	return func(j Job) Job {
		return timedFuncJob(func(t time.Time) {
			defer func() {
				if r := recover(); r != nil {
					const size = 64 << 10
					buf := make([]byte, size)
					buf = buf[:runtime.Stack(buf, false)]
					err, ok := r.(error)
					if !ok {
						err = fmt.Errorf("%v", r)
					}
					logger.Error(err, "panic", "stack", "...\n"+string(buf))
				}
			}()
			runJob(j, t)
		})
	}
}

// DelayIfStillRunning serializes jobs, delaying subsequent runs until the
// previous one is complete. Jobs running after a delay of more than a minute
// have the delay logged at Info.
func DelayIfStillRunning(logger Logger) JobWrapper {
	return func(j Job) Job {
		var mu sync.Mutex
		return timedFuncJob(func(t time.Time) {
			start := time.Now()
			mu.Lock()
			defer mu.Unlock()
			if dur := time.Since(start); dur > time.Minute {
				logger.Info("delay", "duration", dur)
			}
			runJob(j, t)
		})
	}
}

// SkipIfStillRunning skips an invocation of the Job if a previous invocation is
// still running. It logs skips to the given logger at Info level.
func SkipIfStillRunning(logger Logger) JobWrapper {
	return func(j Job) Job {
		var ch = make(chan struct{}, 1)
		ch <- struct{}{}
		return timedFuncJob(func(t time.Time) {
			select {
			case v := <-ch:
				defer func() { ch <- v }()
				runJob(j, t)
			default:
				logger.Info("skip")
			}
		})
	}
}

// CoalesceIfStillRunning merges triggers that arrive while a previous run is
// still in progress into a single pending run, executed immediately after the
// current run completes. Unlike SkipIfStillRunning the triggers are not
// dropped, and unlike DelayIfStillRunning they are not all queued: at most
// one pending run is ever outstanding, and it uses the most recent trigger
// time as its trigger time. Merged triggers are logged at Info.
//
// A trigger that arrives exactly as a run finishes is either merged into the
// pending run (if the run has not yet been marked complete) or starts a new
// run; in both cases it is executed exactly once.
//
// If a run panics, the panic is recovered so that the pending run still
// executes and the job is not left permanently "running"; the first panic is
// re-raised once no more runs are pending, so an outer Recover wrapper still
// observes it.
//
// Remove and Stop do not cancel a pending run: it belongs to a trigger that
// was already delivered to the job, so it runs to completion and the context
// returned by Cron.Stop only completes afterwards. Triggers not yet
// delivered when the scheduler stops never reach the job.
func CoalesceIfStillRunning(logger Logger) JobWrapper {
	return func(j Job) Job {
		c := &coalescingJob{inner: j, logger: logger}
		return timedFuncJob(c.trigger)
	}
}

type coalescingJob struct {
	inner  Job
	logger Logger

	mu        sync.Mutex
	running   bool
	pending   bool
	pendingAt time.Time
}

func (c *coalescingJob) trigger(t time.Time) {
	c.mu.Lock()
	if c.running {
		// A run is in progress: merge this trigger into the single pending
		// run, keeping the most recent trigger time.
		if !c.pending || t.After(c.pendingAt) {
			c.pendingAt = t
		}
		c.pending = true
		c.mu.Unlock()
		c.logger.Info("coalesce", "trigger", t)
		return
	}
	c.running = true
	c.mu.Unlock()

	// Run the job, then keep running while triggers arrived in the meantime.
	var panicked interface{}
	for {
		if p := c.runOnce(t); p != nil && panicked == nil {
			panicked = p
		}
		c.mu.Lock()
		if !c.pending {
			c.running = false
			c.mu.Unlock()
			break
		}
		t = c.pendingAt
		c.pending = false
		c.pendingAt = time.Time{}
		c.mu.Unlock()
	}
	if panicked != nil {
		panic(panicked)
	}
}

// runOnce runs the inner job, recovering any panic so that the coalescing
// state machine always advances; the panic is re-raised by trigger once the
// state is consistent again.
func (c *coalescingJob) runOnce(t time.Time) (panicked interface{}) {
	defer func() { panicked = recover() }()
	runJob(c.inner, t)
	return nil
}
