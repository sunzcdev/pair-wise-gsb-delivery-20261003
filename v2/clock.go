package gobreaker

import "time"

// Clock is the source of wall-clock time used by CircuitBreaker.
// The default Clock is backed by time.Now.
// A controllable Clock (for example a simulated clock in tests) makes it
// possible to advance bucket boundaries and the open-state timeout without
// sleeping in real time.
//
// Request latency is always measured as the elapsed duration between the
// time captured by the Clock at request start and the time captured by the
// same Clock at request completion. When the default Clock is used, the
// returned time.Time values carry Go's monotonic reading, so Sub is immune
// to wall-clock adjustments. A simulated Clock should return values whose
// Sub reflects the simulated elapsed time.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time {
	return time.Now()
}

var defaultClock Clock = systemClock{}
