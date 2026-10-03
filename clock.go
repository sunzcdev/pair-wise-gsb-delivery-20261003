package backoff

import "time"

// Clock is the source of time used by Retry and Ticker. The default
// implementation wraps the time package; tests can inject a fake clock to
// verify timing behavior deterministically without sleeping.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// NewTimer creates a Timer that fires once after d. Each call returns a
	// fresh Timer, so a value left unread in one Timer's channel can never be
	// delivered by a later wait.
	NewTimer(d time.Duration) Timer
}

// Timer represents a single countdown. A Timer is not reusable: create a new
// one with Clock.NewTimer for every wait. The interface mirrors the relevant
// part of *time.Timer.
type Timer interface {
	// C receives the Timer's firing time, exactly once.
	C() <-chan time.Time
	// Stop prevents the Timer from firing. It returns true if the call stops
	// the timer so it did not fire.
	Stop() bool
}

// SystemClock is the default Clock: it reports wall-clock time and creates
// timers backed by the time package.
var SystemClock Clock = systemClock{}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) NewTimer(d time.Duration) Timer {
	return &systemTimer{timer: time.NewTimer(d)}
}

type systemTimer struct {
	timer *time.Timer
}

func (t *systemTimer) C() <-chan time.Time { return t.timer.C }

func (t *systemTimer) Stop() bool { return t.timer.Stop() }
