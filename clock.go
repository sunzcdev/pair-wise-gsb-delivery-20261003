package backoff

import "time"

// Clock is the source of time used by Retry and Ticker.
type Clock interface {
	// Now returns the current time.
	Now() time.Time

	// NewTimer creates a timer that sends the current time on its channel after
	// at least d. Each timer is independent: stopping or replacing one timer
	// must not leave an event that can wake a later wait.
	NewTimer(d time.Duration) Timer
}

// Timer represents a single timer created by a Clock.
type Timer interface {
	// C returns the channel on which the timer sends its firing time.
	C() <-chan time.Time

	// Stop prevents the timer from firing. It reports whether the call stopped
	// the timer before it fired.
	Stop() bool
}

// SystemClock uses the operating system clock.
type SystemClock struct{}

// Now returns the current operating system time.
func (SystemClock) Now() time.Time {
	return time.Now()
}

// NewTimer creates a timer backed by time.Timer.
func (SystemClock) NewTimer(d time.Duration) Timer {
	return &systemTimer{timer: time.NewTimer(d)}
}

type systemTimer struct {
	timer *time.Timer
}

func (t *systemTimer) C() <-chan time.Time {
	return t.timer.C
}

func (t *systemTimer) Stop() bool {
	return t.timer.Stop()
}
