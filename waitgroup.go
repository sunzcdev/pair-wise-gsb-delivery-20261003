package conc

import (
	"sync"

	"github.com/sourcegraph/conc/panics"
)

// NewWaitGroup creates a new WaitGroup.
func NewWaitGroup() *WaitGroup {
	return &WaitGroup{}
}

// WaitGroup is the primary building block for scoped concurrency.
// Goroutines can be spawned in the WaitGroup with the Go method,
// and calling Wait() will ensure that each of those goroutines exits
// before continuing. Any panics in a child goroutine will be caught
// and propagated to the caller of Wait().
//
// A WaitGroup can be reused across rounds. A round ends when a call to
// Wait, WaitAndRecover, or WaitAndRecoverAll returns, and the next call to
// Go starts a new round. Recovery information is scoped to a single round:
// calling any of the wait methods multiple times within a round reports the
// same panics, and panics caught in a previous round are never reported in
// a later round. As with sync.WaitGroup, calls to Go that start a new round
// must happen after all wait calls for the previous round have returned.
//
// The zero value of WaitGroup is usable, just like sync.WaitGroup.
// Also like sync.WaitGroup, it must not be copied after first use.
type WaitGroup struct {
	wg sync.WaitGroup
	pc panics.Catcher

	mu      sync.Mutex
	drained bool
}

// Go spawns a new goroutine in the WaitGroup.
func (h *WaitGroup) Go(f func()) {
	h.mu.Lock()
	if h.drained {
		// The previous round was fully waited on, so this call starts a
		// new round. Discard recovery information from the previous round
		// so it cannot leak into the new one.
		h.pc.Reset()
		h.drained = false
	}
	h.mu.Unlock()

	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.pc.Try(f)
	}()
}

// wait blocks until all goroutines in the current round exit and marks the
// round as drained, so that the next call to Go starts a fresh round.
func (h *WaitGroup) wait() {
	h.wg.Wait()
	h.mu.Lock()
	h.drained = true
	h.mu.Unlock()
}

// Wait will block until all goroutines spawned with Go exit and will
// propagate any panics spawned in a child goroutine.
func (h *WaitGroup) Wait() {
	h.wait()

	// Propagate a panic if we caught one from a child goroutine.
	h.pc.Repanic()
}

// WaitAndRecover will block until all goroutines spawned with Go exit and
// will return the first *panics.Recovered if one of the child goroutines
// panics.
func (h *WaitGroup) WaitAndRecover() *panics.Recovered {
	h.wait()

	// Return a recovered panic if we caught one from a child goroutine.
	return h.pc.Recovered()
}

// WaitAndRecoverAll will block until all goroutines spawned with Go exit and
// will return a panics.Recovered for every panic caught in a child goroutine
// of the current round, ordered by reception: the Seq field of each entry is
// its 0-based position in that order. Goroutines that exit normally do not
// produce an entry, and a panic with a nil value produces an entry with a
// nil Value. It returns nil if no child goroutine panicked.
//
// Like Wait and WaitAndRecover, it always waits for every goroutine in the
// round to exit; it never hands the scope back to the caller after the
// first panic.
func (h *WaitGroup) WaitAndRecoverAll() []panics.Recovered {
	h.wait()

	// Return every recovered panic caught from a child goroutine.
	return h.pc.RecoveredAll()
}
