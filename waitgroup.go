package conc

import (
	"sync"
	"sync/atomic"

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
// The zero value of WaitGroup is usable, just like sync.WaitGroup.
// Also like sync.WaitGroup, it must not be copied after first use.
type WaitGroup struct {
	wg sync.WaitGroup

	// inFlight tracks the number of goroutines spawned with Go that have not
	// yet finished. It is used to detect the first goroutine of a new round
	// so that panics recovered in a previous round can be cleared.
	inFlight atomic.Int64

	// recoveredMu protects recovered, which holds one entry per panicking
	// goroutine in the current round, appended in the order the panics are
	// recovered (which is also the order the goroutines finish).
	recoveredMu sync.Mutex
	recovered   []*panics.Recovered
}

// Go spawns a new goroutine in the WaitGroup.
func (h *WaitGroup) Go(f func()) {
	// If this is the first goroutine of a new round, discard any panics
	// recovered in previous rounds. As with sync.WaitGroup, reusing a
	// WaitGroup by spawning new goroutines is only valid after a previous
	// call to Wait has returned, so no goroutine from a previous round is
	// still writing to recovered when this runs.
	if h.inFlight.Add(1) == 1 {
		h.recoveredMu.Lock()
		h.recovered = nil
		h.recoveredMu.Unlock()
	}

	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer h.inFlight.Add(-1)
		if rp := panics.Catch(f); rp != nil {
			h.recoveredMu.Lock()
			h.recovered = append(h.recovered, rp)
			h.recoveredMu.Unlock()
		}
	}()
}

// Wait will block until all goroutines spawned with Go exit and will
// propagate a panic if any of the child goroutines panicked. The propagated
// value is the *panics.Recovered of the first panic received from a child
// goroutine, even when multiple children panic. To inspect all recovered
// panics, use WaitAndRecoverAll.
//
// Wait blocks until every goroutine spawned with Go has exited; it never
// returns after the first panic while other goroutines are still running.
//
// Wait can be called multiple times after a round has completed (with no
// goroutines in flight); every such call observes the same round and the same
// first recovered panic. A subsequent round started with Go after Wait has
// returned does not inherit the previous round's recovered panics.
func (h *WaitGroup) Wait() {
	h.wg.Wait()
	if p := h.firstRecovered(); p != nil {
		panic(p)
	}
}

// WaitAndRecover will block until all goroutines spawned with Go exit and
// will return a *panics.Recovered if one of the child goroutines panicked.
// When several child goroutines panic, the first recovered panic is returned;
// use WaitAndRecoverAll to retrieve all of them.
//
// As with Wait, calling WaitAndRecover multiple times after a round has
// completed returns the same recovered panic from the same round. A
// subsequent round started with Go returns only the panics of that round.
func (h *WaitGroup) WaitAndRecover() *panics.Recovered {
	h.wg.Wait()
	return h.firstRecovered()
}

// WaitAndRecoverAll will block until all goroutines spawned with Go exit and
// will return a *panics.Recovered for every child goroutine that panicked,
// including panics with nil values. Goroutines that return normally do not
// produce entries. The entries are stable-ordered by the order in which the
// panics were recovered from the child goroutines, which is also the order
// the goroutines finished; the position of an entry in the returned slice is
// its zero-based reception index within the round.
//
// WaitAndRecoverAll blocks until every goroutine spawned with Go has exited,
// so it never hands control back to the caller after the first panic while
// other goroutines are still running.
//
// If no child goroutine panicked, WaitAndRecoverAll returns nil. Calling
// WaitAndRecoverAll multiple times after a round has completed returns the
// same set of recovered panics for the same round. A subsequent round
// started with Go returns only the panics of that round; recovered panics
// from previous rounds never leak into later rounds.
func (h *WaitGroup) WaitAndRecoverAll() []*panics.Recovered {
	h.wg.Wait()

	h.recoveredMu.Lock()
	recovered := append([]*panics.Recovered(nil), h.recovered...)
	h.recoveredMu.Unlock()
	return recovered
}

func (h *WaitGroup) firstRecovered() *panics.Recovered {
	h.recoveredMu.Lock()
	defer h.recoveredMu.Unlock()
	if len(h.recovered) == 0 {
		return nil
	}
	return h.recovered[0]
}
