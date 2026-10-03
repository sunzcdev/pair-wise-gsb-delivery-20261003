package backoff

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestSystemClockIsDefault confirms the public defaults keep using real wall
// clock time and real timers without any injected option.
func TestSystemClockIsDefault(t *testing.T) {
	args := &retryOptions{}
	WithClock(SystemClock)(args)
	if args.Clock != Clock(SystemClock) {
		t.Fatal("SystemClock should be assignable as the Retry clock")
	}

	// A short end-to-end retry on the default path still succeeds and observes
	// real elapsed time.
	start := time.Now()
	calls := 0
	_, err := Retry(context.Background(),
		func() (int, error) {
			calls++
			if calls == 2 {
				return 9, nil
			}
			return 0, errors.New("boom")
		},
		WithBackOff(NewConstantBackOff(time.Millisecond)),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if time.Since(start) < time.Millisecond {
		t.Fatal("default Retry should wait on a real timer")
	}

	// The default ticker constructor uses the real clock too.
	ticker := NewTicker(NewConstantBackOff(time.Millisecond))
	defer ticker.Stop()
	select {
	case <-ticker.C:
	case <-time.After(time.Second):
		t.Fatal("default ticker did not deliver its first tick")
	}
}

// TestNewTickerWithClockHonorsInjectedClock ensures injected clocks drive
// ticks (covered in depth by the virtual-clock boundary tests).
func TestNewTickerWithClockHonorsInjectedClock(t *testing.T) {
	clk := newFakeClock()
	ticker := NewTickerWithClock(NewConstantBackOff(time.Hour), clk)
	defer ticker.Stop()
	<-ticker.C // guaranteed first tick
}
