package ratelimit

import "time"

// Clock is a source of the current time. Implementations must be safe for
// concurrent use.
type Clock interface {
	Now() time.Time
}

// Func adapts a function into a Clock.
type Func func() time.Time

// Now implements Clock.
func (f Func) Now() time.Time { return f() }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
