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
	stopOnce sync.Once
}

// TickerOption configures a Ticker. It is an alias for RetryOption so that
// shared time options, notably WithClock, can be used by both.
type TickerOption = RetryOption

// NewTicker returns a new Ticker containing a channel that will send
// the time at times specified by the BackOff argument. Ticker is
// guaranteed to tick at least once.  The channel is closed when Stop
// method is called or BackOff stops. It is not safe to manipulate the
// provided backoff policy (notably calling NextBackOff or Reset)
// while the ticker is running.
func NewTicker(b BackOff, opts ...TickerOption) *Ticker {
	args := &retryOptions{Clock: SystemClock{}}
	for _, opt := range opts {
		opt(args)
	}
	if args.Clock == nil {
		args.Clock = SystemClock{}
	}

	c := make(chan time.Time)
	t := &Ticker{
		C:     c,
		c:     c,
		b:     b,
		clock: args.Clock,
		stop:  make(chan struct{}),
	}
	t.b.Reset()
	go t.run()
	return t
}

// Stop turns off a ticker. After Stop, no more ticks will be sent.
func (t *Ticker) Stop() {
	t.stopOnce.Do(func() { close(t.stop) })
}

func (t *Ticker) run() {
	c := t.c
	defer close(c)

	// Ticker is guaranteed to tick at least once.
	afterTimer := t.send(t.clock.Now())

	for afterTimer != nil {
		select {
		case tick := <-afterTimer.C():
			afterTimer = t.send(tick)
		case <-t.stop:
			afterTimer.Stop()
			return
		}
	}
}

func (t *Ticker) send(tick time.Time) Timer {
	select {
	case t.c <- tick:
	case <-t.stop:
		return nil
	}

	next := t.b.NextBackOff()
	if next == Stop {
		t.Stop()
		return nil
	}

	return t.clock.NewTimer(next)
}
