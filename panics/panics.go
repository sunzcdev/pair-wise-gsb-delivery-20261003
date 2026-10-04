package panics

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
)

// Catcher is used to catch panics. You can execute a function with Try,
// which will catch any spawned panic. Try can be called any number of times,
// from any number of goroutines. Once all calls to Try have completed, you can
// get the value of the first panic (if any) with Recovered(), every caught
// panic with RecoveredAll(), or you can just propagate the panic (re-panic)
// with Repanic(). Reset discards all caught panics so the Catcher can be
// reused for another round of calls to Try.
type Catcher struct {
	mu        sync.Mutex
	recovered []Recovered
}

// Try executes f, catching any panic it might spawn. It is safe
// to call from multiple goroutines simultaneously.
//
// A panic with a nil value is caught like any other panic and is recorded
// with a nil Value. Since a goroutine that calls runtime.Goexit is
// indistinguishable from one that panics with a nil value, it is recorded
// the same way.
func (p *Catcher) Try(f func()) {
	completed := false
	defer p.tryRecover(&completed)
	f()
	completed = true
}

func (p *Catcher) tryRecover(completed *bool) {
	if *completed {
		return
	}
	// f did not return normally. The recovered value is nil when f
	// panicked with a nil value, which is still recorded.
	rp := NewRecovered(1, recover())
	p.mu.Lock()
	rp.Seq = len(p.recovered)
	p.recovered = append(p.recovered, rp)
	p.mu.Unlock()
}

// Repanic panics if any calls to Try caught a panic. It will panic with the
// value of the first panic caught, wrapped in a panics.Recovered with caller
// information.
func (p *Catcher) Repanic() {
	if val := p.Recovered(); val != nil {
		panic(val)
	}
}

// Recovered returns the value of the first panic caught by Try, or nil if
// no calls to Try panicked.
func (p *Catcher) Recovered() *Recovered {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.recovered) == 0 {
		return nil
	}
	return &p.recovered[0]
}

// RecoveredAll returns every panic caught by Try, ordered by reception: the
// first entry is the first panic that was caught, and the Seq field of each
// entry matches its index in the returned slice. Calls to Try that did not
// panic do not produce entries. It returns nil if no calls to Try panicked.
func (p *Catcher) RecoveredAll() []Recovered {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.recovered) == 0 {
		return nil
	}
	all := make([]Recovered, len(p.recovered))
	copy(all, p.recovered)
	return all
}

// Reset discards all panics caught so far. It is used to reuse a Catcher for
// another round of calls to Try without leaking recovery information from
// the previous round into the next one.
func (p *Catcher) Reset() {
	p.mu.Lock()
	p.recovered = nil
	p.mu.Unlock()
}

// NewRecovered creates a panics.Recovered from a panic value and a collected
// stacktrace. The skip parameter allows the caller to skip stack frames when
// collecting the stacktrace. Calling with a skip of 0 means include the call to
// NewRecovered in the stacktrace.
func NewRecovered(skip int, value any) Recovered {
	// 64 frames should be plenty
	var callers [64]uintptr
	n := runtime.Callers(skip+1, callers[:])
	return Recovered{
		Value:   value,
		Callers: callers[:n],
		Stack:   debug.Stack(),
	}
}

// Recovered is a panic that was caught with recover().
type Recovered struct {
	// The original value of the panic.
	Value any
	// The caller list as returned by runtime.Callers when the panic was
	// recovered. Can be used to produce a more detailed stack information with
	// runtime.CallersFrames.
	Callers []uintptr
	// The formatted stacktrace from the goroutine where the panic was recovered.
	// Easier to use than Callers.
	Stack []byte
	// Seq is the reception sequence number of the panic: it is 0 for the
	// first panic caught by a Catcher, 1 for the second, and so on. It is 0
	// for a Recovered created directly with NewRecovered.
	Seq int
}

// String renders a human-readable formatting of the panic.
func (p *Recovered) String() string {
	return fmt.Sprintf("panic: %v\nstacktrace:\n%s\n", p.Value, p.Stack)
}

// AsError casts the panic into an error implementation. The implementation
// is unwrappable with the cause of the panic, if the panic was provided one.
func (p *Recovered) AsError() error {
	if p == nil {
		return nil
	}
	return &ErrRecovered{*p}
}

// ErrRecovered wraps a panics.Recovered in an error implementation.
type ErrRecovered struct{ Recovered }

var _ error = (*ErrRecovered)(nil)

func (p *ErrRecovered) Error() string { return p.String() }

func (p *ErrRecovered) Unwrap() error {
	if err, ok := p.Value.(error); ok {
		return err
	}
	return nil
}
