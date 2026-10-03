package backoff

import (
	"sync"
	"time"
)

// Ticker holds a channel that delivers `ticks' of a clock at times reported by a BackOff.
//
// Ticks will continue to arrive when the previous operation is still running,
// so operations that take a while to fail could run in quick succession.
type Ticker struct {
	C        <-chan time.Time
	c        chan time.Time
	b        BackOff
	clock    Clock
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// NewTicker returns a new Ticker containing a channel that will send
// the time at times specified by the BackOff argument. Ticker is
// guaranteed to tick at least once.  The channel is closed when Stop
// method is called or BackOff stops. It is not safe to manipulate the
// provided backoff policy (notably calling NextBackOff or Reset)
// while the ticker is running.
func NewTicker(b BackOff) *Ticker {
	return NewTickerWithClock(b, SystemClock)
}

// NewTickerWithClock works like NewTicker but uses the given Clock as the
// source of time for scheduling ticks. Inject a controllable clock in tests to
// verify timing deterministically without sleeping.
func NewTickerWithClock(b BackOff, clock Clock) *Ticker {
	c := make(chan time.Time)
	t := &Ticker{
		C:     c,
		c:     c,
		b:     b,
		clock: clock,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	t.b.Reset()
	go t.run()
	return t
}

// Stop turns off a ticker. After Stop, no more ticks will be sent.
func (t *Ticker) Stop() {
	t.stopOnce.Do(func() { close(t.stop) })
	<-t.done
}

func (t *Ticker) run() {
	defer close(t.c)
	defer close(t.done)

	var prev Timer
	var pending time.Time
	havePending := true
	pending = t.clock.Now() // Ticker is guaranteed to tick at least once.
	defer func() {
		if prev != nil {
			prev.Stop()
		}
	}()

	for {
		// First deliver the tick carried over from the previous iteration (or
		// the guaranteed initial tick). Delivery is scheduled before the next
		// wait is registered, so a controllable clock that sees no timer yet
		// right after a tick keeps waiting instead of assuming the ticker
		// settled.
		if havePending {
			if !t.deliver(pending) {
				return
			}
			havePending = false
		}

		next := t.b.NextBackOff()
		if next == Stop {
			t.stopOnce.Do(func() { close(t.stop) })
			return
		}

		timer := t.clock.NewTimer(next)
		select {
		case tick := <-timer.C():
			if prev != nil {
				prev.Stop()
			}
			prev = timer
			pending, havePending = tick, true
		case <-t.stop:
			timer.Stop()
			return
		}
	}
}

// deliver sends one tick to C and reports whether it was delivered. It returns
// false if Stop was called while waiting to send.
func (t *Ticker) deliver(tick time.Time) bool {
	select {
	case t.c <- tick:
		return true
	case <-t.stop:
		return false
	}
}
