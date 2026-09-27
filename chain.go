package cron

import (
	"fmt"
	"runtime"
	"sync"
	"time"
)

// JobWrapper decorates the given Job with some behavior.
type JobWrapper func(Job) Job

// TriggeredJob is an optional interface that a Job may implement to receive
// the time of the trigger that caused each run. Wrappers that understand
// this interface (such as MergeIfStillRunning) invoke RunWithTrigger instead
// of Run. Jobs that do not implement it are simply invoked via Run.
type TriggeredJob interface {
	RunWithTrigger(trigger time.Time)
}

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

// Recover panics in wrapped jobs and log them with the provided logger.
func Recover(logger Logger) JobWrapper {
	return func(j Job) Job {
		return FuncJob(func() {
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
			j.Run()
		})
	}
}

// DelayIfStillRunning serializes jobs, delaying subsequent runs until the
// previous one is complete. Jobs running after a delay of more than a minute
// have the delay logged at Info.
func DelayIfStillRunning(logger Logger) JobWrapper {
	return func(j Job) Job {
		var mu sync.Mutex
		return FuncJob(func() {
			start := time.Now()
			mu.Lock()
			defer mu.Unlock()
			if dur := time.Since(start); dur > time.Minute {
				logger.Info("delay", "duration", dur)
			}
			j.Run()
		})
	}
}

// SkipIfStillRunning skips an invocation of the Job if a previous invocation is
// still running. It logs skips to the given logger at Info level.
func SkipIfStillRunning(logger Logger) JobWrapper {
	return func(j Job) Job {
		var ch = make(chan struct{}, 1)
		ch <- struct{}{}
		return FuncJob(func() {
			select {
			case v := <-ch:
				defer func() { ch <- v }()
				j.Run()
			default:
				logger.Info("skip")
			}
		})
	}
}

// MergeIfStillRunning coalesces invocations of the Job that arrive while a
// previous invocation is still running into a single pending run. When the
// in-progress run completes, the pending run starts immediately, using the
// most recent trigger time as its metadata. It logs merges to the given
// logger at Info level.
//
// This differs from SkipIfStillRunning (which drops triggers) and
// DelayIfStillRunning (which queues every trigger): any number of triggers
// that arrive during a single run produce exactly one additional run.
//
// Semantics for edge cases:
//   - A trigger that arrives exactly as a run finishes is not lost: it either
//     merges into a pending run consumed by the in-flight invocation, or it
//     starts a fresh invocation. Exactly one run results, never two.
//   - Panics are recovered and logged so that a pending run is still executed
//     and the wrapper is never left stuck in the "running" state. Compose
//     with Recover (inside this wrapper) to also log stack traces.
//   - The pending run executes within the goroutine of the in-flight
//     invocation, so Cron.Stop waits for it via the job waiter, and
//     Cron.Remove cannot cancel it once merged; both only prevent future
//     triggers. No run is started after the scheduler has stopped.
//
// Jobs implementing TriggeredJob receive the trigger time of each run: the
// pending run observes the most recent merged trigger time.
func MergeIfStillRunning(logger Logger) JobWrapper {
	return func(j Job) Job {
		var mu sync.Mutex
		var running, pending bool
		var pendingTrigger time.Time
		return FuncJob(func() {
			trigger := time.Now()
			mu.Lock()
			if running {
				// A run is in progress: merge this trigger into the single
				// pending run, keeping the most recent trigger time.
				pending = true
				pendingTrigger = trigger
				mu.Unlock()
				logger.Info("merge")
				return
			}
			running = true
			mu.Unlock()

			for {
				runTriggered(j, trigger, logger)
				mu.Lock()
				if !pending {
					running = false
					mu.Unlock()
					return
				}
				trigger = pendingTrigger
				pending = false
				mu.Unlock()
			}
		})
	}
}

// runTriggered invokes the job, passing the trigger time to jobs that
// implement TriggeredJob. Panics are recovered and logged so that the
// caller's bookkeeping (e.g. the MergeIfStillRunning state machine) always
// completes and a pending run is never dropped due to a panic.
func runTriggered(j Job, trigger time.Time, logger Logger) {
	defer func() {
		if r := recover(); r != nil {
			err, ok := r.(error)
			if !ok {
				err = fmt.Errorf("%v", r)
			}
			logger.Error(err, "panic")
		}
	}()
	if tj, ok := j.(TriggeredJob); ok {
		tj.RunWithTrigger(trigger)
	} else {
		j.Run()
	}
}
