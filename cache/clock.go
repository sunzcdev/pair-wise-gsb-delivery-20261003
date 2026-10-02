package cache

import "time"

// Clock abstracts time so freshness and eviction can be driven
// deterministically in tests.
type Clock interface {
	Now() time.Time
}

// SystemClock returns the current wall-clock time.
type SystemClock struct{}

// Now implements Clock.
func (SystemClock) Now() time.Time { return time.Now() }

// ManualClock is a Clock whose time is advanced explicitly.
type ManualClock struct {
	t time.Time
}

// NewManualClock returns a ManualClock anchored at the given time.
func NewManualClock(start time.Time) *ManualClock {
	return &ManualClock{t: start}
}

// Now implements Clock.
func (c *ManualClock) Now() time.Time { return c.t }

// Advance moves the clock forward by d.
func (c *ManualClock) Advance(d time.Duration) { c.t = c.t.Add(d) }
