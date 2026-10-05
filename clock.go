// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ntp

import "time"

// A Timer represents a single timed event. It mirrors the behavior of
// time.Timer while allowing implementations driven by a custom Clock, such
// as a controllable fake clock used in tests.
type Timer interface {
	// C returns the channel on which the timer's expiration time is
	// delivered.
	C() <-chan time.Time

	// Stop prevents the timer from firing. It reports false if the timer
	// has already fired or been stopped, as with time.Timer.Stop.
	Stop() bool
}

// A Clock provides the current time and schedules one-shot timers. The
// package uses the system clock by default; tests may substitute a fake
// clock to verify time-dependent behavior (such as RATE cooldowns) without
// waiting in real time.
type Clock interface {
	// Now returns the current time.
	Now() time.Time

	// NewTimer creates a new Timer that sends the current time on its
	// channel after at least the specified duration.
	NewTimer(d time.Duration) Timer
}

// systemClock implements Clock using the standard time package.
type systemClock struct{}

func (systemClock) Now() time.Time {
	return time.Now().UTC()
}

func (systemClock) NewTimer(d time.Duration) Timer {
	return &systemTimer{t: time.NewTimer(d)}
}

// systemTimer adapts a *time.Timer to the Timer interface.
type systemTimer struct {
	t *time.Timer
}

func (t *systemTimer) C() <-chan time.Time { return t.t.C }

func (t *systemTimer) Stop() bool { return t.t.Stop() }
